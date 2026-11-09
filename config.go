package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is ~/.config/flw/config.yaml. Every field is optional; flags and env win.
type Config struct {
	Reviewers struct {
		Default  []string            `yaml:"default"`
		Projects map[string][]string `yaml:"projects"` // project or group path -> reviewers
	} `yaml:"reviewers"`
	Ping      string       `yaml:"ping"`   // e.g. 24h
	Reping    string       `yaml:"reping"` // e.g. 72h
	CodeRoots []string     `yaml:"code_roots"`
	SnoozeAt  string       `yaml:"snooze_at"` // when "z demain" wakes up, default 09:00 // folders holding your clones
	Notify    NotifyConfig `yaml:"notify"`
}

// NotifyConfig: what flw tells you while it is open.
type NotifyConfig struct {
	Enabled  *bool    `yaml:"enabled"`  // default true
	Desktop  *bool    `yaml:"desktop"`  // popup through the terminal (OSC 777), default true
	Activity *bool    `yaml:"activity"` // Rex Activity panel (OSC 7501), default true
	Events   []string `yaml:"events"`   // default: all
	Every    string   `yaml:"every"`    // poll interval, default 1m
}

func on(b *bool) bool                   { return b == nil || *b }
func (n NotifyConfig) enabled() bool    { return on(n.Enabled) }
func (n NotifyConfig) desktop() bool    { return n.enabled() && on(n.Desktop) }
func (n NotifyConfig) activityOn() bool { return n.enabled() && on(n.Activity) }

var cfg Config

const configTemplate = `# flw configuration. Every field is optional.

reviewers:
  # suggested by "v" and added by "V" on an MR (GitLab usernames; the MR author is skipped)
  default: []
  # per project or group, overrides default (the longest matching path wins)
  # projects:
  #   my-group/my-project: [alice, bob]
  #   my-group: [carol]

# business time before an MR waiting on someone shows "ping", then "re-ping"
# ping: 24h
# reping: 72h

# notifications while flw is open: a desktop popup (OSC 777) and the Rex Activity panel (OSC 7501)
# notify:
#   enabled: true
#   desktop: true
#   activity: true
#   every: 1m
#   events: [comment, pipeline, approval, turn, ping, conflict, merged]

# when a snoozed MR ("z demain", "z lundi"…) comes back
# snooze_at: "09:00"

# folders holding your git clones (used by "w"); default: ~/src, ~/code, ~/git, … and ~
# code_roots: [~/src]
`

func configPath() string {
	if p := os.Getenv("FLW_CONFIG"); p != "" {
		return p
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(d, "flw", "config.yaml")
}

// loadConfig reads the config file; a missing file is fine.
func loadConfig() error {
	b, err := os.ReadFile(configPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("%s: %w", configPath(), err)
	}
	for _, x := range []struct {
		s string
		d *time.Duration
	}{{cfg.Ping, &pingAfter}, {cfg.Reping, &repingAfter}} {
		if x.s == "" {
			continue
		}
		d, err := time.ParseDuration(x.s)
		if err != nil {
			return fmt.Errorf("%s: %w", configPath(), err)
		}
		*x.d = d
	}
	return nil
}

// configMain handles `flw config [path|edit]`: shows the file, creating it from a template.
func configMain(args []string) {
	p := configPath()
	if _, err := os.Stat(p); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			die(err)
		}
		if err := os.WriteFile(p, []byte(configTemplate), 0o644); err != nil {
			die(err)
		}
		fmt.Fprintln(os.Stderr, "created", p)
	}
	if len(args) > 0 && args[0] == "path" {
		fmt.Println(p)
		return
	}
	if len(args) > 0 && args[0] == "edit" {
		ed := os.Getenv("VISUAL")
		if ed == "" {
			ed = os.Getenv("EDITOR")
		}
		if ed == "" {
			ed = "vi"
		}
		c := execShell(ed + " " + shellQuote(p))
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			die(err)
		}
		return
	}
	b, _ := os.ReadFile(p)
	fmt.Printf("# %s\n%s", p, b)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// defaultReviewers for a project: the longest matching project/group entry, else default.
func defaultReviewers(project string) []string {
	best, list := -1, cfg.Reviewers.Default
	for k, v := range cfg.Reviewers.Projects {
		k = strings.Trim(k, "/")
		if (project == k || strings.HasPrefix(project, k+"/")) && len(k) > best {
			best, list = len(k), v
		}
	}
	return list
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]*$`)

// parseUsernames splits "@a, b c" into clean, unique usernames.
func parseUsernames(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		f = strings.TrimPrefix(f, "@")
		if f == "" || seen[f] {
			continue
		}
		if !usernameRe.MatchString(f) {
			return nil, fmt.Errorf("not a username: %q", f)
		}
		seen[f] = true
		out = append(out, f)
	}
	return out, nil
}

// reviewersToAdd: wanted minus the author and the current reviewers.
func reviewersToAdd(m *MR, wanted []string) []string {
	cur := map[string]bool{}
	for _, r := range m.Reviewers.Nodes {
		cur[r.Username] = true
	}
	var out []string
	for _, u := range wanted {
		u = strings.TrimPrefix(u, "@")
		if u == "" || cur[u] || (m.Author != nil && m.Author.Username == u) {
			continue
		}
		cur[u] = true
		out = append(out, u)
	}
	return out
}

func execShell(cmd string) *exec.Cmd { return exec.Command("sh", "-c", cmd) }

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	return p
}
