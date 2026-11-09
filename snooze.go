package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Snooze ("z") puts an MR aside until a given time: it leaves the "on you" and "to ping"
// counts and the Rex Activity panel, then flw reminds you when the time comes.

type snoozeEntry struct {
	Until time.Time `json:"until"`
	Title string    `json:"title"`
}

var (
	snoozeMu sync.Mutex
	snoozes  map[string]snoozeEntry // path!iid -> entry
)

// statePath is $XDG_STATE_HOME/flw/snoozes.json (~/.local/state/flw): unlike the cache,
// it must survive a cleanup.
func statePath() string {
	d := os.Getenv("XDG_STATE_HOME")
	if d == "" {
		h, _ := os.UserHomeDir()
		d = filepath.Join(h, ".local", "state")
	}
	return filepath.Join(d, "flw", "snoozes.json")
}

func loadSnoozes() {
	snoozeMu.Lock()
	defer snoozeMu.Unlock()
	snoozes = map[string]snoozeEntry{}
	if b, err := os.ReadFile(statePath()); err == nil {
		json.Unmarshal(b, &snoozes)
	}
}

func saveSnoozesLocked() error {
	p := statePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(snoozes, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func mrKey(m *MR) string { return m.Path() + "!" + m.IID }

func snoozedUntil(m *MR) time.Time {
	snoozeMu.Lock()
	defer snoozeMu.Unlock()
	if e, ok := snoozes[mrKey(m)]; ok && e.Until.After(time.Now()) {
		return e.Until
	}
	return time.Time{}
}

func setSnooze(m *MR, until time.Time) error {
	snoozeMu.Lock()
	defer snoozeMu.Unlock()
	if snoozes == nil {
		snoozes = map[string]snoozeEntry{}
	}
	if until.IsZero() {
		delete(snoozes, mrKey(m))
	} else {
		snoozes[mrKey(m)] = snoozeEntry{Until: until, Title: m.Title}
	}
	return saveSnoozesLocked()
}

// dueSnoozes removes and returns the snoozes whose time has come.
func dueSnoozes(now time.Time) map[string]snoozeEntry {
	snoozeMu.Lock()
	defer snoozeMu.Unlock()
	due := map[string]snoozeEntry{}
	for k, e := range snoozes {
		if !e.Until.After(now) {
			due[k] = e
			delete(snoozes, k)
		}
	}
	if len(due) > 0 {
		saveSnoozesLocked()
	}
	return due
}

// ---------- "when" parsing ----------

// wakeHour is when "tomorrow", "monday", "2d"… wake up. Config: snooze_at (default 09:00).
func wakeHour() (int, int) {
	if t, err := time.Parse("15:04", cfg.SnoozeAt); err == nil {
		return t.Hour(), t.Minute()
	}
	return 9, 0
}

var weekdays = map[string]time.Weekday{
	"monday": time.Monday, "mon": time.Monday, "lundi": time.Monday, "lun": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday, "mardi": time.Tuesday, "mar": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday, "mercredi": time.Wednesday, "mer": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday, "jeudi": time.Thursday, "jeu": time.Thursday,
	"friday": time.Friday, "fri": time.Friday, "vendredi": time.Friday, "ven": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday, "samedi": time.Saturday, "sam": time.Saturday,
	"sunday": time.Sunday, "sun": time.Sunday, "dimanche": time.Sunday, "dim": time.Sunday,
}

var (
	clockRe = regexp.MustCompile(`^(\d{1,2})[:h](\d{2})?$`)
	relRe   = regexp.MustCompile(`^(\d+)\s*(m|min|h|d|j|w|s)$`)
)

func addBusinessDays(t time.Time, n int) time.Time {
	for n > 0 {
		t = t.AddDate(0, 0, 1)
		if wd := t.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n--
		}
	}
	return t
}

func at(day time.Time, h, m int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, day.Location())
}

// parseWhen understands: tomorrow/demain, monday…/lundi…, next week/semaine prochaine,
// 2h, 30m, 3d (business days), 14:00 or 14h, 2026-11-12, 12/11, and "<day> 14:00".
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return time.Time{}, fmt.Errorf("empty")
	}
	h, mi := wakeHour()
	f := strings.Fields(s)
	// a trailing clock time sets the hour of the day given before it
	if len(f) > 1 {
		if c := clockRe.FindStringSubmatch(f[len(f)-1]); c != nil {
			h, _ = strconv.Atoi(c[1])
			mi = 0
			if c[2] != "" {
				mi, _ = strconv.Atoi(c[2])
			}
			s = strings.Join(f[:len(f)-1], " ")
		}
	}
	switch s {
	case "tomorrow", "demain", "tmr", "tmrw":
		return at(addBusinessDays(now, 1), h, mi), nil
	case "next week", "semaine prochaine", "nw":
		d := now.AddDate(0, 0, 1)
		for d.Weekday() != time.Monday {
			d = d.AddDate(0, 0, 1)
		}
		return at(d, h, mi), nil
	case "today", "aujourd'hui", "ce soir", "tonight":
		if s == "ce soir" || s == "tonight" {
			h, mi = 17, 0
		}
		if t := at(now, h, mi); t.After(now) {
			return t, nil
		}
		return time.Time{}, fmt.Errorf("that time has passed today")
	}
	if wd, ok := weekdays[s]; ok {
		d := now.AddDate(0, 0, 1)
		for d.Weekday() != wd {
			d = d.AddDate(0, 0, 1)
		}
		return at(d, h, mi), nil
	}
	if c := relRe.FindStringSubmatch(strings.ReplaceAll(s, " ", "")); c != nil {
		n, _ := strconv.Atoi(c[1])
		switch c[2] {
		case "m", "min":
			return now.Add(time.Duration(n) * time.Minute), nil
		case "h":
			return now.Add(time.Duration(n) * time.Hour), nil
		case "d", "j":
			return at(addBusinessDays(now, n), h, mi), nil
		case "w", "s":
			return at(addBusinessDays(now, 5*n), h, mi), nil
		}
	}
	if c := clockRe.FindStringSubmatch(s); c != nil {
		hh, _ := strconv.Atoi(c[1])
		mm := 0
		if c[2] != "" {
			mm, _ = strconv.Atoi(c[2])
		}
		t := at(now, hh, mm)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	for _, layout := range []string{"2006-01-02", "02/01/2006", "02/01", "2/1"} {
		if d, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			if d.Year() == 0 {
				d = d.AddDate(now.Year(), 0, 0)
				if at(d, h, mi).Before(now) {
					d = d.AddDate(1, 0, 0)
				}
			}
			t := at(d, h, mi)
			if !t.After(now) {
				return time.Time{}, fmt.Errorf("%s is in the past", s)
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("don't understand %q (try: demain, lundi, 2h, 14:00, 3d, 2026-11-12)", s)
}

// whenText: "14:30", "tomorrow 09:00", "Mon 09:00", "12 Nov 09:00".
func whenText(t, now time.Time) string {
	y1, m1, d1 := now.Date()
	y2, m2, d2 := t.Date()
	days := int(time.Date(y2, m2, d2, 0, 0, 0, 0, time.UTC).Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, time.UTC)).Hours() / 24)
	switch {
	case days == 0:
		return t.Format("15:04")
	case days == 1:
		return "tomorrow " + t.Format("15:04")
	case days < 7:
		return t.Format("Mon 15:04")
	}
	return t.Format("2 Jan 15:04")
}
