package queue

import (
	"context"
	"math"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/estimate"
)

func TestRemainingAfter(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	ends := map[int]time.Time{0: t0.Add(60 * time.Second), 1: t0.Add(120 * time.Second), 2: t0.Add(200 * time.Second)}
	millis := map[int]int64{0: 60000, 1: 60000, 2: 100000}
	slots := func(i int) int { return map[int]int{2: 2}[i] } // unit 2 is a gang of two
	cases := []struct {
		at   time.Duration
		want float64
	}{
		{0, 60 + 60 + 2*100},
		{90 * time.Second, 30 + 2*100}, // unit 1 half done, unit 2 not started
		{150 * time.Second, 2 * 50},    // unit 2 half done
		{300 * time.Second, 0},
	}
	for _, c := range cases {
		got, ok := remainingAfter(t0.Add(c.at), ends, millis, slots)
		if !ok || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("after %v: %v %v, want %v", c.at, got, ok, c.want)
		}
	}
	delete(ends, 1)
	if _, ok := remainingAfter(t0, ends, millis, slots); ok {
		t.Error("an unknown end time must make the moment unusable")
	}
}

func TestKeepPoints(t *testing.T) {
	m := newMem()
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	for f := 0; f < 640; f++ {
		m.notePoint(now, f, 640, estimate.Summary{W: 1})
		m.notePoint(now, f, 640, estimate.Summary{W: 2}) // once per count
	}
	if len(m.calPts) != 34+30 { // 0..33, then every 20th from 40 to 620
		t.Fatalf("kept %d moments", len(m.calPts))
	}
	for _, p := range m.calPts {
		if p.sum.W != 1 {
			t.Fatal("a second summary at the same count replaced the first")
		}
	}
	if len(m.calPts) > calPointsMax {
		t.Fatalf("over the cap: %d", len(m.calPts))
	}
}

func TestCalibrationSharing(t *testing.T) {
	var c calibrations
	st := estimate.Stratum{Sampled: 1}
	c.add("a", "render", false, st, 1)
	if c.forJob("a", "render", true).Fallback != nil {
		t.Fatal("a farm calibration exists with no project sharing")
	}
	if len(c.farm) != 0 {
		t.Fatal("a project that does not share added to the farm")
	}
	c.add("b", "render", true, st, 1)
	c.add("c", "render", true, st, 1)
	if c.forJob("b", "render", true).Fallback != nil {
		t.Fatal("farm calibration consulted with two projects")
	}
	c.add("d", "render", true, st, 1)
	if c.forJob("b", "render", true).Fallback == nil {
		t.Fatal("farm calibration not consulted with three projects")
	}
	if c.forJob("a", "render", false).Fallback != nil {
		t.Fatal("a project that does not share reads the farm calibration")
	}
	if c.forJob("b", "ci", true).Fallback != nil {
		t.Fatal("farm calibration crossed classes")
	}
}

// A job run to success leaves scores in its project's calibration, and
// a job whose units take what was declared scores near zero at its start.
func TestSucceededJobCalibrates(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	j := job(ns, "sh020", "render", "cpu-"+ns, map[string]string{"frames": "1001-1008", "estimateSeconds": "60"}, now.Add(time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20 && get(t, ns, "sh020").Status.Phase != "Succeeded"; i++ {
		tick(t, q)
		*now = now.Add(60 * time.Second)
		for _, p := range unitPods(t, ns, "sh020") {
			if p.Status.Phase == "" || p.Status.Phase == corev1.PodPending {
				finish(t, &p, corev1.PodSucceeded, *now, 60*time.Second, nil)
			}
		}
	}
	tick(t, q)
	if ph := get(t, ns, "sh020").Status.Phase; ph != "Succeeded" {
		t.Fatalf("job did not finish: %s", ph)
	}
	cal := q.cal.forJob("namespace/"+ns, "render", false)
	all := estimate.Stratum{Sampled: 0, Measured: -1, Open: -1}
	if cal.Count(all) == 0 {
		t.Fatal("no score at the job's start")
	}
	cal.Min = 1
	x, ok := cal.Quantile(all, 0.5)
	if !ok || math.Abs(x) > 1 {
		t.Fatalf("score at the start %v %v, want near 0 (units took what was declared)", x, ok)
	}
}

func TestProgressCache(t *testing.T) {
	var nilCache *ProgressCache
	if _, ok := nilCache.Get("ns", "p", time.Now()); ok {
		t.Fatal("a nil cache has readings")
	}
	c := &ProgressCache{}
	t0 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	c.Set(t0, map[[2]string]float64{{"ns", "p"}: 0.4})
	if f, ok := c.Get("ns", "p", t0.Add(time.Minute)); !ok || f != 0.4 {
		t.Fatalf("got %v %v", f, ok)
	}
	if _, ok := c.Get("ns", "q", t0); ok {
		t.Fatal("a pod with no reading has one")
	}
	if _, ok := c.Get("ns", "p", t0.Add(progressStale+time.Second)); ok {
		t.Fatal("a stale reading was used")
	}
}

// A running unit that reports progress is estimated from it.
func TestQueueUsesReportedProgress(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 1)
	q.Progress = &ProgressCache{}
	ctx := context.Background()
	j := job(ns, "ml001", "render", "cpu-"+ns, map[string]string{"frames": "1-1", "estimateSeconds": "2160"}, now.Add(24*time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "ml001")
	if len(pods) != 1 {
		t.Fatalf("%d pods", len(pods))
	}
	p := pods[0]
	p.Status.Phase = corev1.PodRunning
	p.Status.StartTime = &metav1.Time{Time: *now}
	if err := k8s.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	tick(t, q)
	without := get(t, ns, "ml001").Status.Estimate.RemainingWork
	q.Progress.Set(*now, map[[2]string]float64{{ns, p.Name}: 0.2})
	*now = now.Add(time.Second)
	tick(t, q)
	with := get(t, ns, "ml001").Status.Estimate.RemainingWork
	if with < 3*3600 || with <= 2*without {
		t.Fatalf("remaining work %ds with 20%% reported after an hour, %ds without", with, without)
	}
}

// The next version of a shot is estimated from the last one's durations.
func TestShotHistoryIsThePrior(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	v1 := job(ns, "sh100-v001", "render", "cpu-"+ns, map[string]string{"frames": "1001-1008", "estimateSeconds": "60"}, now.Add(time.Hour))
	v1.Labels = map[string]string{c4v1.LabelShot: "sh100"}
	if err := k8s.Create(ctx, v1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20 && get(t, ns, "sh100-v001").Status.Phase != "Succeeded"; i++ {
		tick(t, q)
		*now = now.Add(5 * time.Minute)
		for _, p := range unitPods(t, ns, "sh100-v001") {
			if p.Status.Phase == "" || p.Status.Phase == corev1.PodPending {
				finish(t, &p, corev1.PodSucceeded, *now, 4*time.Minute, nil) // 240 s, four times the declared 60
			}
		}
	}
	tick(t, q)
	v2 := job(ns, "sh100-v002", "render", "cpu-"+ns, map[string]string{"frames": "1001-1008", "estimateSeconds": "60"}, now.Add(time.Hour))
	v2.Labels = map[string]string{c4v1.LabelShot: "sh100"}
	other := job(ns, "sh200-v001", "render", "cpu-"+ns, map[string]string{"frames": "1001-1008", "estimateSeconds": "60"}, now.Add(time.Hour))
	other.Labels = map[string]string{c4v1.LabelShot: "sh200"}
	// The same shot's depth pass, another stage of the same class: the
	// lighting is not its previous version.
	depth := job(ns, "sh100-depth-v001", "render", "cpu-"+ns, map[string]string{"frames": "1001-1008", "estimateSeconds": "60"}, now.Add(time.Hour))
	depth.Labels = map[string]string{c4v1.LabelShot: "sh100", c4v1.LabelStage: "depth", c4v1.LabelJobType: "infer"}
	for _, j := range []*c4v1.WorkJob{v2, other, depth} {
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, q)
	w2 := get(t, ns, "sh100-v002").Status.Estimate.RemainingWork
	wo := get(t, ns, "sh200-v001").Status.Estimate.RemainingWork
	if w2 < 7*240 || wo > 8*60+1 {
		t.Fatalf("remaining work: next version %ds (want about %d from the last one), another shot %ds (want %d from its declaration)", w2, 8*240, wo, 8*60)
	}
	if wd := get(t, ns, "sh100-depth-v001").Status.Estimate.RemainingWork; wd > 8*60+1 {
		t.Fatalf("the shot's depth pass took the lighting's history: %ds, want %d from its declaration", wd, 8*60)
	}
}
