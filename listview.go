package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type Column struct {
	Title string
	Width int // 0 = flexible
	Right bool
	Drop  int // hide order on narrow terminals (1 = first to go, 0 = never)
}

type Cell struct {
	Text  string
	Color lipgloss.Color
	Bold  bool
}

type Row struct {
	Key    string
	Cells  []Cell
	Data   any
	Search string
}

// KeyAction is a single-key action on the selected item.
type KeyAction struct {
	Keys  []string
	Help  string
	Hint  string // short label for the footer (defaults to Help)
	Short bool   // shown in the footer
	Fn    func(a *App, r *Row) tea.Cmd
}

func (k KeyAction) match(s string) bool {
	for _, x := range k.Keys {
		if x == s {
			return true
		}
	}
	return false
}

// ListView is a generic, filterable, auto-refreshing table (k9s-like resource view).
type ListView struct {
	title    string
	scope    string
	cols     []Column
	rows     []Row
	view     []int
	cursor   int
	offset   int
	filter   string
	meta     string
	loading  bool
	again    bool
	err      error
	loaded   time.Time
	interval time.Duration
	fast     time.Duration // used while isActive(rows) is true
	isActive func([]Row) bool
	fetch    func(gl *Client) ([]Row, string, error)
	gl       *Client
	stale    time.Time // showing cached rows saved at this time, live refresh pending
	onEnter  func(a *App, r *Row) tea.Cmd
	actions  []KeyAction
	height   int
	pred     func(Row) bool // extra row filter (focus mode)
	predName string
	// keys that also work when no row is selected (e.g. cycle state on an empty list)
	onEmptyKey map[string]func(a *App) tea.Cmd
}

type loadedMsg struct {
	v    View
	data any
	meta string
	err  error
}

func (l *ListView) Crumb() string {
	if l.scope != "" {
		return l.title + "(" + shortScope(l.scope) + ")"
	}
	return l.title
}

// shortScope keeps only the last segment of project paths: "a/b/c #12" -> "c #12".
func shortScope(s string) string {
	f := strings.Fields(s)
	for i, x := range f {
		if j := strings.LastIndex(x, "/"); j >= 0 && !strings.HasPrefix(x, "@") {
			f[i] = x[j+1:]
		}
	}
	return strings.Join(f, " ")
}

func (l *ListView) Init(a *App) tea.Cmd {
	l.gl = a.gl
	if l.fetch != nil && l.loaded.IsZero() {
		// draw the cached copy right away, then refresh from GitLab
		cc := a.gl.Cached()
		if rows, meta, err := l.fetch(cc); err == nil {
			l.loaded = time.Now()
			l.setRows(rows, meta)
			l.stale = cc.CachedAt()
		}
	}
	return l.load()
}
func (l *ListView) Refresh(a *App) tea.Cmd { l.gl = a.gl; return l.load() }

func (l *ListView) load() tea.Cmd {
	if l.fetch == nil {
		return nil
	}
	if l.loading {
		l.again = true // reload once the current fetch is done
		return nil
	}
	l.loading = true
	f, gl := l.fetch, l.gl
	return func() tea.Msg {
		rows, meta, err := f(gl)
		return loadedMsg{v: l, data: rows, meta: meta, err: err}
	}
}

func (l *ListView) Due(now time.Time) bool {
	if l.loading || l.loaded.IsZero() {
		return false
	}
	iv := l.interval
	if l.fast > 0 && l.isActive != nil && l.isActive(l.rows) {
		iv = l.fast
	}
	return iv > 0 && now.Sub(l.loaded) >= iv
}

func (l *ListView) Loaded(a *App, msg loadedMsg) tea.Cmd {
	l.loading = false
	l.loaded = time.Now()
	if l.again {
		l.again = false
		return l.load()
	}
	l.err = msg.err
	if msg.err != nil {
		return nil
	}
	l.stale = time.Time{}
	rows, _ := msg.data.([]Row)
	l.setRows(rows, msg.meta)
	return nil
}

// setRows swaps the rows and keeps the cursor on the same item.
func (l *ListView) setRows(rows []Row, meta string) {
	prev := ""
	if r := l.Selected(); r != nil {
		prev = r.Key
	}
	l.rows = rows
	l.meta = meta
	l.applyFilter()
	if prev != "" {
		for i, idx := range l.view {
			if l.rows[idx].Key == prev {
				l.cursor = i
				break
			}
		}
	}
	l.clamp()
}

func (l *ListView) Filter() string { return l.filter }
func (l *ListView) SetFilter(f string) {
	l.filter = f
	l.applyFilter()
	l.cursor, l.offset = 0, 0
}

func (l *ListView) applyFilter() {
	l.view = l.view[:0]
	f := strings.ToLower(l.filter)
	for i, r := range l.rows {
		if l.pred != nil && !l.pred(r) {
			continue
		}
		if f == "" || strings.Contains(strings.ToLower(r.Search), f) {
			l.view = append(l.view, i)
		}
	}
	l.clamp()
}

func (l *ListView) clamp() {
	if l.cursor >= len(l.view) {
		l.cursor = len(l.view) - 1
	}
	if l.cursor < 0 {
		l.cursor = 0
	}
}

func (l *ListView) Selected() *Row {
	if l.cursor < 0 || l.cursor >= len(l.view) {
		return nil
	}
	return &l.rows[l.view[l.cursor]]
}

func (l *ListView) Actions() []KeyAction { return l.actions }

func (l *ListView) HandleKey(a *App, k tea.KeyMsg) (bool, tea.Cmd) {
	page := l.height - 1
	if page < 1 {
		page = 1
	}
	switch k.String() {
	case "j", "down":
		l.cursor++
	case "k", "up":
		l.cursor--
	case "g", "home":
		l.cursor = 0
	case "G", "end":
		l.cursor = len(l.view) - 1
	case "ctrl+d", "pgdown", "ctrl+f":
		l.cursor += page
	case "ctrl+u", "pgup", "ctrl+b":
		l.cursor -= page
	case "enter":
		if r := l.Selected(); r != nil && l.onEnter != nil {
			return true, l.onEnter(a, r)
		}
		return true, nil
	default:
		for _, act := range l.actions {
			if act.match(k.String()) {
				r := l.Selected()
				if r == nil {
					if f, ok := l.onEmptyKey[k.String()]; ok {
						return true, f(a)
					}
					return true, a.flashErr("nothing selected")
				}
				return true, act.Fn(a, r)
			}
		}
		return false, nil
	}
	l.clamp()
	return true, nil
}

func (l *ListView) Render(a *App, w, h int) string {
	inner := w - 2
	l.height = h - 3 // borders + header row
	if l.height < 1 {
		l.height = 1
	}
	// scroll window
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+l.height {
		l.offset = l.cursor - l.height + 1
	}

	widths := l.colWidths(inner - 1)
	var b strings.Builder
	// header
	hdr := ""
	for i, c := range l.cols {
		if widths[i] < 0 {
			continue
		}
		t := fit(c.Title, widths[i])
		if c.Right {
			t = fitRight(c.Title, widths[i])
		}
		hdr += " " + t
	}
	b.WriteString(sHeader.Render(fit(hdr, inner)))

	switch {
	case l.err != nil && len(l.rows) == 0:
		b.WriteString("\n " + sErr.Render("✘ "+l.err.Error()))
	case l.loaded.IsZero():
		b.WriteString("\n " + sGray.Render(a.spin.View()+" loading…"))
	case len(l.view) == 0:
		b.WriteString("\n " + sGray.Render("nothing here"))
	}
	for i := l.offset; i < len(l.view) && i < l.offset+l.height; i++ {
		r := l.rows[l.view[i]]
		sel := i == l.cursor
		line := ""
		pad := " "
		if sel {
			pad = lipgloss.NewStyle().Foreground(cOrange).Background(cSelBg).Render("▌")
		}
		line += pad
		first := true
		for ci := range l.cols {
			if widths[ci] < 0 {
				continue
			}
			c := Cell{}
			if ci < len(r.Cells) {
				c = r.Cells[ci]
			}
			txt := fit(c.Text, widths[ci])
			if l.cols[ci].Right {
				txt = fitRight(c.Text, widths[ci])
			}
			st := lipgloss.NewStyle()
			if c.Color != "" {
				st = st.Foreground(c.Color)
			} else {
				st = st.Foreground(cFg)
			}
			if c.Bold {
				st = st.Bold(true)
			}
			if sel {
				st = st.Background(cSelBg).Bold(true)
			}
			if !first {
				sep := lipgloss.NewStyle()
				if sel {
					sep = sep.Background(cSelBg)
				}
				line += sep.Render(" ")
			}
			first = false
			line += st.Render(txt)
		}
		if sel {
			line = fit(line, inner)
			// extend the selection background to the edge
			if gap := inner - ansi.StringWidth(line); gap > 0 {
				line += lipgloss.NewStyle().Background(cSelBg).Render(strings.Repeat(" ", gap))
			}
		}
		b.WriteString("\n" + line)
	}

	title := sAccent.Bold(true).Render(l.title)
	if l.scope != "" {
		title += sGray.Render("(") + lipgloss.NewStyle().Foreground(cOrange).Render(l.scope) + sGray.Render(")")
	}
	count := len(l.view)
	if l.filter != "" {
		title += sGray.Render(fmt.Sprintf("[%d/%d]", count, len(l.rows)))
		title += " " + lipgloss.NewStyle().Foreground(cYellow).Render("</"+l.filter+">")
	} else {
		title += sGray.Render(fmt.Sprintf("[%d]", count))
	}
	if l.predName != "" {
		title += " " + lipgloss.NewStyle().Foreground(cAccent2).Bold(true).Render("<"+l.predName+">")
	}
	if l.meta != "" {
		title += " " + l.meta
	}
	if !l.stale.IsZero() {
		title += " " + staleTag(l.stale)
	}
	if l.err != nil && len(l.rows) > 0 {
		title += " " + sErr.Render("⚠ refresh failed")
	}
	return box(title, b.String(), w, h, cAccent)
}

// colWidths sizes columns for the available width; -1 marks a column hidden because
// the terminal is too narrow (lowest Drop value goes first).
func (l *ListView) colWidths(total int) []int {
	const minFlex = 24
	ws := make([]int, len(l.cols))
	for {
		fixed, flex, vis := 0, 0, 0
		for i, c := range l.cols {
			if ws[i] < 0 {
				continue
			}
			vis++
			if c.Width > 0 {
				fixed += c.Width
			} else {
				flex++
			}
		}
		fixed += vis - 1 // separators
		need := fixed + flex*minFlex
		if flex == 0 {
			need = fixed
		}
		if need <= total {
			break
		}
		drop := -1
		for i, c := range l.cols {
			if ws[i] >= 0 && c.Drop > 0 && (drop < 0 || c.Drop < l.cols[drop].Drop) {
				drop = i
			}
		}
		if drop < 0 {
			break
		}
		ws[drop] = -1
	}
	fixed, flex := 0, 0
	for i, c := range l.cols {
		if ws[i] < 0 {
			continue
		}
		if c.Width > 0 {
			ws[i] = c.Width
			fixed += c.Width + 1
		} else {
			flex++
		}
	}
	if flex > 0 {
		each := (total - fixed + 1) / flex
		if each < 1 {
			each = 1
		}
		for i := range ws {
			if ws[i] == 0 {
				ws[i] = each - 1
			}
		}
	}
	return ws
}

// staleTag marks data drawn from the disk cache while the live refresh runs.
func staleTag(at time.Time) string {
	return sGray.Render("⟳ cached " + ago(at))
}
