package queue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// A forecast is planned as the forecast pass plans it: its slot-hours,
	// not the class's plan, waiting until its start, and no suggested
	// deadline.
	fc := job(ns, "sh050-forecast", "render", "cpu-"+ns, nil, now.Add(48*time.Hour))
	start := metav1.NewTime(now.Add(40 * time.Hour))
	fc.Spec.Forecast = &c4v1.WorkForecast{SlotHours: "8", UnitHours: "1", Start: &start}
	if dr, err := q.DryRun(ctx, fc, "namespace/"+ns); err != nil || dr.Units != 1 || dr.Suggestion != nil ||
		dr.Estimate.W < 7*3600 || dr.Estimate.W > 9*3600 || dr.Band.P50.Before(start.Time) {
		t.Fatalf("a forecast's dry run %+v %v", dr, err)
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

// With no history of its own and no estimateSeconds, a job starts from
// what its class's units took in its project, not from the class default:
// after a render job of six-second frames, a new one is estimated at six
// seconds a frame, where the default says ten minutes.
func TestDryRunFromClassHistory(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	tick(t, q)
	draft := job(ns, "sh500", "render", "cpu-"+ns, map[string]string{"frames": "1-8"}, now.Add(4*time.Hour))
	before, err := q.DryRun(ctx, draft, "namespace/"+ns)
	if err != nil {
		t.Fatal(err)
	}
	if before.Estimate.W < 8*500 {
		t.Fatalf("with no history: W %v, want the class default (about 8 x 600)", before.Estimate.W)
	}
	if err := k8s.Create(ctx, job(ns, "sh499", "render", "cpu-"+ns, map[string]string{"frames": "1-4"}, now.Add(4*time.Hour))); err != nil {
		t.Fatal(err)
	}
	runAll(t, q, now, ns, "sh499", 6*time.Second, func(i int) string { return "" })
	after, err := q.DryRun(ctx, draft, "namespace/"+ns)
	if err != nil {
		t.Fatal(err)
	}
	if after.Estimate.W < 8*4 || after.Estimate.W > 8*10 {
		t.Fatalf("after six-second frames: W %v, want about 8 x 6", after.Estimate.W)
	}
	if !after.Band.At(0.5).Before(before.Band.At(0.5)) {
		t.Fatalf("the band did not come in: %v then %v", before.Band.At(0.5), after.Band.At(0.5))
	}
}

// Drafts planned together are planned as a chain: the one that follows
// another starts after it, so the chain is ready when its last job is,
// later than that job planned alone.
func TestDryRunAll(t *testing.T) {
	ns, q, _ := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	tick(t, q)
	first := job(ns, "sm0140-proxies-v001", "render", "cpu-"+ns, map[string]string{"frames": "1001-1016", "estimateSeconds": "600"}, time.Time{})
	second := job(ns, "sm0140-movie-v001", "render", "cpu-"+ns, map[string]string{"frames": "1001-1016", "estimateSeconds": "600"}, time.Time{})
	second.Spec.DependsOn = []string{first.Name}
	alone, err := q.DryRun(ctx, second, "namespace/"+ns)
	if err != nil {
		t.Fatal(err)
	}
	both, err := q.DryRunAll(ctx, []*c4v1.WorkJob{first, second}, "namespace/"+ns, nil)
	if err != nil || len(both) != 2 {
		t.Fatalf("%v %d", err, len(both))
	}
	if !both[1].Band.P50.After(both[0].Band.P50) || !both[1].Band.P50.After(alone.Band.P50) {
		t.Fatalf("chain: first %s, second %s, second alone %s", both[0].Band.P50, both[1].Band.P50, alone.Band.P50)
	}
	if both[0].Reused != 0 || both[1].Reused != 0 {
		t.Fatalf("reused with no book: %d %d", both[0].Reused, both[1].Reused)
	}
}

// A render with no history plans on the class default (ten minutes a
// frame): it is not shown late on that guess, and its message says the
// first frames set the estimate, in the farm's time zone.
func TestDryRunWithNoHistory(t *testing.T) {
	ns, q, now := farm(t, "cpu", 3, 2)
	q.Loc, _ = time.LoadLocation("America/Los_Angeles")
	ctx := context.Background()
	tick(t, q)
	draft := job(ns, "", "render", "cpu-"+ns, map[string]string{"frames": "1001-1048"}, now.Add(30*time.Minute))
	dr, err := q.DryRun(ctx, draft, "namespace/"+ns)
	if err != nil {
		t.Fatal(err)
	}
	if !dr.Estimate.NoHistory || !dr.Feasible || dr.AtRisk {
		t.Fatalf("no history: NoHistory %v feasible %v at risk %v", dr.Estimate.NoHistory, dr.Feasible, dr.AtRisk)
	}
	want := ns + "/dry-run-0 is due Fri 02:30 PDT (09:30 UTC). It has no history yet: the first frames set the estimate."
	if dr.Message != want {
		t.Fatalf("message\n got %q\nwant %q", dr.Message, want)
	}
	// With a guess of its own the same job is late, said in local time.
	stated := draft.DeepCopy()
	stated.Spec.Params["estimateSeconds"] = "600"
	if dr, err := q.DryRun(ctx, stated, "namespace/"+ns); err != nil || dr.Estimate.NoHistory || !dr.AtRisk ||
		!strings.Contains(dr.Message, "will miss Fri 02:30 PDT (09:30 UTC) by about") {
		t.Fatalf("estimateSeconds: %+v %v", dr, err)
	}
}

// Finish scores far below zero (jobs that took seconds against minutes
// predicted, as the farm's own test does) calibrate a band no sooner than
// now plus the critical path: a 48-frame job never reads as ready now.
func TestCalibratedBandIsNeverInstant(t *testing.T) {
	ns, q, now := farm(t, "cpu", 3, 2)
	ctx := context.Background()
	tick(t, q)
	for range 100 {
		q.fcal.add("namespace/"+ns, "render", false, forecast.FinishKey{Class: "render", Stage: 0}, -5.5)
	}
	for _, est := range []string{"", "600"} {
		draft := job(ns, "", "render", "cpu-"+ns, map[string]string{"frames": "1001-1048"}, now.Add(2*time.Hour))
		if est != "" {
			draft.Spec.Params["estimateSeconds"] = est
		}
		dr, err := q.DryRun(ctx, draft, "namespace/"+ns)
		if err != nil {
			t.Fatal(err)
		}
		floor := now.Add(time.Duration(dr.Estimate.TPath * float64(time.Second)))
		if dr.Estimate.TPath < 300 || dr.Band.P50.Before(floor) || dr.Band.P90.Before(floor) {
			t.Fatalf("estimateSeconds %q: band %s to %s, critical path %.0fs from %s", est, dr.Band.P50, dr.Band.P90, dr.Estimate.TPath, *now)
		}
	}
}

// A job that starts on the class default leaves no calibration score from
// the moments before its first unit finished: a guess's error says
// nothing of the forecast's.
func TestGuessLeavesNoCalibration(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	// Due in 15 minutes: on the default, ten minutes a frame, it would read late.
	j := job(ns, "sh044", "render", "cpu-"+ns, map[string]string{"frames": "1001-1008"}, now.Add(15*time.Minute))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if st := get(t, ns, "sh044").Status.Estimate; st == nil || !st.NoHistory || st.Risk != c4v1.RiskNone || st.SlackSeconds != 0 {
		t.Fatalf("status estimate on the class default: %+v", st)
	}
	runAll(t, q, now, ns, "sh044", 6*time.Second, func(i int) string { return "" })
	own := q.fcal.own[[2]string{"namespace/" + ns, "render"}]
	if own == nil || own.Count(forecast.FinishKey{Class: "render", Stage: forecast.AnyStage}) == 0 {
		t.Fatal("no finish scores after the job succeeded")
	}
	if n := own.Count(forecast.FinishKey{Class: "render", Stage: 0}); n != 0 {
		t.Fatalf("%d finish scores from before the first frame finished", n)
	}
	if st := get(t, ns, "sh044").Status.Estimate; st != nil && st.NoHistory {
		t.Fatal("still NoHistory after its frames ran")
	}
}

// The farm's tests of its tools (c4 farm up publishes each, and every
// frames tool lowers to the render class) are no history of production
// work: after two of them, frames of two seconds, a 48-frame render still
// plans on no history, and is never ready now.
func TestDryRunAfterTheFarmsToolTests(t *testing.T) {
	ns, q, now := farm(t, "cpu", 3, 2)
	ctx := context.Background()
	for _, name := range []string{"render-farmtest-1a2b3c4d", "comp-farmtest-5e6f7a8b"} {
		j := job(ns, name, "render", "cpu-"+ns, map[string]string{"frames": "1001-1002"}, time.Time{})
		j.Labels = map[string]string{c4v1.LabelToolTest: strings.Split(name, "-")[0]}
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		runAll(t, q, now, ns, name, 2*time.Second, func(i int) string { return "" })
	}
	if _, ok := q.classPrior("namespace/"+ns, "render"); ok {
		t.Fatal("the farm's tool tests became the render class's history")
	}
	draft := job(ns, "", "render", "cpu-"+ns, map[string]string{"frames": "1001-1048"}, now.Add(2*time.Hour))
	dr, err := q.DryRun(ctx, draft, "namespace/"+ns)
	if err != nil {
		t.Fatal(err)
	}
	if !dr.Estimate.NoHistory || dr.Band.P50.Before(now.Add(10*time.Minute)) {
		t.Fatalf("after the tool tests: NoHistory %v, ready %s at %s", dr.Estimate.NoHistory, dr.Band.P50, *now)
	}
}
