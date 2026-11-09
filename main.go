package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

var version = "dev"

func main() {
	if err := loadConfig(); err != nil {
		fmt.Fprintln(os.Stderr, "flw:", err)
		os.Exit(1)
	}
	loadSnoozes()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "config":
			configMain(os.Args[2:])
			return
		case "daemon":
			daemonMain(os.Args[2:])
			return
		case "service":
			runService(os.Args[2:])
			return
		}
	}
	repo := flag.String("R", "", "project path (group/name); default: the git repo in the current directory")
	host := flag.String("host", "", "GitLab hostname (default: glab's configured host)")
	flag.DurationVar(&pingAfter, "ping", pingAfter, "business time an MR may wait on someone before it shows 'ping'")
	flag.DurationVar(&repingAfter, "reping", repingAfter, "business time before 're-ping' (also: one day after an unanswered ping)")
	ui := flag.String("ui", "", "layout: desktop (dock, tiled windows, palette) or classic (default from config, else desktop)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "flw: a keyboard-driven GitLab TUI on top of glab\n\nUsage: flw [-R group/project] [-host gitlab.example.com]\n       flw daemon [-every 2m]       keep the cache in ~/.cache/flw warm\n       flw service install|uninstall|status|logs   run the daemon as a systemd user service\n       flw config [edit|path]       show or edit ~/.config/flw/config.yaml (default reviewers…)\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("flw", version)
		return
	}
	switch {
	case *ui != "":
		desktopUI = *ui != "classic"
	case cfg.UI != "":
		desktopUI = cfg.UI != "classic"
	}
	if _, err := lookGlab(); err != nil {
		fmt.Fprintln(os.Stderr, "flw needs glab in PATH: https://gitlab.com/gitlab-org/cli")
		os.Exit(1)
	}
	app := newApp(&Client{Host: *host}, *repo)
	p := tea.NewProgram(app, tea.WithAltScreen())
	_, err := p.Run()
	if cfg.Notify.activityOn() {
		for tty, seq := range app.act.clearAll() {
			writeTTY(tty, seq)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
