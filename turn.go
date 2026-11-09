package main

import (
	"fmt"
	"strings"
	"time"
)

// Ping thresholds in business time (weekends do not count). Set by flags.
var (
	pingAfter   = 24 * time.Hour // waiting this long: time to ping
	repingAfter = 72 * time.Hour // waiting this long, or pinged once without answer: ping again
)

// Step is one thing the MR waits for, and who must do it.
type Step struct {
	Owners []string // usernames; empty = nobody (CI, merged…)
	What   string
	Since  time.Time
	Quiet  bool // no ping (draft, CI running…)
}

// Turn says whose move it is on an MR, seen from the current user.
type Turn struct {
	Step
	Mine     bool // I own the next step
	Pings    int  // mentions of the owners since the step started
	LastPing time.Time
	Level    int // waiting on others: 0 fresh, 1 ping, 2 re-ping; mine: 1 = overdue
	Others   []Step
	Me       string
	Cleanup  []string  // merged/closed MR with local leftovers (tab, worktree…)
	Snoozed  time.Time // put aside until then ("z")
}

const (
	bucketMineLate = iota
	bucketMine
	bucketReping
	bucketPing
	bucketCleanup
	bucketWaiting
	bucketSnoozed
	bucketIdle
)

func (t *Turn) Bucket() int {
	switch {
	case t == nil:
		return bucketIdle
	case len(t.Cleanup) > 0:
		return bucketCleanup
	case !t.Snoozed.IsZero():
		return bucketSnoozed
	case t.Mine && t.Level > 0:
		return bucketMineLate
	case t.Mine:
		return bucketMine
	case t.Quiet || len(t.Owners) == 0:
		return bucketIdle
	case t.Level == 2:
		return bucketReping
	case t.Level == 1:
		return bucketPing
	}
	return bucketWaiting
}

// ToPing is true when someone else owes an action and it is time to nudge them.
func (t *Turn) ToPing() bool { b := t.Bucket(); return b == bucketPing || b == bucketReping }

// bizDur is the time between since and now, weekends excluded.
func bizDur(since, now time.Time) time.Duration {
	if since.IsZero() || !now.After(since) {
		return 0
	}
	since, now = since.Local(), now.Local()
	var d time.Duration
	cur := since
	for cur.Before(now) {
		y, m, dd := cur.Date()
		next := time.Date(y, m, dd+1, 0, 0, 0, 0, cur.Location())
		if next.After(now) {
			next = now
		}
		if wd := cur.Weekday(); wd != time.Saturday && wd != time.Sunday {
			d += next.Sub(cur)
		}
		cur = next
	}
	return d
}

func bizAgo(t time.Time) string {
	d := bizDur(t, time.Now())
	switch {
	case t.IsZero():
		return ""
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// steps lists what the MR waits for, most blocking first.
func (m *MR) steps() []Step {
	if m.State != "" && m.State != "opened" {
		return []Step{{What: m.State, Quiet: true}}
	}
	author := ""
	if m.Author != nil {
		author = m.Author.Username
	}
	authorOnly := []string{author}

	lastBy := map[string]time.Time{}
	for _, n := range m.Timeline.Nodes {
		// a comment that @mentions someone is a ping, not work on the MR: it must not reset the wait
		if !n.System && strings.Contains(n.Body, "@") {
			continue
		}
		if n.Author != nil && n.CreatedAt.After(lastBy[n.Author.Username]) {
			lastBy[n.Author.Username] = n.CreatedAt
		}
	}
	var lastCommit time.Time
	if len(m.Commits.Nodes) > 0 {
		lastCommit = m.Commits.Nodes[0].CommittedDate
	}
	authorLast := latest(lastBy[author], lastCommit, m.CreatedAt)

	if m.Draft {
		return []Step{{Owners: authorOnly, What: "finish draft", Since: m.CreatedAt, Quiet: true}}
	}

	var steps []Step
	if p := m.HeadPipeline; p != nil && strings.EqualFold(p.Status, "failed") {
		since := m.UpdatedAt
		if p.FinishedAt != nil {
			since = *p.FinishedAt
		}
		steps = append(steps, Step{Owners: authorOnly, What: "fix CI", Since: since})
	}
	switch {
	case m.Conflicts || m.DetailedMergeStatus == "CONFLICT":
		steps = append(steps, Step{Owners: authorOnly, What: "fix conflict", Since: m.UpdatedAt})
	case m.DetailedMergeStatus == "NEED_REBASE":
		steps = append(steps, Step{Owners: authorOnly, What: "rebase", Since: m.UpdatedAt})
	}

	// reviewers
	var pending, notApproved []string
	var changesSince time.Time
	changes := false
	for _, r := range m.Reviewers.Nodes {
		st, approved := "UNREVIEWED", false
		if r.Interaction != nil {
			st, approved = r.Interaction.ReviewState, r.Interaction.Approved
		}
		if approved || st == "APPROVED" {
			continue
		}
		notApproved = append(notApproved, r.Username)
		switch st {
		case "REQUESTED_CHANGES":
			changes = true
			changesSince = latest(changesSince, lastBy[r.Username])
		case "REVIEWED":
		default: // UNREVIEWED, REVIEW_STARTED, UNAPPROVED
			pending = append(pending, r.Username)
		}
	}
	if changes {
		steps = append(steps, Step{Owners: authorOnly, What: "address changes", Since: latest(changesSince, m.CreatedAt)})
	}

	// threads: who wrote last?
	onAuthor, onStarters := 0, 0
	var authorSince, startersSince time.Time
	var starters []string
	for _, t := range m.Threads.Nodes {
		if !t.Resolvable || t.Resolved || len(t.Last.Nodes) == 0 || len(t.First.Nodes) == 0 {
			continue
		}
		first, last := t.First.Nodes[0], t.Last.Nodes[0]
		if first.Author == nil || last.Author == nil {
			continue
		}
		if last.Author.Username == author {
			if first.Author.Username == author {
				continue // author's own note
			}
			onStarters++
			startersSince = latest(startersSince, last.CreatedAt)
			if !contains(starters, first.Author.Username) {
				starters = append(starters, first.Author.Username)
			}
		} else {
			onAuthor++
			if authorSince.IsZero() || last.CreatedAt.Before(authorSince) {
				authorSince = last.CreatedAt
			}
		}
	}
	if onAuthor > 0 {
		steps = append(steps, Step{Owners: authorOnly, What: fmt.Sprintf("reply %s", plural(onAuthor, "thread")), Since: authorSince})
	}
	if onStarters > 0 {
		steps = append(steps, Step{Owners: starters, What: fmt.Sprintf("answer %s", plural(onStarters, "thread")), Since: startersSince})
	}

	needApproval := !m.Approved || (m.ApprovalsLeft != nil && *m.ApprovalsLeft > 0)
	if needApproval {
		switch {
		case len(pending) > 0:
			steps = append(steps, Step{Owners: pending, What: "review", Since: authorLast})
		case len(m.Reviewers.Nodes) == 0:
			steps = append(steps, Step{Owners: authorOnly, What: "request review", Since: m.CreatedAt})
		case len(notApproved) > 0 && !changes && onAuthor == 0:
			steps = append(steps, Step{Owners: notApproved, What: "approve", Since: authorLast})
		}
	}

	if len(steps) == 0 {
		switch m.DetailedMergeStatus {
		case "MERGEABLE":
			if m.MergeWhenPipelineSucceeds {
				steps = append(steps, Step{What: "auto-merge set", Quiet: true})
			} else {
				steps = append(steps, Step{Owners: authorOnly, What: "merge", Since: m.UpdatedAt})
			}
		case "CI_STILL_RUNNING", "CI_MUST_PASS":
			steps = append(steps, Step{What: "CI running", Quiet: true})
		default:
			label, _ := mergeStatus(m.DetailedMergeStatus)
			steps = append(steps, Step{What: label, Quiet: true})
		}
	}
	return steps
}

// computeTurn picks the step that matters for me and grades how urgent it is.
func computeTurn(m *MR, me string) *Turn {
	steps := m.steps()
	t := &Turn{Step: steps[0], Me: me, Snoozed: snoozedUntil(m)}
	for i, s := range steps {
		if contains(s.Owners, me) {
			t.Step, t.Mine = s, true
			steps = append(append([]Step{}, steps[:i]...), steps[i+1:]...)
			t.Others = steps
			break
		}
	}
	if !t.Mine && len(steps) > 1 {
		t.Others = steps[1:]
	}
	now := time.Now()
	if t.Mine {
		if !t.Quiet && bizDur(t.Since, now) >= pingAfter {
			t.Level = 1
		}
		return t
	}
	if t.Quiet || len(t.Owners) == 0 {
		return t
	}
	// pings: notes by someone else mentioning an owner since the step started
	for _, n := range m.Timeline.Nodes {
		if n.System || n.Author == nil || contains(t.Owners, n.Author.Username) || !n.CreatedAt.After(t.Since) {
			continue
		}
		for _, o := range t.Owners {
			if strings.Contains(n.Body, "@"+o) {
				t.Pings++
				t.LastPing = latest(t.LastPing, n.CreatedAt)
				break
			}
		}
	}
	waited := bizDur(t.Since, now)
	switch {
	case !t.LastPing.IsZero():
		if bizDur(t.LastPing, now) >= pingAfter {
			t.Level = 2
		}
	case waited >= repingAfter:
		t.Level = 2
	case waited >= pingAfter:
		t.Level = 1
	}
	return t
}

func ownersText(o []string) string {
	switch len(o) {
	case 0:
		return ""
	case 1:
		return "@" + o[0]
	}
	return fmt.Sprintf("@%s +%d", o[0], len(o)-1)
}

// nextCell renders the NEXT column.
func nextCell(t *Turn) Cell {
	if t == nil {
		return Cell{Text: "-", Color: cDim}
	}
	if len(t.Cleanup) > 0 {
		return Cell{Text: "🧹 " + t.What + " · " + strings.Join(t.Cleanup, ", "), Color: cAccent2, Bold: true}
	}
	if !t.Snoozed.IsZero() {
		who := "you"
		if !t.Mine {
			who = ownersText(t.Owners)
		}
		if who == "" {
			return Cell{Text: "💤 " + whenText(t.Snoozed, time.Now()) + " · " + t.What, Color: cGray}
		}
		return Cell{Text: "💤 " + whenText(t.Snoozed, time.Now()) + " · " + who + ": " + t.What, Color: cGray}
	}
	since := ""
	if !t.Since.IsZero() && !t.Quiet {
		since = " · " + bizAgo(t.Since)
	}
	switch {
	case t.Mine && t.Quiet:
		return Cell{Text: "▶ you: " + t.What, Color: cGray}
	case t.Mine && t.Level > 0:
		return Cell{Text: "▶ you: " + t.What + since + " !", Color: cRed, Bold: true}
	case t.Mine:
		return Cell{Text: "▶ you: " + t.What + since, Color: cOrange, Bold: true}
	case len(t.Owners) == 0 || t.Quiet:
		txt := "· " + t.What
		if len(t.Owners) > 0 {
			txt = "· " + ownersText(t.Owners) + ": " + t.What
		}
		return Cell{Text: txt, Color: cGray}
	}
	txt := "⏳ " + ownersText(t.Owners) + ": " + t.What + since
	switch t.Level {
	case 2:
		return Cell{Text: txt + " · re-ping", Color: cRed, Bold: true}
	case 1:
		return Cell{Text: txt + " · ping", Color: cYellow, Bold: true}
	}
	if !t.LastPing.IsZero() {
		txt += " · pinged " + bizAgo(t.LastPing)
	}
	return Cell{Text: txt, Color: cBlue}
}

// pingMessage prefills the nudge comment.
func pingMessage(t *Turn) string {
	var at []string
	for _, o := range t.Owners {
		at = append(at, "@"+o)
	}
	who := strings.Join(at, " ")
	prefix := "friendly ping"
	if t.Level == 2 || t.Pings > 0 {
		prefix = "gentle reminder"
	}
	switch {
	case t.What == "review":
		return fmt.Sprintf("%s %s: could you have a look at this MR when you get a chance? 🙏", who, prefix)
	case t.What == "approve":
		return fmt.Sprintf("%s %s: could you approve if it looks good to you? 🙏", who, prefix)
	case strings.HasPrefix(t.What, "answer"):
		return fmt.Sprintf("%s %s: I replied on the open threads, could you have a look? 🙏", who, prefix)
	}
	return fmt.Sprintf("%s %s: this MR is waiting on you (%s) 🙏", who, prefix, t.What)
}
