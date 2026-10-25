package queue

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// hostNodeFor makes a ready host runner's Node: its host label (its own
// name unless given), its labels and annotations.
func hostNodeFor(t *testing.T, name, hostLabel string, labels, annotations map[string]string) {
	t.Helper()
	ctx := context.Background()
	if hostLabel == "" {
		hostLabel = name
	}
	l := map[string]string{c4v1.LabelHost: hostLabel}
	for k, v := range labels {
		l[k] = v
	}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l, Annotations: annotations},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: c4v1.TaintHostRunner, Value: "true", Effect: corev1.TaintEffectNoSchedule}}}}
	if err := k8s.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(context.Background(), n) })
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := k8s.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
}

func hostPool(t *testing.T, name string, sel map[string]string, owners ...string) {
	t.Helper()
	p := &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: c4v1.PoolSpec{NodeSelector: sel, SlotsPerNode: 2,
		Owners:      owners,
		Tolerations: []corev1.Toleration{{Key: c4v1.TaintHostRunner, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}}}
	if err := k8s.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(context.Background(), p) })
}

// A host pool's units are created already bound to a machine that runs
// their owner's work, so no runner binds a pod and none can take another
// person's. A machine's owners are its node-owners annotation (the HUD's
// word), or else its enrolled pool's owners; a Node under another
// machine's host label is never used; a unit with no machine that has room
// waits, and its job says why.
func TestHostPodsAreCreatedBound(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 1)
	ctx := context.Background()
	shared := ns + "-hosts"
	a, b, impostor := ns+"-a", ns+"-b", ns+"-c"
	hostNodeFor(t, a, "", map[string]string{"group": shared}, map[string]string{c4v1.AnnotationNodeOwners: `["noor"]`})
	hostNodeFor(t, b, "", map[string]string{"group": shared}, map[string]string{c4v1.AnnotationNodeOwners: `["ivo","dana"]`})
	// a runner that put another machine's name in its host label
	hostNodeFor(t, impostor, a, map[string]string{"group": shared}, nil)
	pool := "hosts-" + ns
	hostPool(t, pool, map[string]string{"group": shared}, "noor", "ivo")

	owned := func(name, owner, pool, frames string) {
		j := job(ns, name, "render", pool, map[string]string{"frames": frames}, now.Add(time.Hour))
		j.Annotations = map[string]string{c4v1.AnnotationOwner: owner}
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	owned("mine", "noor", pool, "1-3")
	owned("theirs", "ivo", pool, "1-1")
	tick(t, q)

	mine := unitPods(t, ns, "mine")
	if len(mine) != 2 {
		t.Fatalf("noor's job released %d units, want the 2 noor's machine has room for", len(mine))
	}
	for _, p := range mine {
		if p.Spec.NodeName != a {
			t.Errorf("noor's unit %s is on %q, want %s", p.Name, p.Spec.NodeName, a)
		}
	}
	theirs := unitPods(t, ns, "theirs")
	if len(theirs) != 1 || theirs[0].Spec.NodeName != b {
		t.Fatalf("ivo's units: %d, on %v", len(theirs), theirs)
	}
	var j c4v1.WorkJob
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "mine"}, &j); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(j.Status.Conditions, "Admitted")
	want := "no machine of noor's in pool " + pool + " has a free slot"
	if c == nil || c.Reason != "NoMachine" || c.Message != want {
		t.Errorf("Admitted %+v, want NoMachine %q", c, want)
	}

	// a machine enrolled before the HUD wrote owners: its pool's owners
	enrolled, d := "enrolled-"+ns, ns+"-d"
	hostNodeFor(t, d, "", nil, nil)
	hostPool(t, enrolled, map[string]string{c4v1.LabelHost: d}, "noor")
	owned("early", "noor", enrolled, "1-1")
	tick(t, q)
	early := unitPods(t, ns, "early")
	if len(early) != 1 || early[0].Spec.NodeName != d {
		t.Fatalf("on a machine enrolled before the HUD wrote owners: %d units, on %v", len(early), early)
	}
	if got, want := early[0].Annotations[c4v1.AnnotationOwnerBasis], "its pool "+enrolled+"'s owner: no one has enrolled this machine"; got != want {
		t.Errorf("owner basis %q, want %q", got, want)
	}
	for _, p := range append(mine, theirs...) {
		if b := p.Annotations[c4v1.AnnotationOwnerBasis]; b != "" {
			t.Errorf("%s on an enrolled machine says %q", p.Name, b)
		}
	}

	// no fallback for a pool of several owners: one machine's sign-in
	// would run all of their work
	shared2, e := "shared-"+ns, ns+"-e"
	hostNodeFor(t, e, "", nil, nil)
	hostPool(t, shared2, map[string]string{c4v1.LabelHost: e}, "noor", "ivo")
	owned("both", "noor", shared2, "1-1")
	tick(t, q)
	if n := len(unitPods(t, ns, "both")); n != 0 {
		t.Fatalf("an unenrolled machine of a pool with two owners ran %d units", n)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "both"}, &j); err != nil {
		t.Fatal(err)
	}
	c = meta.FindStatusCondition(j.Status.Conditions, "Admitted")
	want = "no machine of noor's in pool " + shared2 + " has been enrolled"
	if c == nil || c.Reason != "NoMachine" || c.Message != want {
		t.Errorf("Admitted %+v, want NoMachine %q", c, want)
	}
}
