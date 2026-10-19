package queue

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// reviewJob starts a graph job whose root expands into the units of
// units (name:label) and a review unit, named review, after all of them,
// and runs the root.
func reviewJob(t *testing.T, name string, units []string) (string, *Queue, *time.Time, func(string) string) {
	t.Helper()
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, name, "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	var docUnits, names []string
	for _, u := range units {
		n, label, _ := strings.Cut(u, ":")
		docUnits = append(docUnits, fmt.Sprintf(`{"name": %q, "label": %q}`, n, label))
		names = append(names, fmt.Sprintf("%q", n))
	}
	docUnits = append(docUnits, fmt.Sprintf(`{"name": "review", "after": [%s]}`, strings.Join(names, ", ")))
	doc := putDoc(t, st, `{"version": 1, "units": [`+strings.Join(docUnits, ", ")+`]}`)
	finish(t, podOfUnit(t, ns, name, 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	return ns, q, now, func(doc string) string { return putDoc(t, st, doc) }
}

// finishUnit ends unit idx's running pod successfully with annotations.
func finishUnit(t *testing.T, now *time.Time, ns, job string, idx int, ann map[string]string) {
	t.Helper()
	if ann == nil {
		ann = map[string]string{}
	}
	if ann[c4v1.AnnotationOutputTree] == "" {
		ann[c4v1.AnnotationOutputTree] = viewID(fmt.Sprintf("%s-%d-%s", job, idx, now))
	}
	finish(t, podOfUnit(t, ns, job, idx), corev1.PodSucceeded, *now, time.Minute, ann)
}

func review(t *testing.T, ns, name string, idx int) c4v1.WorkReview {
	t.Helper()
	r, ok := get(t, ns, name).Status.Reviews[fmt.Sprint(idx)]
	if !ok {
		t.Fatalf("no review of unit %d", idx)
	}
	return r
}

// Render QC, the lead example (PRIMITIVES section 5): a QC unit after four
// frames reworks two (one on other nodes with more memory), escalates one
// and accepts one with a note. The reworked frames run again with the
// verdict's changes, the QC unit runs again after them without waiting on
// the frame it escalated, and a frame reworked past maxPasses is
// escalated instead. The second verdict adds a paint-fix unit.
func TestVerdictRenderQC(t *testing.T) {
	ns, q, now, put := reviewJob(t, "qc1", []string{"f1001:1001", "f1002:1002", "f1003:1003", "f1004:1004"})
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	q.Signer, err = provenance.NewSigner(context.Background(), provenance.LocalIssuer(ca, provenance.ComponentIdentity("queue"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		finishUnit(t, now, ns, "qc1", i, nil)
	}
	tick(t, q)
	v1 := put(`{"version": 1, "verdict": "mixed", "maxPasses": 1, "onExhausted": "escalate:wrangler", "items": [
		{"units": ["1001"], "do": "rework", "why": "NaN in beauty, 312 px", "with": {"avoidNodes": ["node-17"], "memory": "96Gi"}},
		{"units": ["f1002"], "do": "rework", "why": "black frame"},
		{"units": ["1003"], "do": "escalate", "to": "lighting-lead", "why": "flicker against 1002 and 1004"},
		{"units": ["1004"], "do": "accept", "note": "firefly in the defocused background, paint in comp"}]}`)
	finishUnit(t, now, ns, "qc1", 5, map[string]string{c4v1.AnnotationOutputVerdict: v1})
	tick(t, q)
	got := get(t, ns, "qc1")
	if len(got.Status.Verdicts) != 1 || got.Status.Verdicts[0].Reworked != 2 || got.Status.Verdicts[0].Escalated != 1 || got.Status.Verdicts[0].Accepted != 1 {
		t.Fatalf("verdict %+v", got.Status.Verdicts)
	}
	if !slices.Equal(states(t, got, "flagged"), []int{3}) || !slices.Equal(states(t, got, "blocked"), []int{5}) {
		t.Fatalf("states %v", got.Status.UnitStates)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson)
	if c == nil || c.Reason != c4v1.NeedsReview || !strings.Contains(c.Message, "lighting-lead") {
		t.Fatalf("needs person %+v", c)
	}
	if r := review(t, ns, "qc1", 4); r.Do != "accept" || !strings.Contains(r.Note, "firefly") {
		t.Fatalf("accepted %+v", r)
	}
	f1001 := podOfUnit(t, ns, "qc1", 1)
	mem := f1001.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	aff := f1001.Spec.Affinity
	if mem.String() != "96Gi" || aff == nil || !strings.Contains(fmt.Sprint(aff.NodeAffinity), "node-17") {
		t.Fatalf("rework of 1001: memory %s affinity %v", mem.String(), aff)
	}
	podOfUnit(t, ns, "qc1", 2) // 1002 runs again, unchanged

	// The reworks finish; QC runs again and reworks 1001 a second time:
	// past maxPasses, so it is escalated to the wrangler. It also adds a
	// paint-fix unit for 1004.
	*now = now.Add(2 * time.Minute)
	finishUnit(t, now, ns, "qc1", 1, nil)
	finishUnit(t, now, ns, "qc1", 2, nil)
	tick(t, q)
	v2 := put(`{"version": 1, "verdict": "mixed", "maxPasses": 1, "onExhausted": "escalate:wrangler", "items": [
		{"units": ["1001"], "do": "rework", "why": "NaN again"},
		{"do": "expand", "expand": {"version": 1, "units": [{"name": "paint-1004", "label": "paint 1004"}]}}]}`)
	finishUnit(t, now, ns, "qc1", 5, map[string]string{c4v1.AnnotationOutputVerdict: v2})
	tick(t, q)
	got = get(t, ns, "qc1")
	if len(got.Status.Verdicts) != 2 || got.Status.Verdicts[1].Escalated != 1 || got.Status.Verdicts[1].Expanded != 1 {
		t.Fatalf("second verdict %+v", got.Status.Verdicts)
	}
	if got.Status.Verdicts[0].Record == "" || got.Status.Verdicts[1].Record == "" || got.Status.Expansions[len(got.Status.Expansions)-1].Record == "" {
		t.Fatalf("unsigned: %+v %+v", got.Status.Verdicts, got.Status.Expansions)
	}
	if r := review(t, ns, "qc1", 1); r.Do != "escalate" || r.To != "wrangler" || r.Passes != 1 || !strings.Contains(r.Why, "reworked 1 times") {
		t.Fatalf("exhausted %+v", r)
	}
	if r := review(t, ns, "qc1", 2); r.Do != "pass" || r.Passes != 1 {
		t.Fatalf("1002 after its rework passed: %+v", r)
	}
	if !slices.Equal(states(t, got, "flagged"), []int{1, 3}) || !slices.Contains(states(t, got, "succeeded"), 5) || got.Status.Units.Total != 7 {
		t.Fatalf("after the second verdict: %v total %d", got.Status.UnitStates, got.Status.Units.Total)
	}
	// The verdict of a pod is carried out once, even after a restart.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	if n := len(get(t, ns, "qc1").Status.Verdicts); n != 2 {
		t.Fatalf("%d verdicts after a restart", n)
	}
}

// A CI flaky rerun: the review reruns a failed-then-passed shard with the
// same inputs; on the second pass it is green.
func TestVerdictFlakyRerun(t *testing.T) {
	ns, q, now, put := reviewJob(t, "ci1", []string{"s0:shard 0", "s1:shard 1"})
	finishUnit(t, now, ns, "ci1", 1, nil)
	finishUnit(t, now, ns, "ci1", 2, nil)
	tick(t, q)
	finishUnit(t, now, ns, "ci1", 3, map[string]string{c4v1.AnnotationOutputVerdict: put(`{"version": 1, "verdict": "flaky",
		"items": [{"units": ["shard 1"], "do": "rework", "why": "failed, then passed on the same input view"}]}`)})
	tick(t, q)
	finishUnit(t, now, ns, "ci1", 2, nil)
	tick(t, q)
	finishUnit(t, now, ns, "ci1", 3, map[string]string{c4v1.AnnotationOutputVerdict: put(`{"version": 1, "verdict": "pass"}`)})
	tick(t, q)
	got := get(t, ns, "ci1")
	if got.Status.Phase != "Succeeded" || len(got.Status.Verdicts) != 2 || got.Status.Verdicts[1].Passed != 2 {
		t.Fatalf("%s %+v", got.Status.Phase, got.Status.Verdicts)
	}
}

// An agent refutation (Ra): the refuter sends a lane back with its
// counterexample as an input; the third send-back becomes the lead's.
func TestVerdictRefutation(t *testing.T) {
	ns, q, now, put := reviewJob(t, "ra1", []string{"C3.2:C3.2"})
	ctx := context.Background()
	cx := viewID("counterexample")
	refute := put(fmt.Sprintf(`{"version": 1, "verdict": "rework", "onExhausted": "escalate:lead", "items": [
		{"units": ["C3.2"], "do": "rework", "why": "the claim fails on the counterexample",
		 "with": {"inputs": [{"view": %q, "mountPath": "/counterexample"}], "env": {"REFUTED": "1"}}}]}`, cx))
	finishUnit(t, now, ns, "ra1", 1, nil)
	tick(t, q)
	for pass := 1; pass <= 3; pass++ {
		*now = now.Add(time.Minute)
		finishUnit(t, now, ns, "ra1", 2, map[string]string{c4v1.AnnotationOutputVerdict: refute})
		tick(t, q)
		r := review(t, ns, "ra1", 1)
		if pass == 3 {
			if r.Do != "escalate" || r.To != "lead" || r.Passes != 2 {
				t.Fatalf("third send-back %+v", r)
			}
			break
		}
		if r.Do != "rework" || int(r.Passes) != pass {
			t.Fatalf("pass %d: %+v", pass, r)
		}
		// The lane mounts the counterexample once it is ready.
		var m c4v1.Materialization
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: reworkInputName("ra1", 1, 0)}, &m); err != nil || m.Spec.Source.ID != cx {
			t.Fatalf("counterexample %+v %v", m.Spec, err)
		}
		m.Status.Phase, m.Status.ObservedGeneration = c4v1.PhaseReady, m.Generation
		if err := k8s.Status().Update(ctx, &m); err != nil {
			t.Fatal(err)
		}
		tick(t, q)
		lane := podOfUnit(t, ns, "ra1", 1)
		var mounted, env bool
		for _, vm := range lane.Spec.Containers[0].VolumeMounts {
			mounted = mounted || vm.MountPath == "/counterexample"
		}
		for _, e := range lane.Spec.Containers[0].Env {
			env = env || (e.Name == "REFUTED" && e.Value == "1")
		}
		if !mounted || !env {
			t.Fatalf("pass %d: the lane's pod lacks the counterexample (mounted %v, env %v)", pass, mounted, env)
		}
		finishUnit(t, now, ns, "ra1", 1, nil)
		tick(t, q)
	}
	c := meta.FindStatusCondition(get(t, ns, "ra1").Status.Conditions, c4v1.ConditionNeedsPerson)
	if c == nil || c.Reason != c4v1.NeedsReview || !strings.Contains(c.Message, "lead") {
		t.Fatalf("needs person %+v", c)
	}
}

// A verdict on a unit the review does not run after is refused whole.
func TestVerdictRefused(t *testing.T) {
	ns, q, now, put := reviewJob(t, "bad1", []string{"a:a"})
	finishUnit(t, now, ns, "bad1", 1, nil)
	tick(t, q)
	finishUnit(t, now, ns, "bad1", 2, map[string]string{c4v1.AnnotationOutputVerdict: put(`{"version": 1, "items": [
		{"units": ["a"], "do": "accept"}, {"units": ["plan"], "do": "rework"}]}`)})
	tick(t, q)
	got := get(t, ns, "bad1")
	if len(got.Status.Verdicts) != 1 || !strings.Contains(got.Status.Verdicts[0].Error, "plan") || !slices.Equal(states(t, got, "failed"), []int{2}) || len(got.Status.Reviews) != 0 {
		t.Fatalf("%+v %v %v", got.Status.Verdicts, got.Status.UnitStates, got.Status.Reviews)
	}
}
