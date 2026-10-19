package queue

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// The cadence is learned from successive safe points, per safe point
// passed, and follows a change of rhythm.
func TestSafepointCadence(t *testing.T) {
	t0 := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	c := &SafepointCache{}
	k := [2]string{"show", "sh010-0"}
	c.Observe(t0, map[[2]string]Safepoint{k: {Seq: 1, At: t0}})
	if sp, ok := c.Get("show", "sh010-0"); !ok || sp.Every != 0 || !sp.Next().IsZero() {
		t.Fatalf("one safe point: %+v", sp)
	}
	// Three more in six minutes: every two minutes.
	c.Observe(t0.Add(6*time.Minute), map[[2]string]Safepoint{k: {Seq: 4, At: t0.Add(6 * time.Minute)}})
	sp, _ := c.Get("show", "sh010-0")
	if sp.Every != 2*time.Minute || !sp.Next().Equal(t0.Add(8*time.Minute)) {
		t.Fatalf("cadence %v next %v", sp.Every, sp.Next())
	}
	// The same reading again changes nothing; a slower one moves it.
	c.Observe(t0.Add(7*time.Minute), map[[2]string]Safepoint{k: {Seq: 4, At: t0.Add(6 * time.Minute)}})
	c.Observe(t0.Add(16*time.Minute), map[[2]string]Safepoint{k: {Seq: 5, At: t0.Add(16 * time.Minute)}})
	sp, _ = c.Get("show", "sh010-0")
	if sp.Every <= 2*time.Minute || sp.Every >= 10*time.Minute {
		t.Fatalf("cadence after a slower one: %v", sp.Every)
	}
	if got := c.Cadence("show", []string{"sh010-0", "other"}); got != sp.Every {
		t.Fatalf("job cadence %v", got)
	}
	// A unit not seen for long is forgotten.
	c.Observe(t0.Add(40*time.Minute), nil)
	if _, ok := c.Get("show", "sh010-0"); ok {
		t.Fatal("a gone unit is still held")
	}
}

// A unit of a class that does not checkpoint, but that announces safe
// points, is stopped softly: asked to stop at its next safe point, with a
// deadline, instead of being killed; and its last safe point bounds what
// the policy counts as lost.
func TestSafepointUnitStopsSoftly(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	ctx := context.Background()
	q.Safepoints = &SafepointCache{}
	j := job(ns, "sh010", "render", "cpu-"+ns, map[string]string{"frames": "1-4", "estimateSeconds": "600"}, now.Add(time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "sh010")
	if len(pods) != 2 {
		t.Fatalf("%d units running", len(pods))
	}
	// The first announces a safe point.
	sp := now.Add(-time.Minute)
	q.Safepoints.Observe(*now, map[[2]string]Safepoint{{ns, pods[0].Name}: {Seq: 3, At: sp}})
	tick(t, q)
	var found bool
	for _, ru := range q.lastSnap.Jobs[0].RunningUnits {
		if ru.LastCheckpoint.Equal(sp) {
			found = true
		}
	}
	if !found || !q.lastSnap.Jobs[0].Checkpoints {
		t.Fatalf("the policy did not see the safe point: %+v", q.lastSnap.Jobs[0].RunningUnits)
	}
	// Hold-stop the job: the unit with safe points is asked to stop.
	var cur c4v1.WorkJob
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(j), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Nudge.HoldStop = true
	if err := k8s.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	var p0 = pods[0]
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(&p0), &p0); err != nil {
		t.Fatalf("the unit with safe points was deleted, not asked to stop: %v", err)
	}
	by, err := time.Parse(time.RFC3339, p0.Annotations[c4v1.AnnotationYieldBy])
	if p0.Annotations[c4v1.AnnotationYield] == "" || err != nil || !by.Equal(now.Add(yieldGrace).Truncate(time.Second)) {
		t.Fatalf("soft stop annotations %v", p0.Annotations)
	}
	var vol bool
	for _, v := range p0.Spec.Volumes {
		vol = vol || v.Name == "c4-podinfo"
	}
	if !vol {
		t.Fatal("a render unit cannot see its soft stop: no podinfo volume")
	}
}
