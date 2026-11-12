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

// The same across a restart for a model unit: its tokens, their cost and
// its slot time are counted once (the independent reader's test).
func TestRestartDoesNotCountAModelUnitAgain(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "bonm", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "impl", "reasoning": {"does": "model"}}, {"name": "b"}]}`)
	finishUnit(t, now, ns, "bonm", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	p := podOfUnit(t, ns, "bonm", 1)
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
	usage := putDoc(t, st, `{"models": [{"model": "claude-opus-5-5", "inputTokens": 120000, "outputTokens": 8000, "costUSD": 1.92}]}`)
	finishUnit(t, now, ns, "bonm", 1, map[string]string{c4v1.AnnotationOutputUsage: usage})
	tick(t, q)
	got := get(t, ns, "bonm")
	if got.Status.Spend == nil || got.Status.Spend.ModelTokens == 0 {
		t.Fatalf("no tokens: %+v", got.Status.Spend)
	}
	before := *got.Status.Spend
	u1 := got.Status.ModelUsage["1"]
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "bonm")
	a := got.Status.Spend
	if a.ModelTokens != before.ModelTokens || a.TokenCost != before.TokenCost || a.ModelSlotCost != before.ModelSlotCost || a.ModelSlotHours != before.ModelSlotHours {
		t.Errorf("restart double-counted the model unit: before %+v after %+v", before, *a)
	}
	if u := got.Status.ModelUsage["1"]; u.InputTokens != u1.InputTokens || u.Cost != u1.Cost {
		t.Errorf("unit usage changed: %+v -> %+v", u1, u)
	}
}

// A spec edit the planner refuses fails the job, and its status keeps what
// the job has spent: before, it was written without it, and a restart then
// lost the record (the independent reader's test).
func TestAPlanErrorKeepsTheSpend(t *testing.T) {
	ns, q, now, _ := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "perr", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	finishUnit(t, now, ns, "perr", 0, nil)
	tick(t, q)
	got := get(t, ns, "perr")
	if got.Status.Spend == nil {
		t.Fatal("no spend before the edit, so this test shows nothing")
	}
	before := *got.Status.Spend
	got.Spec.Class = "render"
	if err := k8s.Update(ctx, got); err != nil {
		t.Fatalf("edit: %v", err)
	}
	tick(t, q)
	got = get(t, ns, "perr")
	if got.Status.Phase != "Failed" {
		t.Fatalf("the refused edit did not fail the job: %s", got.Status.Phase)
	}
	if got.Status.Spend == nil || *got.Status.Spend != before {
		t.Errorf("a plan error changed the job's spend: %+v, was %+v", got.Status.Spend, before)
	}
}
