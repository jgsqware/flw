package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// PagerView is a scrollable text view (MR details, diff, job log).
type PagerView struct {
	crumb  string
	title  func() string
	vp     viewport.Model
	w, h   int
	data   any
	fetch  func(gl *Client) (any, error)
	gl     *Client
	stale  time.Time // showing cached data saved at this time, live refresh pending
	render func(data any, w int) (lines []string, anchors []int)
	onData func(data any)
	row    *Row
	acts   []KeyAction

	loading  bool
	again    bool
	err      error
	loaded   time.Time
	interval func() time.Duration

	follow, followable bool
	wrap, wrappable    bool
	firstLoad          bool
	bottomOnLoad       bool

	lines   []string
	anchors []int
	search  string
	matches []int
	mi      int
}

func newPager(crumb string) *PagerView {
	vp := viewport.New(10, 10)
	vp.KeyMap = viewport.KeyMap{
		PageDown:     key.NewBinding(key.WithKeys("pgdown", " ", "ctrl+f")),
		PageUp:       key.NewBinding(key.WithKeys("pgup", "ctrl+b")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
		Up:           key.NewBinding(key.WithKeys("up", "k")),
		Down:         key.NewBinding(key.WithKeys("down", "j")),
		Left:         key.NewBinding(key.WithKeys("left", "h")),
		Right:        key.NewBinding(key.WithKeys("right", "l")),
	}
	vp.SetHorizontalStep(8)
	return &PagerView{crumb: crumb, vp: vp, firstLoad: true}
}

func (p *PagerView) Crumb() string { return p.crumb }
func (p *PagerView) Init(a *App) tea.Cmd {
	p.gl = a.gl
	if p.fetch != nil && p.data == nil {
		// draw the cached copy right away, then refresh from GitLab
		cc := a.gl.Cached()
		if d, err := p.fetch(cc); err == nil {
			p.loaded = time.Now()
			p.setData(d)
			p.stale = cc.CachedAt()
		}
	}
	return p.load()
}
func (p *PagerView) Refresh(a *App) tea.Cmd { p.gl = a.gl; return p.load() }
func (p *PagerView) Selected() *Row         { return p.row }
func (p *PagerView) Filter() string         { return p.search }
func (p *PagerView) SetFilter(s string)     { p.search = s; p.findMatches(true) }

func (p *PagerView) Actions() []KeyAction {
	acts := []KeyAction{}
	if p.followable {
		acts = append(acts, KeyAction{Keys: []string{"f"}, Help: "follow", Short: true})
	}
	if p.wrappable {
		acts = append(acts, KeyAction{Keys: []string{"w"}, Help: "wrap", Short: true})
	}
	if len(p.anchors) > 0 {
		acts = append(acts, KeyAction{Keys: []string{"]", "["}, Help: "next/prev file", Short: true})
	}
	if p.search != "" {
		acts = append(acts, KeyAction{Keys: []string{"n", "N"}, Help: "next/prev match", Short: true})
	}
	return append(acts, p.acts...)
}

func (p *PagerView) load() tea.Cmd {
	if p.fetch == nil {
		return nil
	}
	if p.loading {
		p.again = true
		return nil
	}
	p.loading = true
	f, gl := p.fetch, p.gl
	return func() tea.Msg {
		d, err := f(gl)
		return loadedMsg{v: p, data: d, err: err}
	}
}

func (p *PagerView) Due(now time.Time) bool {
	if p.loading || p.loaded.IsZero() || p.interval == nil {
		return false
	}
	iv := p.interval()
	return iv > 0 && now.Sub(p.loaded) >= iv
}

func (p *PagerView) Loaded(a *App, msg loadedMsg) tea.Cmd {
	p.loading = false
	p.loaded = time.Now()
	if p.again {
		p.again = false
		return p.load()
	}
	p.err = msg.err
	if msg.err != nil {
		return nil
	}
	p.stale = time.Time{}
	p.setData(msg.data)
	return nil
}

func (p *PagerView) setData(d any) {
	p.data = d
	if p.onData != nil {
		p.onData(p.data)
	}
	p.rerender()
	if p.firstLoad {
		p.firstLoad = false
		if p.bottomOnLoad {
			p.vp.GotoBottom()
		}
	}
}

func (p *PagerView) rerender() {
	if p.data == nil || p.render == nil || p.w <= 0 {
		return
	}
	lines, anchors := p.render(p.data, p.w)
	if p.wrap {
		// recompute anchors after wrapping
		var out []string
		var na []int
		ai := 0
		for i, l := range lines {
			for ai < len(anchors) && anchors[ai] == i {
				na = append(na, len(out))
				ai++
			}
			out = append(out, wrapLines([]string{l}, p.w)...)
		}
		lines, anchors = out, na
	}
	p.lines, p.anchors = lines, anchors
	atBottom := p.vp.AtBottom()
	p.vp.SetContent(strings.Join(lines, "\n"))
	if p.follow || (p.followable && atBottom && !p.firstLoad) {
		p.vp.GotoBottom()
	}
	if p.search != "" {
		p.findMatches(false)
	}
}

func (p *PagerView) findMatches(jump bool) {
	p.matches = p.matches[:0]
	if p.search == "" {
		return
	}
	s := strings.ToLower(p.search)
	for i, l := range p.lines {
		if strings.Contains(strings.ToLower(ansi.Strip(l)), s) {
			p.matches = append(p.matches, i)
		}
	}
	if jump && len(p.matches) > 0 {
		p.mi = 0
		for i, m := range p.matches {
			if m >= p.vp.YOffset {
				p.mi = i
				break
			}
		}
		p.follow = false
		p.vp.SetYOffset(p.matches[p.mi])
	}
}

func (p *PagerView) HandleKey(a *App, k tea.KeyMsg) (bool, tea.Cmd) {
	s := k.String()
	switch s {
	case "g", "home":
		p.follow = false
		p.vp.GotoTop()
		return true, nil
	case "G", "end":
		p.vp.GotoBottom()
		return true, nil
	case "f":
		if p.followable {
			p.follow = !p.follow
			if p.follow {
				p.vp.GotoBottom()
			}
			return true, nil
		}
	case "w":
		if p.wrappable {
			p.wrap = !p.wrap
			p.rerender()
			return true, nil
		}
	case "]", "[":
		if len(p.anchors) > 0 {
			p.follow = false
			y := p.vp.YOffset
			target := -1
			if s == "]" {
				for _, an := range p.anchors {
					if an > y {
						target = an
						break
					}
				}
			} else {
				for i := len(p.anchors) - 1; i >= 0; i-- {
					if p.anchors[i] < y {
						target = p.anchors[i]
						break
					}
				}
			}
			if target >= 0 {
				p.vp.SetYOffset(target)
			}
			return true, nil
		}
	case "n", "N":
		if len(p.matches) > 0 {
			if s == "n" {
				p.mi = (p.mi + 1) % len(p.matches)
			} else {
				p.mi = (p.mi - 1 + len(p.matches)) % len(p.matches)
			}
			p.follow = false
			p.vp.SetYOffset(p.matches[p.mi])
			return true, nil
		}
	}
	for _, act := range p.acts {
		if act.match(s) {
			if p.row == nil {
				return true, a.flashErr("still loading")
			}
			return true, act.Fn(a, p.row)
		}
	}
	switch s {
	case "up", "k", "pgup", "ctrl+u", "ctrl+b":
		p.follow = false
	}
	var cmd tea.Cmd
	p.vp, cmd = p.vp.Update(k)
	return true, cmd
}

func (p *PagerView) Render(a *App, w, h int) string {
	iw, ih := w-2, h-2
	if iw != p.w || ih != p.vp.Height {
		p.w = iw
		p.vp.Width, p.vp.Height = iw, ih
		p.rerender()
	}
	body := p.vp.View()
	if p.data == nil {
		switch {
		case p.err != nil:
			body = " " + sErr.Render("✘ "+p.err.Error())
		default:
			body = " " + sGray.Render(a.spin.View()+" loading…")
		}
	}
	t := sAccent.Bold(true).Render(p.crumb)
	if p.title != nil {
		t = p.title()
	}
	var tags []string
	if p.followable && p.follow {
		tags = append(tags, lipgloss.NewStyle().Foreground(cGreen).Render("[follow]"))
	}
	if p.wrap {
		tags = append(tags, sGray.Render("[wrap]"))
	}
	if p.search != "" {
		m := fmt.Sprintf("</%s> %d/%d", p.search, min(p.mi+1, len(p.matches)), len(p.matches))
		tags = append(tags, lipgloss.NewStyle().Foreground(cYellow).Render(m))
	}
	if !p.stale.IsZero() {
		tags = append(tags, staleTag(p.stale))
	}
	if p.err != nil && p.data != nil {
		tags = append(tags, sErr.Render("⚠ refresh failed"))
	}
	if len(p.lines) > ih {
		tags = append(tags, sGray.Render(fmt.Sprintf("%d%%", int(p.vp.ScrollPercent()*100))))
	}
	if len(tags) > 0 {
		t += " " + strings.Join(tags, " ")
	}
	return box(t, body, w, h, cAccent)
}
