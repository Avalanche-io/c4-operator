package queue

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A block's branch, as its runner recorded it on the pod, is in the job's
// status once the pod ends: the latest attempt's, kept across a restart
// after the pods are gone, so the HUD and c4 farm get read it there.
func TestUnitBranchInStatus(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "port", "ci", "cpu-"+ns, map[string]string{"shards": "2"}, time.Time{})); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	first := `{"repo": "/Users/sam/src/textkit", "dir": "/Users/sam/src/textkit/.worktrees/c4-port-0", "branch": "c4/port/0", "base": "b0", "head": "h1", "commits": 1}`
	p := podOfUnit(t, ns, "port", 0)
	*now = now.Add(time.Minute)
	finish(t, p, corev1.PodFailed, *now, time.Minute, map[string]string{c4v1.AnnotationOutputBranch: first})
	tick(t, q)
	b, ok := get(t, ns, "port").Status.UnitBranches["0"]
	if !ok || b.Branch != "c4/port/0" || b.Head != "h1" || b.Commits != 1 || b.Repo != "/Users/sam/src/textkit" || b.Attempt != 1 {
		t.Fatalf("after the failed attempt: %+v (%v)", b, ok)
	}
	if _, ok := get(t, ns, "port").Status.UnitBranches["1"]; ok {
		t.Fatal("a unit with no branch has one in status")
	}

	// The retry carries the branch on; its record replaces the first's.
	tick(t, q)
	var again *corev1.Pod
	for _, pp := range unitPods(t, ns, "port") {
		if unitOf(pp) == 0 && pp.Annotations[c4v1.AnnotationAttempt] == "2" {
			again = &pp
		}
	}
	if again == nil {
		t.Fatalf("no retry of unit 0: %v", unitsOf(unitPods(t, ns, "port")))
	}
	second := `{"repo": "/Users/sam/src/textkit", "branch": "c4/port/0", "base": "b0", "head": "h2", "commits": 3}`
	*now = now.Add(time.Minute)
	finish(t, again, corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputBranch: second})
	tick(t, q)
	if b := get(t, ns, "port").Status.UnitBranches["0"]; b.Head != "h2" || b.Commits != 3 || b.Attempt != 2 || b.Dir != "" {
		t.Fatalf("after the retry: %+v", b)
	}

	// The pods go; a queue started afresh keeps the branch from status.
	if err := k8s.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(ns), client.MatchingLabels{c4v1.LabelJob: "port"}); err != nil {
		t.Fatal(err)
	}
	fresh := &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, fresh)
	if b := get(t, ns, "port").Status.UnitBranches["0"]; b.Head != "h2" || b.Commits != 3 {
		t.Fatalf("after a restart: %+v", b)
	}
}
