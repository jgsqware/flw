package main

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// View is one screen in the navigation stack.
type View interface {
	Crumb() string
	Init(a *App) tea.Cmd
	Refresh(a *App) tea.Cmd
	HandleKey(a *App, k tea.KeyMsg) (bool, tea.Cmd)
	Render(a *App, w, h int) string
	Actions() []KeyAction
	Loaded(a *App, msg loadedMsg) tea.Cmd
	Due(now time.Time) bool
	Filter() string
	SetFilter(string)
}

type mode int

const (
	mNormal mode = iota
	mCommand
	mFilter
	mPrompt
	mConfirm
	mText
	mHelp
	mPalette
)

type (
	tickMsg       time.Time
	actionDoneMsg struct {
		text    string
		err     error
		refresh bool
		then    func(a *App) tea.Cmd
	}
	bootMsg struct {
		project string
		err     error
	}
)

type App struct {
	gl      *Client
	notif   notifier
	act     activity
	polling bool
	project string
	booted  bool
	bootErr error

	stack []View
	w, h  int

	mode    mode
	input   textinput.Model
	ta      textarea.Model
	label   string
	onInput func(string) tea.Cmd
	onYes   func() tea.Cmd

	flash    string
	flashBad bool
	flashAt  time.Time

	spin spinner.Model
	busy int

	upd struct {
		latest   string // newer release tag, "" when up to date
		rel      ghRelease
		updating bool
		restart  string // binary to exec after quitting
	}

	desk      deskState
	layout    layoutMap
	lastClick struct {
		row  int
		view View
		at   time.Time
	}
}

func newApp(gl *Client, project string) *App {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 500
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.Placeholder = "Write a comment (markdown)…"
	ta.CharLimit = 0
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = lipgloss.NewStyle().Foreground(cOrange)
	return &App{gl: gl, project: project, input: ti, ta: ta, spin: sp}
}

func (a *App) Init() tea.Cmd {
	gl, proj := a.gl, a.project
	boot := func() tea.Msg {
		// identity and project barely change: take them from the cache when we can
		if cc := gl.Cached(); cc.Bootstrap() == nil {
			gl.Me, gl.BaseURL = cc.Me, cc.BaseURL
			if proj == "" {
				if proj, _ = cc.DetectProject(); proj == "" {
					proj, _ = gl.DetectProject()
				}
			}
			bg := *gl
			go bg.Bootstrap() // refresh the cached copy for next time
			return bootMsg{project: proj}
		}
		if err := gl.Bootstrap(); err != nil {
			return bootMsg{err: err}
		}
		if proj == "" {
			proj, _ = gl.DetectProject()
		}
		return bootMsg{project: proj}
	}
	cmds := []tea.Cmd{boot, a.spin.Tick, tick()}
	if updateCheckOn() {
		cmds = append(cmds, checkUpdateCmd(3*time.Second))
	}
	return tea.Batch(cmds...)
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (a *App) top() View {
	if len(a.stack) == 0 {
		return nil
	}
	return a.stack[len(a.stack)-1]
}

func (a *App) push(v View) tea.Cmd {
	a.stack = append(a.stack, v)
	return v.Init(a)
}

func (a *App) pop() tea.Cmd {
	if len(a.stack) <= 1 {
		return nil
	}
	a.stack = a.stack[:len(a.stack)-1]
	return a.top().Refresh(a)
}

func (a *App) reset(v View) tea.Cmd {
	a.stack = []View{v}
	return v.Init(a)
}

// ---------- helpers used by views ----------

func (a *App) flashOK(s string) tea.Cmd {
	a.flash, a.flashBad, a.flashAt = s, false, time.Now()
	return nil
}

func (a *App) flashErr(s string) tea.Cmd {
	a.flash, a.flashBad, a.flashAt = s, true, time.Now()
	return nil
}

// run executes fn in the background, then flashes the result and refreshes the current view.
func (a *App) run(doing, done string, fn func() error) tea.Cmd {
	a.busy++
	a.flash, a.flashBad, a.flashAt = doing+"…", false, time.Now()
	return func() tea.Msg {
		err := fn()
		return actionDoneMsg{text: done, err: err, refresh: true}
	}
}

func (a *App) confirm(q string, yes func() tea.Cmd) tea.Cmd {
	a.mode, a.label, a.onYes = mConfirm, q, yes
	return nil
}

func (a *App) prompt(label, value string, fn func(string) tea.Cmd) tea.Cmd {
	a.mode, a.label, a.onInput = mPrompt, label, fn
	a.input.SetValue(value)
	a.input.CursorEnd()
	return a.input.Focus()
}

func (a *App) textEditor(label string, fn func(string) tea.Cmd) tea.Cmd {
	return a.textEditorWith(label, "", fn)
}

func (a *App) textEditorWith(label, value string, fn func(string) tea.Cmd) tea.Cmd {
	a.mode, a.label, a.onInput = mText, label, fn
	a.ta.Reset()
	a.ta.SetValue(value)
	return a.ta.Focus()
}

func (a *App) needProject() bool {
	if a.project == "" {
		a.flashErr("no project selected: pick one in :projects or use :project <group/name>")
		return false
	}
	return true
}

func (a *App) execCmd(label string, c *exec.Cmd) tea.Cmd {
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return actionDoneMsg{text: label, err: err, refresh: true}
	})
}

// ---------- update ----------

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.w, a.h = msg.Width, msg.Height
		a.ta.SetWidth(min(100, a.w-6))
		a.ta.SetHeight(max(5, min(15, a.h-12)))
		return a, nil

	case bootMsg:
		a.booted = true
		if msg.err != nil {
			a.bootErr = msg.err
			return a, nil
		}
		a.project = msg.project
		first := a.reset(newMyMRsView(a))
		if a.project != "" {
			first = a.reset(newProjectMRsView(a, a.project))
		}
		a.desk.cur = a.project
		return a, tea.Batch(first, a.poll(), a.scanRexCmd())

	case updateCheckMsg:
		return a, a.onUpdateCheck(msg)

	case updateDoneMsg:
		return a, a.onUpdateDone(msg)

	case previewMsg:
		return a, a.loadPreview(msg.key)

	case rexTabsMsg:
		a.desk.scanning, a.desk.rexAt, a.desk.rexTabs = false, time.Now(), msg.tabs
		return a, nil

	case notifyMsg:
		a.polling = false
		if msg.err != nil {
			return a, nil
		}
		return a, a.onNotify(msg.mrs, msg.done)

	case loadedMsg:
		return a, tea.Batch(msg.v.Loaded(a, msg), a.syncPreview())

	case actionDoneMsg:
		if a.busy > 0 {
			a.busy--
		}
		var cmds []tea.Cmd
		if msg.err != nil {
			if msg.text != "" {
				a.flashErr("✘ " + msg.text + ": " + msg.err.Error())
			} else {
				a.flashErr("✘ " + msg.err.Error())
			}
		} else if msg.text != "" {
			a.flashOK("✔ " + msg.text)
		}
		if msg.refresh {
			if t := a.top(); t != nil {
				cmds = append(cmds, t.Refresh(a))
			}
		}
		if msg.then != nil && msg.err == nil {
			cmds = append(cmds, msg.then(a))
		}
		return a, tea.Batch(cmds...)

	case tickMsg:
		var cmd tea.Cmd
		if t := a.top(); t != nil && a.mode != mText && t.Due(time.Time(msg)) {
			cmd = t.Refresh(a)
		}
		ttl := 6 * time.Second
		if a.flashBad {
			ttl = 12 * time.Second
		}
		if a.flash != "" && a.busy == 0 && time.Since(a.flashAt) > ttl {
			a.flash = ""
		}
		var pc tea.Cmd
		if a.booted && a.gl.Me != "" && time.Since(a.notif.last) >= notifyEvery() {
			pc = a.poll()
		}
		var dc []tea.Cmd
		if a.tiled() && a.mode != mText {
			if p := a.desk.preview; p != nil && p.Due(time.Time(msg)) {
				dc = append(dc, p.Refresh(a))
			}
			if l := a.desk.pipe; l != nil && l.Due(time.Time(msg)) {
				dc = append(dc, l.Refresh(a))
			}
		}
		if a.booted {
			dc = append(dc, a.scanRexCmd())
		}
		return a, tea.Batch(cmd, pc, tick(), tea.Batch(dc...))

	case spinner.TickMsg:
		var cmd tea.Cmd
		a.spin, cmd = a.spin.Update(msg)
		return a, cmd

	case tea.KeyMsg:
		cmd := a.handleKey(msg)
		return a, tea.Batch(cmd, a.syncPreview())

	case tea.MouseMsg:
		return a, a.handleMouse(msg)
	}

	// forward other messages (e.g. textinput blink) to inputs
	switch a.mode {
	case mCommand, mFilter, mPrompt, mPalette:
		var cmd tea.Cmd
		a.input, cmd = a.input.Update(msg)
		return a, cmd
	case mText:
		var cmd tea.Cmd
		a.ta, cmd = a.ta.Update(msg)
		return a, cmd
	}
	return a, nil
}

func (a *App) handleKey(k tea.KeyMsg) tea.Cmd {
	if k.String() == "ctrl+c" {
		return tea.Quit
	}
	// Fast typing can deliver several runes in one KeyMsg ("jj"): replay them one by one
	// outside of text inputs so single-key bindings keep working.
	if k.Type == tea.KeyRunes && len(k.Runes) > 1 && !k.Paste && (a.mode == mNormal || a.mode == mConfirm || a.mode == mHelp) {
		var cmds []tea.Cmd
		for _, r := range k.Runes {
			cmds = append(cmds, a.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: k.Alt}))
		}
		return tea.Batch(cmds...)
	}
	switch a.mode {
	case mHelp:
		a.mode = mNormal
		return nil

	case mPalette:
		return a.paletteKey(k)

	case mConfirm:
		switch k.String() {
		case "y", "Y", "enter":
			a.mode = mNormal
			return a.onYes()
		default:
			a.mode = mNormal
			a.flashOK("cancelled")
		}
		return nil

	case mText:
		switch k.String() {
		case "esc":
			a.mode = mNormal
			a.ta.Blur()
			return nil
		case "ctrl+s", "alt+enter", "ctrl+enter":
			v := strings.TrimSpace(a.ta.Value())
			a.mode = mNormal
			a.ta.Blur()
			if v == "" {
				return a.flashErr("empty comment, nothing sent")
			}
			return a.onInput(v)
		}
		var cmd tea.Cmd
		a.ta, cmd = a.ta.Update(k)
		return cmd

	case mCommand, mFilter, mPrompt:
		switch k.String() {
		case "esc":
			if a.mode == mFilter {
				if t := a.top(); t != nil {
					t.SetFilter("")
				}
			}
			a.mode = mNormal
			a.input.Blur()
			return nil
		case "enter":
			v := strings.TrimSpace(a.input.Value())
			m := a.mode
			a.mode = mNormal
			a.input.Blur()
			switch m {
			case mCommand:
				return a.runCommand(v)
			case mPrompt:
				return a.onInput(v)
			}
			return nil
		case "tab":
			if a.mode == mCommand {
				if s := cmdSuggest(a.input.Value()); len(s) > 0 {
					a.input.SetValue(s[0] + " ")
					a.input.CursorEnd()
				}
				return nil
			}
		}
		var cmd tea.Cmd
		a.input, cmd = a.input.Update(k)
		if a.mode == mFilter {
			if t := a.top(); t != nil {
				t.SetFilter(a.input.Value())
			}
		}
		return cmd
	}

	// normal mode
	if desktopUI {
		if ok, cmd := a.deskKey(k); ok {
			return cmd
		}
	}
	t := a.top()
	switch k.String() {
	case "q":
		if len(a.stack) > 1 {
			return a.pop()
		}
		return tea.Quit
	case "esc":
		if t != nil && t.Filter() != "" {
			t.SetFilter("")
			return nil
		}
		return a.pop()
	case ":":
		a.mode = mCommand
		a.input.SetValue("")
		return a.input.Focus()
	case "/":
		if t == nil {
			return nil
		}
		a.mode = mFilter
		a.input.SetValue(t.Filter())
		a.input.CursorEnd()
		return a.input.Focus()
	case "?":
		a.mode = mHelp
		return nil
	case "ctrl+r":
		if t != nil {
			return tea.Batch(t.Refresh(a), a.flashOK("refreshing…"))
		}
		return nil
	}
	if t != nil {
		_, cmd := t.HandleKey(a, k)
		return cmd
	}
	return nil
}

// ---------- commands ----------

var commands = []struct {
	names []string
	help  string
}{
	{[]string{"my", "home"}, "my open MRs (authored, review requested, assigned)"},
	{[]string{"mr", "mrs"}, "merge requests of the current project"},
	{[]string{"pipelines", "pl", "ci"}, "pipelines of the current project (optional: ref)"},
	{[]string{"issues", "is"}, "issues of the current project"},
	{[]string{"projects", "proj"}, "my projects"},
	{[]string{"project", "p"}, "switch project: :project group/name"},
	{[]string{"update"}, "update flw to the latest release and restart"},
	{[]string{"quit", "q"}, "quit"},
}

func cmdSuggest(prefix string) []string {
	prefix = strings.TrimSpace(prefix)
	var out []string
	for _, c := range commands {
		for _, n := range c.names {
			if prefix != "" && strings.HasPrefix(n, prefix) {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (a *App) runCommand(line string) tea.Cmd {
	f := strings.Fields(line)
	if len(f) == 0 {
		return nil
	}
	arg := strings.Join(f[1:], " ")
	switch f[0] {
	case "q", "quit", "q!":
		return tea.Quit
	case "my", "home":
		return a.reset(newMyMRsView(a))
	case "mr", "mrs", "merge_requests":
		if !a.needProject() {
			return nil
		}
		return a.reset(newProjectMRsView(a, a.project))
	case "pl", "pipelines", "ci":
		if !a.needProject() {
			return nil
		}
		return a.reset(newPipelinesView(a, a.project, arg, nil))
	case "is", "issues":
		if !a.needProject() {
			return nil
		}
		return a.reset(newIssuesView(a, a.project))
	case "proj", "projects":
		return a.reset(newProjectsView(a))
	case "update", "upgrade":
		return a.startUpdate()
	case "p", "project":
		if arg == "" {
			return a.reset(newProjectsView(a))
		}
		a.project = strings.Trim(arg, "/")
		return a.reset(newProjectMRsView(a, a.project))
	}
	return a.flashErr("unknown command: " + f[0] + "  (try :my :mr :pl :issues :projects)")
}

// ---------- view ----------

func (a *App) View() string {
	if a.w == 0 {
		return ""
	}
	if desktopUI {
		return a.viewDesktop()
	}
	header := a.renderHeader()
	footer := a.renderFooter()
	bodyH := a.h - lipgloss.Height(header) - lipgloss.Height(footer)
	if bodyH < 3 {
		bodyH = 3
	}
	var body string
	switch {
	case a.mode == mHelp:
		body = a.renderHelp(bodyH)
	case a.mode == mText:
		body = a.renderTextEditor(bodyH)
	case !a.booted:
		body = box("flw", "\n  "+a.spin.View()+" connecting to GitLab through glab…", a.w, bodyH, cAccent)
	case a.bootErr != nil:
		body = box("flw", "\n  "+sErr.Render("✘ "+a.bootErr.Error())+"\n\n  Check `glab auth status`.", a.w, bodyH, cRed)
	case a.top() != nil:
		body = a.top().Render(a, a.w, bodyH)
	}
	return header + "\n" + body + "\n" + footer
}

func (a *App) renderHeader() string {
	left := sLogo.Render("flw")
	host := strings.TrimPrefix(strings.TrimPrefix(a.gl.BaseURL, "https://"), "http://")
	if host == "" {
		host = a.gl.Host
	}
	proj := a.project
	if proj == "" {
		proj = "—"
	}
	info := []string{sGray.Render("project ") + lipgloss.NewStyle().Foreground(cOrange).Bold(true).Render(proj)}
	if a.gl.Me != "" {
		info = append(info, sGray.Render("user ")+sAccent.Render("@"+a.gl.Me))
	}
	if host != "" {
		info = append(info, sGray.Render("host ")+sAccent.Render(host))
	}
	line1 := left + "  " + strings.Join(info, sDim.Render("  │  "))
	right := ""
	if a.busy > 0 || (a.top() != nil && isLoading(a.top())) {
		right = a.spin.View() + " "
	}
	if b := a.updateBadge(); b != "" {
		right = b + " " + right
	}
	if lw := ansi.StringWidth(line1) + ansi.StringWidth(right); lw < a.w {
		line1 += strings.Repeat(" ", a.w-lw) + right
	}

	var crumbs []string
	for i, v := range a.stack {
		st := sCrumb
		if i == len(a.stack)-1 {
			st = sCrumbOn
		}
		crumbs = append(crumbs, st.Render(ansi.Truncate(v.Crumb(), 40, "…")))
	}
	line2 := strings.Join(crumbs, " ")
	return fit(line1, a.w) + "\n" + fit(line2, a.w)
}

func isLoading(v View) bool {
	switch t := v.(type) {
	case *ListView:
		return t.loading
	case *PagerView:
		return t.loading
	}
	return false
}

func hint(k, h string) string { return sKey.Render(k) + " " + sGray.Render(h) }

func (a *App) renderFooter() string {
	var hints []string
	if t := a.top(); t != nil {
		if _, ok := t.(*ListView); ok {
			hints = append(hints, hint("enter", "open"))
		}
		for _, act := range t.Actions() {
			if act.Short {
				h := act.Hint
				if h == "" {
					h = act.Help
				}
				hints = append(hints, hint(strings.Join(act.Keys, "/"), h))
			}
		}
	}
	hints = append(hints, hint(":", "cmd"), hint("/", "filter"), hint("esc", "back"), hint("?", "help"))
	line1 := fit(strings.Join(hints, "  "), a.w)
	_, line2 := a.footerLines()
	return line1 + "\n" + fit(line2, a.w)
}

// footerLines returns the hint line and the status line (prompt, confirm, flash).
func (a *App) footerLines() (string, string) {
	var line2 string
	switch a.mode {
	case mCommand:
		sug := cmdSuggest(a.input.Value())
		s := sKey.Render(":") + a.input.View()
		if len(sug) > 0 {
			s += "  " + sDim.Render(strings.Join(sug, " · ")+"  (tab)")
		}
		line2 = s
	case mFilter:
		line2 = sKey.Render("/") + a.input.View() + "  " + sDim.Render("enter keep · esc clear")
	case mPrompt:
		line2 = lipgloss.NewStyle().Foreground(cYellow).Render(a.label+" ") + a.input.View()
	case mConfirm:
		line2 = lipgloss.NewStyle().Foreground(cYellow).Bold(true).Render("⚠ "+a.label) + "  " + sKey.Render("y") + sGray.Render("/") + sKey.Render("n")
	default:
		if a.flash != "" {
			if a.flashBad {
				line2 = sErr.Render(a.flash)
			} else {
				line2 = sOK.Render(a.flash)
			}
			if a.busy > 0 {
				line2 = a.spin.View() + " " + line2
			}
		}
	}
	return "", line2
}

func (a *App) renderTextEditor(h int) string {
	body := "\n " + sGray.Render(a.label) + "\n\n" + indent(a.ta.View(), 1) + "\n\n " +
		hint("ctrl+s", "send") + "  " + hint("esc", "cancel")
	return box(sAccent.Bold(true).Render("Comment"), body, a.w, h, cOrange)
}

func indent(s string, n int) string {
	p := strings.Repeat(" ", n)
	return p + strings.ReplaceAll(s, "\n", "\n"+p)
}

func (a *App) renderHelp(h int) string {
	type sec struct {
		title string
		rows  [][2]string
	}
	secs := []sec{{"Global", [][2]string{
		{":", "command (:my :mr :pl :issues :projects :project x)"},
		{"/", "filter list / search text"},
		{"esc", "clear filter, then go back"},
		{"q", "back (quit at root)"},
		{"ctrl+r", "refresh now"},
		{"ctrl+c", "quit"},
		{"?", "this help"},
	}}, {"Navigation", [][2]string{
		{"j/k ↑/↓", "move"},
		{"g/G", "top / bottom"},
		{"ctrl+d/u", "page down / up"},
		{"h/l ←/→", "scroll sideways (text views)"},
		{"enter", "open"},
	}}}
	if t := a.top(); t != nil {
		var rows [][2]string
		for _, act := range t.Actions() {
			rows = append(rows, [2]string{strings.Join(act.Keys, "/"), act.Help})
		}
		if len(rows) > 0 {
			secs = append([]sec{{t.Crumb(), rows}}, secs...)
		}
	}
	var b strings.Builder
	for _, s := range secs {
		b.WriteString("\n " + sHeader.Render(s.title) + "\n")
		for _, r := range s.rows {
			b.WriteString("   " + sKey.Render(fit(r[0], 12)) + " " + r[1] + "\n")
		}
	}
	b.WriteString("\n " + sDim.Render("press any key to close"))
	return box(sAccent.Bold(true).Render("Help"), b.String(), a.w, h, cAccent2)
}

func plural(n int, s string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, s)
	}
	return fmt.Sprintf("%d %ss", n, s)
}

// poll checks my MRs for changes worth a notification.
func (a *App) poll() tea.Cmd {
	if a.polling || !cfg.Notify.enabled() || a.gl.Me == "" {
		return nil
	}
	a.polling = true
	return pollNotify(a.gl)
}

func (a *App) onNotify(mrs, done []*MR) tea.Cmd {
	news := a.notif.diff(mrs, a.gl.Me)
	byID := map[string][]notice{}
	for _, n := range news {
		byID[n.id] = append(byID[n.id], n)
	}
	writes := map[string]string{}
	if cfg.Notify.activityOn() {
		writes = a.act.update(mrs, done, byID)
	}
	if cfg.Notify.desktop() {
		var pop strings.Builder
		for i, n := range news {
			if i == 4 && len(news) > 5 {
				pop.WriteString(desktopSeq("flw", fmt.Sprintf("…and %d more changes", len(news)-4)))
				break
			}
			pop.WriteString(desktopSeq(n.title, n.body))
		}
		writes[""] += pop.String()
	}
	for tty, s := range writes {
		writeTTY(tty, s) // flw's own: one write, so it can't split a frame
	}
	if len(news) > 0 {
		return a.flashOK("🔔 " + news[0].title + " " + news[0].body)
	}
	return nil
}

func (a *App) viewDesktop() string {
	a.layout.reset()
	dock := a.renderDock()
	hints := a.renderDeskFooter()
	_, status := a.footerLines()
	bodyH := a.h - 2 - 2
	if bodyH < 3 {
		bodyH = 3
	}
	var body string
	switch {
	case !a.booted:
		body = chromeBox("flw", "\n  "+a.spin.View()+" connecting to GitLab through glab…", a.w, bodyH, true)
	case a.bootErr != nil:
		body = chromeBox("flw", "\n  "+sErr.Render("✘ "+a.bootErr.Error())+"\n\n  Check `glab auth status`.", a.w, bodyH, true)
	default:
		body = a.renderDesktop(bodyH)
	}
	body = a.renderFloating(body, bodyH)
	return dock + "\n" + body + "\n" + hints + "\n" + fit(status, a.w)
}
