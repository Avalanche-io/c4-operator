// Package power turns a laptop's battery into the slots its host runner
// offers: work stops when the battery falls below one level and resumes
// only once it is back above another, so a weak or loose charger never
// runs the battery flat under a farm's agents. The runner reads the slots
// from a file (c4-agent runner --slots-file) whose first line is the
// number and, after a space, why.
package power

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Reading is the battery now.
type Reading struct {
	// Percent is the charge, 0 to 100.
	Percent int
	// Powered says power is arriving from a charger: attached and
	// delivering, not only attached (a loose plug reads as attached).
	Powered bool
}

// Policy is when work stops and resumes.
type Policy struct {
	// Slots are what the runner offers while work runs.
	Slots int
	// StopBelow stops work when the battery, not powered, falls below it.
	StopBelow int
	// ResumeAt resumes stopped work once the battery is back at it.
	ResumeAt int
}

// DefaultPolicy stops below 25% and resumes at 90%.
func DefaultPolicy(slots int) Policy { return Policy{Slots: slots, StopBelow: 25, ResumeAt: 90} }

// State is what the reader remembers between readings: whether work is
// stopped, waiting for the battery to come back.
type State struct{ Stopped bool }

// Step is the policy applied to a reading: the next state, the slots to
// offer, and why, in a few words.
func (p Policy) Step(s State, r Reading) (State, int, string) {
	on := ""
	if r.Powered {
		on = ", on power"
	}
	switch {
	case s.Stopped && r.Percent >= p.ResumeAt:
		s.Stopped = false
	case !s.Stopped && !r.Powered && r.Percent < p.StopBelow:
		s.Stopped = true
	}
	if s.Stopped {
		return s, 0, fmt.Sprintf("battery %d%%%s: stopped below %d%%, resumes at %d%%", r.Percent, on, p.StopBelow, p.ResumeAt)
	}
	return s, p.Slots, fmt.Sprintf("battery %d%%%s", r.Percent, on)
}

// Write puts "slots reason" in the slots file, replacing it whole, and
// reports whether its content changed.
func Write(path string, slots int, reason string) (bool, error) {
	line := fmt.Sprintf("%d %s\n", slots, reason)
	if b, err := os.ReadFile(path); err == nil && string(b) == line {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(line), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// ErrNoBattery is a machine with no battery to read: a desktop, or a
// platform this package cannot read yet.
var ErrNoBattery = errors.New("no battery to read on this machine")

// Read reads the battery: from the battery's registry entry on macOS
// (which tells a charger that delivers from one only attached), from
// /sys/class/power_supply on Linux.
func Read(ctx context.Context) (Reading, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.CommandContext(ctx, "ioreg", "-rn", "AppleSmartBattery").Output()
		if err != nil {
			return Reading{}, fmt.Errorf("ioreg: %w", err)
		}
		return parseIOReg(string(out))
	case "linux":
		return readSysfs("/sys/class/power_supply")
	}
	return Reading{}, ErrNoBattery
}

var (
	ioCurrent  = regexp.MustCompile(`"CurrentCapacity" = (\d+)`)
	ioMax      = regexp.MustCompile(`"MaxCapacity" = (\d+)`)
	ioExternal = regexp.MustCompile(`"ExternalConnected" = (Yes|No)`)
	ioPowerIn  = regexp.MustCompile(`"SystemPowerIn"=(\d+)`)
)

// parseIOReg reads `ioreg -rn AppleSmartBattery`. A charger counts as
// powering only when the system reports power arriving from it.
func parseIOReg(s string) (Reading, error) {
	cur, mx := ioCurrent.FindStringSubmatch(s), ioMax.FindStringSubmatch(s)
	if cur == nil || mx == nil {
		return Reading{}, ErrNoBattery
	}
	c, _ := strconv.Atoi(cur[1])
	m, _ := strconv.Atoi(mx[1])
	if m <= 0 {
		return Reading{}, ErrNoBattery
	}
	r := Reading{Percent: min(100, c*100/m)}
	if e := ioExternal.FindStringSubmatch(s); e != nil && e[1] == "Yes" {
		r.Powered = true
		if p := ioPowerIn.FindStringSubmatch(s); p != nil && p[1] == "0" {
			r.Powered = false // attached, nothing arriving
		}
	}
	return r, nil
}

// readSysfs reads the first battery and whether any mains supply is online.
func readSysfs(dir string) (Reading, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Reading{}, ErrNoBattery
	}
	r, found := Reading{}, false
	for _, e := range entries {
		read := func(f string) string {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name(), f))
			return strings.TrimSpace(string(b))
		}
		switch read("type") {
		case "Battery":
			if found {
				continue
			}
			if n, err := strconv.Atoi(read("capacity")); err == nil {
				r.Percent, found = n, true
			}
		case "Mains", "USB":
			if read("online") == "1" {
				r.Powered = true
			}
		}
	}
	if !found {
		return Reading{}, ErrNoBattery
	}
	return r, nil
}
