package agent_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/agent"
	"github.com/Avalanche-io/c4-operator/internal/controller"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

var k8s client.Client

// The end-to-end test runs the queue, the bundle controller and the host
// runner against a real API server (envtest). Claude Code is a fake: no
// test touches a real session.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob("../../bin/k8s/k8s/*")
		if len(matches) == 0 {
			os.Exit(m.Run()) // unit tests only; the bundle test skips
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		os.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}
	code := m.Run()
	env.Stop()
	os.Exit(code)
}

// fake is the claude CLI: sessions it has resumed are running and idle.
type fake struct {
	mu      sync.Mutex
	active  map[string]bool
	resumes []string
	stops   []string
}

func (f *fake) Active(ctx context.Context, configDir string, all bool) ([]agent.Active, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []agent.Active
	for s := range f.active {
		out = append(out, agent.Active{SessionID: s, Status: "idle", Pid: 1})
	}
	return out, nil
}

func (f *fake) Resume(ctx context.Context, configDir, dir, session string, flags []string, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active[session] = true
	f.resumes = append(f.resumes, session)
	return agent.ShortID(session), nil
}

func (f *fake) Stop(ctx context.Context, configDir, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, session)
	f.stops = append(f.stops, session)
	return nil
}

func (f *fake) running() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.active) }

// TestBundleOnHostRunner: an AgentBundle of three members, at most two
// running, on one machine's host runner. The queue releases two, the
// runner resumes their sessions; suspending the bundle yields both at a
// turn boundary; clearing it resumes them from their checkpoints; a restart
// of the machine stops every session and a fresh runner resumes them.
func TestBundleOnHostRunner(t *testing.T) {
	if k8s == nil {
		t.Skip("no envtest binaries (make envtest)")
	}
	ctx := context.Background()
	ns := fmt.Sprintf("ab-%d", time.Now().UnixNano()%1e9)
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	host := ns + "-abyss"
	sel := map[string]string{"c4.avalanche.io/host": host}
	pool := &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + ns}, Spec: c4v1.PoolSpec{
		NodeSelector: sel, SlotsPerNode: 2,
		Tolerations: []corev1.Toleration{{Key: agent.TaintHostRunner, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}}}
	if err := k8s.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}

	// The machine: three registered members with transcripts.
	root := t.TempDir()
	cfg, work := filepath.Join(root, "claude"), filepath.Join(root, "work")
	os.MkdirAll(work, 0o755)
	fk := &fake{active: map[string]bool{}}
	h := agent.New(&agent.Store{Dir: filepath.Join(root, "agents")}, fk)
	h.Workspace, h.Poll, h.SettleQuiet, h.SettleMax = "none", time.Millisecond, time.Millisecond, 5*time.Millisecond
	members := []string{"lead", "reviewer", "lane-1"}
	sessions := map[string]string{}
	for i, name := range members {
		s := fmt.Sprintf("%08d-0000-0000-0000-%012d", i+1, i+1)
		sessions[name] = s
		tr := filepath.Join(cfg, "projects", agent.ProjectSlug(work), s+".jsonl")
		os.MkdirAll(filepath.Dir(tr), 0o755)
		os.WriteFile(tr, []byte(`{"turn":1}`+"\n"), 0o644)
		if _, err := h.Register(ctx, agent.RegisterOptions{Name: name, Session: s, Cwd: work, ConfigDir: cfg}); err != nil {
			t.Fatal(err)
		}
	}
	runner := &agent.Runner{Client: k8s, Harness: h, Node: host, Labels: sel, Slots: 2, YieldTimeout: time.Second, Logf: t.Logf}
	t.Cleanup(func() {
		k8s.Delete(ctx, pool)
		k8s.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: host}})
	})

	now := time.Date(2026, 10, 18, 22, 0, 0, 0, time.UTC)
	q := &queue.Queue{Client: k8s, Policy: sched.Decide, Cfg: sched.DefaultConfig(), Log: zap.New(zap.WriteTo(os.Stderr)),
		Now: func() time.Time { return now }, CaptureWait: time.Minute}
	rec := &controller.AgentBundleReconciler{Client: k8s}

	b := &c4v1.AgentBundle{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "team"}, Spec: c4v1.AgentBundleSpec{
		MaxRunning: 2, Pools: []string{pool.Name},
		Deadline: &metav1.Time{Time: now.Add(12 * time.Hour)}}}
	for _, m := range members {
		b.Spec.Members = append(b.Spec.Members, c4v1.AgentMemberSpec{Name: m})
	}
	if err := k8s.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: ns, Name: "team"}
	reconcile := func() *c4v1.AgentBundle {
		t.Helper()
		if _, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		var got c4v1.AgentBundle
		if err := k8s.Get(ctx, key, &got); err != nil {
			t.Fatal(err)
		}
		return &got
	}
	round := func(r *agent.Runner) {
		t.Helper()
		for i := 0; i < 3; i++ {
			if err := r.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if err := q.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			now = now.Add(5 * time.Second)
		}
	}
	livePods := func() []corev1.Pod {
		t.Helper()
		var l corev1.PodList
		if err := k8s.List(ctx, &l, client.InNamespace(ns), client.MatchingLabels{c4v1.LabelJob: "team"}); err != nil {
			t.Fatal(err)
		}
		var out []corev1.Pod
		for _, p := range l.Items {
			if p.DeletionTimestamp == nil && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				out = append(out, p)
			}
		}
		return out
	}

	reconcile()
	round(runner)
	pods := livePods()
	if len(pods) != 2 || fk.running() != 2 {
		t.Fatalf("after release: %d pods, %d sessions running; want 2 and 2", len(pods), fk.running())
	}
	for _, p := range pods {
		if p.Status.Phase != corev1.PodRunning || p.Spec.NodeName != host || p.Annotations[c4v1.AnnotationOutputEpoch] != "1" {
			t.Fatalf("pod %s: phase %s node %q epoch %q", p.Name, p.Status.Phase, p.Spec.NodeName, p.Annotations[c4v1.AnnotationOutputEpoch])
		}
	}
	if st := reconcile().Status; st.Phase != "Running" || st.Job != "team" {
		t.Fatalf("status %+v", st)
	}

	// Suspend: both running members yield at a turn boundary.
	got := reconcile()
	got.Spec.Suspend = true
	if err := k8s.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcile()
	round(runner)
	round(runner)
	if n := fk.running(); n != 0 {
		t.Fatalf("%d sessions still running after suspend", n)
	}
	if len(fk.stops) != 2 {
		t.Fatalf("%d stops, want 2", len(fk.stops))
	}
	st := reconcile().Status
	if st.Phase != "Suspended" {
		t.Fatalf("phase %s, want Suspended", st.Phase)
	}
	parked := 0
	for _, m := range st.Members {
		if m.State == "parked" {
			parked++
			if !strings.HasPrefix(m.Checkpoint, sessions[m.Name]+"#") {
				t.Fatalf("member %s: checkpoint %q", m.Name, m.Checkpoint)
			}
		}
	}
	if parked != 2 {
		t.Fatalf("%d parked members, want 2: %+v", parked, st.Members)
	}
	if len(livePods()) != 0 {
		t.Fatal("a pod was released while the bundle was suspended")
	}

	// Resume: the queue releases again, with the checkpoints.
	got = reconcile()
	got.Spec.Suspend = false
	if err := k8s.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcile()
	round(runner)
	pods = livePods()
	if len(pods) != 2 || fk.running() != 2 {
		t.Fatalf("after resume: %d pods, %d running", len(pods), fk.running())
	}
	withCheckpoint := 0
	for _, p := range pods {
		for _, e := range p.Spec.Containers[0].Env {
			if e.Name == "C4_AGENT_CHECKPOINT" && e.Value != "" {
				withCheckpoint++
			}
		}
	}
	if withCheckpoint == 0 {
		t.Fatal("no resumed pod carries its checkpoint")
	}

	// A restart of the machine: every session stops, and a new runner
	// process finds the pods running. It resumes their sessions.
	fk.mu.Lock()
	fk.active = map[string]bool{}
	before := len(fk.resumes)
	fk.mu.Unlock()
	fresh := &agent.Runner{Client: k8s, Harness: h, Node: host, Labels: sel, Slots: 2, YieldTimeout: time.Second, Logf: t.Logf}
	round(fresh)
	if n := fk.running(); n != 2 {
		t.Fatalf("after the restart: %d sessions running, want 2", n)
	}
	if got := len(fk.resumes) - before; got != 2 {
		t.Fatalf("after the restart: %d resumes, want 2 (one per member, no copies)", got)
	}
	for _, name := range members {
		m, _ := h.Store.Get(name)
		if m.Epoch > 3 {
			t.Fatalf("member %s epoch %d: resumed more often than needed", name, m.Epoch)
		}
	}
}

// TestRunnerCapacity: a power reader lowers the runner's node at once, and
// raises it only after the higher value has held for RaiseAfter.
func TestRunnerCapacity(t *testing.T) {
	if k8s == nil {
		t.Skip("no envtest binaries (make envtest)")
	}
	ctx := context.Background()
	host := fmt.Sprintf("cap-%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { k8s.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: host}}) })
	clock := time.Date(2026, 10, 17, 22, 55, 0, 0, time.UTC)
	slots, reason := 5, ""
	h := agent.New(&agent.Store{Dir: t.TempDir()}, &fake{active: map[string]bool{}})
	r := &agent.Runner{Client: k8s, Harness: h, Node: host, Labels: map[string]string{"c4.avalanche.io/host": host}, Slots: 5,
		Capacity: func() (int, string) { return slots, reason }, RaiseAfter: time.Minute,
		Now: func() time.Time { return clock }, Logf: t.Logf}
	limit := func() (string, string) {
		t.Helper()
		var n corev1.Node
		if err := k8s.Get(ctx, types.NamespacedName{Name: host}, &n); err != nil {
			t.Fatal(err)
		}
		return n.Labels[c4v1.LabelSlotLimit], n.Annotations[c4v1.AnnotationCapacityReason]
	}
	tickAt := func(d time.Duration) {
		t.Helper()
		clock = clock.Add(d)
		if err := r.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	tickAt(0)
	if l, _ := limit(); l != "5" {
		t.Fatalf("limit %q, want 5", l)
	}
	// 22:55, the battery falls on a 45 W adapter: down to 2 at once.
	slots, reason = 2, "power: 47 of 40 W, battery 21% on a 45 W adapter"
	tickAt(5 * time.Second)
	if l, why := limit(); l != "2" || why != reason {
		t.Fatalf("limit %q (%q), want 2 at once", l, why)
	}
	// A reading that flickers back up does not raise it until it holds.
	slots, reason = 4, ""
	tickAt(30 * time.Second)
	if l, _ := limit(); l != "2" {
		t.Fatalf("raised to %s after 30 s", l)
	}
	tickAt(60 * time.Second)
	if l, why := limit(); l != "4" || why != "" {
		t.Fatalf("limit %q (%q) after 60 s at 4, want 4", l, why)
	}
	// Never above the runner's own slots.
	slots = 9
	tickAt(2 * time.Minute)
	tickAt(2 * time.Minute)
	if l, _ := limit(); l != "5" {
		t.Fatalf("limit %q, want 5 (the runner's slots)", l)
	}
}

// TestPowerCapShedsAtTurnBoundary: the night of 2026-10-17 on the farm.
// Two bundles share one machine's runner: a deadline bundle (the push) and
// a research bundle with no deadline. The power reader lowers the machine
// from two slots to one; the queue sheds the member with the most slack,
// the research one, which yields at a turn boundary, and the push keeps
// running. When the power returns and holds, the research member resumes
// from its checkpoint.
func TestPowerCapShedsAtTurnBoundary(t *testing.T) {
	if k8s == nil {
		t.Skip("no envtest binaries (make envtest)")
	}
	ctx := context.Background()
	ns := fmt.Sprintf("pw-%d", time.Now().UnixNano()%1e9)
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	host := ns + "-abyss"
	sel := map[string]string{"c4.avalanche.io/host": host}
	pool := &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + ns}, Spec: c4v1.PoolSpec{NodeSelector: sel, SlotsPerNode: 2,
		Tolerations: []corev1.Toleration{{Key: agent.TaintHostRunner, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}}}
	if err := k8s.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		k8s.Delete(ctx, pool)
		k8s.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: host}})
	})
	root := t.TempDir()
	cfg, work := filepath.Join(root, "claude"), filepath.Join(root, "work")
	os.MkdirAll(work, 0o755)
	fk := &fake{active: map[string]bool{}}
	h := agent.New(&agent.Store{Dir: filepath.Join(root, "agents")}, fk)
	h.Workspace, h.Poll, h.SettleQuiet, h.SettleMax = "none", time.Millisecond, time.Millisecond, 5*time.Millisecond
	sessions := map[string]string{}
	for i, name := range []string{"hud-scenarios", "prov-graph"} {
		s := fmt.Sprintf("%08d-0000-0000-0000-%012d", i+1, i+1)
		sessions[s] = name
		tr := filepath.Join(cfg, "projects", agent.ProjectSlug(work), s+".jsonl")
		os.MkdirAll(filepath.Dir(tr), 0o755)
		os.WriteFile(tr, []byte(`{"turn":1}`+"\n"), 0o644)
		if _, err := h.Register(ctx, agent.RegisterOptions{Name: name, Session: s, Cwd: work, ConfigDir: cfg}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 18, 5, 50, 0, 0, time.UTC) // 22:50 PDT
	slots, reason := 2, ""
	runner := &agent.Runner{Client: k8s, Harness: h, Node: host, Labels: sel, Slots: 2, YieldTimeout: time.Second,
		Capacity: func() (int, string) { return slots, reason }, RaiseAfter: time.Minute,
		Now: func() time.Time { return now }, Logf: t.Logf}
	q := &queue.Queue{Client: k8s, Policy: sched.Decide, Cfg: sched.DefaultConfig(), Log: zap.New(zap.WriteTo(os.Stderr)),
		Now: func() time.Time { return now }, CaptureWait: time.Minute}
	rec := &controller.AgentBundleReconciler{Client: k8s, Now: func() time.Time { return now }}
	push := &c4v1.AgentBundle{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "push"}, Spec: c4v1.AgentBundleSpec{
		Pools: []string{pool.Name}, Members: []c4v1.AgentMemberSpec{{Name: "hud-scenarios"}},
		Deadline: &metav1.Time{Time: now.Add(11 * time.Hour)}}} // Sunday 10:00 PDT
	research := &c4v1.AgentBundle{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "research"}, Spec: c4v1.AgentBundleSpec{
		Pools: []string{pool.Name}, Members: []c4v1.AgentMemberSpec{{Name: "prov-graph"}}}}
	for _, b := range []*c4v1.AgentBundle{push, research} {
		if err := k8s.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	round := func() {
		t.Helper()
		for i := 0; i < 3; i++ {
			for _, name := range []string{"push", "research"} {
				if _, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := runner.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if err := q.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			now = now.Add(5 * time.Second)
		}
	}
	running := func() []string {
		fk.mu.Lock()
		defer fk.mu.Unlock()
		var out []string
		for s := range fk.active {
			out = append(out, sessions[s])
		}
		return out
	}
	round()
	if got := running(); len(got) != 2 {
		t.Fatalf("before the cap: running %v, want both", got)
	}
	// 22:55: the battery falls on a 45 W adapter; the reader allows one.
	slots, reason = 1, "power: battery 21% falling on a 45 W adapter"
	round()
	round()
	if got := running(); len(got) != 1 || got[0] != "hud-scenarios" {
		t.Fatalf("under the cap: running %v, want only hud-scenarios (the push keeps its slot)", got)
	}
	var b c4v1.AgentBundle
	k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "research"}, &b)
	if len(b.Status.Members) != 1 || b.Status.Members[0].State != "parked" || b.Status.Members[0].Checkpoint == "" {
		t.Fatalf("research member: %+v", b.Status.Members)
	}
	// Power returns and holds for a minute: research resumes.
	slots, reason = 2, ""
	round()
	now = now.Add(time.Minute)
	round()
	round()
	if got := running(); len(got) != 2 {
		t.Fatalf("after the power returned: running %v, want both", got)
	}
}
