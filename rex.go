package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Rex integration: one Rex session per git repo, one tab per MR, each tab tied to a
// Claude Code session working in the MR's worktree.

func inRex() bool {
	if os.Getenv("REX_SESSION") == "" {
		return false
	}
	_, err := exec.LookPath("rex")
	return err == nil
}

func rexRun(args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	c := exec.Command("rex", args...)
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(stderr.String()), "Error: "))
		if msg == "" {
			msg = err.Error()
		}
		return out, errors.New(msg)
	}
	return out, nil
}

func rexJSON(out any, args ...string) error {
	// --json is a rex option: it must come before "--", which starts the command to run
	full := append([]string{}, args...)
	i := len(full)
	for j, a := range full {
		if a == "--" {
			i = j
			break
		}
	}
	full = append(full[:i], append([]string{"--json"}, args[i:]...)...)
	b, err := rexRun(full...)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

type rexSession struct {
	ID    string `json:"session_id"`
	Label string `json:"label"`
}

type rexBlock struct {
	ID       string `json:"block_id"`
	WindowID string `json:"window_id"`
	Owner    string `json:"owner"`
}

type rexWindow struct {
	ID    string `json:"window_id"`
	Label string `json:"label"`
}

type rexProc struct {
	Child, Foreground *rexProcInfo
}

type rexProcInfo struct {
	PID  int    `json:"pid"`
	Argv string `json:"argv0"`
	Cwd  string `json:"cwd"`
}

// MRWork is where an MR is worked on.
type MRWork struct {
	Repo, Worktree string // local clone and the worktree of the source branch
	Session        string // Rex session ID
	SessionLabel   string
	Window         string // Rex window (tab) ID
	What           string // what was done, for the flash message
}

// ---------- local repo and worktree ----------

// codeRoots are searched for a clone of the project: FLW_CODE_ROOTS (a PATH-style list),
// else code_roots in the config, else the usual folders.
func codeRoots() []string {
	var roots []string
	if v := os.Getenv("FLW_CODE_ROOTS"); v != "" {
		roots = filepath.SplitList(v)
	} else {
		roots = cfg.CodeRoots
	}
	if len(roots) > 0 {
		for i, r := range roots {
			roots[i] = expandHome(r)
		}
		return roots
	}
	h, _ := os.UserHomeDir()
	var out []string
	for _, d := range []string{"src", "code", "git", "dev", "work", "projects", "repos", "workspace"} {
		out = append(out, filepath.Join(h, d))
	}
	return append(out, h)
}

var (
	repoMu    sync.Mutex
	repoFound = map[string]string{}
)

// findRepo locates the local clone of a GitLab project, whatever the folder layout:
// <root>/<group>/<project> or <root>/<project>, any folder open in a Rex tab, then a
// shallow walk of the roots. A candidate counts only if its git remote is that project.
func findRepo(fullPath string) (string, error) {
	repoMu.Lock()
	defer repoMu.Unlock()
	if p, ok := repoFound[fullPath]; ok {
		return p, nil
	}
	found := func(p string) (string, error) { repoFound[fullPath] = p; return p, nil }
	for _, r := range codeRoots() {
		for _, p := range []string{filepath.Join(r, fullPath), filepath.Join(r, filepath.Base(fullPath))} {
			if isCloneOf(p, fullPath) {
				return found(p)
			}
		}
	}
	for _, d := range rexCwds() {
		if top := mainCheckout(d); top != "" && isCloneOf(top, fullPath) {
			return found(top)
		}
	}
	for _, r := range codeRoots() {
		if p := walkFor(r, fullPath); p != "" {
			return found(p)
		}
	}
	return "", fmt.Errorf("no local clone of %s found (set FLW_CODE_ROOTS to the folders holding your clones)", fullPath)
}

// isCloneOf: dir is a main checkout whose remote points at the GitLab project.
func isCloneOf(dir, fullPath string) bool {
	if st, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !st.IsDir() {
		return false
	}
	out, err := git(dir, "remote", "-v")
	if err != nil {
		return false
	}
	want := strings.ToLower(fullPath)
	for _, f := range strings.Fields(strings.ToLower(out)) {
		f = strings.TrimSuffix(strings.TrimSuffix(f, "/"), ".git")
		if strings.HasSuffix(f, "/"+want) || strings.HasSuffix(f, ":"+want) {
			return true
		}
	}
	return false
}

// mainCheckout returns the main checkout of the repo dir belongs to (also from a worktree).
func mainCheckout(dir string) string {
	common, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	return filepath.Dir(common)
}

// rexCwds lists the working directories of the terminals open in Rex.
func rexCwds() []string {
	if !inRex() {
		return nil
	}
	var ls struct {
		Sessions []rexSession `json:"sessions"`
	}
	if rexJSON(&ls, "ls") != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range ls.Sessions {
		var bl struct {
			Blocks []rexBlock `json:"blocks"`
		}
		if rexJSON(&bl, "block", "ls", "-s", s.ID) != nil {
			continue
		}
		for _, b := range bl.Blocks {
			raw, err := rexRun("block", "call", "-s", s.ID, "-b", b.ID, "process")
			var p rexProc
			if err != nil || json.Unmarshal(raw, &p) != nil {
				continue
			}
			for _, x := range []*rexProcInfo{p.Foreground, p.Child} {
				if x != nil && x.Cwd != "" && !seen[x.Cwd] {
					seen[x.Cwd] = true
					out = append(out, x.Cwd)
				}
			}
		}
	}
	return out
}

// walkFor looks a few levels down root for a clone named like the project.
func walkFor(root, fullPath string) string {
	name := filepath.Base(fullPath)
	const maxDepth, maxDirs = 4, 20000
	visited := 0
	var res string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if res != "" || depth > maxDepth || visited > maxDirs {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			n := e.Name()
			if !e.IsDir() || strings.HasPrefix(n, ".") || n == "node_modules" || n == "vendor" || n == "Library" {
				continue
			}
			visited++
			p := filepath.Join(dir, n)
			if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
				if n == name && isCloneOf(p, fullPath) {
					res = p
					return
				}
				continue // don't descend into other repos
			}
			walk(p, depth+1)
			if res != "" {
				return
			}
		}
	}
	walk(root, 1)
	return res
}

func git(dir string, args ...string) (string, error) {
	var stderr bytes.Buffer
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// worktreeFor returns the checkout of branch, if any (the main checkout counts).
func worktreeFor(repo, branch string) string {
	out, err := git(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	cur := ""
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "worktree "):
			cur = strings.TrimPrefix(l, "worktree ")
		case l == "branch refs/heads/"+branch:
			return cur
		}
	}
	return ""
}

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func branchSlug(branch string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(filepath.Base(branch)), "-"), "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	return s
}

// ensureWorktree creates <repo>/.claude/worktrees/<slug> on the MR branch when needed.
func ensureWorktree(repo, branch string) (string, bool, error) {
	if wt := worktreeFor(repo, branch); wt != "" {
		return wt, false, nil
	}
	if _, err := git(repo, "fetch", "origin", branch); err != nil {
		return "", false, err
	}
	wt := filepath.Join(repo, ".claude", "worktrees", branchSlug(branch))
	args := []string{"worktree", "add", wt, branch}
	if _, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		args = []string{"worktree", "add", "--track", "-b", branch, wt, "origin/" + branch}
	}
	if _, err := git(repo, args...); err != nil {
		return "", false, err
	}
	return wt, true, nil
}

// ---------- Claude Code sessions ----------

func claudeDir() string { h, _ := os.UserHomeDir(); return filepath.Join(h, ".claude") }

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// claudeProjectDir is where Claude Code keeps the transcripts of sessions started in dir.
func claudeProjectDir(dir string) string {
	return filepath.Join(claudeDir(), "projects", nonAlnum.ReplaceAllString(dir, "-"))
}

var (
	branchFieldRe = regexp.MustCompile(`"gitBranch":"([^"]*)"`)
	cwdFieldRe    = regexp.MustCompile(`"cwd":"([^"]*)"`)
)

// transcriptBranch reads the git branch a transcript was last working on.
func transcriptBranch(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 256 << 10
	if st, err := f.Stat(); err == nil && st.Size() > tail {
		f.Seek(st.Size()-tail, 0)
	}
	b, _ := readAll(f)
	m := branchFieldRe.FindAllSubmatch(b, -1)
	if len(m) == 0 {
		return ""
	}
	return string(m[len(m)-1][1])
}

// transcriptStartDir is the directory the session was started from (needed to resume it).
func transcriptStartDir(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for i := 0; sc.Scan() && i < 50; i++ {
		if m := cwdFieldRe.FindSubmatch(sc.Bytes()); m != nil {
			return string(m[1])
		}
	}
	return ""
}

func readAll(f *os.File) ([]byte, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(f)
	return b.Bytes(), err
}

// liveClaudeBranch tells which branch the Claude Code process pid works on.
func liveClaudeBranch(pid int) string {
	b, err := os.ReadFile(filepath.Join(claudeDir(), "sessions", fmt.Sprintf("%d.json", pid)))
	if err != nil {
		return ""
	}
	var s struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}
	if json.Unmarshal(b, &s) != nil || s.SessionID == "" {
		return ""
	}
	return transcriptBranch(filepath.Join(claudeProjectDir(s.Cwd), s.SessionID+".jsonl"))
}

// pastClaudeSession finds the latest transcript of repo that worked on branch.
func pastClaudeSession(repo, branch string) (id, dir string) {
	files, _ := filepath.Glob(claudeProjectDir(repo) + "*/*.jsonl")
	type tf struct {
		path string
		mod  int64
	}
	var all []tf
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			all = append(all, tf{f, st.ModTime().UnixNano()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod > all[j].mod })
	for i, f := range all {
		if i >= 80 {
			break
		}
		if transcriptBranch(f.path) == branch {
			if d := transcriptStartDir(f.path); d != "" {
				return strings.TrimSuffix(filepath.Base(f.path), ".jsonl"), d
			}
		}
	}
	return "", ""
}

// ---------- tabs ----------

func mrTabLabel(m *MR) string { return "!" + m.IID + " " + branchSlug(m.SourceBranch) }

// rexTab is one Rex window (tab) and what runs in it.
type rexTab struct {
	Session rexSession
	Window  string
	Label   string
	Blocks  []rexTerm
}

type rexTerm struct {
	ID     string
	PID    int // the terminal's shell
	Cwd    string
	Claude bool   // Claude Code is the foreground program
	Branch string // the branch that Claude Code session works on
}

var (
	scanMu   sync.Mutex
	scanAt   time.Time
	scanTabs []rexTab
)

// scanRex lists every tab of every Rex session with its terminals. Kept for a few seconds:
// it costs one call per terminal.
func scanRex() []rexTab {
	scanMu.Lock()
	defer scanMu.Unlock()
	if time.Since(scanAt) < 10*time.Second {
		return scanTabs
	}
	var ls struct {
		Sessions []rexSession `json:"sessions"`
	}
	if rexJSON(&ls, "ls") != nil {
		return nil
	}
	var tabs []rexTab
	for _, s := range ls.Sessions {
		var wl struct {
			Windows []rexWindow `json:"windows"`
		}
		var bl struct {
			Blocks []rexBlock `json:"blocks"`
		}
		if rexJSON(&wl, "window", "ls", "-s", s.ID) != nil || rexJSON(&bl, "block", "ls", "-s", s.ID) != nil {
			continue
		}
		idx := map[string]int{}
		for _, w := range wl.Windows {
			idx[w.ID] = len(tabs)
			tabs = append(tabs, rexTab{Session: s, Window: w.ID, Label: w.Label})
		}
		for _, b := range bl.Blocks {
			i, ok := idx[b.WindowID]
			if !ok {
				continue
			}
			t := rexTerm{ID: b.ID}
			if out, err := rexRun("block", "call", "-s", s.ID, "-b", b.ID, "process"); err == nil {
				var p rexProc
				if json.Unmarshal(out, &p) == nil && p.Child != nil {
					t.PID = p.Child.PID
				}
				if json.Unmarshal(out, &p) == nil && p.Foreground != nil {
					t.Cwd = p.Foreground.Cwd
					if strings.TrimPrefix(p.Foreground.Argv, "-") == "claude" {
						t.Claude = true
						t.Branch = liveClaudeBranch(p.Foreground.PID)
					}
				}
			}
			tabs[i].Blocks = append(tabs[i].Blocks, t)
		}
	}
	scanAt, scanTabs = time.Now(), tabs
	return tabs
}

// forgetRexScan drops the cached scan after flw changed tabs.
func forgetRexScan() { scanMu.Lock(); scanAt = time.Time{}; scanMu.Unlock() }

// mrTabs returns the tabs working on an MR: named after it, running a Claude Code session
// on its branch, or with a terminal in its worktree. Tabs named after the MR come first.
func mrTabs(m *MR, wt string) []rexTab {
	prefix := "!" + m.IID + " "
	var named, other []rexTab
	for _, t := range scanRex() {
		if strings.HasPrefix(t.Label, prefix) && strings.HasSuffix(t.Label, " "+branchSlug(m.SourceBranch)) {
			named = append(named, t)
			continue
		}
		for _, b := range t.Blocks {
			inWT := wt != "" && !isMainCheckout(wt) && (b.Cwd == wt || strings.HasPrefix(b.Cwd, wt+"/"))
			if (b.Claude && b.Branch == m.SourceBranch) || inWT {
				other = append(other, t)
				break
			}
		}
	}
	return append(named, other...)
}

func findMRTab(m *MR, wt string) (rexSession, string, bool) {
	if ts := mrTabs(m, wt); len(ts) > 0 {
		return ts[0].Session, ts[0].Window, true
	}
	return rexSession{}, "", false
}

func isMainCheckout(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && st.IsDir()
}

// repoSession returns the Rex session of a repo: labelled after it, or created.
func repoSession(repo string) (rexSession, error) {
	name := filepath.Base(repo)
	var ls struct {
		Sessions []rexSession `json:"sessions"`
	}
	if err := rexJSON(&ls, "ls"); err != nil {
		return rexSession{}, err
	}
	for _, s := range ls.Sessions {
		if s.Label == name {
			return s, nil
		}
	}
	var created struct {
		ID string `json:"session_id"`
	}
	if err := rexJSON(&created, "new", name, "-c", repo); err != nil {
		return rexSession{}, err
	}
	return rexSession{ID: created.ID, Label: name}, nil
}

// claudePrompt is the first message of a new Claude Code session on the MR.
func claudePrompt(m *MR, wt string) string {
	next := ""
	if m.Turn != nil {
		next = strings.TrimSpace(nextCell(m.Turn).Text)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Work on GitLab MR !%s in %s: %q.\n", m.IID, m.Path(), m.Title)
	fmt.Fprintf(&b, "URL: %s\nBranch: %s -> %s, checked out in this worktree (%s).\n", m.WebURL, m.SourceBranch, m.TargetBranch, wt)
	if next != "" {
		fmt.Fprintf(&b, "Next step according to flw: %s\n", next)
	}
	fmt.Fprintf(&b, "\nStart by loading the MR with `glab mr view %s -R %s --comments` (description, open threads, pipeline). "+
		"Then summarise what is left to do on it and wait for my go before changing anything.", m.IID, m.Path())
	return b.String()
}

// openMRWork switches to the MR's tab, or prepares one: worktree + Claude Code session.
func openMRWork(m *MR) (*MRWork, error) {
	if !inRex() {
		return nil, errors.New("not running inside Rex")
	}
	repo, err := findRepo(m.Path())
	if err != nil {
		return nil, err
	}
	w := &MRWork{Repo: repo, Worktree: worktreeFor(repo, m.SourceBranch)}

	if s, win, ok := findMRTab(m, w.Worktree); ok {
		w.Session, w.SessionLabel, w.Window, w.What = s.ID, s.Label, win, "switched to"
		return w, focusTab(w)
	}
	if m.State != "" && m.State != "opened" {
		return nil, fmt.Errorf("!%s is %s and has no tab: nothing to work on", m.IID, m.State)
	}
	defer forgetRexScan()

	s, err := repoSession(repo)
	if err != nil {
		return nil, err
	}
	w.Session, w.SessionLabel = s.ID, s.Label

	var claude []string
	cwd := ""
	if id, dir := pastClaudeSession(repo, m.SourceBranch); id != "" {
		claude, cwd = []string{"claude", "--resume", id}, dir
		w.What = "resumed Claude session " + id[:8] + " in"
		if w.Worktree == "" {
			w.Worktree, _, err = ensureWorktree(repo, m.SourceBranch)
			if err != nil {
				return nil, err
			}
		}
	} else {
		wt, created, err := ensureWorktree(repo, m.SourceBranch)
		if err != nil {
			return nil, err
		}
		w.Worktree, cwd = wt, wt
		claude = []string{"claude", claudePrompt(m, wt)}
		w.What = "new Claude session in"
		if created {
			w.What = "new worktree + Claude session in"
		}
	}

	// keep a shell in the tab once Claude exits
	args := append([]string{"window", "new", "-s", s.ID, mrTabLabel(m), "-c", cwd, "--",
		"bash", "-lc", `"$@"; exec bash -l`, "flw-claude"}, claude...)
	var created struct {
		WindowID string `json:"window_id"`
	}
	if err := rexJSON(&created, args...); err != nil {
		return nil, err
	}
	w.Window = created.WindowID
	return w, focusTab(w)
}

var errNoRemote = errors.New("turn on Remote Control in Rex settings (Rex Server) so flw can switch sessions")

// focusTab makes the tab active in its session, then asks the Rex app to show it.
func focusTab(w *MRWork) error {
	if w.Window != "" {
		rexRun("window", "focus", "-s", w.Session, w.Window)
	}
	var me struct {
		Block rexBlock `json:"block"`
	}
	if err := rexJSON(&me, "block", "inspect", os.Getenv("REX_BLOCK")); err != nil || me.Block.Owner == "" {
		return errNoRemote
	}
	args := []string{"-C", me.Block.Owner, "do", "session.select", "session_id=" + w.Session}
	if w.Window != "" {
		args = append(args, "window_id="+w.Window)
	}
	if _, err := rexRun(args...); err != nil {
		if strings.Contains(err.Error(), "Remote Control") {
			return errNoRemote
		}
		return err
	}
	return nil
}

// tty is the terminal device of the tab's first terminal, e.g. /dev/pts/4.
func (t rexTab) tty() string {
	for _, b := range t.Blocks {
		if b.PID == 0 {
			continue
		}
		if dev, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", b.PID)); err == nil && strings.HasPrefix(dev, "/dev/pts/") {
			return dev
		}
	}
	return ""
}
