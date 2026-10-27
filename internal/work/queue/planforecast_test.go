package queue

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/forecast"
)

// agentLead makes a lead's job of agent work, as a lead's workers lower:
// blocks of tool agent in agent environment env, owned by noor and
// answered by its lead; its root runs.
func agentLead(t *testing.T, ns, name, env string, q *Queue, now time.Time, labels map[string]string) *c4v1.WorkJob {
	t.Helper()
	j := job(ns, name, "graph", "cpu-"+ns, map[string]string{"estimateSeconds": "60"}, now.Add(48*time.Hour))
	j.Labels = labels
	j.Annotations = map[string]string{c4v1.AnnotationOwner: "noor", c4v1.AnnotationLeadSession: "session-1"}
	j.Spec.Answers = &c4v1.WorkAnswers{By: c4v1.AnswersByLead}
	j.Spec.Retries = ptr.To[int32](0)
	j.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "C4_AGENT_BLOCK", Value: "1"},
		{Name: "C4_BLOCK_PROFILE", Value: "agent"}, {Name: "C4_AGENT_ENV", Value: env}}
	if err := k8s.Create(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	return get(t, ns, name)
}

// unmeasured is a forecast's unit by name, or fails.
func unmeasured(t *testing.T, f PlanForecast, name string) UnitEstimate {
	t.Helper()
	for _, u := range f.Unmeasured {
		if u.Name == name {
			return u
		}
	}
	t.Fatalf("%s is not among the unmeasured: %+v", name, f.Unmeasured)
	return UnitEstimate{}
}

// runUnits plays kubelet for the job's running units but its root: each
// takes d.
func runUnits(t *testing.T, q *Queue, now *time.Time, ns, name string, d time.Duration) {
	t.Helper()
	tick(t, q)
	*now = now.Add(d + time.Second)
	for _, p := range unitPods(t, ns, name) {
		if unitOf(p) != 0 && (p.Status.Phase == "" || p.Status.Phase == corev1.PodPending) {
			finish(t, &p, corev1.PodSucceeded, *now, d, map[string]string{c4v1.AnnotationOutputTree: viewID("c4" + name)})
		}
	}
	tick(t, q)
}

// Agent work has no shot: a worker with no history of its own in its job
// is estimated from the runs of the same agent environment and tool,
// before its lead's estimate, and then from its class; the forecast says
// which, per unit. A finished worker's run time is kept under that key,
// saved with the project's calibrations; the farm's tool tests leave none.
func TestAgentWorkEstimate(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	project := "namespace/" + ns
	first := agentLead(t, ns, "first", "env-v1", q, *now, nil)

	// No history: the class default, a guess the first workers set.
	_, f, err := q.ForecastPlan(ctx, first, nil, [][]byte{[]byte(`{"version": 1, "units": [{"name": "parse", "key": "agent", "held": true}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if u := unmeasured(t, f, "parse"); u.From != FromClassDefault || !u.New || !f.Agent || !f.Estimate.NoHistory ||
		!strings.Contains(f.Message, forecast.NoWorkerHistory) {
		t.Fatalf("with no history: %+v, message %q", u, f.Message)
	}

	// Three workers of env-v1 run twenty minutes each.
	work := putDoc(t, st, `{"version": 1, "units": [{"name": "a", "key": "agent"}, {"name": "b", "key": "agent"}, {"name": "c", "key": "agent"}]}`)
	planTo(t, ns, "first", c4v1.WorkPlanRequest{Key: "k1", Document: work, Units: []string{"a", "b", "c"}})
	runUnits(t, q, now, ns, "first", 20*time.Minute)
	if got := states(t, get(t, ns, "first"), "succeeded"); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("succeeded %v", got)
	}
	p, ok := q.classPrior(project, agentKeyPrefix+"env-v1/agent")
	if !ok || math.Abs(p.Mean-1200) > 5 {
		t.Fatalf("the agent environment's history: %+v %v", p, ok)
	}
	if rec := q.calRecordOf(project, *now); len(rec.Durations[agentKeyPrefix+"env-v1/agent"]) != 3 {
		t.Fatalf("saved durations %v", rec.Durations)
	}

	// The next lead's worker in env-v1: its environment's history, not
	// its lead's guess; in another environment, its lead's estimate.
	scan := []byte(`{"version": 1, "units": [{"name": "scan", "key": "agent", "held": true, "work": 60}]}`)
	second := agentLead(t, ns, "second", "env-v1", q, *now, nil)
	_, f, err = q.ForecastPlan(ctx, second, nil, [][]byte{scan})
	if err != nil {
		t.Fatal(err)
	}
	if u := unmeasured(t, f, "scan"); u.From != FromAgentEnv || math.Abs(u.Seconds-1200) > 5 || f.Estimate.NoHistory {
		t.Fatalf("in env-v1: %+v", u)
	}
	third := agentLead(t, ns, "third", "env-v2", q, *now, nil)
	_, f, err = q.ForecastPlan(ctx, third, nil, [][]byte{scan})
	if err != nil {
		t.Fatal(err)
	}
	if u := unmeasured(t, f, "scan"); u.From != FromWork || math.Abs(u.Seconds-60) > 1 {
		t.Fatalf("in env-v2: %+v", u)
	}

	// The farm's test of a tool leaves no history (on slots the other
	// leads' roots leave).
	for _, n := range []string{"first", "second", "third"} {
		finish(t, podOfUnit(t, ns, n, 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4root")})
	}
	tick(t, q)
	agentLead(t, ns, "agent-farmtest-1a2b3c4d", "env-v3", q, *now, map[string]string{c4v1.LabelToolTest: "agent"})
	planTo(t, ns, "agent-farmtest-1a2b3c4d", c4v1.WorkPlanRequest{Key: "k1", Document: work, Units: []string{"a", "b", "c"}})
	runUnits(t, q, now, ns, "agent-farmtest-1a2b3c4d", 2*time.Second)
	if got := states(t, get(t, ns, "agent-farmtest-1a2b3c4d"), "succeeded"); len(got) != 3 {
		t.Fatalf("the tool test's units: succeeded %v", got)
	}
	if _, ok := q.classPrior(project, agentKeyPrefix+"env-v3/agent"); ok {
		t.Fatal("the farm's tool test became its agent environment's history")
	}
}

// The forecast of a lead's plan is a dry run: the job as the last tick
// left it, with the calls not applied yet and then the call's own, held
// units counted as released; it changes nothing of the job's. It names
// the critical path, how many units can run at once on the job's pools,
// and answers busy while a tick holds the queue.
func TestPlanForecastDryRun(t *testing.T) {
	ns, q, now, _ := graphFarm(t)
	ctx := context.Background()
	lead := agentLead(t, ns, "comp", "env-v1", q, *now, nil)
	units := len(q.jobs[lead.UID].plan.Units)
	add := []byte(`{"version": 1, "units": [
		{"name": "x", "key": "x", "held": true, "work": 3600},
		{"name": "y", "key": "y", "held": true, "work": 1800, "after": ["x"]},
		{"name": "z1", "key": "z", "held": true, "work": 60}, {"name": "z2", "key": "z", "held": true, "work": 60},
		{"name": "z3", "key": "z", "held": true, "work": 60}, {"name": "z4", "key": "z", "held": true, "work": 60}]}`)
	withdraw := []byte(`{"version": 1, "withdraw": [{"unit": "y", "why": "x covers it"}]}`)

	before, after, err := q.ForecastPlan(ctx, lead, nil, [][]byte{add})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Unmeasured) > 1 || before.Slots != 4 || !slices.Equal(before.Pools, []string{"cpu-" + ns}) {
		t.Fatalf("before: %+v", before)
	}
	var path []string
	for _, u := range after.Path {
		path = append(path, u.Name)
	}
	if !slices.Equal(path, []string{"x", "y"}) || !after.Path[0].New || after.Workers != 4 || after.Slots != 4 {
		t.Fatalf("after: path %v, workers %d of %d slots", path, after.Workers, after.Slots)
	}
	if !after.Band.P50.After(now.Add(85*time.Minute)) || before.Band.P50.After(now.Add(15*time.Minute)) || !after.AsOf.Equal(*now) {
		t.Fatalf("held units are not the plan's work: ready %s then %s", before.Band.P50, after.Band.P50)
	}
	if u := unmeasured(t, after, "y"); u.From != FromWork || u.Seconds != 1800 {
		t.Fatalf("y %+v", u)
	}

	// A call pending behind it: forecast with it, then with the call.
	withY, withoutY, err := q.ForecastPlan(ctx, lead, [][]byte{add}, [][]byte{withdraw})
	if err != nil {
		t.Fatal(err)
	}
	if !withY.Band.P50.Equal(after.Band.P50) || !withoutY.Band.P50.Before(withY.Band.P50.Add(-20*time.Minute)) ||
		slices.ContainsFunc(withoutY.Unmeasured, func(u UnitEstimate) bool { return u.Name == "y" }) {
		t.Fatalf("withdrawing y: ready %s then %s, unmeasured %+v", withY.Band.P50, withoutY.Band.P50, withoutY.Unmeasured)
	}
	// A call the job would refuse has no forecast.
	if _, _, err := q.ForecastPlan(ctx, lead, nil, [][]byte{withdraw}); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("withdrawing a unit not in the job: %v", err)
	}
	// Nothing of the job's changed.
	if m := q.jobs[lead.UID]; len(m.plan.Units) != units || len(m.plans) != 0 {
		t.Fatalf("the dry run changed the job: %d units, %d calls", len(m.plan.Units), len(m.plans))
	}

	// A job the queue has not planned, and a tick holding the queue.
	other := lead.DeepCopy()
	other.UID = "not-planned"
	if _, _, err := q.ForecastPlan(ctx, other, nil, nil); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("not planned: %v", err)
	}
	q.mu.Lock()
	_, _, err = q.ForecastPlan(ctx, lead, nil, nil)
	q.mu.Unlock()
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("while a tick runs: %v", err)
	}
}

// A lead's workers share their tool's key, but each carries its own
// work: with no history of their environment and tool, each is priced by
// its own, in the forecast of a plan call and in the job's estimate,
// rather than every worker by the first's. Once the environment has
// history, both are priced by it.
func TestWorkerWorkEstimate(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	project := "namespace/" + ns
	const plan = `{"version": 1, "units": [{"name": "docs", "key": "agent"%s, "work": 300},
		{"name": "refactor", "key": "agent"%s, "work": 3600}]}`
	held := []byte(fmt.Sprintf(plan, `, "held": true`, `, "held": true`))
	lead := agentLead(t, ns, "lead", "env-v1", q, *now, nil)

	// No history: each worker by its own work, the refactor the critical
	// path and the plan ready after it.
	_, f, err := q.ForecastPlan(ctx, lead, nil, [][]byte{held})
	if err != nil {
		t.Fatal(err)
	}
	if u := unmeasured(t, f, "docs"); u.From != FromWork || math.Abs(u.Seconds-300) > 1 {
		t.Fatalf("docs: %+v", u)
	}
	if u := unmeasured(t, f, "refactor"); u.From != FromWork || math.Abs(u.Seconds-3600) > 1 {
		t.Fatalf("refactor: %+v", u)
	}
	if n := len(f.Path); n == 0 || f.Path[n-1].Name != "refactor" || math.Abs(f.Estimate.TPath-3600) > 60 ||
		!f.Band.P50.After(now.Add(55*time.Minute)) {
		t.Fatalf("path %+v, critical path %.0fs, ready %s", f.Path, f.Estimate.TPath, f.Band.P50)
	}

	// The job's own estimate, once the call is applied.
	planTo(t, ns, "lead", c4v1.WorkPlanRequest{Key: "k1", Document: putDoc(t, st, fmt.Sprintf(plan, "", "")), Units: []string{"docs", "refactor"}})
	tick(t, q)
	v := q.jobs[lead.UID].last
	names := v.mem.plan.Names()
	docs, refactor := names["docs"], names["refactor"]
	if math.Abs(v.rem[docs]-300) > 1 || math.Abs(v.rem[refactor]-3600) > 1 || math.Abs(v.sum.TPath-3600) > 60 {
		t.Fatalf("the job's estimate: docs %.0fs, refactor %.0fs, critical path %.0fs", v.rem[docs], v.rem[refactor], v.sum.TPath)
	}

	// Twenty minutes a worker in env-v1: both by that history, the next
	// lead's forecast and this job's estimate alike.
	for range 3 {
		q.noteDuration(project, agentKeyPrefix+"env-v1/agent", 1200)
	}
	next := agentLead(t, ns, "next", "env-v1", q, *now, nil)
	if _, f, err = q.ForecastPlan(ctx, next, nil, [][]byte{held}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"docs", "refactor"} {
		if u := unmeasured(t, f, name); u.From != FromAgentEnv || math.Abs(u.Seconds-1200) > 5 {
			t.Fatalf("%s with history: %+v", name, u)
		}
	}
	v = q.jobs[lead.UID].last
	if math.Abs(v.rem[docs]-1200) > 5 || math.Abs(v.rem[refactor]-1200) > 5 || len(v.workKeys) != 0 {
		t.Fatalf("the job's estimate with history: docs %.0fs, refactor %.0fs, own keys %v", v.rem[docs], v.rem[refactor], v.workKeys)
	}

	// Workers of the key in other environments: each by its own
	// environment's history, else by its own work, not by env-v1's.
	for range 3 {
		q.noteDuration(project, agentKeyPrefix+"env-v2/agent", 600)
	}
	mixed := []byte(`{"version": 1, "units": [{"name": "docs", "key": "agent", "held": true, "work": 300},
		{"name": "port", "key": "agent", "held": true, "work": 3600, "env": {"C4_AGENT_ENV": "env-v2"}},
		{"name": "lint", "key": "agent", "held": true, "work": 120, "env": {"C4_AGENT_ENV": "env-v3"}}]}`)
	if _, f, err = q.ForecastPlan(ctx, next, nil, [][]byte{mixed}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]UnitEstimate{"docs": {Seconds: 1200, From: FromAgentEnv},
		"port": {Seconds: 600, From: FromAgentEnv}, "lint": {Seconds: 120, From: FromWork}} {
		if u := unmeasured(t, f, name); u.From != want.From || math.Abs(u.Seconds-want.Seconds) > 5 {
			t.Fatalf("%s: %+v, want %.0fs from %s", name, u, want.Seconds, want.From)
		}
	}
}
