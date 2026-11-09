package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/aymanbagabas/go-osc52/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// openURL opens a URL in the browser ($BROWSER, open, xdg-open).
func openURL(u string) error {
	var cands [][]string
	if b := os.Getenv("BROWSER"); b != "" {
		cands = append(cands, []string{b})
	}
	cands = append(cands, []string{"open"}, []string{"xdg-open"}, []string{"wslview"})
	var last error
	for _, c := range cands {
		if _, err := exec.LookPath(c[0]); err != nil {
			last = err
			continue
		}
		cmd := exec.Command(c[0], append(c[1:], u)...)
		cmd.Stdout, cmd.Stderr = nil, nil
		if err := cmd.Start(); err != nil {
			last = err
			continue
		}
		go cmd.Wait()
		return nil
	}
	return last
}

// copyText copies to the clipboard with a native tool, and OSC52 as a fallback (works over SSH).
func copyText(s string) {
	for _, c := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(s)
		if cmd.Run() == nil {
			return
		}
	}
	_, _ = osc52.New(s).WriteTo(os.Stderr)
}

var (
	tsPrefixRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z [0-9a-f]{2}[OE]\+? ?`)
	sectionRe  = regexp.MustCompile(`section_(start|end):\d+:[^\r\n]*\r`)
	nonSGRRe   = regexp.MustCompile(`\x1b\[[0-9;?]*[A-LN-Za-ln-z]`)
)

// cleanTrace turns a raw GitLab job trace into displayable lines.
func cleanTrace(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	src := strings.Split(raw, "\n")
	out := make([]string, 0, len(src))
	for _, l := range src {
		l = tsPrefixRe.ReplaceAllString(l, "")
		marker := sectionRe.MatchString(l)
		l = sectionRe.ReplaceAllString(l, "")
		l = nonSGRRe.ReplaceAllString(l, "")
		if strings.Contains(l, "\r") {
			parts := strings.Split(l, "\r")
			l = ""
			for i := len(parts) - 1; i >= 0; i-- {
				if strings.TrimSpace(ansi.Strip(parts[i])) != "" {
					l = parts[i]
					break
				}
			}
		}
		l = strings.ReplaceAll(l, "\t", "    ")
		if marker && strings.TrimSpace(ansi.Strip(l)) == "" {
			continue // a line that only held a section marker
		}
		if strings.Contains(l, "\x1b[") {
			l += "\x1b[0m"
		}
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

// fit truncates or pads an (ANSI-aware) string to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	sw := ansi.StringWidth(s)
	if sw > w {
		return ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", w-sw)
}

// box draws a k9s-like frame with a title in the top border.
func box(title, body string, w, h int, color lipgloss.Color) string {
	bc := lipgloss.NewStyle().Foreground(color)
	inner := w - 2
	if inner < 1 || h < 2 {
		return ""
	}
	t := ""
	if title != "" {
		t = " " + title + " "
	}
	tw := ansi.StringWidth(t)
	if tw > inner-2 {
		t = ansi.Truncate(t, inner-2, "…")
		tw = ansi.StringWidth(t)
	}
	left := (inner - tw) / 2
	top := bc.Render("┌"+strings.Repeat("─", left)) + t + bc.Render(strings.Repeat("─", inner-left-tw)+"┐")
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
	b.WriteString("\n" + bc.Render("└"+strings.Repeat("─", inner)+"┘"))
	return b.String()
}

func wrapLines(lines []string, w int) []string {
	if w <= 0 {
		return lines
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if ansi.StringWidth(l) <= w {
			out = append(out, l)
			continue
		}
		out = append(out, strings.Split(ansi.Hardwrap(l, w, true), "\n")...)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lookGlab() (string, error) { return exec.LookPath("glab") }

// fitRight right-aligns s in w cells.
func fitRight(s string, w int) string {
	if ansi.StringWidth(s) > w {
		return ansi.Truncate(s, w, "…")
	}
	return strings.Repeat(" ", w-ansi.StringWidth(s)) + s
}
