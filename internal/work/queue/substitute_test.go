package queue

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/process"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// vanPlacement puts every pool at van and remembers the inputs the queue
// asked about there.
type vanPlacement struct {
	mu   sync.Mutex
	seen map[string][]process.Input // by mount of the first input
}

func (f *vanPlacement) Site(*c4v1.Pool, []string) string { return "van" }

func (f *vanPlacement) Sources(_ string, in []process.Input, _ string, _ bool) []sched.Source {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string][]process.Input{}
	}
	for _, x := range in {
		f.seen[x.Mount] = in
	}
	return nil
}

func (f *vanPlacement) Links() []sched.Link { return nil }

func (f *vanPlacement) Stage(context.Context, string, []process.Input, string, sched.Stage) error {
	return nil
}

func readyMaterialization(t *testing.T, ns, name, view string) {
	t.Helper()
	ctx := context.Background()
	var m c4v1.Materialization
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &m); err != nil {
		m = c4v1.Materialization{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: c4v1.MaterializationSpec{Source: c4v1.ManifestSource{ID: view}}}
		if err := k8s.Create(ctx, &m); err != nil {
			t.Fatal(err)
		}
	}
	m.Status.ViewID, m.Status.Phase, m.Status.ObservedGeneration = view, c4v1.PhaseReady, m.Generation
	if err := k8s.Status().Update(ctx, &m); err != nil {
		t.Fatal(err)
	}
}

// A validated recompute's output stands in for the input it rebuilt, at
// its site, for a job that accepts validated recomputes: the job's unit
// mounts it through a Materialization the job owns, its pod says so, the
// derivation record takes the input it read, and status lists it. A job
// that does not accept them reads its input as named.
func TestValidatedRecomputeIsSubstituted(t *testing.T) {
	ns, q, now := derivationFarm(t, true)
	ctx := context.Background()
	id := func(s string) string { return c4.Identify(strings.NewReader(s)).String() }
	plates, fx, fxAgain := id("plates view"), id("fx as recorded"), id("fx recomputed")
	readyMaterialization(t, ns, "plates", plates)

	// The producer: validated, its unit 0 writes fx.
	src := exactJob(ns, "sh330", *now)
	src.Annotations[AnnotationDerivationClass] = "validated"
	src.Spec.Params["frames"] = "1001"
	if err := k8s.Create(ctx, src); err != nil {
		t.Fatal(err)
	}
	runAll(t, q, now, ns, "sh330", 5*time.Minute, func(int) string { return fx })
	project := "namespace/" + ns
	key, inputs, cost, ok := q.RecomputeCost(project, fx, 0.5)
	if !ok || cost.Class != process.Validated {
		t.Fatalf("record of fx: %+v %v", cost, ok)
	}

	// Its recompute at van writes other bytes.
	q.Placement = &vanPlacement{}
	tick(t, q) // places the pools at van
	if err := q.Recompute(ctx, project, key, inputs, "van", fx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var rc *c4v1.WorkJob
	var l c4v1.WorkJobList
	if err := k8s.List(ctx, &l); err != nil {
		t.Fatal(err)
	}
	for i := range l.Items {
		if j := &l.Items[i]; j.Namespace == ns && j.Labels[LabelRecompute] == "true" {
			rc = j
		}
	}
	if rc == nil || rc.Annotations[AnnotationRecomputeView] != fx || rc.Annotations[AnnotationRecomputeSite] != "van" {
		t.Fatalf("recompute job %+v", rc)
	}
	readyMaterialization(t, ns, rc.Spec.Inputs[0].Materialization, plates)
	runAll(t, q, now, ns, rc.Name, 5*time.Minute, func(int) string { return fxAgain })
	if got := get(t, ns, rc.Name).Annotations[AnnotationRecomputeOutput]; got != fxAgain {
		t.Fatalf("recompute output %q", got)
	}

	// Two consumers of fx: one accepts validated recomputes.
	readyMaterialization(t, ns, "fx", fx)
	consumer := func(name string, accept bool) {
		j := exactJob(ns, name, *now)
		j.Spec.Params["frames"] = "1"
		j.Spec.Inputs = []c4v1.WorkInput{{Materialization: "fx", MountPath: "/fx", ReadOnly: true}}
		if accept {
			j.Annotations[c4v1.AnnotationAcceptValidated] = "true"
		}
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	consumer("comp", true)
	consumer("plain", false)
	// The substitute's Materialization resolves before the unit is placed.
	readyMaterialization(t, ns, subName("comp", 0, "van"), fxAgain)
	tick(t, q)
	mat := func(name string) (string, map[string]string) {
		p := unitPods(t, ns, name)
		if len(p) != 1 {
			t.Fatalf("%s: %d pods", name, len(p))
		}
		for _, v := range p[0].Spec.Volumes {
			if v.Name == "c4-input-0" {
				var read map[string]string
				json.Unmarshal([]byte(p[0].Annotations[AnnotationInputSubstitutes]), &read)
				return v.CSI.VolumeAttributes[c4v1.VolumeAttrMaterialization], read
			}
		}
		t.Fatalf("%s: no input volume", name)
		return "", nil
	}
	if m, read := mat("comp"); m != subName("comp", 0, "van") || read["/fx"] != fxAgain {
		t.Fatalf("comp mounts %s, read %v", m, read)
	}
	if m, read := mat("plain"); m != "fx" || len(read) != 0 {
		t.Fatalf("plain mounts %s, read %v", m, read)
	}
	if seen := q.Placement.(*vanPlacement).seen["/fx"]; len(seen) != 1 {
		t.Fatalf("placement saw %v", seen)
	}

	// It ran on the substitute: so the record says, and status.
	runAll(t, q, now, ns, "comp", 5*time.Minute, func(int) string { return id("comp out") })
	_, in, _, ok := q.RecomputeCost(project, id("comp out"), 0.5)
	if !ok || len(in) != 1 || in[0].View != fxAgain {
		t.Fatalf("comp's record reads %v", in)
	}
	st := get(t, ns, "comp").Status.InputSubstitutes
	if len(st) != 1 || st[0] != (c4v1.WorkInputSubstitute{Mount: "/fx", View: fx, Substitute: fxAgain, Site: "van", Recompute: rc.Name}) {
		t.Fatalf("status %+v", st)
	}
}
