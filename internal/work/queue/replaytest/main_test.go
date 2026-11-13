// Package replaytest replays a recorded run through the work queue against a
// real API server (envtest). It is its own package so that `go test ./...`
// runs it beside the queue package rather than inside it: the replay makes
// some 7,600 API calls, and its time follows the API server's latency under
// load (from about a minute to seven), which made it the long pole of the
// queue package and of its timeout.
//
// The helpers below are the queue package's own (queue_test.go,
// states_test.go, graph_test.go), copied: that package's tests are in
// package queue, and a helper package they shared with this one would import
// queue and so form an import cycle. They play the kubelet's part, as there
// the tests do.
package replaytest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

var k8s client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob("../../../../bin/k8s/k8s/*")
		if len(matches) == 0 {
			println("envtest binaries not found; run `make envtest`")
			os.Exit(0)
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		os.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
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
var farmSeq atomic.Int64

func farm(t *testing.T, pool string, nodes, slots int) (string, *queue.Queue, *time.Time) {
	t.Helper()
	ctx := context.Background()
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
	q := &queue.Queue{Client: k8s, Policy: sched.Decide, Cfg: sched.DefaultConfig(), Log: zap.New(zap.WriteTo(os.Stderr)),
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

func tick(t *testing.T, q *queue.Queue) {
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

// run plays kubelet starting a pod's work.
func run(t *testing.T, p *corev1.Pod, at time.Time) {
	t.Helper()
	p.Status.Phase = corev1.PodRunning
	p.Status.StartTime = &metav1.Time{Time: at}
	if err := k8s.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

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

func putDoc(t *testing.T, st *storeurl.Sharded, doc string) string {
	t.Helper()
	id, err := st.PutBytes(context.Background(), []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func viewID(s string) string { return c4.Identify(strings.NewReader(s)).String() }
