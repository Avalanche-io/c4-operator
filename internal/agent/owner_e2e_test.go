package agent_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/agent"
)

// ownedJob creates a WorkJob owned by owner, as the owner webhook would
// stamp it (envtest runs no webhooks).
func ownedJob(t *testing.T, ns, name, owner string) *c4v1.WorkJob {
	t.Helper()
	ctx := context.Background()
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: map[string]string{c4v1.AnnotationOwner: owner}},
		Spec: c4v1.WorkJobSpec{Class: "agent", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "member", Image: "c4-agent"}}}}}}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(context.Background(), j) })
	return j
}

// unitOf is the controller reference the work queue gives a job's pods.
func unitOf(j *c4v1.WorkJob) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: j.Name, UID: j.UID, Controller: &yes}}
}

// TestRunnerRunsOnlyItsOwnersJobs: a host runner binds a member pod only
// when the WorkJob controlling it is its owner's, read from the job, not
// the pod. A pod of another owner's job is never bound; one bound to the
// runner's node by someone else fails NotTheOwner, as do a pod of no job
// and one whose reference names an earlier job of the same name. The pod's
// own owner annotation counts for nothing.
func TestRunnerRunsOnlyItsOwnersJobs(t *testing.T) {
	if k8s == nil {
		t.Skip("no envtest binaries (make envtest)")
	}
	ctx := context.Background()
	ns := fmt.Sprintf("own-%d", time.Now().UnixNano()%1e9)
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	host := ns + "-laptop"
	sel := map[string]string{"c4.avalanche.io/host": host}
	t.Cleanup(func() { k8s.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: host}}) })
	h := agent.New(&agent.Store{Dir: t.TempDir()}, &fake{active: map[string]bool{}})
	var logs []string
	r := &agent.Runner{Client: k8s, Harness: h, Node: host, Labels: sel, Owners: []string{"noor"}, Slots: 4,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}

	mine, theirs := ownedJob(t, ns, "mine", "noor"), ownedJob(t, ns, "theirs", "ivo")
	gone := ownedJob(t, ns, "gone", "noor")
	stale := unitOf(gone)
	if err := k8s.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	ownedJob(t, ns, "gone", "ivo") // same name, new job, another owner

	pod := func(name, node string, refs []metav1.OwnerReference) *corev1.Pod {
		t.Helper()
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{c4v1.LabelJob: "x"},
			Annotations: map[string]string{c4v1.AnnotationOwner: "noor"}, OwnerReferences: refs},
			Spec: corev1.PodSpec{NodeSelector: sel, NodeName: node, Containers: []corev1.Container{{Name: "member", Image: "c4-agent",
				Env: []corev1.EnvVar{{Name: "C4_AGENT_MEMBER", Value: "lead"}}}}}}
		if err := k8s.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.Status.Phase = corev1.PodPending
		if err := k8s.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pod("mine-0", "", unitOf(mine))
	pod("theirs-0", "", unitOf(theirs))
	pod("theirs-bound", host, unitOf(theirs))
	pod("loose", host, nil)
	pod("stale", host, stale)
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	get := func(name string) *corev1.Pod {
		t.Helper()
		var p corev1.Pod
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p); err != nil {
			t.Fatal(err)
		}
		return &p
	}
	if p := get("mine-0"); p.Spec.NodeName != host {
		t.Errorf("the owner's pod was not bound: %q", p.Spec.NodeName)
	}
	if p := get("theirs-0"); p.Spec.NodeName != "" {
		t.Errorf("another owner's pod was bound to %s", p.Spec.NodeName)
	}
	for name, want := range map[string]string{
		"theirs-bound": "job " + ns + "/theirs is ivo's; this runner runs only the jobs of noor",
		"loose":        "the pod is not a unit of a WorkJob",
		"stale":        "its controller reference names an earlier job " + ns + "/gone",
	} {
		p := get(name)
		if p.Status.Phase != corev1.PodFailed || p.Status.Reason != "NotTheOwner" || p.Status.Message != want {
			t.Errorf("%s: %s %s %q, want Failed NotTheOwner %q", name, p.Status.Phase, p.Status.Reason, p.Status.Message, want)
		}
	}
	// a pod refused before binding is logged once, not every tick
	r.Tick(ctx)
	n := 0
	for _, l := range logs {
		if strings.HasPrefix(l, "not binding "+ns+"/theirs-0") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("refusal logged %d times", n)
	}
}
