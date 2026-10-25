package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/work/process"
)

const pinned = "busybox@sha256:0000000000000000000000000000000000000000000000000000000000000001"

// derivationFarm has a resolved read-only input and, when sealed, a
// network policy that denies egress to every pod; when not, one that
// allows every pod all egress (beside the queue's default, which denies
// units egress).
func derivationFarm(t *testing.T, sealed bool) (string, *Queue, *time.Time) {
	t.Helper()
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	m := &c4v1.Materialization{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "plates"},
		Spec: c4v1.MaterializationSpec{Source: c4v1.ManifestSource{ID: c4.Identify(strings.NewReader("plates")).String()}}}
	if err := k8s.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	m.Status.ViewID = "c4platesview"
	if err := k8s.Status().Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	if sealed {
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "deny-egress"},
			Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}}
		if err := k8s.Create(ctx, np); err != nil {
			t.Fatal(err)
		}
	} else {
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "allow-egress"},
			Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{}}}}
		if err := k8s.Create(ctx, np); err != nil {
			t.Fatal(err)
		}
	}
	return ns, q, now
}

func exactJob(ns, name string, now time.Time) *c4v1.WorkJob {
	j := job(ns, name, "render", "cpu-"+ns, map[string]string{"frames": "1001-1002", "estimateSeconds": "60"}, now.Add(time.Hour))
	j.Annotations = map[string]string{AnnotationDerivationClass: "exact"}
	j.Spec.Template.Spec.Containers[0].Image = pinned
	j.Spec.Inputs = []c4v1.WorkInput{{Materialization: "plates", MountPath: "/plates", ReadOnly: true}}
	return j
}

// runAll ticks a job to success; each unit takes d and writes output(i).
func runAll(t *testing.T, q *Queue, now *time.Time, ns, name string, d time.Duration, output func(int) string) {
	t.Helper()
	for i := 0; i < 20 && get(t, ns, name).Status.Phase != "Succeeded"; i++ {
		tick(t, q)
		*now = now.Add(d + time.Second)
		for _, p := range unitPods(t, ns, name) {
			if p.Status.Phase == "" || p.Status.Phase == corev1.PodPending {
				finish(t, &p, corev1.PodSucceeded, *now, d, map[string]string{c4v1.AnnotationOutputTree: output(unitOf(p))})
			}
		}
	}
	tick(t, q)
	if ph := get(t, ns, name).Status.Phase; ph != "Succeeded" {
		t.Fatalf("%s did not finish: %s", name, ph)
	}
}

// The process ID the queue computes before a unit runs is the one it
// records from the unit's pod.
func TestUnitProcessMatchesPod(t *testing.T) {
	ns, q, now := derivationFarm(t, true)
	ctx := context.Background()
	j := exactJob(ns, "sh300", *now)
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "sh300")
	if len(pods) == 0 {
		t.Fatal("no pods")
	}
	p := pods[0]
	fromPod, sealed, why := containersOf(p.Spec, nil, nil)
	a, ok1 := process.ProcessID(fromPod, "")
	view := q.jobs[get(t, ns, "sh300").UID]
	b, ok2 := process.ProcessID(unitContainers(get(t, ns, "sh300"), view.plan.Units[unitOf(p)], nil), "")
	if !ok1 || !ok2 || a != b {
		t.Fatalf("process from the pod %s, before it ran %s", a, b)
	}
	if !sealed {
		t.Fatalf("a pinned image without host paths is not sealed by spec: %s", why)
	}
}

// A declared-exact, sealed job is verified by a second job doing the same
// work (same output), estimated from the first, and demoted by a third
// that writes something else.
func TestDerivationVerifiedThenDemoted(t *testing.T) {
	ns, q, now := derivationFarm(t, true)
	ctx := context.Background()
	same := func(i int) string { return "c4out" + string(rune('a'+i)) }
	for _, name := range []string{"sh301-v1", "sh301-v2"} {
		if err := k8s.Create(ctx, exactJob(ns, name, *now)); err != nil {
			t.Fatal(err)
		}
		runAll(t, q, now, ns, name, 5*time.Minute, same)
	}
	d := get(t, ns, "sh301-v2").Status.Derivation
	if d == nil || d.Declared != "exact" || d.Class != "exact" || !d.Verified || d.Matched != 2 || d.Mismatched != 0 {
		t.Fatalf("after two matching jobs: %+v", d)
	}
	// What recomputing an output costs, and reuse in its place.
	_, inputs, cost, ok := q.RecomputeCost("namespace/"+ns, "c4outa", 0.5)
	if !ok || cost.Basis != "exact" || cost.Reuse != "c4outa" || len(inputs) != 1 || cost.Work.Mean < 290 || cost.Work.Mean > 310 {
		t.Fatalf("recompute: %+v %v %v", cost, inputs, ok)
	}
	if _, _, c, _ := q.RecomputeCost("namespace/"+ns, "c4outa", 0.01); c.Reuse != "" {
		t.Fatal("a sampled verification still offered reuse")
	}
	// A third run of the same work is estimated from the first two. It is
	// a sampled verification (every reuse opportunity is, here), so it
	// runs instead of reusing.
	q.book("namespace/" + ns).VerifyRate = 1
	if err := k8s.Create(ctx, exactJob(ns, "sh301-v3", *now)); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if w := get(t, ns, "sh301-v3").Status.Estimate.RemainingWork; w < 550 || w > 650 {
		t.Fatalf("estimate from the record: %ds, want about 600", w)
	}
	// It writes different bytes: the declaration is falsified.
	runAll(t, q, now, ns, "sh301-v3", 5*time.Minute, func(i int) string { return "c4other" + string(rune('a'+i)) })
	d = get(t, ns, "sh301-v3").Status.Derivation
	if d == nil || d.Class != "validated" || d.Verified || d.Mismatched == 0 || d.Why == "" {
		t.Fatalf("after a mismatch: %+v", d)
	}
}

// With a policy allowing egress (whatever else denies it), an exact
// declaration reads as validated, and status says why.
func TestUnsealedExactIsValidated(t *testing.T) {
	ns, q, now := derivationFarm(t, false)
	if err := k8s.Create(context.Background(), exactJob(ns, "sh302", *now)); err != nil {
		t.Fatal(err)
	}
	runAll(t, q, now, ns, "sh302", time.Minute, func(i int) string { return "c4o" + string(rune('a'+i)) })
	d := get(t, ns, "sh302").Status.Derivation
	if d == nil || d.Class != "validated" || d.Why == "" {
		t.Fatalf("unsealed: %+v", d)
	}
}

// A project's derivations travel in its saved calibrations object and come
// back with the verification they earned.
func TestDerivationsPersist(t *testing.T) {
	q := &Queue{}
	b := q.book("show-a")
	for range 3 {
		b.Record(process.Run{Key: process.Key{Process: "p", Inputs: "i"}, Program: "g", Declared: process.Exact, Sealed: true, Output: "o", Work: 60})
	}
	js, err := json.Marshal(q.calRecordOf("show-a", time.Unix(0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	var rec calRecord
	if err := json.Unmarshal(js, &rec); err != nil {
		t.Fatal(err)
	}
	q2 := &Queue{}
	q2.restoreProject(&rec)
	c, v := q2.book("show-a").Class("g")
	if c != process.Exact || !v {
		t.Fatalf("restored class %s verified=%v", c, v)
	}
	if q2.book("show-b").Len() != 0 {
		t.Fatal("another project got the record")
	}
}

// A procedural unit whose computation ran before, exact and verified,
// reuses that output: the third run of the same work starts no pods,
// succeeds, and says its units were reused.
func TestExactVerifiedWorkIsReused(t *testing.T) {
	ns, q, now := derivationFarm(t, true)
	ctx := context.Background()
	q.book("namespace/" + ns).VerifyRate = 1e-12 // no sampled re-run in this test
	same := func(i int) string { return c4.Identify(strings.NewReader(fmt.Sprintf("frame-%d", i))).String() }
	for _, name := range []string{"sh400-v1", "sh400-v2"} {
		if err := k8s.Create(ctx, exactJob(ns, name, *now)); err != nil {
			t.Fatal(err)
		}
		runAll(t, q, now, ns, name, 5*time.Minute, same)
	}
	if d := get(t, ns, "sh400-v2").Status.Derivation; d == nil || !d.Verified {
		t.Fatalf("not verified: %+v", d)
	}
	// Planned before it is submitted, the next version says what reuse
	// will skip.
	draft := exactJob(ns, "sh400-v3", *now)
	views := map[string]string{}
	for _, in := range draft.Spec.Inputs {
		var m c4v1.Materialization
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: in.Materialization}, &m); err == nil {
			views[in.Materialization] = m.Status.ViewID
		}
	}
	if dr, err := q.DryRunAll(ctx, []*c4v1.WorkJob{draft}, "namespace/"+ns, views); err != nil || dr[0].Reused != 2 {
		t.Fatalf("dry run of v3: %+v %v", dr, err)
	}
	if err := k8s.Create(ctx, exactJob(ns, "sh400-v3", *now)); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "sh400-v3")
	if got.Status.Phase != "Succeeded" || got.Status.Units.Reused != 2 || got.Status.Reused != "0-1" || len(unitPods(t, ns, "sh400-v3")) != 0 {
		t.Fatalf("v3: %s reused %d %q, pods %d", got.Status.Phase, got.Status.Units.Reused, got.Status.Reused, len(unitPods(t, ns, "sh400-v3")))
	}
	if n := len(got.Status.Decisions); n == 0 || got.Status.Decisions[n-1].Kind != "reuse" {
		t.Fatalf("decisions %+v", got.Status.Decisions)
	}
	// A job that is not declared exact runs its units.
	j := exactJob(ns, "sh400-v4", *now)
	delete(j.Annotations, AnnotationDerivationClass)
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if n := len(unitPods(t, ns, "sh400-v4")); n == 0 {
		t.Fatal("an undeclared job reused output")
	}
}

// A model unit that, run again on the same inputs, writes the same bytes
// every time is reported as work a procedure might do.
func TestModelWorkAProcedureCouldDo(t *testing.T) {
	ns, q, now := derivationFarm(t, true)
	ctx := context.Background()
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	q.Stores = func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "summarize", "reasoning": {"does": "model"}}, {"name": "draft", "reasoning": {"does": "model"}}]}`)
	run := func(name string, draft string) *c4v1.WorkJob {
		t.Helper()
		j := job(ns, name, "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))
		j.Spec.Template.Spec.Containers[0].Image = pinned
		j.Spec.Inputs = []c4v1.WorkInput{{Materialization: "plates", MountPath: "/plates", ReadOnly: true}}
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		tick(t, q)
		*now = now.Add(time.Minute)
		finish(t, podOfUnit(t, ns, name, 0), corev1.PodSucceeded, *now, time.Minute,
			map[string]string{c4v1.AnnotationOutputExpand: doc, c4v1.AnnotationOutputTree: viewID("plan")})
		tick(t, q)
		*now = now.Add(time.Minute)
		finish(t, podOfUnit(t, ns, name, 1), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("the same summary")})
		finish(t, podOfUnit(t, ns, name, 2), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID(draft)})
		tick(t, q)
		tick(t, q)
		return get(t, ns, name)
	}
	run("notes-1", "draft one")
	if got := run("notes-2", "draft two"); len(got.Status.Procedural) != 0 {
		t.Fatalf("two runs are not enough: %+v", got.Status.Procedural)
	}
	got := run("notes-3", "draft three")
	// summarize wrote the same bytes three times; draft never did.
	if len(got.Status.Procedural) != 1 || got.Status.Procedural[0].Name != "summarize" || got.Status.Procedural[0].Runs != 3 {
		t.Fatalf("%s: %+v", got.Status.Phase, got.Status.Procedural)
	}
}
