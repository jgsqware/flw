package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// While flw is open it watches your MRs and sends a desktop notification (OSC 777, which
// Rex, Ghostty, WezTerm, foot… forward to the OS) when something changes.

// Notification kinds, as used in the config (notify.events).
const (
	nComment  = "comment"  // someone else commented
	nPipeline = "pipeline" // head pipeline finished (success, failed, canceled)
	nApproval = "approval" // approved, or changes requested
	nTurn     = "turn"     // the next step became yours
	nPing     = "ping"     // someone you wait on is due for a ping / re-ping
	nConflict = "conflict" // the MR now has conflicts
	nMerged   = "merged"   // left your list: merged or closed
	nSnooze   = "snooze"   // a snoozed MR is back (always told)
)

var allNotify = []string{nComment, nPipeline, nApproval, nTurn, nPing, nConflict, nMerged}

type notifyMsg struct {
	mrs  []*MR
	done []*MR // merged/closed with local leftovers
	err  error
}

type notice struct {
	kind, title, body string
	id                string // activity record of the MR
}

// mrSnap is what we compare between two polls.
type mrSnap struct {
	title, project string
	lastNote       time.Time
	pipeline       string
	approvedBy     map[string]bool
	changes        map[string]bool
	mine           bool
	what           string
	pingLevel      int
	conflict       bool
}

type notifier struct {
	me    string
	snaps map[string]mrSnap
	ready bool
	last  time.Time
}

func notifyEnabled(kind string) bool {
	if !cfg.Notify.enabled() {
		return false
	}
	if kind == nSnooze {
		return true // you asked for it
	}
	if len(cfg.Notify.Events) == 0 {
		return true
	}
	for _, e := range cfg.Notify.Events {
		if e == kind {
			return true
		}
	}
	return false
}

func notifyEvery() time.Duration {
	if d, err := time.ParseDuration(cfg.Notify.Every); err == nil && d >= 15*time.Second {
		return d
	}
	return time.Minute
}

// pollNotify fetches my MRs: from the cache when the daemon refreshed it recently, else live.
func pollNotify(gl *Client) tea.Cmd {
	return func() tea.Msg {
		cc := gl.Cached()
		mrs, err := cc.MyMRs()
		if err != nil || time.Since(cc.CachedAt()) >= notifyEvery() {
			if mrs, err = gl.MyMRs(); err != nil {
				return notifyMsg{err: err}
			}
		}
		var done []*MR
		if inRex() && cfg.Notify.activityOn() {
			done = doneWithLeftovers(gl)
			attachTabs(mrs)
		}
		return notifyMsg{mrs: mrs, done: done}
	}
}

func snapOf(m *MR, me string) mrSnap {
	s := mrSnap{title: m.Title, project: m.Path(), approvedBy: map[string]bool{}, changes: map[string]bool{}, conflict: m.Conflicts}
	for _, n := range m.Timeline.Nodes {
		if !n.System && n.Author != nil && n.Author.Username != me && n.CreatedAt.After(s.lastNote) {
			s.lastNote = n.CreatedAt
		}
	}
	if p := m.HeadPipeline; p != nil {
		s.pipeline = strings.ToLower(p.Status)
	}
	for _, u := range m.ApprovedBy.Nodes {
		s.approvedBy[u.Username] = true
	}
	for _, r := range m.Reviewers.Nodes {
		if r.Interaction != nil && r.Interaction.ReviewState == "REQUESTED_CHANGES" {
			s.changes[r.Username] = true
		}
	}
	if t := m.Turn; t != nil {
		s.mine, s.what = t.Mine, t.What
		if !t.Mine && !t.Quiet {
			s.pingLevel = t.Level
		}
	}
	return s
}

// diff compares the new poll with the previous one and returns what to tell.
func (n *notifier) diff(mrs []*MR, me string) []notice {
	n.me = me
	next := map[string]mrSnap{}
	byKey := map[string]*MR{}
	for _, m := range mrs {
		if m.Turn == nil {
			m.Turn = computeTurn(m, me)
		}
		k := m.Path() + "!" + m.IID
		next[k] = snapOf(m, me)
		byKey[k] = m
	}
	var out []notice
	if n.ready {
		keys := make([]string, 0, len(next))
		for k := range next {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			old, ok := n.snaps[k]
			if !ok {
				continue // new in the list: no history to compare
			}
			out = append(out, changes(byKey[k], old, next[k], me)...)
		}
		for k, old := range n.snaps {
			if _, ok := next[k]; !ok {
				iid := k[strings.LastIndex(k, "!"):]
				out = append(out, notice{kind: nMerged, title: iid + " " + shortScope(old.project), body: "no longer open (merged or closed): " + old.title})
			}
		}
	}
	n.snaps, n.ready, n.last = next, true, time.Now()
	snoozed := map[string]bool{}
	for _, m := range mrs {
		if m.Turn.Bucket() == bucketSnoozed {
			snoozed[activityID(m)] = true
		}
	}
	var kept []notice
	for _, x := range out {
		if snoozed[x.id] && (x.kind == nTurn || x.kind == nPing) {
			continue // it will be told when the snooze ends
		}
		if notifyEnabled(x.kind) {
			kept = append(kept, x)
		}
	}
	return append(kept, n.wakeUps(mrs)...)
}

// wakeUps: snoozed MRs whose time has come, with what they wait for now.
func (n *notifier) wakeUps(mrs []*MR) []notice {
	due := dueSnoozes(time.Now())
	if len(due) == 0 {
		return nil
	}
	byKey := map[string]*MR{}
	for _, m := range mrs {
		byKey[mrKey(m)] = m
	}
	var out []notice
	for k, e := range due {
		m := byKey[k]
		if m == nil {
			iid := k[strings.LastIndex(k, "!"):]
			out = append(out, notice{kind: nSnooze, title: iid + " " + shortScope(k[:strings.LastIndex(k, "!")]), body: "⏰ reminder: " + e.Title})
			continue
		}
		m.Turn = computeTurn(m, n.me) // the snooze is over: recompute
		body := "⏰ reminder: " + nextCell(m.Turn).Text
		if m.Turn.ToPing() || (!m.Turn.Mine && len(m.Turn.Owners) > 0 && !m.Turn.Quiet) {
			body += " (n in flw to ping)"
		}
		out = append(out, notice{nSnooze, "!" + m.IID + " " + shortScope(m.Path()), body, activityID(m)})
	}
	return out
}

func changes(m *MR, old, cur mrSnap, me string) []notice {
	title := "!" + m.IID + " " + shortScope(m.Path())
	var out []notice
	add := func(kind, body string) { out = append(out, notice{kind, title, body, activityID(m)}) }

	if cur.lastNote.After(old.lastNote) {
		var who []string
		last := ""
		for _, nt := range m.Timeline.Nodes {
			if nt.System || nt.Author == nil || nt.Author.Username == me || !nt.CreatedAt.After(old.lastNote) {
				continue
			}
			if !contains(who, nt.Author.Username) {
				who = append(who, nt.Author.Username)
			}
			last = nt.Body
		}
		if len(who) > 0 {
			add(nComment, fmt.Sprintf("💬 @%s: %s", strings.Join(who, ", @"), oneLine(last, 120)))
		}
	}
	if cur.pipeline != old.pipeline {
		switch cur.pipeline {
		case "success":
			add(nPipeline, "✅ pipeline passed")
		case "failed":
			add(nPipeline, "❌ pipeline failed")
		case "canceled":
			add(nPipeline, "⊘ pipeline canceled")
		}
	}
	for u := range cur.approvedBy {
		if !old.approvedBy[u] && u != me {
			add(nApproval, "👍 approved by @"+u)
		}
	}
	for u := range cur.changes {
		if !old.changes[u] && u != me {
			add(nApproval, "✋ @"+u+" requested changes")
		}
	}
	if cur.mine && (!old.mine || cur.what != old.what) {
		add(nTurn, "▶ your turn: "+cur.what)
	}
	if !cur.mine && cur.pingLevel > old.pingLevel && m.Turn != nil {
		verb := "time to ping"
		if cur.pingLevel == 2 {
			verb = "time to ping again"
		}
		add(nPing, fmt.Sprintf("⏳ %s: %s, %s", ownersText(m.Turn.Owners), m.Turn.What, verb))
	}
	if cur.conflict && !old.conflict {
		add(nConflict, "⚠ conflict with "+m.TargetBranch)
	}
	return out
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// desktopSeq is an OSC 777 desktop notification.
func desktopSeq(title, body string) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f || r == ';' {
				return ' '
			}
			return r
		}, s)
	}
	return "\x1b]777;notify;" + clean("flw · "+title) + ";" + clean(body) + "\x07"
}

// ---------- Rex Activity (OSC 7501, the Program Status Protocol) ----------

// statusSeq builds one OSC 7501 report. title and msg are base64, as the protocol asks.
func statusSeq(id, state, kind, title, msg string) string {
	f := []string{"state=" + state}
	if state != "clear" {
		f = append(f, "app=flw")
	}
	if id != "" {
		f = append(f, "id="+id)
	}
	if kind != "" {
		f = append(f, "kind="+kind)
	}
	b64 := func(s string, max int) string {
		s = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return ' '
			}
			return r
		}, s)
		for len(s) > max {
			_, n := utf8.DecodeLastRuneInString(s)
			s = s[:len(s)-n]
		}
		return base64.StdEncoding.EncodeToString([]byte(s))
	}
	if title != "" {
		f = append(f, "title="+b64(title, 192))
	}
	if msg != "" {
		f = append(f, "msg="+b64(msg, 2048))
	}
	return "\x1b]7501;" + strings.Join(f, ":") + "\x1b\\"
}

var activityIDRe = regexp.MustCompile(`[^A-Za-z0-9_.+-]`)

func activityID(m *MR) string {
	id := activityIDRe.ReplaceAllString(filepath.Base(m.Path())+"-"+m.IID, "-")
	if len(id) > 32 {
		id = id[len(id)-32:]
	}
	return id
}

// activity keeps one Activity record per MR that needs you, changed lately or waits for a
// clean-up, plus a summary. A record goes to the terminal of the MR's own Rex tab when it
// has one, so clicking it in Rex opens that tab; else to flw's terminal.
type activity struct {
	shown  map[string]string // record id -> tty it was written to ("" = flw)
	recent map[string]notice // MR id -> last notice
	at     map[string]time.Time
}

const activityKeep = 2 * time.Hour

// update returns the sequences to write, by tty ("" = flw's own output).
func (ac *activity) update(mrs, done []*MR, fresh map[string][]notice) map[string]string {
	if ac.shown == nil {
		ac.shown, ac.recent, ac.at = map[string]string{}, map[string]notice{}, map[string]time.Time{}
	}
	out := map[string]*strings.Builder{}
	put := func(tty, seq string) {
		if out[tty] == nil {
			out[tty] = &strings.Builder{}
		}
		out[tty].WriteString(seq)
	}
	now := time.Now()
	want := map[string]string{}
	mine, ping := 0, 0
	emit := func(m *MR, state, kind, msg string) {
		id := activityID(m)
		want[id] = m.TabTTY
		put(m.TabTTY, statusSeq(id, state, kind, "!"+m.IID+" "+shortScope(m.Path())+" · "+m.Title, msg))
	}
	for _, m := range mrs {
		id := activityID(m)
		if ns := fresh[id]; len(ns) > 0 {
			ac.recent[id], ac.at[id] = ns[len(ns)-1], now
		}
		t := m.Turn
		switch {
		case t.Bucket() == bucketSnoozed:
			// put aside: out of Activity until it wakes up
		case t.Bucket() == bucketMine || t.Bucket() == bucketMineLate:
			mine++
			emit(m, "blocked", "question", "▶ your turn: "+t.What)
		case t != nil && t.ToPing():
			ping++
			emit(m, "idle", "", fmt.Sprintf("⏳ %s: %s · %s", ownersText(t.Owners), t.What, map[int]string{1: "ping", 2: "re-ping"}[t.Level]))
		case now.Sub(ac.at[id]) < activityKeep:
			n := ac.recent[id]
			state := "done"
			if strings.HasPrefix(n.body, "❌") || strings.HasPrefix(n.body, "⚠") {
				state = "error"
			}
			emit(m, state, "", n.body)
		}
	}
	for _, m := range done {
		if m.Turn != nil && len(m.Turn.Cleanup) > 0 {
			emit(m, "done", "", "🧹 "+m.State+" · clean up: "+strings.Join(m.Turn.Cleanup, ", ")+" (X in flw)")
		}
	}
	for id, tty := range ac.shown {
		if w, ok := want[id]; !ok || w != tty {
			put(tty, statusSeq(id, "clear", "", "", ""))
		}
	}
	ac.shown = want
	summary := fmt.Sprintf("%d MRs · ▶ %d on you · ⏳ %d to ping", len(mrs), mine, ping)
	if len(done) > 0 {
		summary += fmt.Sprintf(" · 🧹 %d to clean up", len(done))
	}
	state := "idle"
	if mine > 0 {
		state = "blocked"
	}
	put("", statusSeq("", state, "", "flw", summary))
	res := map[string]string{}
	for k, b := range out {
		res[k] = b.String()
	}
	return res
}

// clearAll removes every record flw wrote, in every terminal.
func (ac *activity) clearAll() map[string]string {
	res := map[string]string{"": clearActivity()}
	for id, tty := range ac.shown {
		if tty != "" {
			res[tty] += statusSeq(id, "clear", "", "", "")
		}
	}
	return res
}

// attachTabs sets TabTTY on each MR that has its own Rex tab.
func attachTabs(mrs []*MR) {
	for _, m := range mrs {
		repo, err := findRepo(m.Path())
		if err != nil {
			continue
		}
		for _, t := range mrTabs(m, worktreeFor(repo, m.SourceBranch)) {
			if tty := t.tty(); tty != "" {
				m.TabTTY = tty
				break
			}
		}
	}
}

// writeTTY writes to another Rex terminal's output. One write call: the kernel keeps it
// whole, so it can't cut through what the program there is drawing.
func writeTTY(tty, s string) {
	if s == "" {
		return
	}
	if tty == "" {
		os.Stdout.WriteString(s)
		return
	}
	f, err := os.OpenFile(tty, os.O_WRONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return
	}
	f.WriteString(s)
	f.Close()
}

// clearActivity removes everything flw put in the Activity panel.
func clearActivity() string { return "\x1b]7501;state=clear\x1b\\" }
