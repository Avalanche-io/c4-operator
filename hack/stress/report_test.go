package main

import (
	"fmt"
	"strings"
	"testing"
)

// The trailing-EDF table names the guard only where its units were taken
// back for a real share of the work lost, and long units by their
// length and spread alone: a guard that started units nobody took back
// is not a cause, and neither is a heavy region of short units.
func TestTrailingCausesAreWhatTrails(t *testing.T) {
	trail := func(name string, pt Point, guardLost, lost float64, guardPreempt int) *pointResult {
		pt.Name, pt.Nodes = name, 16
		return &pointResult{Pt: pt, Runs: map[string]*runResult{
			mainPolicy: {DueJobs: 10, OnTime: 5, Util: 0.8, LostSlotHours: lost, GuardLostSlotHours: guardLost, GuardPreemptions: guardPreempt},
			"edf":      {DueJobs: 10, OnTime: 9, Util: 0.8}}}
	}
	r := &report{Results: []*pointResult{
		// the guard started units, none was taken back; short units in a heavy region
		trail("idle-guard", Point{UnitMin: 5, Spread: 0.2, HeavyMult: 30}, 0, 4, 3),
		// half the work lost was the guard's units taken back
		trail("guard-lost", Point{UnitMin: 5, Spread: 0.2}, 2, 4, 3),
		// long, heavy-tailed units
		trail("long", Point{UnitMin: 60, Spread: 0.5}, 0, 0, 0),
	}}
	var b strings.Builder
	r.writeTrailing(func(f string, a ...any) { fmt.Fprintf(&b, f, a...) })
	rows := map[string]string{}
	for _, l := range strings.Split(b.String(), "\n") {
		for _, n := range []string{"idle-guard", "guard-lost", "long"} {
			if strings.HasPrefix(l, "| "+n+" |") {
				rows[n] = l
			}
		}
	}
	if got := rows["idle-guard"]; !strings.Contains(got, "not classified") || strings.Contains(got, "guard") && !strings.Contains(got, "idle-guard") {
		t.Errorf("a guard that lost nothing and a heavy region of short units: %q", got)
	}
	if strings.Contains(rows["idle-guard"], "starvation guard") || strings.Contains(rows["idle-guard"], "taken back") {
		t.Errorf("an idle guard named as a cause: %q", rows["idle-guard"])
	}
	if got := rows["guard-lost"]; !strings.Contains(got, "guard units taken back (50% of the work lost)") {
		t.Errorf("the guard's losses: %q", got)
	}
	if got := rows["long"]; !strings.Contains(got, "long, heavy-tailed units") || strings.Contains(got, "guard") {
		t.Errorf("long units: %q", got)
	}
}
