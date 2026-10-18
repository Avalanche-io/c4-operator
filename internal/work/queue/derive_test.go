package queue

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/process"
)

const pinned = "busybox@sha256:0000000000000000000000000000000000000000000000000000000000000001"

// derivationFarm has a resolved read-only input and, when sealed, a
// network policy that denies egress to every pod.
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
	// A third run of the same work is estimated from the first two.
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

// Without a policy denying egress, an exact declaration reads as
// validated, and status says why.
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
