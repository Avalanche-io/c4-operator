package queue

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// graphFarm is a farm whose queue reads expansion documents from a store.
func graphFarm(t *testing.T) (string, *Queue, *time.Time, *storeurl.Sharded) {
	t.Helper()
	ns, q, now := farm(t, "cpu", 1, 4)
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	q.Stores = func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
	return ns, q, now, st
}

func putDoc(t *testing.T, st *storeurl.Sharded, doc string) string {
	t.Helper()
	id, err := st.PutBytes(context.Background(), []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func viewID(s string) string { return c4.Identify(strings.NewReader(s)).String() }

func podOfUnit(t *testing.T, ns, job string, idx int) *corev1.Pod {
	t.Helper()
	for _, p := range unitPods(t, ns, job) {
		if unitOf(p) == idx && (p.Status.Phase == "" || p.Status.Phase == corev1.PodPending) {
			return &p
		}
	}
	t.Fatalf("no running pod of unit %d; pods %v", idx, podUnits(unitPods(t, ns, job)))
	return nil
}

func podUnits(ps []corev1.Pod) []int {
	var out []int
	for _, p := range ps {
		out = append(out, unitOf(p))
	}
	return out
}

func states(t *testing.T, j *c4v1.WorkJob, name string) []int {
	t.Helper()
	s, _ := order.DecodeSet(j.Status.UnitStates[name])
	return s
}

// A graph job's root generator writes an expansion document; its units
// join the job under it, run in the order their edges say, read each
// other's output, and survive a restart of the queue.
func TestGraphJobExpands(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	j := job(ns, "plan1", "graph", "cpu-"+ns, map[string]string{"estimateSeconds": "60"}, now.Add(4*time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	root := podOfUnit(t, ns, "plan1", 0)
	if got := get(t, ns, "plan1"); got.Status.Units.Total != 1 {
		t.Fatalf("units %+v", got.Status.Units)
	}

	doc := putDoc(t, st, `{"version": 1, "dependents": "all", "units": [
		{"name": "deep", "work": 120, "group": "layers"},
		{"name": "beauty", "after": ["deep"], "inputs": [{"fromUnit": "deep", "mountPath": "/deep"}], "group": "layers"},
		{"name": "lane", "planned": true, "work": 600}]}`)
	*now = now.Add(time.Minute)
	finish(t, root, corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4root"), c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	got := get(t, ns, "plan1")
	if len(got.Status.Expansions) != 1 || got.Status.Expansions[0].Units != 3 || got.Status.Expansions[0].Document != doc || got.Status.Units.Total != 4 {
		t.Fatalf("after expansion: %+v %+v", got.Status.Expansions, got.Status.Units)
	}
	// deep (1) and the planned lane (3) run; beauty (2) waits on deep.
	if b := states(t, got, "blocked"); !slices.Equal(b, []int{2}) {
		t.Fatalf("blocked %v, states %v", b, got.Status.UnitStates)
	}
	if units := podUnits(unitPods(t, ns, "plan1")); !slices.Contains(units, 1) || !slices.Contains(units, 3) || slices.Contains(units, 2) {
		t.Fatalf("running %v", units)
	}
	plan, _ := q.PlanOf(ns + "/plan1")
	if plan == nil || len(plan.Units) != 4 || plan.Units[2].Name != "beauty" || plan.Units[2].Parent != 0 || plan.Units[2].Depth != 1 {
		t.Fatalf("plan %+v", plan)
	}

	// A restart rebuilds the same table from status and the store.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	plan, _ = q.PlanOf(ns + "/plan1")
	if plan == nil || len(plan.Units) != 4 || !slices.Equal(plan.Units[2].After, []int{1}) {
		t.Fatalf("after restart %+v", plan)
	}
	if n := len(unitPods(t, ns, "plan1")); n != 2 {
		t.Fatalf("after restart %d pods", n)
	}

	// deep finishes: beauty mounts its output once the view is ready.
	*now = now.Add(2 * time.Minute)
	finish(t, podOfUnit(t, ns, "plan1", 1), corev1.PodSucceeded, *now, 2*time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4deepview")})
	tick(t, q)
	var m c4v1.Materialization
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: unitInputName("plan1", 1)}, &m); err != nil || m.Spec.Source.ID != viewID("c4deepview") {
		t.Fatalf("beauty's input %+v %v", m.Spec, err)
	}
	m.Status.Phase, m.Status.ObservedGeneration = c4v1.PhaseReady, m.Generation
	if err := k8s.Status().Update(ctx, &m); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	beauty := podOfUnit(t, ns, "plan1", 2)
	var mounted bool
	for _, vol := range beauty.Spec.Volumes {
		if c := vol.CSI; c != nil && c.VolumeAttributes[c4v1.VolumeAttrMaterialization] == unitInputName("plan1", 1) && *c.ReadOnly {
			mounted = true
		}
	}
	var name string
	for _, e := range beauty.Spec.Containers[0].Env {
		if e.Name == "C4_UNIT_NAME" {
			name = e.Value
		}
	}
	if !mounted || name != "beauty" {
		t.Fatalf("beauty's pod: mounted %v, name %q", mounted, name)
	}
	if got := get(t, ns, "plan1"); got.Status.UnitOutputs["1"] != viewID("c4deepview") {
		t.Fatalf("unit outputs %v", got.Status.UnitOutputs)
	}

	// The planned lane writes no document: it is evaluated, and adds
	// nothing. Then beauty ends the job.
	*now = now.Add(time.Minute)
	finish(t, podOfUnit(t, ns, "plan1", 3), corev1.PodSucceeded, *now, 3*time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4lane")})
	finish(t, beauty, corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4beauty")})
	tick(t, q)
	got = get(t, ns, "plan1")
	if got.Status.Phase != "Succeeded" || len(got.Status.Expansions) != 2 || got.Status.Expansions[1].Units != 0 {
		t.Fatalf("end: %s %+v", got.Status.Phase, got.Status.Expansions)
	}
}

// A document the queue cannot use fails its generator, adds nothing, and
// says why.
func TestGraphRefusesBadDocument(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	j := job(ns, "plan2", "graph", "cpu-"+ns, nil, now.Add(4*time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a", "after": ["b"]}, {"name": "b", "after": ["a"]}]}`)
	finish(t, podOfUnit(t, ns, "plan2", 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	got := get(t, ns, "plan2")
	if got.Status.Phase != "Failed" || got.Status.Units.Total != 1 || len(got.Status.Expansions) != 1 || !strings.Contains(got.Status.Expansions[0].Error, "cycle") {
		t.Fatalf("%s %+v %+v", got.Status.Phase, got.Status.Units, got.Status.Expansions)
	}
}

// A unit that waits on a failed one is stuck: the job fails once nothing
// else can run, instead of waiting forever.
func TestGraphStuckOnFailedUnit(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	j := job(ns, "plan3", "graph", "cpu-"+ns, nil, now.Add(4*time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a"}, {"name": "b", "after": ["a"]}, {"name": "c", "after": ["b"]}]}`)
	finish(t, podOfUnit(t, ns, "plan3", 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	// It fails, and its retry fails too.
	for i := 0; i < 2; i++ {
		finish(t, podOfUnit(t, ns, "plan3", 1), corev1.PodFailed, *now, time.Minute, nil)
		tick(t, q)
	}
	got := get(t, ns, "plan3")
	if got.Status.Phase != "Failed" || !slices.Equal(states(t, got, "blocked"), []int{2, 3}) || !slices.Equal(states(t, got, "failed"), []int{1}) {
		t.Fatalf("%s %v", got.Status.Phase, got.Status.UnitStates)
	}
}

// Ra's P-21 rule as holds: lanes that share a file run in sequence. Lane
// B claims a file under a directory lane A holds, so it is locked by A
// until A ends, while lane C, sharing nothing, runs beside A; a shared
// claim (readers) does not lock another shared one.
func TestHoldsSerializeSharedFiles(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "lanes", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [
		{"name": "A", "holds": ["src/shared/", "src/a.go"], "work": 600},
		{"name": "B", "holds": ["src/shared/util.go"], "work": 600},
		{"name": "C", "holds": ["src/c.go"], "work": 600},
		{"name": "R1", "claims": [{"on": "db:fixtures", "mode": "shared"}]},
		{"name": "R2", "claims": [{"on": "db:fixtures", "mode": "shared"}]}]}`)
	finish(t, podOfUnit(t, ns, "lanes", 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "lanes")
	running := podUnits(unitPods(t, ns, "lanes"))
	sort.Ints(running)
	// A (1), C (3), R1 (4) and R2 (5) run; B (2) waits for A.
	if !slices.Equal(running, []int{1, 3, 4, 5}) || !slices.Equal(states(t, got, "locked"), []int{2}) || got.Status.Units.Locked != 1 {
		t.Fatalf("running %v, states %v", running, got.Status.UnitStates)
	}
	if len(got.Status.Holds) != 1 || got.Status.Holds[0].Name != "A" || len(got.Status.Holds[0].Waiting) != 1 ||
		got.Status.Holds[0].Waiting[0].Name != "B" || got.Status.Holds[0].Waiting[0].On != "src/shared/util.go" {
		t.Fatalf("holds %+v", got.Status.Holds)
	}
	// The two lanes that share a file are a chain on the critical path:
	// at least A's and B's work.
	plan, _ := q.PlanOf(ns + "/lanes")
	if plan == nil || len(plan.Units[2].Claims) != 1 || plan.Units[2].Claims[0].Mode != "exclusive" {
		t.Fatalf("claims %+v", plan)
	}
	for _, sj := range q.lastSnap.Jobs {
		if sj.ID == ns+"/lanes" && sj.Estimate.TPath < 1150 {
			t.Fatalf("critical path %v, want A then B", sj.Estimate.TPath)
		}
	}

	// A ends: B runs.
	*now = now.Add(10 * time.Minute)
	finish(t, podOfUnit(t, ns, "lanes", 1), corev1.PodSucceeded, *now, 10*time.Minute, nil)
	tick(t, q)
	got = get(t, ns, "lanes")
	if !slices.Contains(podUnits(unitPods(t, ns, "lanes")), 2) || len(got.Status.Holds) != 0 || len(states(t, got, "locked")) != 0 {
		t.Fatalf("after A: pods %v, states %v, holds %+v", podUnits(unitPods(t, ns, "lanes")), got.Status.UnitStates, got.Status.Holds)
	}
}

// A merge gate is a one-slot resource shared by a namespace's jobs, taken
// first come, first served: the unit that became ready first passes next,
// whatever its job, and the others wait locked, in line, with when they
// are expected to pass.
func TestGateIsFirstComeFirstServed(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	for _, name := range []string{"lane-a", "lane-b"} {
		if err := k8s.Create(ctx, job(ns, name, "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, q)
	expand := func(job, doc string) {
		t.Helper()
		finish(t, podOfUnit(t, ns, job, 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: putDoc(t, st, doc)})
		tick(t, q)
		tick(t, q)
	}
	expand("lane-a", `{"version": 1, "units": [{"name": "a1", "gate": "merge", "work": 600}, {"name": "w"}, {"name": "a2", "gate": "merge", "after": ["w"], "work": 600}]}`)
	if p := podUnits(unitPods(t, ns, "lane-a")); !slices.Contains(p, 1) || !slices.Contains(p, 2) {
		t.Fatalf("lane-a runs %v, want a1 and w", p)
	}
	*now = now.Add(time.Minute)
	expand("lane-b", `{"version": 1, "units": [{"name": "b1", "gate": "merge", "work": 600}]}`)
	*now = now.Add(time.Minute)
	finish(t, podOfUnit(t, ns, "lane-a", 2), corev1.PodSucceeded, *now, time.Minute, nil) // w done: a2 joins the line
	tick(t, q)
	tick(t, q)

	b, a := get(t, ns, "lane-b"), get(t, ns, "lane-a")
	if !slices.Equal(states(t, b, "locked"), []int{1}) || len(b.Status.Gates) != 1 || b.Status.Gates[0].Position != 1 || b.Status.Gates[0].Holder != ns+"/lane-a#a1" {
		t.Fatalf("lane-b: %v %+v", b.Status.UnitStates, b.Status.Gates)
	}
	var a2 *c4v1.WorkGateEntry
	for i, g := range a.Status.Gates {
		if g.Name == "a2" {
			a2 = &a.Status.Gates[i]
		}
	}
	if a2 == nil || a2.Position != 2 || a2.ExpectStart == nil || !a2.ExpectStart.After(b.Status.Gates[0].ExpectStart.Time) {
		t.Fatalf("lane-a gates %+v", a.Status.Gates)
	}

	// a1 passes: b1, first in line, is next, not lane-a's own a2.
	*now = now.Add(10 * time.Minute)
	finish(t, podOfUnit(t, ns, "lane-a", 1), corev1.PodSucceeded, *now, 10*time.Minute, nil)
	tick(t, q)
	if !slices.Contains(podUnits(unitPods(t, ns, "lane-b")), 1) || slices.Contains(podUnits(unitPods(t, ns, "lane-a")), 3) {
		t.Fatalf("after a1: lane-b %v, lane-a %v", podUnits(unitPods(t, ns, "lane-b")), podUnits(unitPods(t, ns, "lane-a")))
	}
	// The line survives a restart.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	a = get(t, ns, "lane-a")
	if len(a.Status.Gates) != 1 || a.Status.Gates[0].Name != "a2" || a.Status.Gates[0].Position != 1 || a.Status.Gates[0].Holder != ns+"/lane-b#b1" {
		t.Fatalf("after a restart: %+v", a.Status.Gates)
	}
}

// A job's measure of done comes from a unit's reading. Required, it is a
// completion predicate: a job whose units finished short of it fails and
// needs a person; one that reached it succeeds.
func TestMeasureOfDone(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	of := int64(379)
	for _, name := range []string{"short", "met"} {
		j := job(ns, name, "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))
		j.Spec.Measure = &c4v1.WorkMeasureSpec{Name: "rules sited", Of: &of, Required: true}
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, q)
	if m := get(t, ns, "short").Status.Measure; m == nil || m.Name != "rules sited" || m.Of != 379 || m.Done {
		t.Fatalf("before any reading: %+v", m)
	}
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "lane"}, {"name": "count", "after": ["lane"]}]}`)
	for _, name := range []string{"short", "met"} {
		finish(t, podOfUnit(t, ns, name, 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	}
	tick(t, q)
	for _, name := range []string{"short", "met"} {
		finish(t, podOfUnit(t, ns, name, 1), corev1.PodSucceeded, *now, time.Minute, nil)
	}
	tick(t, q)
	*now = now.Add(time.Minute)
	for name, value := range map[string]int{"short": 361, "met": 379} {
		reading := putDoc(t, st, fmt.Sprintf(`{"value": %d, "detail": "%d pending"}`, value, 379-value))
		finish(t, podOfUnit(t, ns, name, 2), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputMeasure: reading})
	}
	tick(t, q)
	tick(t, q)
	short, met := get(t, ns, "short"), get(t, ns, "met")
	c := meta.FindStatusCondition(short.Status.Conditions, c4v1.ConditionNeedsPerson)
	if short.Status.Phase != "Failed" || short.Status.Measure.Value != 361 || short.Status.Measure.Done || c == nil || c.Reason != c4v1.NeedsMeasure ||
		!strings.Contains(c.Message, "361 of 379 rules sited") {
		t.Fatalf("short: %s %+v %+v", short.Status.Phase, short.Status.Measure, c)
	}
	if met.Status.Phase != "Succeeded" || !met.Status.Measure.Done || met.Status.Measure.Unit != 2 {
		t.Fatalf("met: %s %+v", met.Status.Phase, met.Status.Measure)
	}
}

// An edge source re-run in the job adds orderings between units already
// in it: a document of edges alone. A unit not yet started waits on its
// new edge; edges that would close a cycle are refused.
func TestEdgesFromASource(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "edges", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "scan", "planned": true}, {"name": "A"},
		{"name": "B", "after": ["scan"]}, {"name": "C", "after": ["scan"]}]}`)
	finish(t, podOfUnit(t, ns, "edges", 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	// The scan finds that B waits on A (a Pending marker in B's code).
	found := putDoc(t, st, `{"version": 1, "units": [], "edges": [{"from": "A", "to": "B", "cite": {"file": "src/b.go", "line": 12}}]}`)
	finish(t, podOfUnit(t, ns, "edges", 1), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: found})
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "edges")
	units := podUnits(unitPods(t, ns, "edges"))
	if !slices.Contains(units, 2) || !slices.Contains(units, 4) || slices.Contains(units, 3) || !slices.Equal(states(t, got, "blocked"), []int{3}) {
		t.Fatalf("running %v, states %v", units, got.Status.UnitStates)
	}
	plan, _ := q.PlanOf(ns + "/edges")
	if !slices.Equal(plan.Units[3].After, []int{1, 2}) {
		t.Fatalf("B after %v", plan.Units[3].After)
	}
	// A restart replays the edges.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	if plan, _ := q.PlanOf(ns + "/edges"); !slices.Equal(plan.Units[3].After, []int{1, 2}) {
		t.Fatalf("after a restart B after %v", plan.Units[3].After)
	}
}

func TestEdgesThatCloseACycleAreRefused(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "loop", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "scan", "planned": true}, {"name": "A", "after": ["scan"]}, {"name": "B", "after": ["A"]}]}`)
	finish(t, podOfUnit(t, ns, "loop", 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	bad := putDoc(t, st, `{"version": 1, "units": [], "edges": [{"from": "B", "to": "A"}]}`)
	finish(t, podOfUnit(t, ns, "loop", 1), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: bad})
	tick(t, q)
	got := get(t, ns, "loop")
	if n := len(got.Status.Expansions); n != 2 || !strings.Contains(got.Status.Expansions[1].Error, "cycle") {
		t.Fatalf("expansions %+v", got.Status.Expansions)
	}
}

// Procedural and model units: each attempt's slot time is priced at its
// pool's rate, a model unit's reported tokens at their price, and the
// job's model share of spend is what its model units cost over all.
func TestSpendByKind(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "kinds", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "impl", "reasoning": {"does": "model"}}, {"name": "check", "after": ["impl"]}]}`)
	finish(t, podOfUnit(t, ns, "kinds", 0), corev1.PodSucceeded, *now, 30*time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	usage := putDoc(t, st, `{"models": [{"model": "claude-opus-5-5", "inputTokens": 120000, "outputTokens": 8000, "cacheReadTokens": 90000, "costUSD": 1.92},
		{"model": "claude-haiku-5-5", "inputTokens": 2000}], "costSource": "metering-proxy",
		"reasoning": {"does": "model", "model": "claude-opus-5-5", "effort": "high", "efforts": ["medium", "high"], "toolNames": ["Read", "Edit"]}}`)
	finish(t, podOfUnit(t, ns, "kinds", 1), corev1.PodSucceeded, *now, 30*time.Minute, map[string]string{c4v1.AnnotationOutputUsage: usage})
	tick(t, q)
	finish(t, podOfUnit(t, ns, "kinds", 2), corev1.PodSucceeded, *now, 30*time.Minute, nil)
	tick(t, q)
	check := func(when string) {
		t.Helper()
		got := get(t, ns, "kinds")
		sp := got.Status.Spend
		// Three units ran: root and check procedural, impl a model unit.
		// Each ran half an hour on one slot at $0.40 a slot-hour.
		if sp == nil || sp.ModelUnits != 1 || sp.ProceduralUnits != 2 || sp.ModelTokens != 220000 || sp.UnpricedTokens != 2000 || sp.TokenCost != "1.9200" ||
			sp.ModelSlotCost != "0.2000" || sp.ProceduralSlotCost != "0.4000" || sp.ModelShare != "0.841" ||
			sp.EstimatedTokenCost != "0.0002" || sp.ModelShareBasis != "estimated" || sp.CacheReadTokens != 90000 || sp.InputTokens != 122000 {
			t.Fatalf("%s: spend %+v", when, sp)
		}
		// Of the slot time, models did a third (half an hour of an hour
		// and a half), though most of the cost.
		if sp.ModelSlotHours != "0.5000" || sp.ProceduralSlotHours != "1.0000" || sp.ModelWorkShare != "0.333" {
			t.Fatalf("%s: slot hours %s and %s, work share %s", when, sp.ModelSlotHours, sp.ProceduralSlotHours, sp.ModelWorkShare)
		}
		if u := got.Status.ModelUsage["1"]; len(u.Models) != 2 || u.Models[1].Model != "claude-opus-5-5" || u.InputTokens != 122000 || u.Cost != "1.9200" ||
			u.Models[0].Cost != "" || u.CostSource != "metering-proxy" || u.SlotCost != "0.2000" || u.SlotHours != "0.5000" ||
			u.Reasoning == nil || u.Reasoning.Model != "claude-opus-5-5" || u.Reasoning.Effort != "high" || len(u.Reasoning.ToolNames) != 2 {
			t.Fatalf("%s: usage %+v", when, got.Status.ModelUsage)
		}
	}
	check("after the run")
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	check("after a restart")
}

// Cache reads are weighed by what they cost, not counted at full weight:
// a session that reads its context from the cache, priced by nobody,
// comes to its published price, and a model with no known price leaves
// the share partial.
func TestUnpricedCacheReadsAreWeighted(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "agent", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "lane", "reasoning": {"does": "model"}}, {"name": "other", "reasoning": {"does": "model"}}]}`)
	finish(t, podOfUnit(t, ns, "agent", 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	// agent-jobs' measured session, on Sonnet 5.5 at $2 a million input.
	usage := putDoc(t, st, `{"models": [{"model": "claude-sonnet-5-5", "inputTokens": 2112, "outputTokens": 1060000,
		"cacheReadTokens": 514000000, "cacheWriteTokens": 9233}]}`)
	finish(t, podOfUnit(t, ns, "agent", 1), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputUsage: usage})
	tick(t, q)
	sp := get(t, ns, "agent").Status.Spend
	// weighted: 2112 + 5 x 1,060,000 + 1.25 x 9,233 + 0.1 x 514,000,000 = 56,713,653
	if sp.ModelTokens != 515071345 || sp.CacheReadTokens != 514000000 || sp.CacheWriteTokens != 9233 ||
		sp.WeightedTokens != 56713653 || sp.UnpricedWeightedTokens != 56713653 || sp.EstimatedTokenCost != "113.4273" || sp.ModelShareBasis != "estimated" {
		t.Fatalf("spend %+v", sp)
	}
	other := putDoc(t, st, `{"models": [{"model": "some-other-model", "inputTokens": 1000}]}`)
	finish(t, podOfUnit(t, ns, "agent", 2), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputUsage: other})
	tick(t, q)
	if sp := get(t, ns, "agent").Status.Spend; sp.ModelShareBasis != "partial" || sp.EstimatedTokenCost != "113.4273" {
		t.Fatalf("with an unknown model: %+v", sp)
	}
}
