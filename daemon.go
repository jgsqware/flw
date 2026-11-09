package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runDaemon keeps the disk cache warm so the TUI opens on fresh data.
// Each round: identity, my MRs, the opened MRs of their projects, the project list,
// and the detail of every MR that changed since the previous round.
func runDaemon(gl *Client, every time.Duration, extra []string) {
	log.SetFlags(0)
	if cacheDir == "" {
		log.Fatal("cache disabled (FLW_NO_CACHE is set)")
	}
	log.Printf("flw daemon: warming %s every %s", cacheDir, every)
	seen := map[string]time.Time{} // MR -> updatedAt already cached
	round := 0
	for {
		start := time.Now()
		n, err := warm(gl, seen, extra, round)
		if err != nil {
			log.Printf("warm failed: %v", err)
		} else {
			log.Printf("warmed %d queries in %s", n, time.Since(start).Round(100*time.Millisecond))
		}
		if round%30 == 0 {
			cachePrune(7 * 24 * time.Hour)
		}
		round++
		time.Sleep(every)
	}
}

func warm(gl *Client, seen map[string]time.Time, extra []string, round int) (int, error) {
	if err := gl.Bootstrap(); err != nil {
		return 0, err
	}
	n := 1
	mrs, err := gl.MyMRs()
	if err != nil {
		return n, err
	}
	n += 3
	if _, err := gl.MyDoneMRs(30); err == nil {
		n += 6
	}
	projects := map[string]bool{}
	for _, p := range extra {
		projects[p] = true
	}
	for _, m := range mrs {
		projects[m.Path()] = true
		k := m.Path() + "!" + m.IID
		if !m.UpdatedAt.After(seen[k]) {
			continue
		}
		if _, err := gl.MRDetail(m.Path(), m.IID); err != nil {
			log.Printf("detail %s: %v", k, err)
			continue
		}
		n++
		seen[k] = m.UpdatedAt
	}
	for p := range projects {
		if _, err := gl.ProjectMRs(p, mrStates[0]); err != nil {
			log.Printf("project %s: %v", p, err)
			continue
		}
		n++
	}
	// the project list changes slowly: every 10th round is enough
	if round%10 == 0 {
		if _, err := gl.Projects(); err == nil {
			n++
		}
	}
	return n, nil
}

const unitName = "flw-cache.service"

func unitPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(d, "systemd", "user", unitName)
}

// runService installs or removes the systemd user service that runs `flw daemon`.
func runService(args []string) {
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	hostFlag := fs.String("host", "", "GitLab hostname (default: glab's configured host)")
	every := fs.Duration("every", 2*time.Minute, "refresh interval")
	var extra multiFlag
	fs.Var(&extra, "R", "also warm this project (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: flw service install [-every 2m] [-R group/project]... | uninstall | status | logs\n")
		fs.PrintDefaults()
	}
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	cmd := args[0]
	fs.Parse(args[1:])
	switch cmd {
	case "install":
		exe, err := os.Executable()
		if err == nil {
			exe, err = filepath.EvalSymlinks(exe)
		}
		if err != nil {
			die(err)
		}
		line := fmt.Sprintf("%s daemon -every %s", exe, every)
		if *hostFlag != "" {
			line += " -host " + *hostFlag
		}
		for _, p := range extra {
			line += " -R " + p
		}
		unit := fmt.Sprintf(`[Unit]
Description=flw: keep the GitLab cache warm for the flw TUI
After=network-online.target

[Service]
ExecStart=%s
Environment=PATH=%s
Restart=on-failure
RestartSec=60
Nice=10

[Install]
WantedBy=default.target
`, line, os.Getenv("PATH"))
		p := unitPath()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			die(err)
		}
		if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
			die(err)
		}
		fmt.Println("wrote", p)
		systemctl("daemon-reload")
		systemctl("enable", "--now", unitName)
		systemctl("restart", unitName) // pick up a new binary or new flags
		fmt.Println("✓ running. Logs: flw service logs")
	case "uninstall":
		systemctl("disable", "--now", unitName)
		os.Remove(unitPath())
		systemctl("daemon-reload")
		fmt.Println("✓ removed")
	case "status":
		systemctl("status", "--no-pager", unitName)
	case "logs":
		c := exec.Command("journalctl", "--user", "-u", unitName, "-n", "50", "--no-pager")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		c.Run()
	default:
		fs.Usage()
		os.Exit(2)
	}
}

func systemctl(args ...string) {
	c := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil && args[0] != "status" {
		die(fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err))
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "flw:", err)
	os.Exit(1)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// daemonMain parses `flw daemon` flags.
func daemonMain(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	every := fs.Duration("every", 2*time.Minute, "refresh interval")
	host := fs.String("host", "", "GitLab hostname (default: glab's configured host)")
	var extra multiFlag
	fs.Var(&extra, "R", "also warm this project (repeatable)")
	fs.Parse(args)
	if _, err := lookGlab(); err != nil {
		die(fmt.Errorf("glab not found in PATH"))
	}
	runDaemon(&Client{Host: *host}, *every, extra)
}
