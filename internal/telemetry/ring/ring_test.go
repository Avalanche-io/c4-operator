package ring

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

func key(context.Context) (telemetry.Key, error) {
	b := make([]byte, telemetry.KeySize)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return telemetry.NewKey(b, 1)
}

// tick is a small farm at now with runnable units of one job.
func tick(now time.Time, runnable int) (sched.Snapshot, sched.StateDoc, sched.Config, sched.Decisions) {
	j := sched.Job{ID: "saltmarsh/sh0420", Project: "saltmarsh", Ready: now.Add(-time.Hour), Deadline: now.Add(2 * time.Hour),
		Pools: []string{"bk-cpu"}, SlotsPerUnit: 1, RunnableTotal: runnable, Estimate: estimate.Summary{W: 3600, W90: 4000, TPath: 300}}
	for i := 0; i < runnable; i++ {
		j.Runnable = append(j.Runnable, sched.UnitRef{ID: j.ID + "#" + strconv.Itoa(i)})
	}
	s := sched.Snapshot{Now: now, Pools: []sched.Pool{{Name: "bk-cpu", Slots: 4}}, Projects: []sched.Project{{Name: "saltmarsh", Weight: 1}}, Jobs: []sched.Job{j}}
	st := &sched.State{}
	before := st.Doc()
	cfg := sched.DefaultConfig()
	return s, before, cfg, sched.Decide(s, st, cfg)
}

// At tier a nothing is kept in advance: a bundle holds the present moment.
// At tier b every tick that changed a decision is kept, redacted, and a
// tick that changed nothing is not.
func TestRingKeepsDecisionChangingTicks(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	off := New(TierOff, telemetry.PolicyObjective, key)
	on := New(TierBundles, telemetry.PolicyObjective, key)
	for i, n := range []int{3, 3, 2, 2, 5} {
		s, b, c, d := tick(now.Add(time.Duration(i)*time.Minute), n)
		off.Note(s, b, c, d)
		on.Note(s, b, c, d)
	}
	snaps, _, err := off.Snapshots(context.Background(), 0)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("tier a: %d snapshots, %v; want the present moment only", len(snaps), err)
	}
	snaps, dropped, err := on.Snapshots(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 || dropped {
		t.Fatalf("tier b kept %d snapshots (dropped %v); want the 3 that changed a decision", len(snaps), dropped)
	}
	for _, s := range snaps {
		if s.KeyEpoch != 1 || len(s.Jobs) != 1 {
			t.Fatalf("a kept snapshot: %+v", s)
		}
	}
}

// The ring stays in its bounds: past the window or the byte bound the
// oldest snapshots go, and the bundle says some were dropped.
func TestRingBounds(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	r := New(TierBundles, telemetry.PolicyObjective, key)
	r.SetBounds(30*time.Minute, 0)
	for i := 0; i < 10; i++ {
		s, b, c, d := tick(now.Add(time.Duration(i)*10*time.Minute), 1+i%4)
		r.Note(s, b, c, d)
	}
	snaps, dropped, err := r.Snapshots(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !dropped || len(snaps) > 4 {
		t.Fatalf("a 30-minute window kept %d snapshots of 10-minute ticks (dropped %v)", len(snaps), dropped)
	}
	small := New(TierBundles, telemetry.PolicyObjective, key)
	small.SetBounds(0, 1)
	for i := 0; i < 3; i++ {
		s, b, c, d := tick(now.Add(time.Duration(i)*time.Minute), 1+i)
		small.Note(s, b, c, d)
	}
	if snaps, dropped, _ := small.Snapshots(context.Background(), 0); !dropped || len(snaps) > 1 {
		t.Fatalf("a 1-byte bound kept %d snapshots (dropped %v)", len(snaps), dropped)
	}
}

// C4_TELEMETRY=off holds every tier at off, whatever the farm asks.
func TestKillSwitch(t *testing.T) {
	t.Setenv("C4_TELEMETRY", "off")
	if TierFrom(true) != TierOff {
		t.Fatal("the kill switch did not hold the ring off")
	}
	t.Setenv("C4_TELEMETRY", "")
	if TierFrom(true) != TierBundles || TierFrom(false) != TierOff {
		t.Fatal("the tier does not follow the setting")
	}
}

// The queue notes while the HUD writes a bundle: run under -race.
func TestRingConcurrent(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	r := New(TierBundles, telemetry.PolicyObjective, key)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			s, b, c, d := tick(now.Add(time.Duration(i)*time.Second), 1+i%5)
			r.Note(s, b, c, d)
		}
	}()
	for i := 0; i < 20; i++ {
		if _, _, err := r.Snapshots(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}
