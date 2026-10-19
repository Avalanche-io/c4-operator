package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/node"
)

type fakeDocs struct {
	project string
	id      c4.ID
	data    []byte
}

func (f *fakeDocs) PutBytes(_ context.Context, project string, id c4.ID, data []byte) error {
	f.project, f.id, f.data = project, id, data
	return nil
}

func hostPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "lead-0", UID: types.UID("pod-uid-1"),
			Labels: map[string]string{c4v1.LabelJob: "team"}, Annotations: map[string]string{"c4.avalanche.io/note": `a "quoted" value`}},
		Spec: corev1.PodSpec{NodeName: "host", Containers: []corev1.Container{{Name: "member",
			Env: []corev1.EnvVar{{Name: "C4_AGENT_MEMBER", Value: "lead"}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func hostRunner(t *testing.T, e *env, p *corev1.Pod) (*Runner, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := c4v1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p).WithStatusSubresource(p).Build()
	r := &Runner{Client: c, Harness: e.h, Node: "host",
		seen: map[types.UID]bool{}, asked: map[types.UID]time.Time{}, relays: map[types.UID]*relayState{}}
	return r, c
}

func getPod(t *testing.T, c client.Client) *corev1.Pod {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: "lead-0"}, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// A host unit's safe point goes to its pod as the node daemon would send
// it; the queue's answer comes back to the unit's control directory; the
// pod's annotations reach its podinfo file; and after a stop decision the
// session's end is a yield, not the member finishing.
func TestHostSafepointRelay(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m := e.register("lead", sess)
	r, c := hostRunner(t, e, hostPod())
	p := getPod(t, c)

	u, err := r.prepareUnit(p, m)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := e.h.Store.Get("lead"); got.UnitDir != u.Root {
		t.Fatalf("member's unit dir %q, want %q", got.UnitDir, u.Root)
	}
	at := time.Date(2026, 10, 18, 20, 0, 0, 0, time.UTC)
	sp := "seq 1\nat " + at.Format(time.RFC3339Nano) + "\ncheckpoint " + sess + "#3:c4abc\n"
	if err := os.WriteFile(filepath.Join(u.Out, ".c4", "safepoint"), []byte(sp), 0o644); err != nil {
		t.Fatal(err)
	}
	// the same file reads the same as the node daemon reads it
	ours, ok := readSafepoint(u.Out)
	theirs, ok2 := node.ReadSafepoint(u.Out)
	if !ok || !ok2 || ours.Seq != theirs.Seq || !ours.At.Equal(theirs.At) || ours.Checkpoint != theirs.Checkpoint {
		t.Fatalf("safepoint %+v, node reads %+v", ours, theirs)
	}

	if err := r.relay(ctx, p, u); err != nil {
		t.Fatal(err)
	}
	p = getPod(t, c)
	var note node.SafepointNote
	if json.Unmarshal([]byte(p.Annotations[c4v1.AnnotationSafepoint]), &note) != nil || note.Seq != 1 || note.Checkpoint != sess+"#3:c4abc" {
		t.Fatalf("safepoint annotation %q", p.Annotations[c4v1.AnnotationSafepoint])
	}
	info, _ := os.ReadFile(u.PodInfo)
	if !strings.Contains(string(info), `c4.avalanche.io/note="a \"quoted\" value"`) {
		t.Fatalf("podinfo %q", info)
	}
	if _, err := os.Stat(filepath.Join(u.Control, "safepoint.decision")); err == nil {
		t.Fatal("a decision before the queue answered")
	}

	// the queue answers: stop
	dec, _ := json.Marshal(node.SafepointDecision{Seq: 1, Do: "stop", Why: "capacity: battery low", At: at})
	if err := r.annotate(ctx, p, map[string]string{c4v1.AnnotationSafepointDecision: string(dec)}); err != nil {
		t.Fatal(err)
	}
	if err := r.relay(ctx, getPod(t, c), u); err != nil {
		t.Fatal(err)
	}
	var got node.SafepointDecision
	b, err := os.ReadFile(filepath.Join(u.Control, "safepoint.decision"))
	if err != nil || json.Unmarshal(b, &got) != nil || got.Seq != 1 || got.Do != "stop" {
		t.Fatalf("decision file %s, %v", b, err)
	}

	// the hook then ended the session at that turn: the pod yields
	r.seen[p.UID] = true // not the runner's first look, so not a restart
	if err := r.handle(ctx, getPod(t, c)); err != nil {
		t.Fatal(err)
	}
	p = getPod(t, c)
	if p.Status.Phase != corev1.PodSucceeded || p.Status.Reason != "Yielded" || !strings.Contains(p.Status.Message, "battery low") {
		t.Fatalf("pod %s %s %q", p.Status.Phase, p.Status.Reason, p.Status.Message)
	}
}

// A generator member's expansion document, and a review member's verdict,
// are stored and named on its pod
// when its session ends, as the node daemon does at capture; without a
// store the runner adds nothing.
func TestHostControlDocs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m := e.register("lead", sess)
	r, c := hostRunner(t, e, hostPod())
	p := getPod(t, c)
	u, err := r.prepareUnit(p, m)
	if err != nil {
		t.Fatal(err)
	}
	doc := []byte(`{"version":1,"units":[{"name":"C1.1","exists":1,"planned":true}],"dependents":"all"}`)
	if err := os.MkdirAll(filepath.Join(m.Cwd, ".c4"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Cwd, ".c4", "expand.json"), doc, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.storeControlDocs(ctx, p, m, u); err != nil {
		t.Fatal(err)
	}
	if getPod(t, c).Annotations[c4v1.AnnotationOutputExpand] != "" {
		t.Fatal("no store, yet the pod names a document")
	}

	docs := &fakeDocs{}
	r.Docs, r.DocsProject = docs, "show-a"
	// the unit's own output root wins over the working directory
	inOut := append(bytes.Clone(doc[:len(doc)-1]), []byte(`,"x":1}`)...)
	if err := os.WriteFile(filepath.Join(u.Out, ".c4", "expand.json"), inOut, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.storeControlDocs(ctx, p, m, u); err != nil {
		t.Fatal(err)
	}
	want := c4.Identify(bytes.NewReader(inOut))
	if docs.project != "show-a" || docs.id != want || !bytes.Equal(docs.data, inOut) {
		t.Fatalf("stored %s in %s", docs.id, docs.project)
	}
	if got := getPod(t, c).Annotations[c4v1.AnnotationOutputExpand]; got != want.String() {
		t.Fatalf("output-expand %q, want %s", got, want)
	}

	// a review member's verdict, the same way
	verdict := []byte(`{"version":1,"verdict":"rework","items":[{"units":["C3.2/phase-2"],"do":"rework","why":"counterexample"}]}`)
	if err := os.WriteFile(filepath.Join(u.Out, ".c4", "verdict.json"), verdict, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.storeControlDocs(ctx, p, m, u); err != nil {
		t.Fatal(err)
	}
	vid := c4.Identify(bytes.NewReader(verdict))
	pod := getPod(t, c)
	if pod.Annotations[c4v1.AnnotationOutputVerdict] != vid.String() || pod.Annotations[c4v1.AnnotationOutputExpand] != want.String() {
		t.Fatalf("output-verdict %q, want %s; output-expand %q", pod.Annotations[c4v1.AnnotationOutputVerdict], vid, pod.Annotations[c4v1.AnnotationOutputExpand])
	}
}

// A pod the queue deletes (a disruption) keeps its finalizer until the
// queue records it: the runner stops the member at a turn boundary and
// gives the pod its phase, Yielded with its checkpoint, before deleting it.
func TestHostDeletedPodYieldsFirst(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.register("lead", sess)
	p := hostPod()
	now := metav1.Now()
	p.DeletionTimestamp, p.Finalizers = &now, []string{"c4.avalanche.io/unit"}
	r, c := hostRunner(t, e, p)
	r.seen[p.UID] = true
	if err := r.handle(ctx, getPod(t, c)); err != nil {
		t.Fatal(err)
	}
	got := getPod(t, c)
	if got.Status.Phase != corev1.PodSucceeded || got.Status.Reason != "Yielded" || !strings.Contains(got.Status.Message, "deleted") {
		t.Fatalf("pod %s %s %q", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
}
