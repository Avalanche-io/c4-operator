package queue

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
