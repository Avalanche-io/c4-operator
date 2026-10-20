package queue

import (
	"context"
	"fmt"
	"k8s.io/utils/ptr"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/order"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

var k8s client.Client

// The queue runs against a real API server (envtest). There is no kubelet,
// so the tests play its part: they move unit pods to Succeeded or Failed.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob("../../../bin/k8s/k8s/*")
		if len(matches) == 0 {
			println("envtest binaries not found; run `make envtest`")
			os.Exit(0)
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		os.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}
	code := m.Run()
	env.Stop()
	os.Exit(code)
}

// farm makes nodes labeled for one pool and the Pool itself, in a fresh
// namespace, and returns the namespace and a queue on a manual clock.
// farmSeq numbers the farms the package's tests make.
var farmSeq atomic.Int64

func farm(t *testing.T, pool string, nodes, slots int) (string, *Queue, *time.Time) {
	t.Helper()
	ctx := context.Background()
	// a counter, not the clock alone: two farms made a whole number of
	// seconds apart got one name
	ns := fmt.Sprintf("q-%d-%d", farmSeq.Add(1), time.Now().UnixNano()%1e6)
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	label := ns + "-" + pool
	for i := 0; i < nodes; i++ {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", label, i), Labels: map[string]string{"pool": label}}}
		if err := k8s.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		if err := k8s.Status().Update(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	p := &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: pool + "-" + ns}, Spec: c4v1.PoolSpec{
		NodeSelector: map[string]string{"pool": label}, SlotsPerNode: int32(slots), CostPerSlotHour: "0.40"}}
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	q := &Queue{Client: k8s, Policy: sched.Decide, Cfg: sched.DefaultConfig(), Log: zap.New(zap.WriteTo(os.Stderr)),
		Now: func() time.Time { return now }, CaptureWait: time.Minute}
	t.Cleanup(func() {
		k8s.Delete(ctx, p)
		for i := 0; i < nodes; i++ {
			k8s.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", label, i)}})
		}
	})
	return ns, q, &now
}

func job(ns, name, class, pool string, params map[string]string, deadline time.Time) *c4v1.WorkJob {
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: c4v1.WorkJobSpec{
		Class: class, Params: params, Pools: []string{pool}, Retries: ptr.To[int32](1),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "work", Image: "busybox"}}}}}}
	if !deadline.IsZero() {
		j.Spec.Deadline = &metav1.Time{Time: deadline}
	}
	return j
}

func tick(t *testing.T, q *Queue) {
	t.Helper()
	if err := q.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func unitPods(t *testing.T, ns, name string) []corev1.Pod {
	t.Helper()
	var l corev1.PodList
	if err := k8s.List(context.Background(), &l, client.InNamespace(ns), client.MatchingLabels{c4v1.LabelJob: name}); err != nil {
		t.Fatal(err)
	}
	var out []corev1.Pod
	for _, p := range l.Items {
		if p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(a, b int) bool { return unitOf(out[a]) < unitOf(out[b]) })
	return out
}

func unitOf(p corev1.Pod) int { n, _ := strconv.Atoi(p.Labels[c4v1.LabelUnit]); return n }

// finish plays kubelet: the pod ran for d and ended in phase.
func finish(t *testing.T, p *corev1.Pod, phase corev1.PodPhase, end time.Time, d time.Duration, annotations map[string]string) {
	t.Helper()
	ctx := context.Background()
	if len(annotations) > 0 {
		patch := client.MergeFrom(p.DeepCopy())
		for k, v := range annotations {
			p.Annotations[k] = v
		}
		if err := k8s.Patch(ctx, p, patch); err != nil {
			t.Fatal(err)
		}
	}
	code := int32(0)
	if phase == corev1.PodFailed {
		code = 1
	}
	p.Status.Phase = phase
	p.Status.StartTime = &metav1.Time{Time: end.Add(-d)}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "work", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, StartedAt: metav1.Time{Time: end.Add(-d)}, FinishedAt: metav1.Time{Time: end}}}}}
	if err := k8s.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, ns, name string) *c4v1.WorkJob {
	t.Helper()
	var j c4v1.WorkJob
	if err := k8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &j); err != nil {
		t.Fatal(err)
	}
	return &j
}

func TestRenderReleasesInBinaryOrderUpToSlots(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	j := job(ns, "sh010", "render", "cpu-"+ns, map[string]string{"frames": "1001-1008", "estimateSeconds": "60"}, now.Add(time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "sh010")
	if len(pods) != 4 {
		t.Fatalf("released %d units into 4 slots", len(pods))
	}
	want := append([]int(nil), order.BinaryOrder(8)[:4]...)
	sort.Ints(want)
	for i, p := range pods {
		if unitOf(p) != want[i] {
			t.Fatalf("released units %v, binary order starts %v", unitsOf(pods), want)
		}
		c := p.Spec.Containers[0]
		if q := c.Resources.Requests[SlotResource]; q.Value() != 1 {
			t.Fatalf("pod requests %v slots", q.Value())
		}
		if p.Spec.NodeSelector["pool"] != ns+"-cpu" || p.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Fatalf("pod placement: %v %v", p.Spec.NodeSelector, p.Spec.RestartPolicy)
		}
		if envOf(c, "C4_FRAME_START") != strconv.Itoa(1001+unitOf(p)) {
			t.Fatalf("unit %d has C4_FRAME_START=%s", unitOf(p), envOf(c, "C4_FRAME_START"))
		}
		if len(p.OwnerReferences) != 1 || p.OwnerReferences[0].UID != j.UID {
			t.Fatal("unit pod is not owned by its job")
		}
	}
	// Two units finish; their slots are refilled, and status records them.
	*now = now.Add(70 * time.Second)
	finish(t, &pods[0], corev1.PodSucceeded, *now, 60*time.Second, nil)
	finish(t, &pods[1], corev1.PodSucceeded, *now, 50*time.Second, nil)
	tick(t, q)
	st := get(t, ns, "sh010").Status
	if st.Units.Succeeded != 2 || st.UnitMillis[unitOf(pods[0])] != 60000 || st.UnitMillis[unitOf(pods[1])] != 50000 {
		t.Fatalf("status after two units: %+v %v", st.Units, st.UnitMillis)
	}
	if st.Phase != "Running" || st.Order != "binary" || st.Estimate == nil || st.Deadline == nil {
		t.Fatalf("status %+v", st)
	}
	tick(t, q)
	if n := len(unitPods(t, ns, "sh010")); n != 4 {
		t.Fatalf("%d units running after two finished, want 4", n)
	}
}

func TestFailuresRetryThenFail(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 1)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "ci-1", "ci", "cpu-"+ns, map[string]string{"shards": "1"}, time.Time{})); err != nil {
		t.Fatal(err)
	}
	// Retries is 1: two attempts, then the unit fails.
	for attempt := 1; attempt <= 2; attempt++ {
		tick(t, q)
		pods := unitPods(t, ns, "ci-1")
		if len(pods) != 1 || pods[0].Annotations[c4v1.AnnotationAttempt] != strconv.Itoa(attempt) {
			t.Fatalf("attempt %d: pods %v", attempt, unitsOf(pods))
		}
		*now = now.Add(time.Minute)
		finish(t, &pods[0], corev1.PodFailed, *now, 30*time.Second, nil)
		tick(t, q)
	}
	tick(t, q)
	st := get(t, ns, "ci-1").Status
	if st.Phase != "Failed" || st.Units.Failed != 1 || len(unitPods(t, ns, "ci-1")) != 0 {
		t.Fatalf("after two failures: phase %s, units %+v", st.Phase, st.Units)
	}
}

func TestHoldReleasesNothing(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	j := job(ns, "held", "render", "cpu-"+ns, map[string]string{"frames": "1-4"}, now.Add(time.Hour))
	j.Spec.Nudge.Hold = true
	if err := k8s.Create(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	tick(t, q)
	if n := len(unitPods(t, ns, "held")); n != 0 {
		t.Fatalf("a held job released %d units", n)
	}
	if st := get(t, ns, "held").Status; st.Phase != "Held" || st.Units.Held != 4 {
		t.Fatalf("held job status %s %+v", st.Phase, st.Units)
	}
}

func TestYieldedUnitResumesFromItsCapturedView(t *testing.T) {
	ns, q, now := farm(t, "gpu", 1, 2)
	ctx := context.Background()
	j := job(ns, "train", "ml", "gpu-"+ns, map[string]string{"ranks": "2", "steps": "100"}, now.Add(24*time.Hour))
	j.Spec.Inputs = []c4v1.WorkInput{{Materialization: "data", MountPath: "/data"}}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "train")
	if len(pods) != 1 {
		t.Fatalf("released %d pods for one gang", len(pods))
	}
	if slots := pods[0].Spec.Containers[0].Resources.Requests[SlotResource]; slots.Value() != 2 {
		t.Fatalf("the gang was not released as one 2-slot unit: %v", unitsOf(pods))
	}
	if envOf(pods[0].Spec.Containers[0], "C4_PODINFO") == "" {
		t.Fatal("a checkpointing unit has no downward API file to see a yield request")
	}
	if e := envVar(pods[0].Spec.Containers[0], "C4_NODE"); e == nil || e.ValueFrom == nil || e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != "spec.nodeName" {
		t.Fatalf("a unit is not told its node through the downward API: %+v", e)
	}
	// The unit yields: it saved a checkpoint, and capture made a view.
	const saved = "c45ZQpdqHMDeGu6tLjk8jmz9XJ2E8Bw7D5Kyxv4Kx3j8bWDE5WgtQ6vqvPXR4ofJXWhSWMLWHmNHx1QgGWy2Bgp6aR"
	*now = now.Add(time.Minute)
	finish(t, &pods[0], corev1.PodSucceeded, *now, 50*time.Second, map[string]string{
		c4v1.AnnotationYield: "reclaim-nominal", c4v1.AnnotationOutputTree: saved})
	tick(t, q)
	st := get(t, ns, "train").Status
	if st.Resume["0"] != saved || len(st.Yields) != 1 || st.Units.Succeeded != 0 {
		t.Fatalf("after the yield: resume %v, yields %v, units %+v", st.Resume, st.Yields, st.Units)
	}
	// The next attempt waits for the resume Materialization, which mounts
	// the captured view.
	var m c4v1.Materialization
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "train-resume-u0"}, &m); err != nil || m.Spec.Source.ID != saved {
		t.Fatalf("resume materialization: %v %+v", err, m.Spec)
	}
	if n := len(unitPods(t, ns, "train")); n != 0 {
		t.Fatalf("relaunched before the resume view was ready: %d pods", n)
	}
	m.Status.Phase, m.Status.ObservedGeneration = c4v1.PhaseReady, m.Generation
	if err := k8s.Status().Update(ctx, &m); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods = unitPods(t, ns, "train")
	if len(pods) != 1 {
		t.Fatalf("not relaunched: %v", unitsOf(pods))
	}
	vol := pods[0].Spec.Volumes
	found := false
	for _, v := range vol {
		if v.CSI != nil && v.CSI.VolumeAttributes[c4v1.VolumeAttrMaterialization] == "train-resume-u0" {
			found = true
		}
	}
	if !found || envOf(pods[0].Spec.Containers[0], "C4_RESUME_VIEW") != saved || pods[0].Annotations[c4v1.AnnotationAttempt] != "1" {
		t.Fatalf("the relaunch does not mount the captured view, or counted the yield as a failure: %+v", pods[0].Annotations)
	}
	// A restarted queue keeps the record of the yield.
	q2 := &Queue{Client: k8s, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait}
	*now = now.Add(20 * time.Second)
	tick(t, q2)
	st = get(t, ns, "train").Status
	if len(st.Yields) != 1 || st.Yields[0].View != saved || st.Resume["0"] != saved {
		t.Fatalf("after a restart: yields %v, resume %v", st.Yields, st.Resume)
	}
}

func TestUnitWiderThanAnyNodeIsReported(t *testing.T) {
	ns, q, now := farm(t, "gpu", 2, 2)
	if err := k8s.Create(context.Background(), job(ns, "wide", "ml", "gpu-"+ns, map[string]string{"ranks": "4"}, now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	j := get(t, ns, "wide")
	ok := false
	for _, c := range j.Status.Conditions {
		if c.Type == "Schedulable" && c.Status == metav1.ConditionFalse {
			ok = true
		}
	}
	if !ok || len(unitPods(t, ns, "wide")) != 0 {
		t.Fatalf("a 4-slot unit on 2-slot nodes: %+v", j.Status.Conditions)
	}
}

func unitsOf(pods []corev1.Pod) []int {
	var out []int
	for _, p := range pods {
		out = append(out, unitOf(p))
	}
	return out
}

func envOf(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func envVar(c corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

// C4_OUTPUT names the first writable input; it is absent when every input
// is read-only.
func TestOutputEnv(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	ctx := context.Background()
	rw := job(ns, "sh050", "render", "cpu-"+ns, map[string]string{"frames": "1-1", "estimateSeconds": "60"}, now.Add(time.Hour))
	rw.Spec.Inputs = []c4v1.WorkInput{{Materialization: "plates", MountPath: "/plates", ReadOnly: true},
		{Materialization: "work", MountPath: "/work"}, {Materialization: "more", MountPath: "/more"}}
	ro := job(ns, "sh051", "render", "cpu-"+ns, map[string]string{"frames": "1-1", "estimateSeconds": "60"}, now.Add(time.Hour))
	ro.Spec.Inputs = []c4v1.WorkInput{{Materialization: "plates", MountPath: "/plates", ReadOnly: true}}
	for _, j := range []*c4v1.WorkJob{rw, ro} {
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, q)
	for name, want := range map[string]string{"sh050": "/work", "sh051": ""} {
		pods := unitPods(t, ns, name)
		if len(pods) != 1 {
			t.Fatalf("%s: %d pods", name, len(pods))
		}
		if got := envOf(pods[0].Spec.Containers[0], "C4_OUTPUT"); got != want {
			t.Errorf("%s: C4_OUTPUT %q, want %q", name, got, want)
		}
	}
}

// A dependent's deadline reaches the job it waits on: the queue passes
// DependsOn to the policy as After, sched plans the upstream job for the
// dependent's deadline less the dependent's duration, and the waiting
// job starts no earlier than its upstream job's planned finish.
func TestDependsOnPropagatesDeadline(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	ctx := context.Background()
	var dec sched.Decisions
	q.Policy = func(s sched.Snapshot, st *sched.State, cfg sched.Config) sched.Decisions {
		dec = sched.Decide(s, st, cfg)
		return dec
	}
	params := map[string]string{"frames": "1-4", "estimateSeconds": "600"}
	// light's own deadline, after comp's, is what the dependency must move.
	due := now.Add(3 * time.Hour)
	light := job(ns, "light", "render", "cpu-"+ns, params, now.Add(5*time.Hour))
	comp := job(ns, "comp", "render", "cpu-"+ns, params, due)
	comp.Spec.DependsOn = []string{"light"}
	for _, j := range []*c4v1.WorkJob{light, comp} {
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, q)
	tick(t, q)
	ls, cs := dec.Jobs[ns+"/light"], dec.Jobs[ns+"/comp"]
	if ls.DeadlineFor != ns+"/comp" || !ls.Deadline.Before(due) {
		t.Fatalf("light planned for %v (for %q), want before comp's %v", ls.Deadline, ls.DeadlineFor, due)
	}
	if cs.StartAfter.IsZero() || !cs.StartAfter.Equal(ls.ETA90) {
		t.Fatalf("comp starts after %v, light's ETA90 is %v", cs.StartAfter, ls.ETA90)
	}
	if n := len(unitPods(t, ns, "comp")); n != 0 {
		t.Fatalf("comp released %d units before light succeeded", n)
	}
}

// A node's slot-limit label lowers what it offers (a host runner under a
// power cap), never raises it, and 0 offers nothing.
func TestNodeSlotLimit(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	setLimit := func(node, v string) {
		t.Helper()
		var n corev1.Node
		if err := k8s.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
			t.Fatal(err)
		}
		patch := client.MergeFrom(n.DeepCopy())
		if v == "" {
			delete(n.Labels, c4v1.LabelSlotLimit)
		} else {
			n.Labels[c4v1.LabelSlotLimit] = v
		}
		if err := k8s.Patch(ctx, &n, patch); err != nil {
			t.Fatal(err)
		}
	}
	n0, n1 := ns+"-cpu-0", ns+"-cpu-1"
	setLimit(n0, "1")
	setLimit(n1, "9") // above the pool's shape: no effect
	j := job(ns, "lim", "render", "cpu-"+ns, map[string]string{"frames": "1-8", "estimateSeconds": "60"}, now.Add(time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if n := len(unitPods(t, ns, "lim")); n != 3 {
		t.Fatalf("released %d units, want 3 (1 + 2 slots)", n)
	}
	var p c4v1.Pool
	if err := k8s.Get(ctx, types.NamespacedName{Name: "cpu-" + ns}, &p); err != nil {
		t.Fatal(err)
	}
	if p.Status.Slots != 3 {
		t.Fatalf("pool slots %d, want 3", p.Status.Slots)
	}
	var node corev1.Node
	k8s.Get(ctx, types.NamespacedName{Name: n0}, &node)
	if q := node.Status.Capacity[SlotResource]; q.Value() != 1 {
		t.Fatalf("node %s advertises %v slots, want 1", n0, q.Value())
	}
	setLimit(n0, "0")
	setLimit(n1, "0")
	tick(t, q)
	k8s.Get(ctx, types.NamespacedName{Name: "cpu-" + ns}, &p)
	if p.Status.Slots != 0 {
		t.Fatalf("pool slots %d after both limits went to 0", p.Status.Slots)
	}
}

// A unit pod holds no Kubernetes API token, whatever its template says,
// and knows its namespace from its environment.
func TestUnitHoldsNoToken(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	j := job(ns, "notoken", "render", "cpu-"+ns, map[string]string{"frames": "1-1"}, now.Add(time.Hour))
	yes := true
	j.Spec.Template.Spec.AutomountServiceAccountToken = &yes
	if err := k8s.Create(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "notoken")
	if len(pods) != 1 {
		t.Fatalf("%d pods", len(pods))
	}
	p := pods[0]
	if a := p.Spec.AutomountServiceAccountToken; a == nil || *a {
		t.Fatalf("automountServiceAccountToken %v", a)
	}
	if envOf(p.Spec.Containers[0], "C4_NAMESPACE") != ns {
		t.Fatalf("C4_NAMESPACE %q", envOf(p.Spec.Containers[0], "C4_NAMESPACE"))
	}
}
