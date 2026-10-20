package queue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A pool with owners runs only their jobs: another owner's job, or one
// with no owner, is never released there and says why (Schedulable), and
// a dry run of it is refused with the same words. A job that also names a
// pool open to anyone runs there.
func TestPoolOwners(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	pool := "cpu-" + ns
	var p c4v1.Pool
	if err := k8s.Get(ctx, types.NamespacedName{Name: pool}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Owners = []string{"joshua"}
	if err := k8s.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	owned := func(name, owner string, pools ...string) *c4v1.WorkJob {
		j := job(ns, name, "render", pool, map[string]string{"frames": "1-1"}, now.Add(time.Hour))
		if len(pools) > 0 {
			j.Spec.Pools = pools
		}
		if owner != "" {
			j.Annotations = map[string]string{c4v1.AnnotationOwner: owner}
		}
		return j
	}
	for _, j := range []*c4v1.WorkJob{owned("mine", "joshua"), owned("theirs", "ivo"), owned("nobodys", "")} {
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, q)
	if n := len(unitPods(t, ns, "mine")); n != 1 {
		t.Fatalf("the owner's job released %d units", n)
	}
	for name, whose := range map[string]string{"theirs": "this job is ivo's", "nobodys": "this job has no owner"} {
		if n := len(unitPods(t, ns, name)); n != 0 {
			t.Errorf("%s: released %d units in another owner's pool", name, n)
		}
		var j c4v1.WorkJob
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &j); err != nil {
			t.Fatal(err)
		}
		c := meta.FindStatusCondition(j.Status.Conditions, "Schedulable")
		want := "pool " + pool + " runs only the jobs of joshua; " + whose
		if c == nil || c.Status != "False" || c.Message != want {
			t.Errorf("%s: Schedulable %+v, want %q", name, c, want)
		}
	}
	_, err := q.DryRun(ctx, owned("draft", "ivo"), "namespace/"+ns)
	if !errors.Is(err, ErrInvalidJob) || !strings.Contains(err.Error(), "runs only the jobs of joshua; this job is ivo's") {
		t.Errorf("dry run: %v", err)
	}
	if _, err := q.DryRun(ctx, owned("draft", "joshua"), "namespace/"+ns); err != nil {
		t.Errorf("the owner's dry run: %v", err)
	}

	// a pool anyone may use, on the same node: the job runs there
	open := &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "open-" + ns}, Spec: c4v1.PoolSpec{
		NodeSelector: p.Spec.NodeSelector, SlotsPerNode: 4, CostPerSlotHour: "0.40"}}
	if err := k8s.Create(ctx, open); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(context.Background(), open) })
	if err := k8s.Create(ctx, owned("both", "ivo", pool, open.Name)); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "both")
	if len(pods) != 1 || pods[0].Labels[c4v1.LabelPool] != open.Name {
		t.Fatalf("%d pods, in pool %v", len(pods), pods)
	}
}
