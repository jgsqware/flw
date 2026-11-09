package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	cAccent  = lipgloss.Color("#7aa2f7")
	cAccent2 = lipgloss.Color("#bb9af7")
	cGreen   = lipgloss.Color("#9ece6a")
	cRed     = lipgloss.Color("#f7768e")
	cYellow  = lipgloss.Color("#e0af68")
	cBlue    = lipgloss.Color("#7dcfff")
	cGray    = lipgloss.Color("#737aa2")
	cDim     = lipgloss.Color("#565f89")
	cFg      = lipgloss.Color("#c0caf5")
	cSelBg   = lipgloss.Color("#283457")
	cOrange  = lipgloss.Color("#ff9e64")

	sBold    = lipgloss.NewStyle().Bold(true)
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sGray    = lipgloss.NewStyle().Foreground(cGray)
	sAccent  = lipgloss.NewStyle().Foreground(cAccent)
	sKey     = lipgloss.NewStyle().Foreground(cAccent2).Bold(true)
	sLogo    = lipgloss.NewStyle().Foreground(lipgloss.Color("#1a1b26")).Background(cOrange).Bold(true).Padding(0, 1)
	sCrumb   = lipgloss.NewStyle().Foreground(lipgloss.Color("#1a1b26")).Background(cAccent).Padding(0, 1)
	sCrumbOn = lipgloss.NewStyle().Foreground(lipgloss.Color("#1a1b26")).Background(cOrange).Bold(true).Padding(0, 1)
	sHeader  = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sErr     = lipgloss.NewStyle().Foreground(cRed).Bold(true)
	sOK      = lipgloss.NewStyle().Foreground(cGreen)
)

// statusIcon returns an icon + color for a GitLab CI status.
func statusInfo(s string) (string, lipgloss.Color) {
	switch strings.ToLower(s) {
	case "success", "passed":
		return "✔", cGreen
	case "failed":
		return "✘", cRed
	case "running":
		return "●", cBlue
	case "pending", "waiting_for_resource", "preparing", "scheduled", "waiting_for_callback":
		return "◷", cYellow
	case "created":
		return "○", cGray
	case "canceled", "canceling":
		return "⊘", cGray
	case "skipped":
		return "»", cGray
	case "manual":
		return "▶", cAccent2
	case "":
		return "-", cDim
	}
	return "?", cGray
}

func statusText(s string) string {
	s = strings.ToLower(s)
	if s == "" {
		return "-"
	}
	i, _ := statusInfo(s)
	return i + " " + strings.ReplaceAll(s, "_", " ")
}

func statusColor(s string) lipgloss.Color { _, c := statusInfo(s); return c }

func isActive(s string) bool {
	switch strings.ToLower(s) {
	case "running", "pending", "created", "preparing", "waiting_for_resource", "scheduled", "canceling", "waiting_for_callback":
		return true
	}
	return false
}

// mergeStatus maps detailedMergeStatus to a short label + color.
func mergeStatus(s string) (string, lipgloss.Color) {
	switch s {
	case "MERGEABLE":
		return "✔ ready", cGreen
	case "CONFLICT":
		return "✘ conflict", cRed
	case "NOT_APPROVED":
		return "needs approval", cYellow
	case "DRAFT_STATUS":
		return "draft", cGray
	case "CI_MUST_PASS":
		return "ci must pass", cRed
	case "CI_STILL_RUNNING":
		return "ci running", cBlue
	case "DISCUSSIONS_NOT_RESOLVED":
		return "open threads", cYellow
	case "NEED_REBASE":
		return "needs rebase", cOrange
	case "CHECKING", "UNCHECKED", "PREPARING":
		return "checking…", cGray
	case "NOT_OPEN":
		return "not open", cGray
	case "BLOCKED_STATUS":
		return "blocked", cRed
	case "SECURITY_POLICIES_VIOLATIONS":
		return "policy", cRed
	case "REQUESTED_CHANGES":
		return "changes req.", cOrange
	case "MERGE_REQUEST_BLOCKED":
		return "blocked", cRed
	case "":
		return "-", cDim
	}
	return strings.ToLower(strings.ReplaceAll(s, "_", " ")), cGray
}

func stateColor(s string) lipgloss.Color {
	switch strings.ToLower(s) {
	case "opened":
		return cGreen
	case "merged":
		return cAccent
	case "closed":
		return cRed
	}
	return cGray
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy", int(d.Hours()/24/365))
}

func dur(sec *int) string {
	if sec == nil {
		return "-"
	}
	d := time.Duration(*sec) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%ds", *sec)
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), *sec%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
