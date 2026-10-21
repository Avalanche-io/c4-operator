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
		status := "idle" // done, or blocked: the CLI's "idle blocked" when a task gave up
		switch st {
		case "working":
			status = "busy"
		case "idle-working": // a part ended with more to come: "idle working"
			st = "working"
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
		Owners: []string{"noor"}, BlocksCwd: shared, HookCommand: "/bin/c4-agent checkpoint --hook", Docs: docs, DocsProject: "practice", Logf: t.Logf}
	tick := func() {
		t.Helper()
		if err := r.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	job := ownedJob(t, "default", "practice-1", "noor")
	pod := func(name string, extra ...corev1.EnvVar) *corev1.Pod {
		t.Helper()
		env := append([]corev1.EnvVar{{Name: agent.EnvBlock, Value: "1"}, {Name: agent.EnvBlockRepo, Value: "/repo"},
			{Name: agent.EnvBlockPrompt, Value: "the job's default task"}, {Name: "C4_UNIT_NAME", Value: "count"},
			{Name: agent.EnvBlockPrompt, Value: "Write Count(s string) map[string]int with its tests."}}, extra...)
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{c4v1.LabelJob: "practice-1"},
			OwnerReferences: unitOf(job)},
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
	for _, want := range []string{"--restricted", "--strict-mcp-config", "--permission-mode dontAsk", "--settings", "/bin/c4-agent checkpoint --hook",
		"--tools Read Edit Write Glob Grep Bash --allowedTools", "Bash(git:*)", "-n default.practice-1.count"} {
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

	// a block that gives up: its turn ends with the CLI reporting it blocked
	third := pod("count-a3", corev1.EnvVar{Name: agent.EnvBlockRework, Value: "Again."})
	tick()
	endTurn(session, nil)
	cli.mu.Lock()
	cli.state[session] = "blocked"
	cli.mu.Unlock()
	tick()
	if p := get(third); p.Status.Phase != corev1.PodFailed || p.Status.Reason != "BlockGaveUp" {
		t.Fatalf("gave up: %s %s", p.Status.Phase, p.Status.Reason)
	}

	// the block's own word decides: it wrote done, though the CLI read its
	// last message as blocked
	said := pod("count-a3b", corev1.EnvVar{Name: agent.EnvBlockRework, Value: "Once more."})
	tick()
	endTurn(session, map[string]string{"done": "Count is case-insensitive; nothing was left to do."})
	cli.mu.Lock()
	cli.state[session] = "blocked"
	cli.mu.Unlock()
	tick()
	if p := get(said); p.Status.Phase != corev1.PodSucceeded || p.Status.Reason != "Completed" {
		t.Fatalf("done by its own word: %s %s", p.Status.Phase, p.Status.Reason)
	}

	// a block in parts: a part done with a note of the next wakes the same
	// session for it, in the same attempt, and the last part ends the pod
	parts := pod("count-a4", corev1.EnvVar{Name: agent.EnvBlockRework, Value: "In two parts."})
	tick()
	m4, _ := h.Store.Get("default.practice-1.count")
	endTurn(session, map[string]string{"more": "part 2: the tests"})
	cli.mu.Lock()
	cli.state[session] = "idle-working" // the CLI: idle, its task still working
	cli.mu.Unlock()
	tick()
	if p := get(parts); p.Status.Phase != corev1.PodRunning {
		t.Fatalf("after part 1: %s", p.Status.Phase)
	}
	tick()
	n := len(cli.prompts[session])
	if last := cli.prompts[session][n-1]; !strings.Contains(last, "part 2: the tests") || !strings.Contains(last, m4.UnitDir) {
		t.Fatalf("part prompt:\n%s", last)
	}
	tick() // its old turn must not end the new part
	if p := get(parts); p.Status.Phase != corev1.PodRunning {
		t.Fatalf("the old turn ended the new part: %s", p.Status.Phase)
	}
	endTurn(session, nil)
	tick()
	if p := get(parts); p.Status.Phase != corev1.PodSucceeded || p.Status.Reason != "Completed" {
		t.Fatalf("after the last part: %s %s", p.Status.Phase, p.Status.Reason)
	}

	// a stop at a part's safe point: the farm's decision yields the pod,
	// with the part's note left for its session, and nothing more starts
	stopAt := pod("count-a5", corev1.EnvVar{Name: agent.EnvBlockRework, Value: "Two more parts."})
	tick()
	m5, _ := h.Store.Get("default.practice-1.count")
	out := filepath.Join(m5.UnitDir, "out")
	os.WriteFile(filepath.Join(out, ".c4", "safepoint"), []byte("seq 1\nat "+time.Now().UTC().Format(time.RFC3339Nano)+"\ncheckpoint x\n"), 0o644)
	tick() // the runner relays the safe point
	q := get(stopAt)
	q.Annotations[c4v1.AnnotationSafepointDecision] = `{"seq":1,"do":"stop","why":"the job is on hold-stop"}`
	if err := k8s.Update(ctx, q); err != nil {
		t.Fatal(err)
	}
	tick() // and the decision back
	before := len(cli.prompts[session])
	endTurn(session, map[string]string{"more": "part 3"})
	tick()
	tick()
	if p := get(stopAt); p.Status.Phase != corev1.PodSucceeded || p.Status.Reason != "Yielded" || p.Annotations[c4v1.AnnotationOutputCheckpoint] == "" {
		t.Fatalf("stopped at a safe point: %s %s, checkpoint %q", p.Status.Phase, p.Status.Reason, p.Annotations[c4v1.AnnotationOutputCheckpoint])
	}
	if len(cli.prompts[session]) != before {
		t.Fatal("the runner woke a block the farm stopped")
	}

	// a stop asked and then cancelled: the pod is asked to yield while the
	// block works, the farm answers continue at its safe point (the reason
	// passed), and the block goes on to its next part instead of yielding
	asked := pod("count-a6", corev1.EnvVar{Name: agent.EnvBlockRework, Value: "Two parts again."})
	tick()
	m6, _ := h.Store.Get("default.practice-1.count")
	q = get(asked)
	q.Annotations[c4v1.AnnotationYield] = "hold"
	if err := k8s.Update(ctx, q); err != nil {
		t.Fatal(err)
	}
	tick() // working: the runner waits, and does not block
	if p := get(asked); p.Status.Phase != corev1.PodRunning {
		t.Fatalf("yielded while working: %s", p.Status.Phase)
	}
	out = filepath.Join(m6.UnitDir, "out")
	os.WriteFile(filepath.Join(out, ".c4", "safepoint"), []byte("seq 1\nat "+time.Now().UTC().Format(time.RFC3339Nano)+"\ncheckpoint y\n"), 0o644)
	endTurn(session, map[string]string{"more": "part 2"})
	tick() // at its safe point, unanswered: it waits
	if p := get(asked); p.Status.Phase != corev1.PodRunning {
		t.Fatalf("yielded before the farm answered: %s", p.Status.Phase)
	}
	q = get(asked)
	q.Annotations[c4v1.AnnotationSafepointDecision] = `{"seq":1,"do":"continue","why":"the stop is no longer pending"}`
	delete(q.Annotations, c4v1.AnnotationYield)
	if err := k8s.Update(ctx, q); err != nil {
		t.Fatal(err)
	}
	before = len(cli.prompts[session])
	tick()
	tick()
	if p := get(asked); p.Status.Phase != corev1.PodRunning || len(cli.prompts[session]) != before+1 || !strings.Contains(cli.prompts[session][before], "part 2") {
		t.Fatalf("after continue: %s, prompts %d -> %d", p.Status.Phase, before, len(cli.prompts[session]))
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

// A session that cannot become the block's member (here its transcript
// never appears) is stopped, so the next attempt does not leave it running.
func TestBlockStartFailureStopsSession(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	cli := &noTranscriptCLI{blockCLI{dir: cfg, state: map[string]string{}, cwd: map[string]string{}, prompts: map[string][]string{}, flags: map[string][]string{}}}
	h := agent.New(&agent.Store{Dir: t.TempDir()}, cli)
	_, err := h.Start(context.Background(), agent.StartOptions{Name: "b", Cwd: t.TempDir(), Prompt: "x", TranscriptWait: 50 * time.Millisecond})
	if err == nil || len(cli.stops) != 1 {
		t.Fatalf("err %v, stops %v", err, cli.stops)
	}
	if _, err := h.Store.Get("b"); err == nil {
		t.Fatal("a member was registered for a session that never wrote a transcript")
	}
}

// noTranscriptCLI starts sessions that never write a transcript.
type noTranscriptCLI struct{ blockCLI }

func (f *noTranscriptCLI) Start(ctx context.Context, configDir, dir string, flags []string, prompt string) (string, error) {
	id, err := f.blockCLI.Start(ctx, configDir, dir, flags, prompt)
	f.mu.Lock()
	defer f.mu.Unlock()
	for s := range f.state {
		os.Remove(f.transcript(s))
	}
	return id, err
}
