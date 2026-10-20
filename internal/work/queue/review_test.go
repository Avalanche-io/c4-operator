package queue

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// A job with a review is done only once the review is decided: its last
// frame leaves it waiting for the reviewer, who is needed; the approval
// finishes it. Work run again is reviewed again.
func TestReviewDecidesTheJob(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	pool := "cpu-" + ns
	j := job(ns, "sh040", "render", pool, map[string]string{"frames": "1-2", "chunk": "1"}, now.Add(72*time.Hour))
	j.Spec.Review = &c4v1.WorkReviewSpec{Server: "person:dana", Ask: "approve sh040?", Choices: []string{"approve", "rework"}}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	finishAll := func() {
		t.Helper()
		for _, p := range unitPods(t, ns, "sh040") {
			if p.Status.Phase == "" || p.Status.Phase == corev1.PodPending || p.Status.Phase == corev1.PodRunning {
				finish(t, &p, corev1.PodSucceeded, *now, time.Minute, nil)
			}
		}
	}
	tick(t, q)
	finishAll()
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "sh040")
	if got.Status.Phase == "Succeeded" || got.Status.Units.Total != 3 {
		t.Fatalf("before the review: phase %s, %d units", got.Status.Phase, got.Status.Units.Total)
	}
	if len(got.Status.Waits) != 1 {
		t.Fatalf("waits %+v", got.Status.Waits)
	}
	w := got.Status.Waits[0]
	if w.Name != class.ReviewName || w.Unit != 2 || w.Server != "person:dana" || !w.Blocking || w.Since == nil {
		t.Fatalf("review %+v", w)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson); c == nil || c.Reason != c4v1.NeedsDecision {
		t.Fatalf("needs person %+v", c)
	}

	*now = now.Add(time.Hour)
	decide(t, ns, "sh040", class.ReviewName, "approve", *now)
	tick(t, q)
	tick(t, q)
	if got = get(t, ns, "sh040"); got.Status.Phase != "Succeeded" {
		t.Fatalf("after approval: %s %v", got.Status.Phase, got.Status.UnitStates)
	}

	// A frame run again reopens the review: the old approval passes
	// nothing new.
	annotate(t, got, c4v1.AnnotationRequeue, "r1:0")
	tick(t, q)
	finishAll()
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "sh040")
	if got.Status.Phase == "Succeeded" {
		t.Fatal("the job passed again on the earlier approval")
	}
	if len(got.Status.Waits) != 1 || got.Status.Waits[0].Do != "" || !got.Status.Waits[0].Blocking {
		t.Fatalf("the review after the requeue: %+v", got.Status.Waits)
	}
	*now = now.Add(time.Hour)
	decide(t, ns, "sh040", class.ReviewName, "approve", *now)
	tick(t, q)
	tick(t, q)
	if got = get(t, ns, "sh040"); got.Status.Phase != "Succeeded" {
		t.Fatalf("after the second approval: %s", got.Status.Phase)
	}
}
