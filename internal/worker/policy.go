// Package worker implements the opt-in fleet worker: it claims queued tasks
// from the coordinator and executes them headlessly on the seat-holder's own
// machine, under their own Claude Code and git credentials. Capacity moves
// to the work — credentials never move at all.
package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Policy is the seat-holder's standing consent: which repos their machine
// may work on, how many tokens per day it may burn, and when.
type Policy struct {
	Enabled            bool     `json:"enabled"`
	ReposAllowed       []string `json:"repos_allowed"`        // URL prefixes; empty = claim nothing
	MaxTokensPerDay    int64    `json:"max_tokens_per_day"`   // weighted tokens the worker may spend per local day
	Windows            []string `json:"windows"`              // e.g. "Sat 00:00-24:00", "Mon-Fri 19:00-07:00"; empty = always
	TaskTimeoutMinutes int      `json:"task_timeout_minutes"`
	AllowedTools       string   `json:"allowed_tools"`
	Workdir            string   `json:"workdir"`
	PollSeconds        int      `json:"poll_seconds"`
	LeaseSeconds       int64    `json:"lease_seconds"`
}

func DefaultPolicy() Policy {
	home, _ := os.UserHomeDir()
	return Policy{
		MaxTokensPerDay:    2_000_000,
		TaskTimeoutMinutes: 90,
		AllowedTools:       "Bash(git:*),Edit,Write,Read,Glob,Grep",
		Workdir:            filepath.Join(home, ".ses", "worker"),
		PollSeconds:        60,
		LeaseSeconds:       600,
	}
}

func PolicyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ses", "worker.json")
}

func LoadPolicy() (Policy, error) {
	p := DefaultPolicy()
	data, err := os.ReadFile(PolicyPath())
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("corrupt worker config at %s: %w", PolicyPath(), err)
	}
	if p.Workdir == "" {
		p.Workdir = DefaultPolicy().Workdir
	}
	if p.PollSeconds <= 0 {
		p.PollSeconds = 60
	}
	if p.LeaseSeconds <= 0 {
		p.LeaseSeconds = 600
	}
	if p.TaskTimeoutMinutes <= 0 {
		p.TaskTimeoutMinutes = 90
	}
	return p, nil
}

func SavePolicy(p Policy) error {
	path := PolicyPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

// StopFile is the local kill switch: `ses away off` writes it, the poll loop
// exits when it appears, and any running task is terminated.
func (p Policy) StopFile() string {
	return filepath.Join(p.Workdir, "STOP")
}

// InWindow reports whether t falls inside any configured window. Windows look
// like "Sat 00:00-24:00" or "Mon-Fri 19:00-07:00"; an end before the start
// means the window wraps past midnight. No windows means always-on.
func (p Policy) InWindow(t time.Time) bool {
	if len(p.Windows) == 0 {
		return true
	}
	for _, w := range p.Windows {
		if windowMatches(w, t) {
			return true
		}
	}
	return false
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func windowMatches(w string, t time.Time) bool {
	parts := strings.Fields(w)
	if len(parts) != 2 {
		return false
	}
	days, ok := parseDays(parts[0])
	if !ok {
		return false
	}
	start, end, ok := parseRange(parts[1])
	if !ok {
		return false
	}

	mins := t.Hour()*60 + t.Minute()
	if start <= end {
		return days[t.Weekday()] && mins >= start && mins < end
	}
	// Overnight: matches the evening of a listed day or the morning after it.
	prev := (t.Weekday() + 6) % 7
	return (days[t.Weekday()] && mins >= start) || (days[prev] && mins < end)
}

func parseDays(s string) (map[time.Weekday]bool, bool) {
	out := make(map[time.Weekday]bool)
	if from, to, ok := strings.Cut(strings.ToLower(s), "-"); ok {
		f, fok := dayNames[from]
		t, tok := dayNames[to]
		if !fok || !tok {
			return nil, false
		}
		for d := f; ; d = (d + 1) % 7 {
			out[d] = true
			if d == t {
				break
			}
		}
		return out, true
	}
	d, ok := dayNames[strings.ToLower(s)]
	if !ok {
		return nil, false
	}
	out[d] = true
	return out, true
}

func parseRange(s string) (int, int, bool) {
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, false
	}
	start, ok1 := parseHHMM(from)
	end, ok2 := parseHHMM(to)
	return start, end, ok1 && ok2
}

func parseHHMM(s string) (int, bool) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}
