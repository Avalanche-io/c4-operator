package queue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/process"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// A recompute reruns the unit that wrote an output, at another site: a
// WorkJob made from the producing job's spec, planned as that one unit,
// reading the views the unit read (pinned by ID), in the producing job's
// pools at the site, due when the stage needs it. It runs the same
// process, so an exact rerun that writes the same output verifies it, and
// the job it reran stays the one the record would rerun next.
func TestRecomputeRerunsTheProducingUnit(t *testing.T) {
	ns, q, now := derivationFarm(t, true)
	ctx := context.Background()
	// A view ID a Materialization's source accepts.
	plates := c4.Identify(strings.NewReader("plates view")).String()
	var m c4v1.Materialization
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "plates"}, &m); err != nil {
		t.Fatal(err)
	}
	m.Status.ViewID = plates
	if err := k8s.Status().Update(ctx, &m); err != nil {
		t.Fatal(err)
	}
	out := func(i int) string { return "c4fx" + string(rune('a'+i)) }
	if err := k8s.Create(ctx, exactJob(ns, "sh310", *now)); err != nil {
		t.Fatal(err)
	}
	runAll(t, q, now, ns, "sh310", 5*time.Minute, out)

	project := "namespace/" + ns
	key, inputs, cost, ok := q.RecomputeCost(project, "c4fxb", 0.5)
	if !ok || len(inputs) != 1 || inputs[0].View != plates {
		t.Fatalf("record of c4fxb: %+v %v %v", cost, inputs, ok)
	}
	pool := "cpu-" + ns
	by := now.Add(40 * time.Minute)
	if err := q.Recompute(ctx, project, key, inputs, "van", "c4fxb", by); !errors.Is(err, errNotRerunnable) {
		t.Fatalf("no pool at van: %v", err)
	}
	q.derivMu.Lock()
	q.sites = map[string]string{pool: "van"}
	q.derivMu.Unlock()
	for range 2 { // the second finds it made
		if err := q.Recompute(ctx, project, key, inputs, "van", "c4fxb", by); err != nil {
			t.Fatal(err)
		}
	}
	var l c4v1.WorkJobList
	if err := k8s.List(ctx, &l); err != nil {
		t.Fatal(err)
	}
	var rc *c4v1.WorkJob
	for i := range l.Items {
		if j := &l.Items[i]; j.Namespace == ns && j.Labels[LabelRecompute] == "true" {
			if rc != nil {
				t.Fatalf("two recomputes: %s, %s", rc.Name, j.Name)
			}
			rc = j
		}
	}
	switch {
	case rc == nil:
		t.Fatal("no recompute job")
	case rc.Annotations[AnnotationRecomputeUnit] != "1" || rc.Annotations[AnnotationRecomputeOf] != ns+"/sh310#1":
		t.Fatalf("annotations %v", rc.Annotations)
	case rc.Annotations[AnnotationDerivationClass] != "exact":
		t.Fatal("the declared class did not carry over")
	case len(rc.Spec.Pools) != 1 || rc.Spec.Pools[0] != pool:
		t.Fatalf("pools %v", rc.Spec.Pools)
	case rc.Spec.Deadline == nil || !rc.Spec.Deadline.Time.Equal(by.Truncate(time.Second)):
		t.Fatalf("deadline %v, want %v", rc.Spec.Deadline, by)
	case len(rc.Spec.Inputs) != 1 || rc.Spec.Inputs[0].MountPath != "/plates" || !rc.Spec.Inputs[0].ReadOnly:
		t.Fatalf("inputs %+v", rc.Spec.Inputs)
	}
	var in c4v1.Materialization
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: rc.Spec.Inputs[0].Materialization}, &in); err != nil {
		t.Fatal(err)
	}
	if in.Spec.Source.ID != plates || len(in.OwnerReferences) != 1 || in.OwnerReferences[0].Name != rc.Name {
		t.Fatalf("pinned input %+v %+v", in.Spec.Source, in.OwnerReferences)
	}
	in.Status.ViewID = plates
	if err := k8s.Status().Update(ctx, &in); err != nil {
		t.Fatal(err)
	}

	// It plans the one unit, with the same process.
	tick(t, q)
	v := q.jobs[get(t, ns, rc.Name).UID]
	if v == nil || len(v.plan.Units) != 1 {
		t.Fatalf("recompute plan: %+v", v)
	}
	src := q.jobs[get(t, ns, "sh310").UID]
	if got, want := v.plan.Units[0].Label, src.plan.Units[1].Label; got != want {
		t.Fatalf("recompute runs unit %q, want %q", got, want)
	}
	if p, ok := process.ProcessID(unitContainers(get(t, ns, rc.Name), v.plan.Units[0], q.digests), ""); !ok || p != key.Process {
		t.Fatalf("recompute process %s, recorded %s", p, key.Process)
	}
	runAll(t, q, now, ns, rc.Name, 5*time.Minute, func(int) string { return "c4fxb" })
	if pr, ok := q.book(project).ProducerOf(key); !ok || pr.Job != ns+"/sh310" || pr.Unit != 1 {
		t.Fatalf("producer after the recompute: %+v %v", pr, ok)
	}
	if d := get(t, ns, rc.Name).Status.Derivation; d == nil || d.Matched != 1 || d.Mismatched != 0 {
		t.Fatalf("the rerun did not verify: %+v", d)
	}

	// The Placement prices recomputes and starts them from inside a tick,
	// which holds the queue's lock.
	if err := k8s.Create(ctx, exactJob(ns, "sh311", *now)); err != nil {
		t.Fatal(err)
	}
	rp := &reentrantPlacement{q: q, project: project, key: key, inputs: inputs}
	q.Placement = rp
	done := make(chan error, 1)
	go func() { done <- q.Tick(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a tick deadlocked on the Placement calling back into the queue")
	}
	if rp.calls == 0 {
		t.Fatal("the tick never asked the Placement for sources")
	}
}

// reentrantPlacement calls back into the queue as placement.Provider does:
// RecomputeCost while listing sources, Recompute while staging.
type reentrantPlacement struct {
	q       *Queue
	project string
	key     process.Key
	inputs  []process.Input
	calls   int
}

func (r *reentrantPlacement) Site(*c4v1.Pool, []string) string { return "van" }

func (r *reentrantPlacement) Sources(project string, in []process.Input, site string, _ bool) []sched.Source {
	r.calls++
	r.q.RecomputeCost(r.project, "c4fxb", 0.5)
	if err := r.q.Recompute(context.Background(), r.project, r.key, r.inputs, site, "c4fxb", time.Now().Add(time.Hour)); err != nil {
		panic(err)
	}
	return nil
}

func (r *reentrantPlacement) Links() []sched.Link { return nil }

func (r *reentrantPlacement) Stage(context.Context, string, []process.Input, string, sched.Stage) error {
	return nil
}
