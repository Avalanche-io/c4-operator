package main

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/bundle"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

var updateBundle = flag.Bool("update-bundle", false, "write testdata/gpu-tight.tgz again from a simulated week")

const testBundle = "testdata/gpu-tight.tgz"

// writeTestBundle writes a bundle of an overloaded GPU farm's busiest
// moments in a simulated week: the ticks that preempted, burst or changed
// a capacity request, and a few that only dispatched.
func writeTestBundle(t *testing.T) {
	key, err := telemetry.NewKey(make32(11), 1)
	if err != nil {
		t.Fatal(err)
	}
	r := telemetry.NewRedactor(key)
	p := lightParams()
	p.DailiesPerEvening = map[string]float64{"showA": 14, "showB": 10, "showC": 8}
	w := generate(p, 2)
	h := buildHistory(p, w.targets)
	pol := policyByName("objective+burst")
	var snaps []telemetry.Snapshot
	var preempts, bursts, plain int
	observeDecide = func(_ policySpec, s sched.Snapshot, before sched.StateDoc, cfg sched.Config, d sched.Decisions) {
		keep := false
		switch {
		case len(d.Preempt) > 0 && preempts < 4:
			preempts++
			keep = true
		case (len(d.Bursts) > 0 || len(d.Capacity) > 0) && bursts < 4:
			bursts++
			keep = true
		case len(d.Dispatch) > 0 && plain < 2:
			plain++
			keep = true
		}
		if keep {
			rec, _ := r.Redact(s, sched.StateFrom(before), cfg, d)
			snaps = append(snaps, rec)
		}
	}
	defer func() { observeDecide = nil }()
	newEngine(w, h, 2, pol, gpuTightFarm()).run()
	if preempts == 0 || bursts == 0 {
		t.Fatalf("the week preempted %d times and burst %d; the bundle would not show either", preempts, bursts)
	}
	var buf bytes.Buffer
	if _, err := bundle.Write(&buf, bundle.Bundle{Snapshots: snaps}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testBundle, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A bundle written on one machine replays on another: the committed
// bundle's decisions, preemptions and bursts among them, replay to the
// same decision hashes on every architecture CI runs (amd64 and arm64).
// The forward run plays the farm on from its last snapshot.
func TestCommittedBundleReplays(t *testing.T) {
	if *updateBundle {
		writeTestBundle(t)
	}
	var out bytes.Buffer
	ok, err := replayFile(testBundle, "decide", 0, 0, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("the committed bundle does not replay here:\n%s", out.String())
	}
	f, err := os.Open(testBundle)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := bundle.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	var preempts, bursts int
	for _, s := range c.Snapshots {
		preempts += len(s.Decisions.Preempt)
		bursts += len(s.Decisions.Bursts) + len(s.Decisions.Capacity)
	}
	if preempts == 0 || bursts == 0 {
		t.Fatalf("the committed bundle holds %d preemptions and %d bursts; it should hold both", preempts, bursts)
	}
	out.Reset()
	if _, err := replayFile(testBundle, "forward", 12*time.Hour, time.Minute, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "finishes +") || !strings.Contains(out.String(), "dispatches") {
		t.Fatalf("the forward run finished no job:\n%s", out.String())
	}
	t.Logf("%s", out.String())
}
