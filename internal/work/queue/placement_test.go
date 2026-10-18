package queue

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/process"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// fakePlacement puts pools at the site their "site" attribute names; the
// inputs are at "lon", and "van" lacks them until a stage there arrives.
type fakePlacement struct {
	mu      sync.Mutex
	arrived bool
	stages  []sched.Stage
	inputs  [][]process.Input
	accept  []bool
}

func (f *fakePlacement) Site(p *c4v1.Pool, _ []string) string { return p.Spec.Attributes["site"] }

func (f *fakePlacement) Sources(_ string, in []process.Input, site string, accept bool) []sched.Source {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accept = append(f.accept, accept)
	if site != "van" || f.arrived {
		return nil
	}
	if len(f.stages) > 0 {
		return []sched.Source{{Via: "staging", Latency: time.Minute}}
	}
	return []sched.Source{{Via: "move", Channel: "interconnect", Link: "lon-van", Bytes: 1e9, Cost: 2}}
}

func (f *fakePlacement) Links() []sched.Link {
	return []sched.Link{{Name: "lon-van", BytesPerSecond: 1.25e9}}
}

func (f *fakePlacement) Stage(_ context.Context, _ string, in []process.Input, site string, st sched.Stage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stages = append(f.stages, st)
	f.inputs = append(f.inputs, in)
	return nil
}

// addPool adds a pool of n nodes of slots each, labelled for it, with
// attributes.
func addPool(t *testing.T, ns, name string, n, slots int, attrs map[string]string) *c4v1.Pool {
	t.Helper()
	ctx := context.Background()
	label := ns + "-" + name
	for i := 0; i < n; i++ {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", label, i), Labels: map[string]string{"pool": label}}}
		if err := k8s.Create(ctx, node); err != nil {
			t.Fatal(err)
		}
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		if err := k8s.Status().Update(ctx, node); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { k8s.Delete(ctx, node) })
	}
	p := &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + ns}, Spec: c4v1.PoolSpec{
		NodeSelector: map[string]string{"pool": label}, SlotsPerNode: int32(slots), CostPerSlotHour: "0.40", Attributes: attrs}}
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(ctx, p) })
	return p
}

// A job at risk at lon, whose inputs van lacks: the queue passes van's
// Sources to the policy, carries out the stage it approves, and releases
// nothing into van until the inputs are there.
func TestPlacementStagesInputs(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	ctx := context.Background()
	var lon c4v1.Pool
	if err := k8s.Get(ctx, client.ObjectKey{Name: "cpu-" + ns}, &lon); err != nil {
		t.Fatal(err)
	}
	lon.Spec.Attributes = map[string]string{"site": "lon"}
	if err := k8s.Update(ctx, &lon); err != nil {
		t.Fatal(err)
	}
	van := addPool(t, ns, "van", 4, 2, map[string]string{"site": "van"})
	m := &c4v1.Materialization{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "plates"},
		Spec: c4v1.MaterializationSpec{Source: c4v1.ManifestSource{ID: c4.Identify(strings.NewReader("plates")).String()}}}
	if err := k8s.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	m.Status.ViewID = "c4platesview"
	if err := k8s.Status().Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	pr := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-" + ns}, Spec: c4v1.ProjectSpec{Namespaces: []string{ns},
		Store: c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: "file:///tmp/none"}}},
		Work:  &c4v1.ProjectWork{BurstBudgetPerDay: "500", LateCostPerHour: "300"}}}
	if err := k8s.Create(ctx, pr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(ctx, pr) })
	fp := &fakePlacement{}
	q.Placement = fp
	// 16 frames of half an hour each, due in 4 hours: 4 hours of work on
	// lon's 2 slots before any margin, late; van's spare slots close it.
	j := job(ns, "sh010", "render", "cpu-"+ns, map[string]string{"frames": "1-16", "estimateSeconds": "1800"}, now.Add(4*time.Hour))
	j.Spec.Pools = append(j.Spec.Pools, van.Name)
	j.Spec.Inputs = []c4v1.WorkInput{{Materialization: "plates", MountPath: "/plates", ReadOnly: true}}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	tick(t, q)
	fp.mu.Lock()
	stages, inputs := len(fp.stages), fp.inputs
	fp.mu.Unlock()
	if stages != 1 || len(inputs[0]) != 1 || inputs[0][0].View != "c4platesview" {
		var msgs []string
		for _, sj := range q.lastSnap.Jobs {
			js := q.lastDec.Jobs[sj.ID]
			msgs = append(msgs, fmt.Sprintf("%s pools %v sources %v risk %v burst %+v stage %+v: %s", sj.ID, sj.Pools, sj.Sources, js.AtRisk, js.Burst, js.Stage, js.Message))
		}
		t.Fatalf("stages %d inputs %+v; decision %+v; jobs %v; projects %+v", stages, inputs, q.lastDec.Stage, msgs, q.lastSnap.Projects)
	}
	st := q.lastSnap.Jobs[0]
	if len(q.lastSnap.Links) != 1 || st.Sources[van.Name] == nil {
		t.Fatalf("snapshot links %+v sources %+v", q.lastSnap.Links, st.Sources)
	}
	for _, p := range unitPods(t, ns, "sh010") {
		if p.Spec.NodeSelector["pool"] == ns+"-van" {
			t.Fatalf("unit %d released into van before its inputs arrived", unitOf(p))
		}
	}
	// Once the inputs are there, van takes units.
	fp.mu.Lock()
	fp.arrived = true
	fp.mu.Unlock()
	*now = now.Add(time.Minute)
	tick(t, q)
	inVan := 0
	for _, p := range unitPods(t, ns, "sh010") {
		if p.Spec.NodeSelector["pool"] == ns+"-van" {
			inVan++
		}
	}
	if inVan == 0 {
		t.Fatalf("no unit in van after the inputs arrived; decision %+v", q.lastDec.Dispatch)
	}
}
