package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// A merged or closed MR stays on the dashboard while something is left of it locally:
// a Rex tab (often with its Claude Code session), a worktree, the main checkout still on
// its branch. "X" cleans that up.

// Leftover is what remains locally of a merged or closed MR.
type Leftover struct {
	Repo        string
	Worktree    string // its own worktree (not the main checkout)
	Dirty       bool   // the worktree has uncommitted changes
	OnMain      bool   // the main checkout is still on the MR branch
	LocalBranch bool
	BranchSafe  bool // the local branch tip is the MR's last commit: nothing would be lost
	Tabs        []rexTab
}

func (l *Leftover) Any() bool { return l != nil && (len(l.Tabs) > 0 || l.Worktree != "" || l.OnMain) }

// Items is the short list for the NEXT column: "tab+claude, worktree".
func (l *Leftover) Items() []string {
	var out []string
	if n := len(l.Tabs); n > 0 {
		s := "tab"
		if n > 1 {
			s = fmt.Sprintf("%d tabs", n)
		}
		for _, t := range l.Tabs {
			if t.hasClaude() {
				s += "+claude"
				break
			}
		}
		out = append(out, s)
	}
	if l.Worktree != "" {
		s := "worktree"
		if l.Dirty {
			s += " (dirty)"
		}
		out = append(out, s)
	}
	if l.OnMain {
		out = append(out, "checkout on branch")
	}
	return out
}

func (t rexTab) hasClaude() bool {
	for _, b := range t.Blocks {
		if b.Claude {
			return true
		}
	}
	return false
}

// findLeftovers fills m.Leftover for each MR that still has something local.
func findLeftovers(mrs []*MR) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, m := range mrs {
		wg.Add(1)
		go func(m *MR) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m.Leftover = leftoverOf(m)
		}(m)
	}
	wg.Wait()
}

func leftoverOf(m *MR) *Leftover {
	if m.SourceBranch == "" {
		return nil
	}
	repo, err := findRepo(m.Path())
	if err != nil {
		return nil
	}
	l := &Leftover{Repo: repo}
	wt := worktreeFor(repo, m.SourceBranch)
	switch {
	case wt == "":
	case isMainCheckout(wt):
		l.OnMain = true
	default:
		l.Worktree = wt
		if out, err := git(wt, "status", "--porcelain"); err == nil && out != "" {
			l.Dirty = true
		}
	}
	if tip, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+m.SourceBranch); err == nil {
		l.LocalBranch = true
		l.BranchSafe = m.State == "merged" && m.DiffHeadSha != "" && tip == m.DiffHeadSha
	}
	if inRex() {
		l.Tabs = mrTabs(m, wt)
	}
	if !l.Any() {
		return nil
	}
	return l
}

// ownWindow is the Rex tab flw runs in: never close it.
func ownWindow() string {
	var me struct {
		Block rexBlock `json:"block"`
	}
	if os.Getenv("REX_BLOCK") == "" || rexJSON(&me, "block", "inspect", os.Getenv("REX_BLOCK")) != nil {
		return ""
	}
	return me.Block.WindowID
}

// cleanupPlan says what X will do, and what it will leave alone.
func cleanupPlan(m *MR) (do, keep []string) {
	l := m.Leftover
	own := ownWindow()
	for _, t := range l.Tabs {
		what := fmt.Sprintf("close tab %q in %s", t.Label, t.Session.Label)
		if t.hasClaude() {
			what += " (stops its Claude session; resume it later with claude --resume)"
		}
		if t.Window == own {
			keep = append(keep, fmt.Sprintf("tab %q: flw runs in it", t.Label))
			continue
		}
		do = append(do, what)
	}
	if l.Worktree != "" {
		if l.Dirty {
			keep = append(keep, "worktree "+shortPath(l.Worktree)+": uncommitted changes")
		} else {
			do = append(do, "remove worktree "+shortPath(l.Worktree))
		}
	}
	if l.OnMain {
		keep = append(keep, "main checkout still on "+m.SourceBranch+": switch it yourself")
	}
	switch {
	case !l.LocalBranch || l.OnMain || l.Dirty:
	case l.BranchSafe:
		do = append(do, "delete local branch "+m.SourceBranch+" (same commit as the merged MR)")
	default:
		keep = append(keep, "local branch "+m.SourceBranch+": has commits that are not in the MR")
	}
	return do, keep
}

// runCleanup does the plan. It never forces: a dirty worktree or an unmerged branch is kept.
func runCleanup(m *MR) (string, error) {
	l := m.Leftover
	own := ownWindow()
	var done, failed []string
	for _, t := range l.Tabs {
		if t.Window == own {
			continue
		}
		if _, err := rexRun("window", "close", "-s", t.Session.ID, t.Window); err != nil {
			failed = append(failed, "tab "+t.Label+": "+err.Error())
		} else {
			done = append(done, "tab closed")
		}
	}
	forgetRexScan()
	if l.Worktree != "" && !l.Dirty {
		if _, err := git(l.Repo, "worktree", "remove", l.Worktree); err != nil {
			failed = append(failed, err.Error())
		} else {
			done = append(done, "worktree removed")
		}
	}
	if l.LocalBranch && l.BranchSafe && !l.OnMain && !l.Dirty {
		// -D: a squash merge looks unmerged to git, but the tip is the MR's last commit
		if _, err := git(l.Repo, "branch", "-D", m.SourceBranch); err != nil {
			failed = append(failed, err.Error())
		} else {
			done = append(done, "branch deleted")
		}
	}
	msg := "!" + m.IID + ": " + strings.Join(done, ", ")
	if len(done) == 0 {
		msg = "!" + m.IID + ": nothing removed"
	}
	if len(failed) > 0 {
		return msg, fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	return msg, nil
}

func shortPath(p string) string {
	if h, _ := os.UserHomeDir(); h != "" && strings.HasPrefix(p, h) {
		return "~" + p[len(h):]
	}
	return p
}
