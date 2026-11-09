package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The desktop UI (tuios style): a dock with workspaces on top, tiled windows with
// rounded borders and ● ● ● buttons, an attention sidebar, floating windows over a dimmed
// desktop, and a command palette. `-ui classic` keeps the single-view layout.

var (
	desktopUI     = true
	chromeFocused = true // set by the compositor before each window renders
)

const (
	focMaster = iota
	focPreview
	focPipe
	focCount
)

type deskState struct {
	cur    string            // current workspace: "" = dashboard, else a project path
	stacks map[string][]View // view stack of every workspace
	focus  int
	zoom   bool

	preview    *PagerView
	previewKey string
	pipe       *ListView
	pipeKey    string
	wantKey    string

	rexTabs  []rexTab
	rexAt    time.Time
	scanning bool

	palItems []palItem
	palSel   int
}

type previewMsg struct{ key string }
type rexTabsMsg struct{ tabs []rexTab }

// ---------- workspaces ----------

func (a *App) dashMRs() []*MR {
	st := a.desk.stacks[""]
	if a.desk.cur == "" {
		st = a.stack
	}
	if len(st) == 0 {
		return nil
	}
	return mrsOf(st[0])
}

func mrsOf(v View) []*MR {
	l, ok := v.(*ListView)
	if !ok {
		return nil
	}
	var out []*MR
	for _, r := range l.rows {
		if m, ok := r.Data.(*MR); ok {
			out = append(out, m)
		}
	}
	return out
}

// workspaces: the dashboard, then the projects of my MRs (most MRs first), plus the current.
func (a *App) workspaces() []string {
	count := map[string]int{}
	for _, m := range a.dashMRs() {
		count[m.Path()]++
	}
	if a.desk.cur != "" {
		count[a.desk.cur] += 0
	}
	for k := range a.desk.stacks {
		if k != "" {
			count[k] += 0
		}
	}
	var ps []string
	for p := range count {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool {
		if count[ps[i]] != count[ps[j]] {
			return count[ps[i]] > count[ps[j]]
		}
		return ps[i] < ps[j]
	})
	if len(ps) > 8 {
		ps = ps[:8]
	}
	return append([]string{""}, ps...)
}

func (a *App) switchWS(name string) tea.Cmd {
	if name == a.desk.cur && len(a.stack) > 0 {
		return nil
	}
	if a.desk.stacks == nil {
		a.desk.stacks = map[string][]View{}
	}
	a.desk.stacks[a.desk.cur] = a.stack
	a.desk.cur = name
	a.desk.zoom, a.desk.focus = false, focMaster
	a.desk.previewKey, a.desk.pipeKey, a.desk.wantKey = "", "", ""
	a.desk.preview, a.desk.pipe = nil, nil
	if name != "" {
		a.project = name
	}
	if st, ok := a.desk.stacks[name]; ok && len(st) > 0 {
		a.stack = st
		return tea.Batch(a.top().Refresh(a), a.syncPreview())
	}
	var v View
	if name == "" {
		v = newMyMRsView(a)
	} else {
		v = newProjectMRsView(a, name)
	}
	return tea.Batch(a.reset(v), a.syncPreview())
}

func wsLabel(name string) string {
	if name == "" {
		return "Dashboard"
	}
	return shortScope(name)
}

// turnCounts: ▶ on you, ⏳ to ping, 💤 snoozed, 🧹 to clean up.
func turnCounts(mrs []*MR) (mine, ping, zz, clean int) {
	for _, m := range mrs {
		switch m.Turn.Bucket() {
		case bucketMine, bucketMineLate:
			mine++
		case bucketPing, bucketReping:
			ping++
		case bucketSnoozed:
			zz++
		case bucketCleanup:
			clean++
		}
	}
	return
}

// ---------- preview windows that follow the cursor ----------

func (a *App) tiled() bool {
	if !desktopUI || a.desk.zoom || len(a.stack) != 1 {
		return false
	}
	l, ok := a.stack[0].(*ListView)
	return ok && strings.Contains(l.title, "MergeRequests")
}

func (a *App) selectedMR() *MR {
	if len(a.stack) == 0 {
		return nil
	}
	l, ok := a.stack[0].(*ListView)
	if !ok {
		return nil
	}
	if r := l.Selected(); r != nil {
		m, _ := r.Data.(*MR)
		return m
	}
	return nil
}

// syncPreview schedules a preview reload when the selected MR changed (debounced).
func (a *App) syncPreview() tea.Cmd {
	if !a.tiled() {
		return nil
	}
	m := a.selectedMR()
	if m == nil {
		return nil
	}
	key := mrKey(m)
	if key == a.desk.previewKey || key == a.desk.wantKey {
		return nil
	}
	a.desk.wantKey = key
	return tea.Tick(180*time.Millisecond, func(time.Time) tea.Msg { return previewMsg{key} })
}

func (a *App) loadPreview(key string) tea.Cmd {
	a.desk.wantKey = ""
	m := a.selectedMR()
	if m == nil || mrKey(m) != key || key == a.desk.previewKey {
		return nil
	}
	a.desk.previewKey = key
	a.desk.preview = newMRDetailView(a, m)
	cmds := []tea.Cmd{a.desk.preview.Init(a)}
	a.desk.pipe, a.desk.pipeKey = nil, ""
	if m.HeadPipeline != nil && m.HeadPipeline.IID != "" {
		a.desk.pipeKey = m.Path() + "#" + m.HeadPipeline.IID
		a.desk.pipe = newJobsView(a, m.Path(), m.HeadPipeline.IID)
		cmds = append(cmds, a.desk.pipe.Init(a))
	}
	return tea.Batch(cmds...)
}

func (a *App) focusedView() View {
	if a.tiled() {
		switch a.desk.focus {
		case focPreview:
			if a.desk.preview != nil {
				return a.desk.preview
			}
		case focPipe:
			if a.desk.pipe != nil {
				return a.desk.pipe
			}
		}
	}
	return a.top()
}

func (a *App) scanRexCmd() tea.Cmd {
	if !inRex() || a.desk.scanning || time.Since(a.desk.rexAt) < 20*time.Second {
		return nil
	}
	a.desk.scanning = true
	return func() tea.Msg { return rexTabsMsg{scanRex()} }
}

// deskKey handles the desktop keys; ok=false lets the normal handling go on.
func (a *App) deskKey(k tea.KeyMsg) (bool, tea.Cmd) {
	s := k.String()
	if i, ok := wsKeys[s]; ok {
		if ws := a.workspaces(); i < len(ws) {
			return true, a.switchWS(ws[i])
		}
		return true, nil
	}
	switch s {
	case "ctrl+p":
		return true, a.openPalette()
	case "tab", "shift+tab":
		if !a.tiled() {
			return false, nil
		}
		d := 1
		if s == "shift+tab" {
			d = focCount - 1
		}
		for i := 0; i < focCount; i++ {
			a.desk.focus = (a.desk.focus + d) % focCount
			if a.desk.focus == focMaster || (a.desk.focus == focPreview && a.desk.preview != nil) || (a.desk.focus == focPipe && a.desk.pipe != nil) {
				break
			}
		}
		return true, nil
	case "f":
		switch {
		case len(a.stack) > 1:
			return false, nil
		case a.tiled() && a.desk.focus == focPreview && a.desk.preview != nil:
			if m := mrOf(a.desk.preview.row); m != nil {
				return true, a.push(newMRDetailView(a, m))
			}
		case a.tiled() && a.desk.focus == focPipe && a.desk.pipe != nil:
			if m := a.selectedMR(); m != nil && m.HeadPipeline != nil {
				return true, a.push(newJobsView(a, m.Path(), m.HeadPipeline.IID))
			}
		}
		a.desk.zoom = !a.desk.zoom
		if !a.desk.zoom {
			return true, a.syncPreview()
		}
		return true, nil
	case " ":
		if _, ok := a.focusedView().(*ListView); ok {
			a.mode = mHelp
			return true, nil
		}
	case "esc", "q":
		if a.tiled() && a.desk.focus != focMaster {
			a.desk.focus = focMaster
			return true, nil
		}
	}
	if a.tiled() && a.desk.focus != focMaster {
		if v := a.focusedView(); v != nil {
			_, cmd := v.HandleKey(a, k)
			return true, cmd
		}
	}
	return false, nil
}

// ---------- palette ----------

type palItem struct {
	label, kind string
	run         func(a *App) tea.Cmd
}

func (a *App) paletteItems() []palItem {
	var items []palItem
	for i, ws := range a.workspaces() {
		ws := ws
		items = append(items, palItem{fmt.Sprintf("%d  %s", i+1, wsLabel(ws)), "workspace", func(a *App) tea.Cmd { return a.switchWS(ws) }})
	}
	for _, m := range a.dashMRs() {
		m := m
		items = append(items, palItem{fmt.Sprintf("!%s %s · %s  %s", m.IID, shortScope(m.Path()), m.Title, strings.TrimSpace(nextCell(m.Turn).Text)), "merge request",
			func(a *App) tea.Cmd { return a.push(newMRDetailView(a, m)) }})
	}
	for _, c := range []struct{ cmd, help string }{
		{"my", "my open merge requests"}, {"projects", "my projects"}, {"pl", "pipelines of the current project"},
		{"issues", "issues of the current project"}, {"mr", "merge requests of the current project"}, {"quit", "quit flw"},
	} {
		c := c
		items = append(items, palItem{":" + c.cmd + "  " + c.help, "command", func(a *App) tea.Cmd { return a.runCommand(c.cmd) }})
	}
	return items
}

func (a *App) openPalette() tea.Cmd {
	a.mode = mPalette
	a.desk.palSel = 0
	a.desk.palItems = a.paletteItems()
	a.input.SetValue("")
	return a.input.Focus()
}

func (a *App) paletteMatches() []palItem {
	words := strings.Fields(strings.ToLower(a.input.Value()))
	var out []palItem
	for _, it := range a.desk.palItems {
		l := strings.ToLower(it.label + " " + it.kind)
		ok := true
		for _, w := range words {
			if !strings.Contains(l, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, it)
		}
	}
	return out
}

func (a *App) paletteKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc", "ctrl+p":
		a.mode = mNormal
		a.input.Blur()
		return nil
	case "up", "ctrl+k", "shift+tab":
		if a.desk.palSel > 0 {
			a.desk.palSel--
		}
		return nil
	case "down", "ctrl+j", "tab":
		if a.desk.palSel < len(a.paletteMatches())-1 {
			a.desk.palSel++
		}
		return nil
	case "enter":
		ms := a.paletteMatches()
		a.mode = mNormal
		a.input.Blur()
		if a.desk.palSel < len(ms) {
			return ms[a.desk.palSel].run(a)
		}
		return nil
	}
	var cmd tea.Cmd
	a.input, cmd = a.input.Update(k)
	a.desk.palSel = 0
	return cmd
}

// ---------- rendering ----------

var (
	cDot1 = lipgloss.Color("#f7768e")
	cDot2 = lipgloss.Color("#e0af68")
	cDot3 = lipgloss.Color("#9ece6a")
)

// chromeBox draws a tuios-style window: rounded border, ● ● ● buttons, title on the right.
func chromeBox(title, body string, w, h int, focused bool) string {
	inner := w - 2
	if inner < 4 || h < 2 {
		return ""
	}
	bc := lipgloss.NewStyle().Foreground(cDim)
	dots := sDim.Render("● ● ●")
	if focused {
		bc = lipgloss.NewStyle().Foreground(cOrange)
		dots = lipgloss.NewStyle().Foreground(cDot1).Render("●") + " " + lipgloss.NewStyle().Foreground(cDot2).Render("●") + " " + lipgloss.NewStyle().Foreground(cDot3).Render("●")
	}
	t := ""
	if title != "" {
		t = " " + title + " "
	}
	room := inner - 8 // "╭ ● ● ● ─" + "╮"
	if ansi.StringWidth(t) > room {
		t = ansi.Truncate(t, max(room, 0), "… ")
	}
	fill := inner - 7 - ansi.StringWidth(t)
	if fill < 1 {
		fill = 1
	}
	top := bc.Render("╭ ") + dots + bc.Render(" "+strings.Repeat("─", fill)) + t + bc.Render("╮")
	lines := strings.Split(body, "\n")
	var b strings.Builder
	b.WriteString(top)
	for i := 0; i < h-2; i++ {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		b.WriteString("\n" + bc.Render("│") + fit(l, inner) + bc.Render("│"))
	}
	b.WriteString("\n" + bc.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return b.String()
}

// renderWin renders a view inside its own chrome (views draw with box(), which follows chromeFocused).
func renderWin(a *App, v View, w, h int, focused bool) string {
	old := chromeFocused
	chromeFocused = focused
	defer func() { chromeFocused = old }()
	return v.Render(a, w, h)
}

func hjoin(cols ...string) string {
	var parts [][]string
	maxH := 0
	for _, c := range cols {
		ls := strings.Split(c, "\n")
		parts = append(parts, ls)
		maxH = max(maxH, len(ls))
	}
	var b strings.Builder
	for i := 0; i < maxH; i++ {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, p := range parts {
			if i < len(p) {
				b.WriteString(p[i])
			}
		}
	}
	return b.String()
}

func (a *App) renderDock() string {
	logo := sLogo.Render("▣ flw")
	var pills []string
	for i, ws := range a.workspaces() {
		var mrs []*MR
		for _, m := range a.dashMRs() {
			if ws == "" || m.Path() == ws {
				mrs = append(mrs, m)
			}
		}
		mine, ping, _, _ := turnCounts(mrs)
		badge := ""
		if mine > 0 {
			badge += " ▶" + fmt.Sprint(mine)
		}
		if ping > 0 && ws == "" {
			badge += " ⏳" + fmt.Sprint(ping)
		}
		txt := fmt.Sprintf(" %d %s%s ", i+1, wsLabel(ws), badge)
		if ws == a.desk.cur {
			pills = append(pills, lipgloss.NewStyle().Foreground(lipgloss.Color("#1a1b26")).Background(cOrange).Bold(true).Render(txt))
		} else {
			pills = append(pills, sGray.Render(txt))
		}
	}
	x := ansi.StringWidth(logo) + 1
	for i, ws := range a.workspaces() {
		if i >= len(pills) {
			break
		}
		pw, ws := ansi.StringWidth(pills[i]), ws
		a.layout.addSpot(rect{x, 0, pw, 1}, func(a *App) tea.Cmd { return a.switchWS(ws) })
		x += pw + 1
	}
	left := logo + " " + strings.Join(pills, sDim.Render("│"))
	var right []string
	if a.busy > 0 || (a.top() != nil && isLoading(a.top())) {
		right = append(right, a.spin.View())
	}
	if l, ok := a.top().(*ListView); ok && !l.stale.IsZero() {
		right = append(right, sGray.Render("⟳ "+ago(l.stale)))
	}
	host := strings.TrimPrefix(strings.TrimPrefix(a.gl.BaseURL, "https://"), "http://")
	if host == "" {
		host = a.gl.Host
	}
	if host != "" {
		right = append(right, sGray.Render("⛁ ")+lipgloss.NewStyle().Foreground(cBlue).Render(host))
	}
	if a.gl.Me != "" {
		right = append(right, sAccent.Render("@"+a.gl.Me))
	}
	right = append(right, sGray.Render(time.Now().Format("15:04")))
	r := strings.Join(right, "  ") + " "
	line := left
	if gap := a.w - ansi.StringWidth(left) - ansi.StringWidth(r); gap > 0 {
		line += strings.Repeat(" ", gap) + r
	}
	// rule, with the breadcrumb of the zoomed views
	rule := ""
	if len(a.stack) > 1 {
		var crumbs []string
		for _, v := range a.stack {
			crumbs = append(crumbs, ansi.Truncate(v.Crumb(), 30, "…"))
		}
		rule = sDim.Render("━━ ") + sGray.Render(strings.Join(crumbs, " › ")) + " "
	}
	if rw := ansi.StringWidth(rule); rw < a.w {
		rule += lipgloss.NewStyle().Foreground(cDim).Render(strings.Repeat("━", a.w-rw))
	}
	return fit(line, a.w) + "\n" + fit(rule, a.w)
}

func (a *App) renderSidebar(x0, y0, w, h int) string {
	var b []string
	click := func(fn func(a *App) tea.Cmd) {
		a.layout.addSpot(rect{x0, y0 + len(b) - 1, w, 1}, fn)
	}
	head := func(s string) {
		if len(b) > 0 {
			b = append(b, "", sDim.Render(strings.Repeat(" ", max(w-6, 0))+"───"))
		}
		b = append(b, " "+sHeader.Render(s))
	}
	row := func(on bool, label, val string, c lipgloss.Color) {
		mark := " "
		if on {
			mark = lipgloss.NewStyle().Foreground(cOrange).Render("▎")
		}
		label = ansi.Truncate(label, w-6-ansi.StringWidth(val), "…")
		pad := w - 3 - ansi.StringWidth(label) - ansi.StringWidth(val)
		b = append(b, mark+lipgloss.NewStyle().Foreground(c).Render(label)+strings.Repeat(" ", max(pad, 1))+sGray.Render(val))
	}
	mrs := mrsOf(a.stack[0])
	mine, ping, zz, clean := turnCounts(mrs)
	pred := ""
	if l, ok := a.stack[0].(*ListView); ok {
		pred = l.predName
	}
	head("attention")
	focusOn := func(name string) func(a *App) tea.Cmd {
		return func(a *App) tea.Cmd {
			l, ok := a.stack[0].(*ListView)
			if !ok {
				return nil
			}
			for i, f := range focusModes {
				if f.name == name {
					if l.predName == name {
						i = 0 // clicking the active one shows everything again
					}
					a.desk.focus = focMaster
					return tea.Batch(setFocusMode(a, l, i), a.syncPreview())
				}
			}
			return nil
		}
	}
	row(pred == "on me", "▶ on you", fmt.Sprint(mine), cOrange)
	click(focusOn("on me"))
	row(pred == "to ping", "⏳ to ping", fmt.Sprint(ping), cYellow)
	click(focusOn("to ping"))
	row(pred == "snoozed", "💤 snoozed", fmt.Sprint(zz), cGray)
	click(focusOn("snoozed"))
	row(pred == "to clean up", "🧹 to clean up", fmt.Sprint(clean), cAccent2)
	click(focusOn("to clean up"))
	head("workspaces")
	for i, ws := range a.workspaces() {
		n := 0
		for _, m := range a.dashMRs() {
			if ws == "" || m.Path() == ws {
				n++
			}
		}
		row(ws == a.desk.cur, fmt.Sprintf("%d %s", i+1, wsLabel(ws)), fmt.Sprint(n), cFg)
		ws := ws
		click(func(a *App) tea.Cmd { return a.switchWS(ws) })
	}
	if inRex() {
		head("rex tabs")
		shown := 0
		for _, t := range a.desk.rexTabs {
			if !strings.HasPrefix(t.Label, "!") {
				continue
			}
			v := ""
			if t.hasClaude() {
				v = "claude ●"
			}
			row(false, t.Label, v, cBlue)
			if shown++; shown >= 8 {
				break
			}
		}
		if shown == 0 {
			b = append(b, " "+sDim.Render("none yet: w on an MR"))
		}
	}
	for len(b) < h {
		b = append(b, "")
	}
	bar := sDim.Render("│")
	for i := range b {
		b[i] = bar + fit(b[i], w-1)
	}
	return strings.Join(b[:h], "\n")
}

func (a *App) renderDesktop(bodyH int) string {
	if len(a.stack) == 0 {
		return ""
	}
	w := a.w
	const y0 = 2 // under the dock
	if !a.tiled() {
		a.layout.addView(a.top(), rect{0, y0, w, bodyH}, -1)
		return renderWin(a, a.top(), w, bodyH, true)
	}
	sw := 0
	if w >= 120 {
		sw = 30
	}
	rest := w - sw
	mw := rest * 52 / 100
	pw := rest - mw
	ph := bodyH * 58 / 100
	a.layout.addView(a.stack[0], rect{0, y0, mw, bodyH}, focMaster)
	if a.desk.preview != nil {
		a.layout.addView(a.desk.preview, rect{mw, y0, pw, ph}, focPreview)
	}
	if a.desk.pipe != nil {
		a.layout.addView(a.desk.pipe, rect{mw, y0 + ph, pw, bodyH - ph}, focPipe)
	}
	master := renderWin(a, a.stack[0], mw, bodyH, a.desk.focus == focMaster)
	var prev, pipe string
	if a.desk.preview != nil {
		prev = renderWin(a, a.desk.preview, pw, ph, a.desk.focus == focPreview)
	} else {
		prev = chromeBox(sGray.Render("preview"), "\n "+sGray.Render("select a merge request"), pw, ph, false)
	}
	if a.desk.pipe != nil {
		pipe = renderWin(a, a.desk.pipe, pw, bodyH-ph, a.desk.focus == focPipe)
	} else {
		pipe = chromeBox(sGray.Render("pipeline"), "\n "+sGray.Render("no pipeline"), pw, bodyH-ph, false)
	}
	cols := []string{master, prev + "\n" + pipe}
	if sw > 0 {
		cols = append(cols, a.renderSidebar(rest, y0, sw, bodyH))
	}
	return hjoin(cols...)
}

// ---------- floating windows ----------

// overlay draws fg centered over a dimmed copy of bg.
func overlay(bg, fg string, w, h int) string {
	bgl := strings.Split(bg, "\n")
	fgl := strings.Split(fg, "\n")
	fw := 0
	for _, l := range fgl {
		fw = max(fw, ansi.StringWidth(l))
	}
	x, y := max((w-fw)/2, 0), max((h-len(fgl))/2, 0)
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("#3b4261"))
	out := make([]string, h)
	for i := 0; i < h; i++ {
		plain := ""
		if i < len(bgl) {
			plain = ansi.Strip(bgl[i])
		}
		plain = fit(plain, w)
		if i >= y && i < y+len(fgl) {
			out[i] = dim.Render(ansi.Cut(plain, 0, x)) + fit(fgl[i-y], fw) + dim.Render(ansi.Cut(plain, x+fw, w))
		} else {
			out[i] = dim.Render(plain)
		}
	}
	return strings.Join(out, "\n")
}

func (a *App) renderPalette() string {
	fw := min(96, a.w-6)
	ms := a.paletteMatches()
	var b strings.Builder
	b.WriteString(" " + sKey.Render("›") + " " + a.input.View() + "\n " + sDim.Render(strings.Repeat("─", fw-4)) + "\n")
	n := min(len(ms), 14)
	start := 0
	if a.desk.palSel >= n {
		start = a.desk.palSel - n + 1
	}
	for i := start; i < start+n && i < len(ms); i++ {
		it := ms[i]
		label := ansi.Truncate(it.label, fw-20, "…")
		kind := sDim.Render(it.kind)
		pad := fw - 5 - ansi.StringWidth(label) - ansi.StringWidth(it.kind)
		line := label + strings.Repeat(" ", max(pad, 1)) + kind
		if i == a.desk.palSel {
			line = lipgloss.NewStyle().Background(cSelBg).Foreground(cOrange).Bold(true).Render(fit(ansi.Strip(label)+strings.Repeat(" ", max(pad, 1))+it.kind, fw-4))
		}
		b.WriteString(" " + line + "\n")
	}
	if len(ms) == 0 {
		b.WriteString(" " + sGray.Render("no match") + "\n")
	}
	b.WriteString("\n " + hint("enter", "run") + "  " + hint("↑↓", "move") + "  " + hint("esc", "close"))
	h := strings.Count(b.String(), "\n") + 3
	return chromeBox(sAccent.Bold(true).Render("palette"), b.String(), fw, h, true)
}

func (a *App) renderFloating(bg string, bodyH int) string {
	var fg string
	switch a.mode {
	case mPalette:
		fg = a.renderPalette()
	case mText:
		fw := min(104, a.w-4)
		body := "\n " + sGray.Render(a.label) + "\n\n" + indent(a.ta.View(), 1) + "\n\n " + hint("ctrl+s", "send") + "  " + hint("esc", "cancel")
		fg = chromeBox(sAccent.Bold(true).Render("comment"), body, fw, min(bodyH-2, a.ta.Height()+8), true)
	case mHelp:
		fg = a.renderWhichKey(bodyH)
	default:
		return bg
	}
	return overlay(bg, fg, a.w, bodyH)
}

// renderWhichKey lists the keys of the focused window, in columns.
func (a *App) renderWhichKey(bodyH int) string {
	v := a.focusedView()
	var rows [][2]string
	if v != nil {
		for _, act := range v.Actions() {
			rows = append(rows, [2]string{strings.Join(act.Keys, "/"), act.Help})
		}
	}
	rows = append(rows, [2]string{"tab", "next window"}, [2]string{"f", "zoom / open full"}, [2]string{"ctrl+p", "palette"},
		[2]string{"1-9", "workspace"}, [2]string{":", "command"}, [2]string{"/", "filter"}, [2]string{"esc", "back"})
	fw := min(110, a.w-6)
	colW := (fw - 4) / 2
	var b strings.Builder
	half := (len(rows) + 1) / 2
	for i := 0; i < half; i++ {
		cell := func(r [2]string) string {
			return fit(sKey.Render(fit(r[0], 9))+" "+ansi.Truncate(r[1], colW-11, "…"), colW)
		}
		l := cell(rows[i])
		if i+half < len(rows) {
			l += cell(rows[i+half])
		}
		b.WriteString(" " + l + "\n")
	}
	b.WriteString("\n " + sDim.Render("any key to close"))
	title := "keys"
	if v != nil {
		title = "keys · " + ansi.Strip(v.Crumb())
	}
	return chromeBox(sAccent.Bold(true).Render(title), b.String(), fw, min(bodyH-2, half+4), true)
}

func (a *App) renderDeskFooter() string {
	var hints []string
	if a.tiled() {
		hints = append(hints, hint("tab", "window"))
	}
	if v := a.focusedView(); v != nil {
		if _, ok := v.(*ListView); ok {
			hints = append(hints, hint("enter", "open"))
		}
		for _, act := range v.Actions() {
			if act.Short {
				h := act.Hint
				if h == "" {
					h = act.Help
				}
				hints = append(hints, hint(strings.Join(act.Keys, "/"), h))
			}
		}
	}
	hints = append(hints, hint("f", "zoom"), hint("ctrl+p", "palette"), hint("space", "keys"), hint("1-9", "workspace"))
	return fit(strings.Join(hints, "  "), a.w)
}
