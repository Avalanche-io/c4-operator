package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/forecast"
)

func TestDryRun(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	draft := job(ns, "sh040", "render", "cpu-"+ns, map[string]string{"frames": "1001-1016", "estimateSeconds": "600"}, time.Time{})
	if _, err := q.DryRun(ctx, draft, "namespace/"+ns); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("before a tick: %v, want ErrNoPlan", err)
	}
	busy := job(ns, "sh041", "render", "cpu-"+ns, map[string]string{"frames": "1-8", "estimateSeconds": "600"}, now.Add(time.Hour))
	if err := k8s.Create(ctx, busy); err != nil {
		t.Fatal(err)
	}
	tick(t, q)

	bad := draft.DeepCopy()
	bad.Spec.Params["frames"] = "nope"
	if _, err := q.DryRun(ctx, bad, "namespace/"+ns); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("bad frames: %v, want ErrInvalidJob", err)
	}

	dr, err := q.DryRun(ctx, draft, "namespace/"+ns)
	if err != nil {
		t.Fatal(err)
	}
	if dr.Units != 16 || dr.DeadlineSource != "suggested" || dr.Suggestion == nil || dr.Band.IsZero() {
		t.Fatalf("dry run %+v", dr)
	}
	if dr.Suggestion.Policy != "achievable" || dr.Suggestion.State != string(forecast.Estimating) || !dr.Deadline.Equal(dr.Suggestion.Deadline.Time) {
		t.Fatalf("suggestion %+v", dr.Suggestion)
	}
	if dr.Deadline.Before(now.Add(15*time.Minute)) || dr.Band.P80.After(dr.Deadline) {
		t.Fatalf("deadline %s, band P80 %s", dr.Deadline, dr.Band.P80)
	}
	if dr.Estimate.W <= 0 {
		t.Fatalf("estimate %+v", dr.Estimate)
	}

	pinned := draft.DeepCopy()
	pinned.Name = "sh042"
	pinned.Spec.Deadline = busy.Spec.Deadline
	if dr, err := q.DryRun(ctx, pinned, "namespace/"+ns); err != nil || dr.DeadlineSource != "spec" || dr.Suggestion != nil {
		t.Fatalf("pinned dry run %+v %v", dr, err)
	}

	// Nothing was written: one job, and its pods only.
	var jobs c4v1.WorkJobList
	if err := k8s.List(ctx, &jobs, client.InNamespace(ns)); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("jobs after dry runs: %d %v", len(jobs.Items), err)
	}
	var pods corev1.PodList
	if err := k8s.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{c4v1.LabelJob: "sh040"}); err != nil || len(pods.Items) != 0 {
		t.Fatalf("pods for a dry run: %d %v", len(pods.Items), err)
	}
}

func TestFinishCalsSharing(t *testing.T) {
	var c finishCals
	k := forecast.FinishKey{Class: "render", Stage: 1}
	for i := range 100 {
		c.add("a", "render", false, k, float64(i)/100)
	}
	if c.adjust("a", "render", false, 1) == nil {
		t.Fatal("a project with 100 scores is not calibrated")
	}
	if c.adjust("b", "render", true, 1) != nil {
		t.Fatal("a project borrowed another's that does not share")
	}
	for _, p := range []string{"b", "c", "d"} {
		for i := range 40 {
			c.add(p, "render", true, k, float64(i)/40)
		}
	}
	if c.adjust("b", "render", true, 1) == nil {
		t.Fatal("three sharing projects with 120 scores: no farm calibration")
	}
	if c.adjust("e", "render", false, 1) != nil {
		t.Fatal("a project that does not share reads the farm calibration")
	}
}

func TestNoteFinish(t *testing.T) {
	m := newMem()
	t0 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	p50 := t0.Add(5 * time.Hour)
	m.noteFinish(t0, 0, 10, p50)
	m.noteFinish(t0.Add(time.Minute), 0, 10, p50) // same count, within the hour
	m.noteFinish(t0.Add(2*time.Minute), 1, 10, p50)
	m.noteFinish(t0.Add(62*time.Minute), 1, 10, p50) // an hour on
	m.noteFinish(t0.Add(63*time.Minute), 10, 10, p50)
	m.noteFinish(t0.Add(64*time.Minute), 2, 10, time.Time{})
	if len(m.finPts) != 3 || m.finPts[1].stage != 1 {
		t.Fatalf("kept %+v", m.finPts)
	}
}

// A job run to success leaves finish scores for its project and class.
func TestSucceededJobCalibratesFinish(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	j := job(ns, "sh043", "render", "cpu-"+ns, map[string]string{"frames": "1001-1008", "estimateSeconds": "60"}, now.Add(time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20 && get(t, ns, "sh043").Status.Phase != "Succeeded"; i++ {
		tick(t, q)
		*now = now.Add(60 * time.Second)
		for _, p := range unitPods(t, ns, "sh043") {
			if p.Status.Phase == "" || p.Status.Phase == corev1.PodPending {
				finish(t, &p, corev1.PodSucceeded, *now, 60*time.Second, nil)
			}
		}
	}
	tick(t, q)
	own := q.fcal.own[[2]string{"namespace/" + ns, "render"}]
	if own == nil || own.Count(forecast.FinishKey{Class: "render", Stage: forecast.AnyStage}) == 0 {
		t.Fatal("no finish scores after the job succeeded")
	}
}
