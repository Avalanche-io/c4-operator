package queue

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// The manager restarts while a finished unit's pod is still listed (held
// by another's finalizer, as a pod is for a moment on a real node, or kept
// while the queue finishes with it). The new queue sees the pod again. The
// job's spend stays as it was: the pod is counted once.
func TestRestartDoesNotCountAFinishedPodAgain(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "bon", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a1"}, {"name": "b"}]}`)
	finishUnit(t, now, ns, "bon", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)

	p := podOfUnit(t, ns, "bon", 1)
	p.Finalizers = append(p.Finalizers, "test.c4.avalanche.io/hold")
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	defer func() {
		var held corev1.Pod
		if k8s.Get(ctx, client.ObjectKeyFromObject(p), &held) == nil {
			held.Finalizers = nil
			k8s.Update(ctx, &held)
		}
	}()
	finishUnit(t, now, ns, "bon", 1, map[string]string{c4v1.AnnotationOutputChangeset: writeChangeset(t, st, "a1.txt", "unit one's file")})
	tick(t, q)
	got := get(t, ns, "bon")
	if got.Status.Spend == nil || got.Status.Spend.ProceduralSlotHours == "" {
		t.Fatalf("no spend after the unit: %+v", got.Status.Spend)
	}
	before := *got.Status.Spend
	var all corev1.PodList
	if err := k8s.List(ctx, &all, client.InNamespace(ns), client.MatchingLabels{c4v1.LabelJob: "bon"}); err != nil {
		t.Fatal(err)
	}
	still := false
	for _, pod := range all.Items {
		still = still || (unitOf(pod) == 1 && pod.Status.Phase == corev1.PodSucceeded)
	}
	if !still {
		t.Fatal("unit 1's finished pod is gone, so this test shows nothing")
	}

	// The restart: a new queue with no memory, seeing that pod again.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "bon")
	if got.Status.Spend == nil {
		t.Fatal("no spend after the restart")
	}
	if a, b := got.Status.Spend.ProceduralSlotHours, before.ProceduralSlotHours; a != b {
		t.Errorf("the restart counted the finished pod again: slot hours %s, were %s", a, b)
	}
	if a, b := got.Status.Spend.ProceduralSlotCost, before.ProceduralSlotCost; a != b {
		t.Errorf("the restart counted the finished pod again: slot cost %s, was %s", a, b)
	}
	if !contains(got.Status.CountedPods, string(p.UID)) {
		t.Fatalf("the held pod is not in CountedPods: %v", got.Status.CountedPods)
	}

	// Once the pod is gone the status stops naming it.
	var held corev1.Pod
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), &held); err != nil {
		t.Fatal(err)
	}
	held.Finalizers = nil
	if err := k8s.Update(ctx, &held); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		tick(t, q)
		got = get(t, ns, "bon")
		if !contains(got.Status.CountedPods, string(p.UID)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a pod that is gone is still in CountedPods: %v", got.Status.CountedPods)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
