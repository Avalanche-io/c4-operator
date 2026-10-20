package queue

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A forecast job is estimated and placed among the real work, never run:
// no pods even when it cannot make its deadline, phase Forecast, and the
// real job's ETA is what it was without it. A Milestone counts it apart,
// so its risk is still the real work's.
func TestForecastNeverRuns(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	ctx := context.Background()
	pool := "cpu-" + ns
	real := job(ns, "sh040-light-v001", "render", pool, map[string]string{"frames": "1-4", "estimateSeconds": "600"}, now.Add(6*time.Hour))
	real.Labels = map[string]string{c4v1.LabelShot: "sh040"}
	if err := k8s.Create(ctx, real); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	tick(t, q)
	before := get(t, ns, "sh040-light-v001").Status.Estimate
	if before == nil || before.ETA50 == nil {
		t.Fatalf("the real job's estimate: %+v", before)
	}

	// Forty slot-hours due in four: it cannot make it on two slots.
	fc := job(ns, "sh050-light-forecast", "render", pool, map[string]string{"frames": "1-1"}, now.Add(4*time.Hour))
	fc.Labels = map[string]string{c4v1.LabelShot: "sh040"}
	fc.Spec.Forecast = &c4v1.WorkForecast{SlotHours: "40", UnitHours: "1"}
	// By frames, from the class's own plan: a hundred of them.
	byFrames := job(ns, "sh060-light-forecast", "render", pool, map[string]string{"frames": "1-100", "estimateSeconds": "600"}, now.Add(48*time.Hour))
	byFrames.Spec.Forecast = &c4v1.WorkForecast{}
	for _, j := range []*c4v1.WorkJob{fc, byFrames} {
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	m := &c4v1.Milestone{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "sh040-final"},
		Spec: c4v1.MilestoneSpec{Due: metav1.NewTime(now.Add(48 * time.Hour)),
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{c4v1.LabelShot: "sh040"}}}}
	if err := k8s.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		tick(t, q)
	}
	for _, name := range []string{"sh050-light-forecast", "sh060-light-forecast"} {
		if n := len(unitPods(t, ns, name)); n != 0 {
			t.Fatalf("%s: %d pods", name, n)
		}
		got := get(t, ns, name).Status
		if got.Phase != "Forecast" || got.Estimate == nil {
			t.Fatalf("%s: phase %q, estimate %+v", name, got.Phase, got.Estimate)
		}
	}
	if e := get(t, ns, "sh050-light-forecast").Status.Estimate; e.Risk == "" || e.Risk == "none" {
		t.Fatalf("forty slot-hours on two slots in four hours should be at risk: %+v", e)
	}
	if u := get(t, ns, "sh060-light-forecast").Status.Units.Total; u != 100 {
		t.Fatalf("the class's plan: %d units", u)
	}
	after := get(t, ns, "sh040-light-v001").Status.Estimate
	if after == nil || after.ETA50 == nil || !after.ETA50.Equal(before.ETA50) {
		t.Fatalf("a forecast moved the real job's ETA: %v → %v", before.ETA50, after.ETA50)
	}
	if fq := q.Forecast(); fq.Jobs != 2 || fq.At.IsZero() {
		t.Fatalf("the farm's forecast: %+v", fq)
	}
	var ms c4v1.Milestone
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sh040-final"}, &ms); err != nil {
		t.Fatal(err)
	}
	if ms.Status.Forecast != 1 || ms.Status.ForecastRisk == "" || ms.Status.ForecastRisk == "none" || ms.Status.Count != 2 {
		t.Fatalf("milestone: %+v", ms.Status)
	}
	if ms.Status.Risk == ms.Status.ForecastRisk && ms.Status.Risk != "none" {
		t.Fatalf("the milestone's risk took the forecast's: %+v", ms.Status)
	}
}
