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
	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"
	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/agent"
)

// blockCLI is the claude CLI for blocks: it starts fresh sessions, each
// working until the test ends its turn, and wakes a stopped one by ID.
type blockCLI struct {
	mu      sync.Mutex
	dir     string // CLAUDE_CONFIG_DIR
	n       int
	state   map[string]string // session -> working, done; absent: stopped
	cwd     map[string]string
	prompts map[string][]string
	flags   map[string][]string
	stops   []string
}

func (f *blockCLI) transcript(session string) string {
	return filepath.Join(f.dir, "projects", agent.ProjectSlug(f.cwd[session]), session+".jsonl")
}

func (f *blockCLI) Active(ctx context.Context, configDir string, all bool) ([]agent.Active, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []agent.Active
	for s, st := range f.state {
		status := "busy"
		if st == "done" {
			status = "idle"
		}
		out = append(out, agent.Active{SessionID: s, Status: status, State: st, Kind: "background", Pid: 1})
	}
	return out, nil
}

func (f *blockCLI) Start(ctx context.Context, configDir, dir string, flags []string, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	s := fmt.Sprintf("%08x-0000-4000-8000-%012x", f.n, f.n)
	f.state[s], f.cwd[s] = "working", dir
	f.prompts[s] = append(f.prompts[s], prompt)
	f.flags[s] = flags
	path := f.transcript(s)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"content":"start"}}`+"\n"), 0o644); err != nil {
		return "", err
	}
	return agent.ShortID(s), nil
}

func (f *blockCLI) Resume(ctx context.Context, configDir, dir, session string, flags []string, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[session] = "working"
	f.prompts[session] = append(f.prompts[session], prompt)
	return agent.ShortID(session), nil
}

func (f *blockCLI) Stop(ctx context.Context, configDir, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.state, session)
	f.stops = append(f.stops, session)
	return nil
}

// TestBlockOnHostRunner: a plan job's unit runs as a block. Its first
// attempt starts a fresh session with the unit's task and its output
// directory; the session never stops to ask, and checkpoints every turn
// through the Stop hook. When its task is done the runner stops it and
// finishes the pod Completed, storing what it wrote in its own output
// directory and nothing from the shared folder. A review's rework comes
// back as a second pod for the same unit: the runner wakes the same
// session by its ID with what the review asks.
func TestBlockOnHostRunner(t *testing.T) {
	if k8s == nil {
		t.Skip("no envtest binaries (make envtest)")
	}
	ctx := context.Background()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	host := fmt.Sprintf("blk-%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { k8s.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: host}}) })
	cli := &blockCLI{dir: cfg, state: map[string]string{}, cwd: map[string]string{}, prompts: map[string][]string{}, flags: map[string][]string{}}
	h := agent.New(&agent.Store{Dir: t.TempDir()}, cli)
	h.SettleQuiet, h.SettleMax = time.Millisecond, 50*time.Millisecond
	shared := t.TempDir()
	// a stale document in the shared folder must never be taken as a block's
	if err := os.MkdirAll(filepath.Join(shared, ".c4"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(shared, ".c4", "verdict.json"), []byte(`{"stale":true}`), 0o644)
	docs := &docStore{}
	r := &agent.Runner{Client: k8s, Harness: h, Node: host, Labels: map[string]string{"c4.avalanche.io/host": host}, Slots: 1,
		BlocksCwd: shared, HookCommand: "/bin/c4-agent checkpoint --hook", Docs: docs, DocsProject: "practice", Logf: t.Logf}
	tick := func() {
		t.Helper()
		if err := r.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pod := func(name string, extra ...corev1.EnvVar) *corev1.Pod {
		t.Helper()
		env := append([]corev1.EnvVar{{Name: agent.EnvBlock, Value: "1"}, {Name: agent.EnvBlockRepo, Value: "/repo"},
			{Name: agent.EnvBlockPrompt, Value: "the job's default task"}, {Name: "C4_UNIT_NAME", Value: "count"},
			{Name: agent.EnvBlockPrompt, Value: "Write Count(s string) map[string]int with its tests."}}, extra...)
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{c4v1.LabelJob: "practice-1"}},
			Spec: corev1.PodSpec{NodeName: host, Containers: []corev1.Container{{Name: "member", Image: "c4-agent", Env: env}}}}
		if err := k8s.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { k8s.Delete(ctx, p) })
		p.Status.Phase = corev1.PodPending
		if err := k8s.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	get := func(p *corev1.Pod) *corev1.Pod {
		t.Helper()
		var q corev1.Pod
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, &q); err != nil {
			t.Fatal(err)
		}
		return &q
	}
	endTurn := func(session string, write map[string]string) {
		t.Helper()
		m, err := h.Store.BySession(session)
		if err != nil {
			t.Fatal(err)
		}
		f, _ := os.OpenFile(m.Transcript, os.O_APPEND|os.O_WRONLY, 0)
		fmt.Fprintln(f, `{"type":"assistant","message":{"id":"m-`+fmt.Sprint(time.Now().UnixNano())+`","model":"opus","usage":{"input_tokens":3,"output_tokens":90}}}`)
		f.Close()
		for file, body := range write {
			if err := os.WriteFile(filepath.Join(m.UnitDir, "out", ".c4", file), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := h.Hook(ctx, strings.NewReader(`{"session_id":"`+session+`","transcript_path":"`+m.Transcript+`","hook_event_name":"Stop"}`)); err != nil {
			t.Fatal(err)
		}
		cli.mu.Lock()
		cli.state[session] = "done"
		cli.mu.Unlock()
	}

	// the first attempt: a fresh session with the unit's own task
	first := pod("count-a1")
	tick()
	if p := get(first); p.Status.Phase != corev1.PodRunning {
		t.Fatalf("first attempt %s (%s)", p.Status.Phase, p.Status.Message)
	}
	m, err := h.Store.Get("default.practice-1.count")
	if err != nil || !m.Block || m.Cwd != shared {
		t.Fatalf("member %+v: %v", m, err)
	}
	session := m.Session
	prompt := cli.prompts[session][0]
	for _, want := range []string{"block count of job default/practice-1", "Write Count(s string)", "/repo", filepath.Join(m.UnitDir, "out") + "/.c4/"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("first prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "the job's default task") {
		t.Error("the unit's own task did not win over the job's")
	}
	flags := strings.Join(cli.flags[session], " ")
	for _, want := range []string{"--permission-mode dontAsk", "--settings", "/bin/c4-agent checkpoint --hook", "Bash(git:*)", "-n default.practice-1.count"} {
		if !strings.Contains(flags, want) {
			t.Errorf("flags lack %q: %s", want, flags)
		}
	}

	// working: still running; its turn ends and its task is done
	tick()
	if p := get(first); p.Status.Phase != corev1.PodRunning {
		t.Fatalf("while working: %s", p.Status.Phase)
	}
	endTurn(session, nil)
	tick()
	p := get(first)
	if p.Status.Phase != corev1.PodSucceeded || p.Status.Reason != "Completed" || len(cli.stops) != 1 || cli.stops[0] != session {
		t.Fatalf("done: %s %s, stops %v", p.Status.Phase, p.Status.Reason, cli.stops)
	}
	if p.Annotations[c4v1.AnnotationOutputVerdict] != "" {
		t.Error("the shared folder's stale verdict was taken as the block's")
	}

	// a rework: the same unit again, woken by its session ID
	second := pod("count-a2", corev1.EnvVar{Name: agent.EnvBlockRework, Value: "Count must ignore case."})
	tick()
	if p := get(second); p.Status.Phase != corev1.PodRunning {
		t.Fatalf("rework attempt %s (%s)", p.Status.Phase, p.Status.Message)
	}
	if cli.n != 1 || len(cli.prompts[session]) != 2 {
		t.Fatalf("rework started %d sessions; %d prompts to the first", cli.n, len(cli.prompts[session]))
	}
	re := cli.prompts[session][1]
	if !strings.Contains(re, "Count must ignore case.") || strings.Contains(re, m.UnitDir) {
		t.Errorf("rework prompt (old unit dir %s):\n%s", m.UnitDir, re)
	}
	m2, _ := h.Store.Get("default.practice-1.count")
	if m2.Epoch != m.Epoch+1 || m2.UnitDir == m.UnitDir || !strings.Contains(re, m2.UnitDir) {
		t.Errorf("epoch %d -> %d, unit dir %s -> %s", m.Epoch, m2.Epoch, m.UnitDir, m2.UnitDir)
	}
	endTurn(session, map[string]string{"verdict.json": `{"version":1,"verdict":"pass"}`})
	tick()
	if p := get(second); p.Status.Phase != corev1.PodSucceeded || p.Annotations[c4v1.AnnotationOutputVerdict] == "" {
		t.Fatalf("rework done: %s, verdict %q", p.Status.Phase, p.Annotations[c4v1.AnnotationOutputVerdict])
	}
}

// docStore keeps the documents a runner stores.
type docStore struct {
	mu   sync.Mutex
	docs map[string][]byte
}

func (d *docStore) PutBytes(ctx context.Context, project string, id c4.ID, data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.docs == nil {
		d.docs = map[string][]byte{}
	}
	d.docs[id.String()] = data
	return nil
}

