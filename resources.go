package main

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// ================= Merge requests =================

func mrOf(r *Row) *MR { m, _ := r.Data.(*MR); return m }

func mrRow(m *MR, dashboard bool) Row {
	tc := cFg
	if m.Draft {
		tc = cGray
	}
	pl := Cell{Text: "-", Color: cDim}
	if m.HeadPipeline != nil {
		pl = Cell{Text: statusText(m.HeadPipeline.Status), Color: statusColor(m.HeadPipeline.Status)}
	}
	appr := Cell{Text: "-", Color: cDim}
	switch {
	case m.Approved && len(m.ApprovedBy.Nodes) > 0:
		appr = Cell{Text: fmt.Sprintf("✔ %d", len(m.ApprovedBy.Nodes)), Color: cGreen}
	case m.Approved:
		appr = Cell{Text: "✔", Color: cGreen}
	case m.ApprovalsLeft != nil && *m.ApprovalsLeft > 0:
		appr = Cell{Text: fmt.Sprintf("need %d", *m.ApprovalsLeft), Color: cYellow}
	}
	thr := Cell{Text: "-", Color: cDim}
	if m.ResolvableDiscussionsCount > 0 {
		c := cYellow
		if m.ResolvedDiscussionsCount == m.ResolvableDiscussionsCount {
			c = cGreen
		}
		thr = Cell{Text: fmt.Sprintf("%d/%d", m.ResolvedDiscussionsCount, m.ResolvableDiscussionsCount), Color: c}
	}
	ms, mc := mergeStatus(m.DetailedMergeStatus)
	if m.State != "" && m.State != "opened" {
		ms, mc = m.State, stateColor(m.State)
	}
	author := ""
	if m.Author != nil {
		author = "@" + m.Author.Username
	}
	next := nextCell(m.Turn)
	cells := []Cell{{Text: "!" + m.IID, Color: cAccent2}}
	if dashboard {
		rc := cAccent
		switch m.Role {
		case "review":
			rc = cYellow
		case "assignee":
			rc = cGray
		}
		project := m.Path()
		if i := strings.LastIndex(project, "/"); i >= 0 {
			project = project[i+1:]
		}
		cells = append(cells, Cell{Text: project, Color: cBlue}, Cell{Text: m.Role, Color: rc})
	}
	cells = append(cells, next, Cell{Text: m.Title, Color: tc, Bold: !m.Draft})
	if !dashboard {
		cells = append(cells, Cell{Text: author, Color: cGray})
	}
	cells = append(cells, pl, appr, thr, Cell{Text: ms, Color: mc}, Cell{Text: ago(m.UpdatedAt), Color: cGray})
	return Row{
		Key:    m.Path() + "!" + m.IID,
		Cells:  cells,
		Data:   m,
		Search: strings.Join([]string{"!" + m.IID, m.Title, author, m.SourceBranch, m.Path(), m.Role, ms, next.Text}, " "),
	}
}

func mrCols(dashboard bool) []Column {
	cols := []Column{{Title: "IID", Width: 6}}
	if dashboard {
		cols = append(cols, Column{Title: "PROJECT", Width: 18, Drop: 6}, Column{Title: "ROLE", Width: 8, Drop: 5})
	}
	cols = append(cols, Column{Title: "NEXT", Width: 40}, Column{Title: "TITLE"})
	if !dashboard {
		cols = append(cols, Column{Title: "AUTHOR", Width: 18, Drop: 3})
	}
	return append(cols,
		Column{Title: "PIPELINE", Width: 11, Drop: 7},
		Column{Title: "APPROVED", Width: 8, Drop: 4},
		Column{Title: "THREADS", Width: 7, Drop: 1},
		Column{Title: "MERGE", Width: 14, Drop: 8},
		Column{Title: "AGE", Width: 4, Right: true, Drop: 2},
	)
}

// sortByTurn puts what needs me first, then what needs a ping, then the rest; oldest first.
func sortByTurn(mrs []*MR) {
	sort.SliceStable(mrs, func(i, j int) bool {
		bi, bj := mrs[i].Turn.Bucket(), mrs[j].Turn.Bucket()
		if bi != bj {
			return bi < bj
		}
		return mrs[i].Turn.Since.Before(mrs[j].Turn.Since)
	})
}

// turnSummary is the title badge: "3 on you · 2 to ping".
func turnSummary(mrs []*MR) string {
	mine, late, ping, clean, zz := 0, 0, 0, 0, 0
	for _, m := range mrs {
		switch m.Turn.Bucket() {
		case bucketSnoozed:
			zz++
		case bucketCleanup:
			clean++
		case bucketMineLate:
			late++
			mine++
		case bucketMine:
			mine++
		case bucketPing, bucketReping:
			ping++
		}
	}
	var parts []string
	if mine > 0 {
		c := cOrange
		if late > 0 {
			c = cRed
		}
		parts = append(parts, lipgloss.NewStyle().Foreground(c).Bold(true).Render(fmt.Sprintf("▶ %d on you", mine)))
	}
	if ping > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cYellow).Bold(true).Render(fmt.Sprintf("⏳ %d to ping", ping)))
	}
	if clean > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cAccent2).Bold(true).Render(fmt.Sprintf("🧹 %d to clean up", clean)))
	}
	if zz > 0 {
		parts = append(parts, sGray.Render(fmt.Sprintf("💤 %d snoozed", zz)))
	}
	return strings.Join(parts, sGray.Render(" · "))
}

// focus modes, cycled with "t"
var focusModes = []struct {
	name string
	keep func(t *Turn) bool
}{
	{"all", nil},
	{"on me", func(t *Turn) bool { b := t.Bucket(); return b == bucketMine || b == bucketMineLate }},
	{"to ping", func(t *Turn) bool { return t != nil && t.ToPing() }},
	{"waiting on others", func(t *Turn) bool {
		b := t.Bucket()
		return b == bucketWaiting || b == bucketPing || b == bucketReping
	}},
	{"to clean up", func(t *Turn) bool { return t != nil && len(t.Cleanup) > 0 }},
	{"snoozed", func(t *Turn) bool { return t.Bucket() == bucketSnoozed }},
}

// addFocus wires the "t" key that narrows an MR list to what needs me / what needs a ping.
func addFocus(l *ListView) {
	focus := 0
	cycle := func(a *App) tea.Cmd {
		focus = (focus + 1) % len(focusModes)
		f := focusModes[focus]
		l.predName = ""
		l.pred = nil
		if f.keep != nil {
			l.predName = f.name
			l.pred = func(r Row) bool { return f.keep(mrOf(&r).Turn) }
		}
		l.applyFilter()
		l.cursor, l.offset = 0, 0
		return a.flashOK("showing: " + f.name)
	}
	l.actions = append([]KeyAction{{Keys: []string{"t"}, Help: "focus: all / on me / to ping / waiting on others / to clean up / snoozed", Hint: "focus", Short: true,
		Fn: func(a *App, r *Row) tea.Cmd { return cycle(a) }}}, l.actions...)
	if l.onEmptyKey == nil {
		l.onEmptyKey = map[string]func(a *App) tea.Cmd{}
	}
	l.onEmptyKey["t"] = cycle
}

// doneWithLeftovers: my MRs merged or closed lately that still have a tab, a worktree…
// The cached first draw skips the local checks so it stays instant.
func doneWithLeftovers(gl *Client) []*MR {
	done, err := gl.MyDoneMRs(30)
	if err != nil || gl.offline {
		return nil
	}
	findLeftovers(done)
	var out []*MR
	for _, m := range done {
		if m.Leftover == nil {
			continue
		}
		m.Turn = &Turn{Me: gl.Me, Step: Step{What: m.State}, Cleanup: m.Leftover.Items()}
		for _, t := range m.Leftover.Tabs {
			if tty := t.tty(); tty != "" {
				m.TabTTY = tty
				break
			}
		}
		if m.MergedAt != nil {
			m.Turn.Since = *m.MergedAt
		}
		out = append(out, m)
	}
	return out
}

func newMyMRsView(a *App) *ListView {
	gl := a.gl
	l := &ListView{
		title:    "MyMergeRequests",
		scope:    "@" + gl.Me,
		cols:     mrCols(true),
		interval: 30 * time.Second,
		fetch: func(gl *Client) ([]Row, string, error) {
			mrs, err := gl.MyMRs()
			if err != nil {
				return nil, "", err
			}
			for _, m := range mrs {
				m.Turn = computeTurn(m, gl.Me)
			}
			mrs = append(mrs, doneWithLeftovers(gl)...)
			sortByTurn(mrs)
			rows := make([]Row, 0, len(mrs))
			for _, m := range mrs {
				rows = append(rows, mrRow(m, true))
			}
			return rows, turnSummary(mrs), nil
		},
		onEnter: func(a *App, r *Row) tea.Cmd { return a.push(newMRDetailView(a, mrOf(r))) },
		actions: mrActions(),
	}
	addFocus(l)
	return l
}

var mrStates = []string{"opened", "merged", "closed", "all"}

func newProjectMRsView(a *App, project string) *ListView {
	state := 0
	l := &ListView{
		title:    "MergeRequests",
		scope:    project,
		cols:     mrCols(false),
		interval: 20 * time.Second,
		onEnter:  func(a *App, r *Row) tea.Cmd { return a.push(newMRDetailView(a, mrOf(r))) },
	}
	l.fetch = func(gl *Client) ([]Row, string, error) {
		st := mrStates[state]
		mrs, err := gl.ProjectMRs(project, st)
		if err != nil {
			return nil, "", err
		}
		for _, m := range mrs {
			m.Turn = computeTurn(m, gl.Me)
		}
		if st == "opened" {
			sortByTurn(mrs)
		}
		rows := make([]Row, 0, len(mrs))
		for _, m := range mrs {
			rows = append(rows, mrRow(m, false))
		}
		meta := lipgloss.NewStyle().Foreground(stateColor(st)).Render("<" + st + ">")
		if sum := turnSummary(mrs); sum != "" {
			meta += " " + sum
		}
		return rows, meta, nil
	}
	cycleState := func(a *App) tea.Cmd {
		state = (state + 1) % len(mrStates)
		return l.Refresh(a)
	}
	l.actions = append([]KeyAction{{
		Keys: []string{"s"}, Help: "cycle state (opened/merged/closed/all)", Hint: "state", Short: true,
		Fn: func(a *App, r *Row) tea.Cmd { return cycleState(a) },
	}}, mrActions()...)
	l.onEmptyKey = map[string]func(a *App) tea.Cmd{"s": cycleState}
	addFocus(l)
	return l
}

func mrActions() []KeyAction {
	return []KeyAction{
		{Keys: []string{"d"}, Help: "diff", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			return a.push(newDiffView(a, mrOf(r)))
		}},
		{Keys: []string{"p"}, Help: "pipelines", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			return a.push(newPipelinesView(a, m.Path(), "", m))
		}},
		{Keys: []string{"J"}, Help: "jobs of the head pipeline", Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			if m.HeadPipeline == nil {
				return a.flashErr("this MR has no pipeline")
			}
			return a.push(newJobsView(a, m.Path(), m.HeadPipeline.IID))
		}},
		{Keys: []string{"a"}, Help: "approve", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			return a.confirm(fmt.Sprintf("Approve !%s %q?", m.IID, m.Title), func() tea.Cmd {
				return a.run("approving !"+m.IID, "approved !"+m.IID, func() error { return a.gl.Approve(m.Path(), m.IID) })
			})
		}},
		{Keys: []string{"A"}, Help: "revoke my approval", Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			return a.confirm(fmt.Sprintf("Revoke your approval on !%s?", m.IID), func() tea.Cmd {
				return a.run("revoking approval on !"+m.IID, "approval revoked on !"+m.IID, func() error { return a.gl.Unapprove(m.Path(), m.IID) })
			})
		}},
		{Keys: []string{"m"}, Help: "merge now", Hint: "merge", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			return a.confirm(fmt.Sprintf("Merge !%s %s → %s now?", m.IID, m.SourceBranch, m.TargetBranch), func() tea.Cmd {
				return a.run("merging !"+m.IID, "merged !"+m.IID, func() error { return a.gl.Merge(m.Path(), m.IID, false) })
			})
		}},
		{Keys: []string{"M"}, Help: "merge when pipeline succeeds (auto-merge)", Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			return a.confirm(fmt.Sprintf("Auto-merge !%s when the pipeline succeeds?", m.IID), func() tea.Cmd {
				return a.run("setting auto-merge on !"+m.IID, "auto-merge set on !"+m.IID, func() error { return a.gl.Merge(m.Path(), m.IID, true) })
			})
		}},
		{Keys: []string{"r"}, Help: "rebase on target branch", Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			return a.confirm(fmt.Sprintf("Rebase !%s %s on %s?", m.IID, m.SourceBranch, m.TargetBranch), func() tea.Cmd {
				return a.run("rebasing !"+m.IID, "rebase requested on !"+m.IID, func() error { return a.gl.Rebase(m.Path(), m.IID) })
			})
		}},
		{Keys: []string{"n"}, Help: "nudge: ping the people this MR waits on (prefilled comment)", Hint: "ping", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			t := m.Turn
			switch {
			case t == nil:
				return a.flashErr("still loading")
			case t.Mine:
				return a.flashErr("nobody to ping: it's your turn (" + t.What + ")")
			case len(t.Owners) == 0 || t.Quiet:
				return a.flashErr("nobody to ping: " + t.What)
			}
			return a.textEditorWith(fmt.Sprintf("Ping on !%s %s", m.IID, m.Title), pingMessage(t), func(body string) tea.Cmd {
				return a.run("pinging on !"+m.IID, "ping posted on !"+m.IID, func() error { return a.gl.CommentMR(m.Path(), m.IID, body) })
			})
		}},
		{Keys: []string{"c"}, Help: "comment", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			return a.textEditor(fmt.Sprintf("Comment on !%s %s", m.IID, m.Title), func(body string) tea.Cmd {
				return a.run("commenting on !"+m.IID, "comment posted on !"+m.IID, func() error { return a.gl.CommentMR(m.Path(), m.IID, body) })
			})
		}},
		{Keys: []string{"D"}, Help: "toggle draft", Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			to := !m.Draft
			label := "marked ready"
			if to {
				label = "marked draft"
			}
			q := fmt.Sprintf("Mark !%s ready for review?", m.IID)
			if to {
				q = fmt.Sprintf("Mark !%s as draft?", m.IID)
			}
			return a.confirm(q, func() tea.Cmd {
				return a.run("updating !"+m.IID, "!"+m.IID+" "+label, func() error { return a.gl.SetDraft(m.Path(), m.IID, m.Title, to) })
			})
		}},
		{Keys: []string{"x"}, Help: "close / reopen", Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			ev, verb := "close", "Close"
			if m.State == "closed" {
				ev, verb = "reopen", "Reopen"
			}
			return a.confirm(fmt.Sprintf("%s !%s %q?", verb, m.IID, m.Title), func() tea.Cmd {
				return a.run(strings.ToLower(verb)+" !"+m.IID, "!"+m.IID+" "+ev+"ed", func() error { return a.gl.SetMRState(m.Path(), m.IID, ev) })
			})
		}},
		{Keys: []string{"v"}, Help: "set reviewers (prefilled with the current ones, or your defaults)", Hint: "reviewers", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			var cur []string
			for _, u := range m.Reviewers.Nodes {
				cur = append(cur, "@"+u.Username)
			}
			if len(cur) == 0 {
				for _, u := range reviewersToAdd(m, defaultReviewers(m.Path())) {
					cur = append(cur, "@"+u)
				}
			}
			value := strings.Join(cur, " ")
			if value != "" {
				value += " "
			}
			return a.prompt("reviewers of !"+m.IID+" (space-separated, empty = none)", value, func(s string) tea.Cmd {
				users, err := parseUsernames(s)
				if err != nil {
					return a.flashErr(err.Error())
				}
				done := "reviewers of !" + m.IID + ": " + strings.Join(users, ", ")
				if len(users) == 0 {
					done = "reviewers removed from !" + m.IID
				}
				return a.run("setting reviewers on !"+m.IID, done, func() error {
					return a.gl.SetReviewers(m.Path(), m.IID, users, "REPLACE")
				})
			})
		}},
		{Keys: []string{"V"}, Help: "add your default reviewers (config: reviewers.default)", Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			def := defaultReviewers(m.Path())
			if len(def) == 0 {
				return a.flashErr("no default reviewers: add them with `flw config edit` (reviewers.default)")
			}
			add := reviewersToAdd(m, def)
			if len(add) == 0 {
				return a.flashErr("default reviewers already on !" + m.IID)
			}
			return a.confirm(fmt.Sprintf("Add %s as reviewers of !%s?", "@"+strings.Join(add, ", @"), m.IID), func() tea.Cmd {
				return a.run("adding reviewers to !"+m.IID, "added "+strings.Join(add, ", ")+" to !"+m.IID, func() error {
					return a.gl.SetReviewers(m.Path(), m.IID, add, "APPEND")
				})
			})
		}},
		{Keys: []string{"w"}, Help: "work on it: switch to its Rex tab, or open one with a worktree + Claude session", Hint: "work", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			if !inRex() {
				return a.flashErr("w needs Rex (REX_SESSION not set)")
			}
			a.busy++
			a.flash, a.flashBad, a.flashAt = "opening the tab of !"+m.IID+"…", false, time.Now()
			return func() tea.Msg {
				w, err := openMRWork(m)
				if w == nil {
					return actionDoneMsg{err: err}
				}
				where := fmt.Sprintf("%s session %q, tab %s", w.What, w.SessionLabel, mrTabLabel(m))
				if errors.Is(err, errNoRemote) {
					return actionDoneMsg{err: fmt.Errorf("%s ready, but Rex can't switch to it: %w", where, err)}
				}
				return actionDoneMsg{text: where, err: err}
			}
		}},
		{Keys: []string{"z"}, Help: "snooze until… (demain, lundi, 2h, 14:00, 3d, 2026-11-12); empty wakes it up", Hint: "snooze", Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			value := "demain"
			if u := snoozedUntil(m); !u.IsZero() {
				value = ""
			}
			return a.prompt("snooze !"+m.IID+" until (demain, lundi, 2h, 14:00, 3d…; empty = wake up now)", value, func(s string) tea.Cmd {
				if strings.TrimSpace(s) == "" {
					if err := setSnooze(m, time.Time{}); err != nil {
						return a.flashErr(err.Error())
					}
					a.flashOK("⏰ !" + m.IID + " is back")
					return a.top().Refresh(a)
				}
				until, err := parseWhen(s, time.Now())
				if err != nil {
					return a.flashErr(err.Error())
				}
				if err := setSnooze(m, until); err != nil {
					return a.flashErr(err.Error())
				}
				a.flashOK("💤 !" + m.IID + " snoozed until " + until.Format("Mon 2 Jan 15:04") + ": flw will remind you")
				return a.top().Refresh(a)
			})
		}},
		{Keys: []string{"X"}, Help: "clean up a merged/closed MR: close its tabs, remove its worktree and local branch", Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			if m.State == "opened" || m.State == "" {
				return a.flashErr("!" + m.IID + " is still open: X cleans up merged or closed MRs")
			}
			if !m.Leftover.Any() {
				return a.flashErr("nothing left locally for !" + m.IID)
			}
			do, keep := cleanupPlan(m)
			if len(do) == 0 {
				return a.flashErr("nothing safe to remove: " + strings.Join(keep, "; "))
			}
			q := "Clean up !" + m.IID + "? " + strings.Join(do, "; ")
			if len(keep) > 0 {
				q += ". Kept: " + strings.Join(keep, "; ")
			}
			return a.confirm(q, func() tea.Cmd {
				a.busy++
				a.flash, a.flashBad, a.flashAt = "cleaning up !"+m.IID+"…", false, time.Now()
				return func() tea.Msg {
					msg, err := runCleanup(m)
					return actionDoneMsg{text: msg, err: err, refresh: true}
				}
			})
		}},
		{Keys: []string{"C"}, Help: "checkout branch locally (glab mr checkout)", Fn: func(a *App, r *Row) tea.Cmd {
			m := mrOf(r)
			c := exec.Command("glab", "mr", "checkout", m.IID, "-R", m.Path())
			return a.execCmd("checked out "+m.SourceBranch, c)
		}},
		{Keys: []string{"o"}, Help: "open in browser", Hint: "browser", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			return openCmd(a, mrOf(r).WebURL)
		}},
		{Keys: []string{"y"}, Help: "copy URL", Fn: func(a *App, r *Row) tea.Cmd {
			return copyCmd(a, mrOf(r).WebURL)
		}},
	}
}

func mrAction(k string) func(a *App, r *Row) tea.Cmd {
	for _, act := range mrActions() {
		if act.match(k) {
			return act.Fn
		}
	}
	return nil
}

func openCmd(a *App, u string) tea.Cmd {
	if u == "" {
		return a.flashErr("no URL")
	}
	if err := openURL(u); err != nil {
		copyText(u)
		return a.flashErr("could not open a browser, URL copied instead")
	}
	return a.flashOK("opened " + u)
}

func copyCmd(a *App, s string) tea.Cmd {
	copyText(s)
	return a.flashOK("copied " + s)
}

// ---------- MR detail ----------

var (
	mdMu    sync.Mutex
	mdCache = map[int]*glamour.TermRenderer{}
)

func markdown(s string, w int) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	if w > 120 {
		w = 120
	}
	mdMu.Lock()
	r, ok := mdCache[w]
	if !ok {
		var err error
		r, err = glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(w), glamour.WithEmoji())
		if err != nil {
			mdMu.Unlock()
			return s
		}
		mdCache[w] = r
	}
	out, err := r.Render(s)
	mdMu.Unlock()
	if err != nil {
		return s
	}
	return strings.Trim(out, "\n")
}

func newMRDetailView(a *App, m *MR) *PagerView {
	gl := a.gl
	path, iid := m.Path(), m.IID
	p := newPager("!" + iid)
	p.row = &Row{Data: m, Key: path + "!" + iid}
	showSystem := false
	p.title = func() string {
		cur := mrOf(p.row)
		return sAccent2().Render("!"+iid) + " " + sBold.Render(cur.Title)
	}
	p.fetch = func(gl *Client) (any, error) {
		d, err := gl.MRDetail(path, iid)
		if err != nil {
			return nil, err
		}
		if d.Project == nil {
			d.Project = m.Project
		}
		return d, nil
	}
	p.onData = func(d any) {
		mr := d.(*MR)
		mr.Turn = computeTurn(mr, gl.Me)
		p.row.Data = mr
	}
	p.interval = func() time.Duration {
		if cur := mrOf(p.row); cur.HeadPipeline != nil && isActive(cur.HeadPipeline.Status) {
			return 10 * time.Second
		}
		return 30 * time.Second
	}
	p.render = func(data any, w int) ([]string, []int) {
		return renderMR(data.(*MR), w, showSystem)
	}
	p.acts = append([]KeyAction{{
		Keys: []string{"S"}, Help: "show/hide system notes",
		Fn: func(a *App, r *Row) tea.Cmd { showSystem = !showSystem; p.rerender(); return nil },
	}}, mrActions()...)
	return p
}

func sAccent2() lipgloss.Style { return lipgloss.NewStyle().Foreground(cAccent2).Bold(true) }

func renderMR(m *MR, w int, showSystem bool) ([]string, []int) {
	var b strings.Builder
	lbl := func(s string) string { return sGray.Render(fmt.Sprintf("%-10s", s)) }
	col := func(c lipgloss.Color, s string) string { return lipgloss.NewStyle().Foreground(c).Render(s) }

	state := col(stateColor(m.State), "● "+m.State)
	if m.Draft {
		state += " " + col(cGray, "[draft]")
	}
	b.WriteString("\n " + sBold.Render(m.Title) + "\n")
	b.WriteString(" " + state + "  " + col(cBlue, m.SourceBranch) + sGray.Render(" → ") + col(cBlue, m.TargetBranch) + "\n\n")
	b.WriteString(turnBanner(m.Turn, w) + "\n")

	author := "-"
	if m.Author != nil {
		author = "@" + m.Author.Username
	}
	b.WriteString(" " + lbl("Author") + author + sGray.Render("  opened "+ago(m.CreatedAt)+" ago, updated "+ago(m.UpdatedAt)+" ago") + "\n")
	if len(m.Reviewers.Nodes) > 0 {
		var rv []string
		for _, r := range m.Reviewers.Nodes {
			st := "UNREVIEWED"
			if r.Interaction != nil {
				st = r.Interaction.ReviewState
				if r.Interaction.Approved {
					st = "APPROVED"
				}
			}
			icon, c := "○", cGray
			switch st {
			case "APPROVED":
				icon, c = "✔", cGreen
			case "REQUESTED_CHANGES":
				icon, c = "✘", cRed
			case "REVIEWED":
				icon, c = "✎", cBlue
			case "REVIEW_STARTED":
				icon, c = "◐", cYellow
			}
			rv = append(rv, col(c, icon+" @"+r.Username))
		}
		b.WriteString(" " + lbl("Reviewers") + strings.Join(rv, "  ") + sDim.Render("   (✔ approved ✎ commented ✘ changes ◐ started ○ not yet)") + "\n")
	}
	if as := usernames(m.Assignees); len(as) > 0 {
		b.WriteString(" " + lbl("Assignees") + strings.Join(as, " ") + "\n")
	}
	if m.HeadPipeline != nil {
		b.WriteString(" " + lbl("Pipeline") + col(statusColor(m.HeadPipeline.Status), statusText(m.HeadPipeline.Status)) + sGray.Render(" #"+m.HeadPipeline.IID) + "\n")
	} else {
		b.WriteString(" " + lbl("Pipeline") + sDim.Render("none") + "\n")
	}
	appr := ""
	if m.Approved {
		appr = col(cGreen, "✔ approved")
	} else {
		appr = col(cYellow, "not approved")
	}
	if ab := usernames(m.ApprovedBy); len(ab) > 0 {
		appr += sGray.Render(" by ") + strings.Join(ab, " ")
	}
	if m.ApprovalsLeft != nil && *m.ApprovalsLeft > 0 {
		appr += col(cYellow, fmt.Sprintf("  (%d more needed)", *m.ApprovalsLeft))
	}
	b.WriteString(" " + lbl("Approvals") + appr + "\n")
	ms, mc := mergeStatus(m.DetailedMergeStatus)
	merge := col(mc, ms)
	if m.MergeWhenPipelineSucceeds {
		merge += col(cBlue, "  ⏱ auto-merge set")
	}
	if m.MergeUser != nil && m.State == "merged" {
		merge = col(cAccent, "merged by @"+m.MergeUser.Username)
	}
	b.WriteString(" " + lbl("Merge") + merge + "\n")
	if m.ResolvableDiscussionsCount > 0 {
		c := cYellow
		if m.ResolvedDiscussionsCount == m.ResolvableDiscussionsCount {
			c = cGreen
		}
		b.WriteString(" " + lbl("Threads") + col(c, fmt.Sprintf("%d/%d resolved", m.ResolvedDiscussionsCount, m.ResolvableDiscussionsCount)) + "\n")
	}
	if ds := m.DiffStatsSummary; ds != nil {
		b.WriteString(" " + lbl("Changes") + col(cGreen, fmt.Sprintf("+%d", ds.Additions)) + " " + col(cRed, fmt.Sprintf("−%d", ds.Deletions)) + sGray.Render(fmt.Sprintf("  in %s", plural(ds.FileCount, "file"))) + "\n")
	}
	if len(m.Labels.Nodes) > 0 {
		var ls []string
		for _, l := range m.Labels.Nodes {
			ls = append(ls, lipgloss.NewStyle().Foreground(cAccent2).Render("~"+l.Title))
		}
		b.WriteString(" " + lbl("Labels") + strings.Join(ls, " ") + "\n")
	}

	rule := sDim.Render(" " + strings.Repeat("─", max(1, w-2)))
	b.WriteString("\n" + rule + "\n")
	var anchors []int
	if d := markdown(m.Description, w-2); d != "" {
		b.WriteString(d + "\n")
	} else {
		b.WriteString(" " + sDim.Render("no description") + "\n")
	}

	// discussions
	var threads []Discuss
	for _, d := range m.Discussions.Nodes {
		if len(d.Notes.Nodes) == 0 {
			continue
		}
		if d.Notes.Nodes[0].System && !showSystem {
			continue
		}
		threads = append(threads, d)
	}
	b.WriteString("\n" + rule + "\n")
	b.WriteString(" " + sHeader.Render(fmt.Sprintf("Discussions (%d)", len(threads))) + sDim.Render("   ] / [ to jump between threads") + "\n")
	lines := strings.Split(b.String(), "\n")
	for _, d := range threads {
		anchors = append(anchors, len(lines))
		bar := lipgloss.NewStyle().Foreground(cAccent)
		if d.Resolvable && !d.Resolved {
			bar = lipgloss.NewStyle().Foreground(cYellow)
		} else if d.Resolved {
			bar = lipgloss.NewStyle().Foreground(cGreen)
		}
		lines = append(lines, "")
		for i, n := range d.Notes.Nodes {
			who := "?"
			if n.Author != nil {
				who = "@" + n.Author.Username
			}
			head := sBold.Render(who) + sGray.Render(" · "+ago(n.CreatedAt)+" ago")
			if i == 0 && n.Position != nil && n.Position.FilePath != "" {
				loc := n.Position.FilePath
				if n.Position.NewLine != nil {
					loc += ":" + strconv.Itoa(*n.Position.NewLine)
				} else if n.Position.OldLine != nil {
					loc += ":" + strconv.Itoa(*n.Position.OldLine)
				}
				head += sGray.Render(" on ") + lipgloss.NewStyle().Foreground(cBlue).Render(loc)
			}
			if i == 0 && d.Resolvable {
				if d.Resolved {
					head += " " + lipgloss.NewStyle().Foreground(cGreen).Render("✔ resolved")
				} else {
					head += " " + lipgloss.NewStyle().Foreground(cYellow).Render("● unresolved")
				}
			}
			if i > 0 {
				head = sGray.Render("↳ ") + head
			}
			lines = append(lines, " "+bar.Render("┃ ")+head)
			body := n.Body
			if n.System {
				body = "_" + body + "_"
			}
			for _, l := range strings.Split(markdown(body, w-6), "\n") {
				lines = append(lines, " "+bar.Render("┃ ")+l)
			}
		}
	}
	if len(threads) == 0 {
		lines = append(lines, " "+sDim.Render("no comments yet: press c to add one"))
	}
	lines = append(lines, "")
	return lines, anchors
}

// ---------- diff ----------

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

func newDiffView(a *App, m *MR) *PagerView {
	p := newPager("diff !" + m.IID)
	p.row = &Row{Data: m}
	p.wrappable = true
	p.fetch = func(gl *Client) (any, error) { return gl.MRDiffs(m.Path(), m.IID) }
	p.title = func() string {
		n := 0
		if d, ok := p.data.([]FileDiff); ok {
			n = len(d)
		}
		return sAccent.Bold(true).Render("Diff") + sGray.Render("(") + sAccent2().Render("!"+m.IID) + sGray.Render(fmt.Sprintf(")[%s]", plural(n, "file")))
	}
	p.render = func(data any, w int) ([]string, []int) { return renderDiff(data.([]FileDiff), w) }
	p.acts = []KeyAction{
		{Keys: []string{"c"}, Help: "comment", Fn: mrAction("c")},
		{Keys: []string{"a"}, Help: "approve", Fn: mrAction("a")},
		{Keys: []string{"o"}, Help: "open in browser", Hint: "browser", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			return openCmd(a, mrOf(r).WebURL+"/diffs")
		}},
	}
	return p
}

func renderDiff(files []FileDiff, w int) ([]string, []int) {
	var lines []string
	var anchors []int
	add := lipgloss.NewStyle().Foreground(cGreen)
	del := lipgloss.NewStyle().Foreground(cRed)
	hunk := lipgloss.NewStyle().Foreground(cBlue)
	gut := lipgloss.NewStyle().Foreground(cDim)
	fileHdr := lipgloss.NewStyle().Foreground(lipgloss.Color("#1a1b26")).Background(cAccent).Bold(true)
	totalAdd, totalDel := 0, 0
	for _, f := range files {
		for _, l := range strings.Split(f.Diff, "\n") {
			if strings.HasPrefix(l, "+") {
				totalAdd++
			} else if strings.HasPrefix(l, "-") {
				totalDel++
			}
		}
	}
	lines = append(lines, " "+add.Render(fmt.Sprintf("+%d", totalAdd))+" "+del.Render(fmt.Sprintf("−%d", totalDel))+sGray.Render("  ] / [ next / previous file · w wrap"), "")
	for _, f := range files {
		anchors = append(anchors, len(lines))
		name := f.NewPath
		tag := ""
		switch {
		case f.NewFile:
			tag = " (new)"
		case f.DeletedFile:
			tag = " (deleted)"
			name = f.OldPath
		case f.RenamedFile:
			name = f.OldPath + " → " + f.NewPath
		}
		lines = append(lines, fileHdr.Render(fit(" "+name+tag, w)))
		if strings.TrimSpace(f.Diff) == "" {
			lines = append(lines, sDim.Render("   (binary or empty diff)"), "")
			continue
		}
		oldN, newN := 0, 0
		for _, l := range strings.Split(strings.TrimRight(f.Diff, "\n"), "\n") {
			if mm := hunkRe.FindStringSubmatch(l); mm != nil {
				oldN, _ = strconv.Atoi(mm[1])
				newN, _ = strconv.Atoi(mm[2])
				lines = append(lines, gut.Render("          │")+hunk.Render(l))
				continue
			}
			l = strings.ReplaceAll(l, "\t", "    ")
			switch {
			case strings.HasPrefix(l, "+"):
				lines = append(lines, gut.Render(fmt.Sprintf("     %4d │", newN))+add.Render(l))
				newN++
			case strings.HasPrefix(l, "-"):
				lines = append(lines, gut.Render(fmt.Sprintf("%4d      │", oldN))+del.Render(l))
				oldN++
			case strings.HasPrefix(l, `\`):
				lines = append(lines, gut.Render("          │")+sDim.Render(l))
			default:
				lines = append(lines, gut.Render(fmt.Sprintf("%4d %4d │", oldN, newN))+l)
				oldN++
				newN++
			}
		}
		lines = append(lines, "")
	}
	if len(files) == 0 {
		lines = append(lines, sDim.Render(" no changes"))
	}
	return lines, anchors
}

// ================= Pipelines =================

func plOf(r *Row) Pipeline { p, _ := r.Data.(Pipeline); return p }

func newPipelinesView(a *App, project, ref string, mr *MR) *ListView {
	gl := a.gl
	scope := project
	if mr != nil {
		scope = "!" + mr.IID
	} else if ref != "" {
		scope = project + "@" + ref
	}
	l := &ListView{
		title:    "Pipelines",
		scope:    scope,
		interval: 20 * time.Second,
		fast:     4 * time.Second,
		isActive: func(rows []Row) bool {
			for _, r := range rows {
				if isActive(plOf(&r).Status) {
					return true
				}
			}
			return false
		},
		cols: []Column{
			{Title: "#", Width: 7},
			{Title: "STATUS", Width: 12},
			{Title: "REF", Width: 30, Drop: 5},
			{Title: "COMMIT"},
			{Title: "SOURCE", Width: 14, Drop: 1},
			{Title: "USER", Width: 18, Drop: 2},
			{Title: "DURATION", Width: 8, Right: true, Drop: 3},
			{Title: "AGE", Width: 4, Right: true, Drop: 4},
		},
	}
	l.fetch = func(gl *Client) ([]Row, string, error) {
		var pls []Pipeline
		var err error
		if mr != nil {
			pls, err = gl.MRPipelines(project, mr.IID)
		} else {
			pls, err = gl.Pipelines(project, ref)
		}
		if err != nil {
			return nil, "", err
		}
		rows := make([]Row, 0, len(pls))
		for _, p := range pls {
			commit, user := "", ""
			if p.Commit != nil {
				commit = p.Commit.Title
			}
			if p.User != nil {
				user = "@" + p.User.Username
			}
			sha := p.Sha
			if len(sha) > 8 {
				sha = sha[:8]
			}
			if commit == "" {
				commit = sha
			}
			rows = append(rows, Row{
				Key: p.Num(),
				Cells: []Cell{
					{Text: "#" + p.IID, Color: cAccent2},
					{Text: statusText(p.Status), Color: statusColor(p.Status)},
					{Text: strings.TrimPrefix(p.Ref, "refs/"), Color: cBlue},
					{Text: commit},
					{Text: strings.ReplaceAll(p.Source, "_", " "), Color: cGray},
					{Text: user, Color: cGray},
					{Text: dur(p.Duration), Color: cGray},
					{Text: ago(p.CreatedAt), Color: cGray},
				},
				Data:   p,
				Search: strings.Join([]string{"#" + p.IID, p.Status, p.Ref, commit, user, p.Source, sha}, " "),
			})
		}
		return rows, "", nil
	}
	l.onEnter = func(a *App, r *Row) tea.Cmd {
		p := plOf(r)
		return a.push(newJobsView(a, p.Project, p.IID))
	}
	l.actions = []KeyAction{
		{Keys: []string{"r"}, Help: "retry failed jobs", Hint: "retry", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			p := plOf(r)
			return a.confirm("Retry the failed jobs of pipeline #"+p.IID+"?", func() tea.Cmd {
				return a.run("retrying pipeline #"+p.IID, "pipeline #"+p.IID+" retried", func() error { return gl.PipelineAction(p.Project, p.Num(), "retry") })
			})
		}},
		{Keys: []string{"x"}, Help: "cancel", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			p := plOf(r)
			return a.confirm("Cancel pipeline #"+p.IID+"?", func() tea.Cmd {
				return a.run("cancelling pipeline #"+p.IID, "pipeline #"+p.IID+" cancelled", func() error { return gl.PipelineAction(p.Project, p.Num(), "cancel") })
			})
		}},
		{Keys: []string{"n"}, Help: "run a new pipeline", Hint: "new", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			return newPipelinePrompt(a, project, ref, mr)
		}},
		{Keys: []string{"o"}, Help: "open in browser", Hint: "browser", Short: true, Fn: func(a *App, r *Row) tea.Cmd { return openCmd(a, plOf(r).WebURL) }},
		{Keys: []string{"y"}, Help: "copy URL", Fn: func(a *App, r *Row) tea.Cmd { return copyCmd(a, plOf(r).WebURL) }},
	}
	l.onEmptyKey = map[string]func(a *App) tea.Cmd{"n": func(a *App) tea.Cmd { return newPipelinePrompt(a, project, ref, mr) }}
	return l
}

func newPipelinePrompt(a *App, project, ref string, mr *MR) tea.Cmd {
	def := ref
	if mr != nil {
		def = mr.SourceBranch
	}
	return a.prompt("Run pipeline on ref (empty = default branch):", def, func(v string) tea.Cmd {
		gl := a.gl
		return a.run("creating pipeline", "pipeline created", func() error {
			if v == "" {
				v = gl.DefaultBranch(project)
			}
			_, err := gl.NewPipeline(project, v)
			return err
		})
	})
}

// ================= Jobs =================

func jobOf(r *Row) Job { j, _ := r.Data.(Job); return j }

func newJobsView(a *App, project, pipelineIID string) *ListView {
	gl := a.gl
	var mu sync.Mutex
	plStatus := ""
	l := &ListView{
		title:    "Jobs",
		scope:    project + " #" + pipelineIID,
		interval: 20 * time.Second,
		fast:     3 * time.Second,
		cols: []Column{
			{Title: "STAGE", Width: 18},
			{Title: "NAME"},
			{Title: "STATUS", Width: 14},
			{Title: "DURATION", Width: 8, Right: true},
			{Title: "", Width: 16, Drop: 1},
		},
	}
	l.isActive = func([]Row) bool { mu.Lock(); defer mu.Unlock(); return isActive(plStatus) }
	l.fetch = func(gl *Client) ([]Row, string, error) {
		res, err := gl.Jobs(project, pipelineIID)
		if err != nil {
			return nil, "", err
		}
		mu.Lock()
		plStatus = res.Status
		mu.Unlock()
		rows := make([]Row, 0, len(res.Jobs))
		prevStage := ""
		for _, j := range res.Jobs {
			stage := j.Stage
			sc := cFg
			if stage == prevStage {
				sc = cDim
			}
			prevStage = j.Stage
			var flags []string
			if j.Downstream != nil {
				flags = append(flags, "↳ downstream")
			}
			if j.AllowFailure {
				flags = append(flags, "allow fail")
			}
			if j.ManualJob && j.Downstream == nil {
				flags = append(flags, "manual")
			}
			rows = append(rows, Row{
				Key: j.Num(),
				Cells: []Cell{
					{Text: stage, Color: sc},
					{Text: j.Name, Bold: true},
					{Text: statusText(j.Status), Color: statusColor(j.Status)},
					{Text: dur(j.Duration), Color: cGray},
					{Text: strings.Join(flags, " "), Color: cGray},
				},
				Data:   j,
				Search: strings.Join([]string{j.Stage, j.Name, j.Status}, " "),
			})
		}
		meta := lipgloss.NewStyle().Foreground(statusColor(res.Status)).Render(statusText(res.Status))
		return rows, meta, nil
	}
	openLog := func(a *App, r *Row) tea.Cmd {
		j := jobOf(r)
		if j.Downstream != nil {
			return a.push(newJobsView(a, j.Downstream.Project.FullPath, j.Downstream.IID))
		}
		return a.push(newLogView(a, j))
	}
	l.onEnter = openLog
	l.actions = []KeyAction{
		{Keys: []string{"l"}, Help: "logs", Short: true, Fn: openLog},
		{Keys: []string{"r"}, Help: "retry job", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			j := jobOf(r)
			return a.confirm("Retry "+j.Name+"?", func() tea.Cmd {
				return a.run("retrying "+j.Name, j.Name+" retried", func() error { return gl.JobAction(project, j.Num(), "retry") })
			})
		}},
		{Keys: []string{"p"}, Help: "play manual job", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			j := jobOf(r)
			if !j.Playable {
				return a.flashErr(j.Name + " is not a manual job that can be played")
			}
			return a.confirm("Play "+j.Name+"?", func() tea.Cmd {
				return a.run("starting "+j.Name, j.Name+" started", func() error { return gl.JobAction(project, j.Num(), "play") })
			})
		}},
		{Keys: []string{"x"}, Help: "cancel job", Fn: func(a *App, r *Row) tea.Cmd {
			j := jobOf(r)
			return a.confirm("Cancel "+j.Name+"?", func() tea.Cmd {
				return a.run("cancelling "+j.Name, j.Name+" cancelled", func() error { return gl.JobAction(project, j.Num(), "cancel") })
			})
		}},
		{Keys: []string{"R"}, Help: "retry all failed jobs of the pipeline", Fn: func(a *App, r *Row) tea.Cmd {
			return a.confirm("Retry the failed jobs of pipeline #"+pipelineIID+"?", func() tea.Cmd {
				return a.run("retrying pipeline", "pipeline retried", func() error { return retryPipelineByIID(gl, project, pipelineIID) })
			})
		}},
		{Keys: []string{"o"}, Help: "open in browser", Hint: "browser", Short: true, Fn: func(a *App, r *Row) tea.Cmd { return openCmd(a, jobOf(r).WebURL) }},
		{Keys: []string{"y"}, Help: "copy URL", Fn: func(a *App, r *Row) tea.Cmd { return copyCmd(a, jobOf(r).WebURL) }},
	}
	return l
}

func retryPipelineByIID(gl *Client, project, iid string) error {
	var r struct {
		Project *struct {
			Pipeline *struct {
				ID string `json:"id"`
			} `json:"pipeline"`
		} `json:"project"`
	}
	if err := gl.GQL(`query($p: ID!, $iid: ID!) { project(fullPath: $p) { pipeline(iid: $iid) { id } } }`, map[string]string{"p": project, "iid": iid}, &r); err != nil {
		return err
	}
	if r.Project == nil || r.Project.Pipeline == nil {
		return fmt.Errorf("pipeline not found")
	}
	return gl.PipelineAction(project, gidNum(r.Project.Pipeline.ID), "retry")
}

// ---------- job log ----------

type logData struct {
	lines  []string
	status string
}

func newLogView(a *App, j Job) *PagerView {
	gl := a.gl
	p := newPager("log " + j.Name)
	p.row = &Row{Data: j}
	p.followable, p.wrappable = true, true
	p.follow = isActive(j.Status)
	p.bottomOnLoad = true
	status := j.Status
	var mu sync.Mutex
	p.fetch = func(gl *Client) (any, error) {
		var (
			tr  string
			st  string
			err error
			wg  sync.WaitGroup
			e2  error
		)
		wg.Add(2)
		go func() { defer wg.Done(); tr, err = gl.Trace(j.Project, j.Num()) }()
		go func() { defer wg.Done(); st, e2 = gl.JobStatus(j.Project, j.Num()) }()
		wg.Wait()
		if err != nil {
			return nil, err
		}
		if e2 == nil {
			mu.Lock()
			status = st
			mu.Unlock()
		}
		return &logData{lines: cleanTrace(tr), status: st}, nil
	}
	p.interval = func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		if isActive(status) {
			return 2 * time.Second
		}
		return 0
	}
	p.title = func() string {
		mu.Lock()
		st := status
		mu.Unlock()
		return sAccent.Bold(true).Render(j.Name) + " " + lipgloss.NewStyle().Foreground(statusColor(st)).Render(statusText(st)) + sGray.Render(" "+j.Stage)
	}
	p.render = func(data any, w int) ([]string, []int) {
		d := data.(*logData)
		if len(d.lines) == 0 {
			return []string{sDim.Render(" (no log yet)")}, nil
		}
		return d.lines, nil
	}
	p.acts = []KeyAction{
		{Keys: []string{"r"}, Help: "retry job", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			return a.confirm("Retry "+j.Name+"?", func() tea.Cmd {
				return a.run("retrying "+j.Name, j.Name+" retried (see the jobs list)", func() error { return gl.JobAction(j.Project, j.Num(), "retry") })
			})
		}},
		{Keys: []string{"x"}, Help: "cancel job", Fn: func(a *App, r *Row) tea.Cmd {
			return a.confirm("Cancel "+j.Name+"?", func() tea.Cmd {
				return a.run("cancelling "+j.Name, j.Name+" cancelled", func() error { return gl.JobAction(j.Project, j.Num(), "cancel") })
			})
		}},
		{Keys: []string{"o"}, Help: "open in browser", Hint: "browser", Short: true, Fn: func(a *App, r *Row) tea.Cmd { return openCmd(a, j.WebURL) }},
		{Keys: []string{"y"}, Help: "copy URL", Fn: func(a *App, r *Row) tea.Cmd { return copyCmd(a, j.WebURL) }},
	}
	return p
}

// ================= Projects =================

func newProjectsView(a *App) *ListView {
	l := &ListView{
		title:    "Projects",
		interval: 5 * time.Minute,
		cols: []Column{
			{Title: "PATH"},
			{Title: "DESCRIPTION", Drop: 3},
			{Title: "MRS", Width: 4, Right: true},
			{Title: "ISSUES", Width: 6, Right: true, Drop: 2},
			{Title: "★", Width: 3, Right: true, Drop: 1},
			{Title: "ACTIVITY", Width: 8, Right: true},
		},
	}
	l.fetch = func(gl *Client) ([]Row, string, error) {
		ps, err := gl.Projects()
		if err != nil {
			return nil, "", err
		}
		rows := make([]Row, 0, len(ps))
		for _, p := range ps {
			mc := cGray
			if p.OpenMergeRequestsCount > 0 {
				mc = cYellow
			}
			rows = append(rows, Row{
				Key: p.FullPath,
				Cells: []Cell{
					{Text: p.FullPath, Color: cBlue, Bold: true},
					{Text: firstLine(p.Description), Color: cGray},
					{Text: strconv.Itoa(p.OpenMergeRequestsCount), Color: mc},
					{Text: strconv.Itoa(p.OpenIssuesCount), Color: cGray},
					{Text: strconv.Itoa(p.StarCount), Color: cGray},
					{Text: ago(p.LastActivityAt), Color: cGray},
				},
				Data:   p,
				Search: p.FullPath + " " + p.Name + " " + p.Description,
			})
		}
		return rows, "", nil
	}
	sel := func(a *App, r *Row) string {
		p, _ := r.Data.(Project)
		a.project = p.FullPath
		return p.FullPath
	}
	l.onEnter = func(a *App, r *Row) tea.Cmd { return a.push(newProjectMRsView(a, sel(a, r))) }
	l.actions = []KeyAction{
		{Keys: []string{"m"}, Help: "merge requests", Short: true, Fn: func(a *App, r *Row) tea.Cmd { return a.push(newProjectMRsView(a, sel(a, r))) }},
		{Keys: []string{"p"}, Help: "pipelines", Short: true, Fn: func(a *App, r *Row) tea.Cmd { return a.push(newPipelinesView(a, sel(a, r), "", nil)) }},
		{Keys: []string{"i"}, Help: "issues", Short: true, Fn: func(a *App, r *Row) tea.Cmd { return a.push(newIssuesView(a, sel(a, r))) }},
		{Keys: []string{"o"}, Help: "open in browser", Hint: "browser", Short: true, Fn: func(a *App, r *Row) tea.Cmd { p, _ := r.Data.(Project); return openCmd(a, p.WebURL) }},
		{Keys: []string{"y"}, Help: "copy URL", Fn: func(a *App, r *Row) tea.Cmd { p, _ := r.Data.(Project); return copyCmd(a, p.WebURL) }},
	}
	return l
}

// ================= Issues =================

var issueStates = []string{"opened", "closed", "all"}

func issueOf(r *Row) Issue { i, _ := r.Data.(Issue); return i }

func newIssuesView(a *App, project string) *ListView {
	gl := a.gl
	state := 0
	l := &ListView{
		title:    "Issues",
		scope:    project,
		interval: 60 * time.Second,
		cols: []Column{
			{Title: "IID", Width: 6},
			{Title: "TITLE"},
			{Title: "AUTHOR", Width: 18, Drop: 4},
			{Title: "ASSIGNEES", Width: 20, Drop: 2},
			{Title: "LABELS", Width: 24, Drop: 3},
			{Title: "💬", Width: 3, Right: true, Drop: 1},
			{Title: "AGE", Width: 4, Right: true},
		},
	}
	l.fetch = func(gl *Client) ([]Row, string, error) {
		st := issueStates[state]
		is, err := gl.Issues(project, st)
		if err != nil {
			return nil, "", err
		}
		rows := make([]Row, 0, len(is))
		for _, i := range is {
			author := ""
			if i.Author != nil {
				author = "@" + i.Author.Username
			}
			var labels []string
			for _, lb := range i.Labels.Nodes {
				labels = append(labels, "~"+lb.Title)
			}
			rows = append(rows, Row{
				Key: i.IID,
				Cells: []Cell{
					{Text: "#" + i.IID, Color: stateColor(i.State)},
					{Text: i.Title, Bold: true},
					{Text: author, Color: cGray},
					{Text: strings.Join(usernames(i.Assignees), " "), Color: cGray},
					{Text: strings.Join(labels, " "), Color: cAccent2},
					{Text: strconv.Itoa(i.UserNotesCount), Color: cGray},
					{Text: ago(i.UpdatedAt), Color: cGray},
				},
				Data:   i,
				Search: strings.Join([]string{"#" + i.IID, i.Title, author, strings.Join(labels, " ")}, " "),
			})
		}
		return rows, lipgloss.NewStyle().Foreground(stateColor(st)).Render("<" + st + ">"), nil
	}
	cycle := func(a *App) tea.Cmd { state = (state + 1) % len(issueStates); return l.Refresh(a) }
	l.onEnter = func(a *App, r *Row) tea.Cmd { return openCmd(a, issueOf(r).WebURL) }
	l.actions = []KeyAction{
		{Keys: []string{"s"}, Help: "cycle state (opened/closed/all)", Hint: "state", Short: true, Fn: func(a *App, r *Row) tea.Cmd { return cycle(a) }},
		{Keys: []string{"c"}, Help: "comment", Short: true, Fn: func(a *App, r *Row) tea.Cmd {
			i := issueOf(r)
			return a.textEditor("Comment on #"+i.IID+" "+i.Title, func(body string) tea.Cmd {
				return a.run("commenting on #"+i.IID, "comment posted on #"+i.IID, func() error { return gl.CommentIssue(project, i.IID, body) })
			})
		}},
		{Keys: []string{"x"}, Help: "close / reopen", Fn: func(a *App, r *Row) tea.Cmd {
			i := issueOf(r)
			ev, verb := "close", "Close"
			if i.State == "closed" {
				ev, verb = "reopen", "Reopen"
			}
			return a.confirm(fmt.Sprintf("%s #%s %q?", verb, i.IID, i.Title), func() tea.Cmd {
				return a.run(strings.ToLower(verb)+" #"+i.IID, "#"+i.IID+" "+ev+"ed", func() error { return gl.SetIssueState(project, i.IID, ev) })
			})
		}},
		{Keys: []string{"o"}, Help: "open in browser", Hint: "browser", Short: true, Fn: func(a *App, r *Row) tea.Cmd { return openCmd(a, issueOf(r).WebURL) }},
		{Keys: []string{"y"}, Help: "copy URL", Fn: func(a *App, r *Row) tea.Cmd { return copyCmd(a, issueOf(r).WebURL) }},
	}
	l.onEmptyKey = map[string]func(a *App) tea.Cmd{"s": cycle}
	return l
}

// turnBanner is the "whose move is it" block at the top of the MR details.
func turnBanner(t *Turn, w int) string {
	if t == nil {
		return ""
	}
	since := ""
	if !t.Since.IsZero() && !t.Quiet {
		since = " for " + bizAgo(t.Since) + " (business time)"
	}
	var head string
	var c lipgloss.Color
	switch {
	case t.Mine && t.Level > 0:
		head, c = "▶ YOUR TURN: "+t.What+since+", others are blocked", cRed
	case t.Mine:
		head, c = "▶ YOUR TURN: "+t.What+since, cOrange
	case len(t.Owners) == 0 || t.Quiet:
		head, c = "· nothing to do: "+t.What, cGray
		if len(t.Owners) > 0 {
			head = "· " + ownersText(t.Owners) + ": " + t.What
		}
	default:
		var at []string
		for _, o := range t.Owners {
			at = append(at, "@"+o)
		}
		head, c = "⏳ WAITING ON "+strings.Join(at, " ")+": "+t.What+since, cBlue
		switch t.Level {
		case 2:
			head += ", ping again (n)"
			c = cRed
		case 1:
			head += ", time to ping (n)"
			c = cYellow
		}
		if t.Pings > 0 {
			head += fmt.Sprintf(" · pinged %d× (last %s ago)", t.Pings, bizAgo(t.LastPing))
		}
	}
	bar := lipgloss.NewStyle().Foreground(c).Bold(true)
	out := " " + bar.Render("┃ ") + bar.Render(head)
	for _, o := range t.Others {
		who := ownersText(o.Owners)
		if contains(o.Owners, t.Me) {
			who = "you:"
		}
		if who == "" {
			who = "·"
		}
		out += "\n " + bar.Render("┃ ") + sGray.Render("also: "+who+" "+o.What)
	}
	return out + "\n"
}
