package queue

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/bundle"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/ring"
)

// The queue tells its recorder every decision, and what the farm's ring
// keeps of a real queue's ticks replays to the queue's own decisions,
// through a written and read-back bundle.
func TestQueueDecisionsReplayFromABundle(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	key := func(context.Context) (telemetry.Key, error) {
		return telemetry.NewKey(bytes.Repeat([]byte{9}, telemetry.KeySize), 1)
	}
	r := ring.New(ring.TierBundles, telemetry.PolicyObjective, key)
	q.Recorder = r
	j := job(ns, "sh010", "render", "cpu-"+ns, map[string]string{"frames": "1001-1008", "estimateSeconds": "60"}, now.Add(time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "sh010")
	*now = now.Add(70 * time.Second)
	finish(t, &pods[0], "Succeeded", *now, 60*time.Second, nil)
	tick(t, q)
	tick(t, q)
	snaps, _, err := r.Snapshots(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) < 2 {
		t.Fatalf("the ring kept %d snapshots of a farm whose decisions changed", len(snaps))
	}
	var buf bytes.Buffer
	if _, err := bundle.Write(&buf, bundle.Bundle{Snapshots: snaps}); err != nil {
		t.Fatal(err)
	}
	c, err := bundle.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	dispatched := 0
	for i, s := range c.Snapshots {
		dispatched += len(s.Decisions.Dispatch)
		if !s.Faithful || telemetry.ReplayHash(s, s.Policy.Func()) != s.DecisionHash {
			t.Errorf("snapshot %d of the queue does not replay: %+v", i, s.Divergences)
		}
	}
	if dispatched == 0 {
		t.Fatal("the bundle holds no dispatch; the test checks nothing")
	}
}
