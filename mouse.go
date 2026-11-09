package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Mouse support for the desktop layout: click a window to focus it, a row to select it
// (double-click opens it), a dock pill or a sidebar line to switch; the wheel scrolls
// whatever is under the pointer. The layout records where things are while it renders.

type rect struct{ x, y, w, h int }

func (r rect) has(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

type region struct {
	r     rect
	view  View
	focus int // focMaster…, or -1 for a zoomed view
}

type hotspot struct {
	r  rect
	fn func(a *App) tea.Cmd
}

type layoutMap struct {
	regions []region
	spots   []hotspot
}

func (lm *layoutMap) reset() { lm.regions, lm.spots = lm.regions[:0], lm.spots[:0] }

func (lm *layoutMap) addView(v View, r rect, focus int) {
	if v != nil {
		lm.regions = append(lm.regions, region{r, v, focus})
	}
}

func (lm *layoutMap) addSpot(r rect, fn func(a *App) tea.Cmd) {
	lm.spots = append(lm.spots, hotspot{r, fn})
}

// workspace keys: 1-9, alt+1-9, and what alt+digit sends on an AZERTY keyboard or with
// macOS Option on a US layout.
var wsKeys = map[string]int{}

func init() {
	azerty := []string{"&", "é", "\"", "'", "(", "-", "è", "_", "ç"}
	macUS := []string{"¡", "™", "£", "¢", "∞", "§", "¶", "•", "ª"}
	for i := 0; i < 9; i++ {
		d := string(rune('1' + i))
		wsKeys[d], wsKeys["alt+"+d] = i, i
		wsKeys["alt+"+azerty[i]] = i
		wsKeys[macUS[i]] = i
	}
}

func (a *App) handleMouse(m tea.MouseMsg) tea.Cmd {
	if !desktopUI || a.w == 0 {
		return nil
	}
	ev := tea.MouseEvent(m)
	// floating windows: a click outside closes them
	if a.mode == mPalette || a.mode == mHelp {
		if ev.Action == tea.MouseActionPress && ev.Button == tea.MouseButtonLeft && a.mode == mHelp {
			a.mode = mNormal
		}
		return nil
	}
	if a.mode != mNormal {
		return nil
	}
	if ev.IsWheel() {
		key := tea.KeyMsg{Type: tea.KeyDown}
		if ev.Button == tea.MouseButtonWheelUp {
			key = tea.KeyMsg{Type: tea.KeyUp}
		}
		for _, rg := range a.layout.regions {
			if rg.r.has(ev.X, ev.Y) {
				var cmds []tea.Cmd
				for i := 0; i < 3; i++ {
					_, c := rg.view.HandleKey(a, key)
					cmds = append(cmds, c)
				}
				return tea.Batch(append(cmds, a.syncPreview())...)
			}
		}
		return nil
	}
	if ev.Action != tea.MouseActionPress || ev.Button != tea.MouseButtonLeft {
		return nil
	}
	for _, s := range a.layout.spots {
		if s.r.has(ev.X, ev.Y) {
			return s.fn(a)
		}
	}
	for _, rg := range a.layout.regions {
		if !rg.r.has(ev.X, ev.Y) {
			continue
		}
		if rg.focus >= 0 {
			a.desk.focus = rg.focus
		}
		l, ok := rg.view.(*ListView)
		if !ok {
			return nil
		}
		// rows start under the top border and the header line
		i := l.offset + ev.Y - rg.r.y - 2
		if ev.Y-rg.r.y < 2 || i < 0 || i >= len(l.view) {
			return nil
		}
		double := i == a.lastClick.row && rg.view == a.lastClick.view && time.Since(a.lastClick.at) < 400*time.Millisecond
		l.cursor = i
		a.lastClick.row, a.lastClick.view, a.lastClick.at = i, rg.view, time.Now()
		if double {
			a.lastClick.at = time.Time{}
			_, cmd := l.HandleKey(a, tea.KeyMsg{Type: tea.KeyEnter})
			return cmd
		}
		return a.syncPreview()
	}
	return nil
}
