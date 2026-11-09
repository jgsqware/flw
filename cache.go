package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The disk cache keeps the raw output of every read call to glab. Views first draw
// from it (instant), then refresh from GitLab. `flw daemon` keeps it warm.

var errNoCache = errors.New("not cached")

// cacheDir is $XDG_CACHE_HOME/flw (~/.cache/flw). Empty when caching is off.
var cacheDir = func() string {
	if os.Getenv("FLW_NO_CACHE") != "" {
		return ""
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "flw")
}()

// cacheable tells read calls apart from writes and from big, fast-changing job logs.
func cacheable(args []string) bool {
	if len(args) < 2 || args[0] != "api" {
		return false
	}
	for i, a := range args {
		switch {
		case a == "-X" && i+1 < len(args) && args[i+1] != "GET":
			return false
		case strings.HasPrefix(a, "query=") && strings.HasPrefix(strings.TrimSpace(a[6:]), "mutation"):
			return false
		}
	}
	return !strings.HasSuffix(args[1], "/trace")
}

func cacheKey(args []string) string {
	h := sha256.New()
	for _, a := range args {
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	// ":fullpath" and friends are resolved by glab from the git repo in the current directory
	if strings.Contains(args[1], ":") {
		wd, _ := os.Getwd()
		h.Write([]byte(wd))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func cachePath(args []string) string {
	return filepath.Join(cacheDir, cacheKey(args)+".json")
}

func cacheGet(args []string) ([]byte, time.Time, error) {
	if cacheDir == "" {
		return nil, time.Time{}, errNoCache
	}
	p := cachePath(args)
	st, err := os.Stat(p)
	if err != nil {
		return nil, time.Time{}, errNoCache
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, time.Time{}, errNoCache
	}
	return b, st.ModTime(), nil
}

// cachePut writes atomically: the daemon and the TUI may write the same entry.
func cachePut(args []string, b []byte) {
	if cacheDir == "" || len(b) == 0 {
		return
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return
	}
	p := cachePath(args)
	f, err := os.CreateTemp(cacheDir, ".tmp-*")
	if err != nil {
		return
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Rename(f.Name(), p) != nil {
		os.Remove(f.Name())
	}
}

// cachePrune drops entries nobody read or wrote for a week.
func cachePrune(maxAge time.Duration) {
	if cacheDir == "" {
		return
	}
	ents, _ := os.ReadDir(cacheDir)
	for _, e := range ents {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > maxAge {
			os.Remove(filepath.Join(cacheDir, e.Name()))
		}
	}
}

// Cached returns a copy of the client that only reads the disk cache (no network).
func (c *Client) Cached() *Client {
	cc := *c
	cc.offline = true
	cc.oldest = &stamp{}
	return &cc
}

// stamp tracks the oldest cache entry used (calls may run in parallel).
type stamp struct {
	mu sync.Mutex
	t  time.Time
}

func (s *stamp) see(t time.Time) {
	s.mu.Lock()
	if s.t.IsZero() || t.Before(s.t) {
		s.t = t
	}
	s.mu.Unlock()
}

// CachedAt is the age of the oldest entry a cached client served.
func (c *Client) CachedAt() time.Time {
	if c.oldest == nil {
		return time.Time{}
	}
	c.oldest.mu.Lock()
	defer c.oldest.mu.Unlock()
	return c.oldest.t
}
