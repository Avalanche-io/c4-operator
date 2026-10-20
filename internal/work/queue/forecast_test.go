package queue

import (
	"context"
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/sched"
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
	if fq := q.Forecast(); len(fq.Jobs) != 2 || fq.At.IsZero() {
		t.Fatalf("the farm's forecast: %+v", fq)
	}
	var ms c4v1.Milestone
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sh040-final"}, &ms); err != nil {
		t.Fatal(err)
	}
	if ms.Status.Forecast != 1 || ms.Status.ForecastRisk == "" || ms.Status.ForecastRisk == "none" || ms.Status.Count != 1 || len(ms.Status.Jobs) != 1 {
		t.Fatalf("milestone: %+v", ms.Status)
	}
	if ms.Status.Risk == ms.Status.ForecastRisk && ms.Status.Risk != "none" {
		t.Fatalf("the milestone's risk took the forecast's: %+v", ms.Status)
	}
}

// The key invariant: adding forecast jobs changes nothing real. On a farm
// with a burst pool and a project quota, a real job's estimate (its ETA,
// required and attainable slots, risk), its pods and the farm's capacity
// requests are what they were without the forecast; the forecast's own
// pass wants burst capacity, and says so only in the farm's forecast.
func TestForecastChangesNothingReal(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	cpu := "cpu-" + ns
	spot := &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "spot-" + ns}, Spec: c4v1.PoolSpec{
		NodeSelector: map[string]string{"pool": ns + "-spot"}, SlotsPerNode: 4, CostPerSlotHour: "0.40",
		Burst: &c4v1.PoolBurst{MaxNodes: 1, ProvisioningSeconds: 300, CostPerSlotHour: "1.00", Provider: "kind-sim"}}}
	pr := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-" + ns}, Spec: c4v1.ProjectSpec{Namespaces: []string{ns},
		Store: c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: "file:///tmp/none"}}},
		Work: &c4v1.ProjectWork{BurstBudgetPerDay: "500", LateCostPerHour: "300",
			Quotas: []c4v1.Quota{{Pool: cpu, Nominal: 2, BorrowingLimit: ptr.To[int32](2)}}}}}
	for _, o := range []client.Object{spot, pr} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { k8s.Delete(ctx, spot); k8s.Delete(ctx, pr) })
	real := job(ns, "sh010-light-v001", "render", cpu, map[string]string{"frames": "1-8", "estimateSeconds": "1800"}, now.Add(3*time.Hour))
	real.Spec.Pools = append(real.Spec.Pools, spot.Name)
	if err := k8s.Create(ctx, real); err != nil {
		t.Fatal(err)
	}
	type farmState struct {
		est  *c4v1.WorkEstimate
		pods []string
		crs  []c4v1.CapacityRequestSpec
		dec  []sched.CapacityRequest
	}
	look := func() farmState {
		var s farmState
		s.est = get(t, ns, real.Name).Status.Estimate
		for _, p := range unitPods(t, ns, real.Name) {
			s.pods = append(s.pods, p.Name)
		}
		var crs c4v1.CapacityRequestList
		if err := k8s.List(ctx, &crs); err != nil {
			t.Fatal(err)
		}
		for _, c := range crs.Items {
			if c.Spec.Pool == spot.Name {
				s.crs = append(s.crs, c.Spec)
			}
		}
		s.dec = q.lastDec.Capacity
		return s
	}
	tick(t, q)
	tick(t, q)
	before := look()
	if before.est == nil || before.est.ETA50 == nil || len(before.pods) == 0 {
		t.Fatalf("the real job before: %+v", before)
	}

	// Forty slot-hours due in three, beside it on the same pools: more
	// than the farm can do on what it has.
	fc := job(ns, "sh020-light-forecast", "render", cpu, nil, now.Add(3*time.Hour))
	fc.Spec.Pools = append(fc.Spec.Pools, spot.Name)
	fc.Spec.Forecast = &c4v1.WorkForecast{SlotHours: "40", UnitHours: "1"}
	if err := k8s.Create(ctx, fc); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		tick(t, q)
	}
	after := look()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a forecast changed the real farm:\nbefore %+v\n       %+v\nafter  %+v\n       %+v", before, *before.est, after, *after.est)
	}
	for _, j := range q.lastSnap.Jobs {
		if j.ID == ns+"/"+fc.Name {
			t.Fatal("the forecast is in the live snapshot")
		}
	}
	if _, ok := q.lastDec.Jobs[ns+"/"+fc.Name]; ok {
		t.Fatal("the forecast is in the live decision")
	}
	if n := len(unitPods(t, ns, fc.Name)); n != 0 {
		t.Fatalf("the forecast has %d pods", n)
	}
	st := get(t, ns, fc.Name).Status
	if c := meta.FindStatusCondition(st.Conditions, "Forecast"); st.Phase != "Forecast" || c == nil || c.Status != metav1.ConditionTrue || st.Estimate == nil || st.Estimate.ETA50 == nil {
		t.Fatalf("the forecast's status: phase %q condition %+v estimate %+v", st.Phase, c, st.Estimate)
	}
	f := q.Forecast()
	if len(f.Jobs) != 1 || f.Jobs[0] != ns+"/"+fc.Name {
		t.Fatalf("the farm's forecast jobs: %v", f.Jobs)
	}
	if len(f.Bursts) != 1 || f.Bursts[0].Pool != spot.Name || f.Bursts[0].Slots <= f.Bursts[0].Live {
		t.Fatalf("the bursts the forecast would add: %+v (live %+v)", f.Bursts, q.lastDec.Capacity)
	}
	if len(f.Worse) != 0 {
		t.Fatalf("the farm serves the real job first: %+v", f.Worse)
	}
}

// A forecast's start moves its finish; without a deadline it is not
// planned, and says why; exists weights a class-planned forecast; its
// units are blocked on every tick, whether the pass ran or not; and a new
// forecast is planned on the next tick, not after the pass's interval.
func TestForecastStartDeadlineExists(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	cpu := "cpu-" + ns
	mk := func(name string, due time.Duration, f c4v1.WorkForecast, params map[string]string) {
		var d time.Time
		if due > 0 {
			d = now.Add(due)
		}
		j := job(ns, name, "render", cpu, params, d)
		j.Spec.Forecast = &f
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	// four slot-hours on four slots, due in six hours: an hour's work
	mk("now-forecast", 6*time.Hour, c4v1.WorkForecast{SlotHours: "4", UnitHours: "1"}, nil)
	start := metav1.NewTime(now.Add(5*time.Hour + 30*time.Minute))
	mk("late-forecast", 6*time.Hour, c4v1.WorkForecast{SlotHours: "4", UnitHours: "1", Start: &start}, nil)
	mk("undated-forecast", 0, c4v1.WorkForecast{SlotHours: "4"}, nil)
	mk("half-forecast", 24*time.Hour, c4v1.WorkForecast{Exists: "0.5"}, map[string]string{"frames": "1-10", "estimateSeconds": "600"})
	typo := job(ns, "typo-forecast", "render", cpu, nil, now.Add(time.Hour))
	typo.Spec.Forecast = &c4v1.WorkForecast{SlotHours: "4O"}
	if err := k8s.Create(ctx, typo); err == nil {
		t.Fatal("slotHours 4O was taken: it would have sized the forecast from the class plan")
	}
	tick(t, q)
	tick(t, q)
	if e := get(t, ns, "now-forecast").Status.Estimate; e == nil || e.Risk != c4v1.RiskNone {
		t.Fatalf("an hour's work due in six: %+v", e)
	}
	e := get(t, ns, "late-forecast").Status.Estimate
	if e == nil || e.Risk == c4v1.RiskNone || e.ETA50 == nil || e.ETA50.Time.Before(start.Time) {
		t.Fatalf("starting at 5h30 with an hour's work due at 6h: %+v", e)
	}
	st := get(t, ns, "undated-forecast").Status
	if c := meta.FindStatusCondition(st.Conditions, "Forecast"); st.Phase != "Forecast" || c == nil || c.Status != metav1.ConditionFalse || st.Estimate != nil {
		t.Fatalf("a forecast with no deadline: phase %q condition %+v estimate %+v", st.Phase, c, st.Estimate)
	}
	if e := get(t, ns, "half-forecast").Status.Estimate; e == nil || e.RemainingWork < 2900 || e.RemainingWork > 3100 {
		t.Fatalf("ten 600 s frames at a half chance: %+v", e)
	}
	for i := 0; i < 5; i++ {
		*now = now.Add(2 * time.Second)
		tick(t, q)
		us := get(t, ns, "half-forecast").Status.UnitStates
		if us["blocked"] == "" || us["queued"] != "" {
			t.Fatalf("tick %d: a forecast's units %v", i, us)
		}
	}
	mk("next-forecast", 6*time.Hour, c4v1.WorkForecast{SlotHours: "2"}, nil)
	*now = now.Add(2 * time.Second)
	tick(t, q)
	tick(t, q)
	if e := get(t, ns, "next-forecast").Status.Estimate; e == nil || e.ETA50 == nil {
		t.Fatalf("a forecast added after the pass: %+v", e)
	}
}

// A forecast that would take a real job's capacity is listed with the
// real job's risk with it and without it; the real job's own status
// keeps the risk the farm plans now.
func TestForecastWorsensARealJob(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	cpu := "cpu-" + ns
	real := job(ns, "real", "render", cpu, map[string]string{"frames": "1-16", "estimateSeconds": "1800"}, now.Add(210*time.Minute))
	real.Labels = map[string]string{c4v1.LabelShot: "sh070"}
	if err := k8s.Create(ctx, real); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	urgent := job(ns, "urgent-forecast", "render", cpu, nil, now.Add(time.Hour))
	urgent.Spec.Forecast = &c4v1.WorkForecast{SlotHours: "2", SD: "0.2", UnitHours: "0.25"}
	urgent.Labels = map[string]string{c4v1.LabelShot: "sh070"}
	if err := k8s.Create(ctx, urgent); err != nil {
		t.Fatal(err)
	}
	ms := &c4v1.Milestone{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "sh070-final"},
		Spec: c4v1.MilestoneSpec{Due: metav1.NewTime(now.Add(48 * time.Hour)),
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{c4v1.LabelShot: "sh070"}}}}
	if err := k8s.Create(ctx, ms); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	tick(t, q)
	tick(t, q)
	// sixteen half-hour frames on four slots, due in 3h30: on track
	if r := get(t, ns, "real").Status.Estimate; r == nil || r.Risk != c4v1.RiskNone {
		t.Fatalf("the real job: %+v", r)
	}
	// two slot-hours due within the hour come first, and the real job is
	// late with them
	w := q.Forecast().Worse
	if len(w) != 1 || w[0].Job != ns+"/real" || w[0].Live != c4v1.RiskNone || riskRank[w[0].Risk] <= riskRank[w[0].Live] {
		t.Fatalf("worse: %+v", w)
	}
	// The Milestone: risk is the real work's now; forecastRisk is the
	// real job's with the forecast work, though the forecast itself is on
	// track.
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: ms.Name}, ms); err != nil {
		t.Fatal(err)
	}
	if e := get(t, ns, urgent.Name).Status.Estimate; e == nil || e.Risk != c4v1.RiskNone {
		t.Fatalf("the urgent forecast: %+v", e)
	}
	if st := ms.Status; st.Risk != c4v1.RiskNone || st.Forecast != 1 || st.ForecastRisk != w[0].Risk || st.Count != 1 {
		t.Fatalf("milestone: %+v", st)
	}
}
