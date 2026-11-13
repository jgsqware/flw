package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Self-update from GitHub releases: flw checks the latest release in the background,
// shows "⬆ vX" when one is newer, and ":update" (or "flw update") downloads the archive
// for this OS/arch, checks it against checksums.txt, swaps the binary and restarts.

const releaseRepo = "jgsqware/flw"

var releaseAPI = func() string {
	if u := os.Getenv("FLW_RELEASE_API"); u != "" { // tests / mirrors
		return u
	}
	return "https://api.github.com/repos/" + releaseRepo + "/releases/latest"
}()

type ghRelease struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r ghRelease) asset(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// currentVersion is the -ldflags version, else the module version of a go install, else "dev".
func currentVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

// parseVer reads "v1.2.3" / "1.2.3"; ok is false for dev builds and pseudo-versions.
func parseVer(s string) (v [3]int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" || strings.ContainsAny(s, "-+") {
		return v, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// newer reports whether release tag is newer than the running version. A dev build is
// never told to update on its own (but ":update" still works).
func newer(tag, cur string) bool {
	t, ok1 := parseVer(tag)
	c, ok2 := parseVer(cur)
	if !ok1 || !ok2 {
		return false
	}
	for i := range t {
		if t[i] != c[i] {
			return t[i] > c[i]
		}
	}
	return false
}

func latestRelease() (ghRelease, error) {
	var r ghRelease
	req, _ := http.NewRequest("GET", releaseAPI, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "flw")
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return r, errors.New("no release published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return r, fmt.Errorf("GitHub: %s", resp.Status)
	}
	return r, json.NewDecoder(resp.Body).Decode(&r)
}

func archiveName() string { return fmt.Sprintf("flw_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH) }

func download(url string, max int64) ([]byte, error) {
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", filepath.Base(url), resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

// expectedSum finds the archive's sha256 in a goreleaser checksums.txt.
func expectedSum(sums []byte, name string) string {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == name {
			return f[0]
		}
	}
	return ""
}

// binaryFromTarGz returns the "flw" file of the archive.
func binaryFromTarGz(b []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("no flw binary in the archive")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "flw" {
			return io.ReadAll(io.LimitReader(tr, 200<<20))
		}
	}
}

// selfUpdate installs release r over the running executable and returns its path.
func selfUpdate(r ghRelease) (string, error) {
	name := archiveName()
	aURL, sURL := r.asset(name), r.asset("checksums.txt")
	if aURL == "" {
		return "", fmt.Errorf("release %s has no %s", r.Tag, name)
	}
	if sURL == "" {
		return "", fmt.Errorf("release %s has no checksums.txt", r.Tag)
	}
	sums, err := download(sURL, 1<<20)
	if err != nil {
		return "", err
	}
	want := expectedSum(sums, name)
	if want == "" {
		return "", fmt.Errorf("%s is not in checksums.txt", name)
	}
	arc, err := download(aURL, 200<<20)
	if err != nil {
		return "", err
	}
	got := sha256.Sum256(arc)
	if hex.EncodeToString(got[:]) != want {
		return "", errors.New("checksum mismatch, update aborted")
	}
	bin, err := binaryFromTarGz(arc)
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	// write next to the binary, then rename: atomic, and the running process keeps its inode
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".flw-update-*")
	if err != nil {
		return "", fmt.Errorf("cannot write to %s (%v): reinstall with sudo or move flw to a folder you own", filepath.Dir(exe), err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return "", err
	}
	return exe, nil
}

// restartSelf replaces the process with the (new) binary, same arguments.
func restartSelf(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}

// ---------- in the app ----------

type updateCheckMsg struct {
	rel ghRelease
	err error
}

type updateDoneMsg struct {
	tag string
	exe string
	err error
}

const updateEvery = 6 * time.Hour

func updateCheckOn() bool { return on(cfg.Update.Check) && os.Getenv("FLW_NO_UPDATE_CHECK") == "" }

func checkUpdateCmd(delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(time.Time) tea.Msg {
		r, err := latestRelease()
		return updateCheckMsg{rel: r, err: err}
	})
}

// startUpdate asks, then installs the latest release in the background.
func (a *App) startUpdate() tea.Cmd {
	cur := currentVersion()
	if a.upd.updating {
		return a.flashOK("already updating…")
	}
	a.busy++
	a.flash, a.flashBad, a.flashAt = "checking the latest release…", false, time.Now()
	return func() tea.Msg {
		r, err := latestRelease()
		if err != nil {
			return actionDoneMsg{text: "update check", err: err}
		}
		return updateCheckMsg{rel: r, err: errAskUpdate{cur}}
	}
}

// errAskUpdate marks a check started by ":update": answer it with a confirmation.
type errAskUpdate struct{ cur string }

func (errAskUpdate) Error() string { return "" }

func (a *App) onUpdateCheck(msg updateCheckMsg) tea.Cmd {
	var ask errAskUpdate
	asked := errors.As(msg.err, &ask)
	if asked {
		if a.busy > 0 {
			a.busy--
		}
	} else {
		next := checkUpdateCmd(updateEvery)
		if msg.err == nil && newer(msg.rel.Tag, currentVersion()) {
			if a.upd.latest != msg.rel.Tag {
				a.flashOK("⬆ flw " + msg.rel.Tag + " is out: :update to install it")
			}
			a.upd.latest, a.upd.rel = msg.rel.Tag, msg.rel
		}
		return next
	}
	r, cur := msg.rel, ask.cur
	if _, ok := parseVer(cur); ok && !newer(r.Tag, cur) {
		return a.flashOK("✔ flw " + cur + " is the latest")
	}
	q := fmt.Sprintf("Update flw %s → %s and restart?", cur, r.Tag)
	return a.confirm(q, func() tea.Cmd {
		a.upd.updating = true
		a.busy++
		a.flash, a.flashBad, a.flashAt = "downloading flw "+r.Tag+"…", false, time.Now()
		return func() tea.Msg {
			exe, err := selfUpdate(r)
			return updateDoneMsg{tag: r.Tag, exe: exe, err: err}
		}
	})
}

func (a *App) onUpdateDone(msg updateDoneMsg) tea.Cmd {
	a.upd.updating = false
	if a.busy > 0 {
		a.busy--
	}
	if msg.err != nil {
		return a.flashErr("✘ update: " + msg.err.Error())
	}
	a.upd.restart = msg.exe
	return tea.Quit
}

// updateBadge is the dock / header hint when a newer release exists.
func (a *App) updateBadge() string {
	if a.upd.latest == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(cYellow).Bold(true).Render("⬆ " + a.upd.latest)
}

// ---------- flw update (CLI) ----------

func updateMain(args []string) {
	cur := currentVersion()
	r, err := latestRelease()
	if err != nil {
		fmt.Fprintln(os.Stderr, "flw update:", err)
		os.Exit(1)
	}
	force := len(args) > 0 && (args[0] == "-f" || args[0] == "--force")
	if _, ok := parseVer(cur); ok && !newer(r.Tag, cur) && !force {
		fmt.Println("flw", cur, "is the latest")
		return
	}
	fmt.Printf("updating flw %s → %s…\n", cur, r.Tag)
	exe, err := selfUpdate(r)
	if err != nil {
		fmt.Fprintln(os.Stderr, "flw update:", err)
		os.Exit(1)
	}
	fmt.Println("✔ installed", exe)
}
