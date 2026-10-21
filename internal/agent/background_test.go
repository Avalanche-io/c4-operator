package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Transcript lines as Claude Code writes them: a background agent's launch
// (its tool result marked async_launched), a background command's, the
// notification that one finished, and TaskStop.
func launchAgent(toolUse, agentID, desc string, at time.Time) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"Async agent launched successfully."}]},"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":%q,"description":%q}}`+"\n",
		at.UTC().Format(time.RFC3339Nano), toolUse, agentID, desc)
}

func launchCommand(toolUse, taskID string, at time.Time) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"Command running in background with ID: %s."}]},"toolUseResult":{"backgroundTaskId":%q,"stdout":"","stderr":""}}`+"\n",
		at.UTC().Format(time.RFC3339Nano), toolUse, taskID, taskID)
}

func notified(toolUse, taskID string) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>%s</task-id>\n<tool-use-id>%s</tool-use-id>\n<status>completed</status>\n</task-notification>"}}`+"\n", taskID, toolUse)
}

func taskStop(taskID string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_stop","name":"TaskStop","input":{"task_id":%q}}]}}`+"\n", taskID)
}

// The work a session started in the background and has not heard back
// from: launches with no notification and no TaskStop, since the session's
// process started (work from an earlier process died with it).
func TestOpenBackground(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "s.jsonl")
	start := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	doc := launchCommand("toolu_old", "bold", start.Add(-time.Hour)) + // an earlier process's
		launchAgent("toolu_a", "aresearch", "Audience evidence research", start.Add(time.Minute)) +
		launchAgent("toolu_b", "adone", "Inventory the docs", start.Add(2*time.Minute)) +
		launchCommand("toolu_c", "bsuite", start.Add(3*time.Minute)) +
		launchCommand("toolu_d", "bstopped", start.Add(4*time.Minute)) +
		notified("toolu_b", "adone") + taskStop("bstopped")
	if err := os.WriteFile(tr, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	bg, err := OpenBackground(tr, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(bg) != 2 || bg[0].Task != "aresearch" || bg[0].Kind != "agent" || bg[0].Description != "Audience evidence research" ||
		bg[0].Transcript != filepath.Join(dir, "s", "subagents", "agent-aresearch.jsonl") || bg[1].Task != "bsuite" || bg[1].Kind != "command" {
		t.Fatalf("open %+v", bg)
	}
	if all, _ := OpenBackground(tr, time.Time{}); len(all) != 3 {
		t.Fatalf("with no process start: %d open, want the earlier process's too", len(all))
	}
}

// A yield does not stop a session over background work that has not
// reported: until harvestAt it waits (the member stays running); once the
// work reports, it stops at once and records nothing lost.
func TestYieldWaitsForBackground(t *testing.T) {
	e := newEnv(t)
	start := e.clock
	e.register("lead", sess)
	e.transcript(sess, launchAgent("toolu_a", "aresearch", "Audience evidence research", start.Add(time.Minute)))
	ctx := context.Background()
	e.fake.active[sess] = Active{SessionID: sess, Status: "idle", Kind: "background", StartedAt: start.UnixMilli()}
	res, err := e.h.Yield(ctx, "lead", 0, start.Add(time.Hour))
	if !errors.Is(err, ErrBusy) || len(res.Waiting) != 1 || len(e.fake.stops) != 0 {
		t.Fatalf("yield over running background work: %+v %v, %d stops", res, err, len(e.fake.stops))
	}
	e.transcript(sess, notified("toolu_a", "aresearch"))
	res, err = e.h.Yield(ctx, "lead", 0, start.Add(time.Hour))
	if err != nil || res.State != "stopped" || len(res.Checkpoint.Background) != 0 {
		t.Fatalf("yield once it reported: %+v %v", res, err)
	}
}

// Past harvestAt the yield stops the session anyway and records the work it
// cut short in the checkpoint; the attempt that resumes the member is told
// what was lost and where its transcript is, once.
func TestYieldRecordsBackgroundItCannotWaitFor(t *testing.T) {
	e := newEnv(t)
	start := e.clock
	e.register("lead", sess)
	e.transcript(sess, launchAgent("toolu_a", "aresearch", "Audience evidence research", start.Add(time.Minute))+
		launchCommand("toolu_c", "bsuite", start.Add(2*time.Minute)))
	ctx := context.Background()
	e.fake.active[sess] = Active{SessionID: sess, Status: "idle", Kind: "background", StartedAt: start.UnixMilli()}
	res, err := e.h.Yield(ctx, "lead", time.Second, start)
	if err != nil || res.State != "stopped" || len(e.fake.stops) != 1 {
		t.Fatalf("yield past its wait: %+v %v", res, err)
	}
	cp := res.Checkpoint
	if cp.Event != "yield" || len(cp.Background) != 2 || cp.Background[0].Description != "Audience evidence research" {
		t.Fatalf("checkpoint %+v", cp)
	}
	if last, _ := e.h.Store.Last("lead"); last == nil || len(last.Background) != 2 {
		t.Fatalf("the log's last checkpoint does not record it: %+v", last)
	}
	if _, err := e.h.Resume(ctx, "lead", "carry on"); err != nil {
		t.Fatal(err)
	}
	p := e.fake.resumes[len(e.fake.resumes)-1].prompt
	for _, want := range []string{"background work you had started was still running", `agent "Audience evidence research"`,
		"agent-aresearch.jsonl", "command (no description)", "\n\ncarry on"} {
		if !strings.Contains(p, want) {
			t.Errorf("the resume prompt lacks %q:\n%s", want, p)
		}
	}
	// the resumed session is a new process: what the old one ran is gone
	e.fake.active[sess] = Active{SessionID: sess, Status: "idle", Kind: "background", StartedAt: start.Add(time.Hour).UnixMilli()}
	if _, err := e.h.Yield(ctx, "lead", time.Second, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.Resume(ctx, "lead", ""); err != nil {
		t.Fatal(err)
	}
	if p := e.fake.resumes[len(e.fake.resumes)-1].prompt; strings.Contains(p, "background work") {
		t.Errorf("told twice: %q", p)
	}
}

// A hard stop records what it kills, too.
func TestForceStopRecordsBackground(t *testing.T) {
	e := newEnv(t)
	start := e.clock
	e.register("lead", sess)
	e.transcript(sess, launchAgent("toolu_a", "aresearch", "Audience evidence research", start.Add(time.Minute)))
	e.fake.active[sess] = Active{SessionID: sess, Status: "busy", Kind: "background", StartedAt: start.UnixMilli()}
	cp, err := e.h.ForceStop(context.Background(), "lead")
	if err != nil || cp.Event != "forced-stop" || len(cp.Background) != 1 {
		t.Fatalf("forced stop: %+v %v", cp, err)
	}
}

// On a host, a member asked to yield with background work running keeps its
// pod and session until the work reports or the farm's deadline nears (a
// minute before yield-by); then it yields, and the pod's message says what
// was cut short.
func TestHostYieldWaitsForBackground(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	start := e.clock
	m := e.register("lead", sess)
	e.transcript(sess, launchAgent("toolu_a", "aresearch", "Audience evidence research", start.Add(time.Minute)))
	e.fake.active[sess] = Active{SessionID: sess, Status: "idle", Kind: "background", StartedAt: start.UnixMilli()}
	p := hostPod()
	p.Annotations[c4v1.AnnotationYield] = "capacity"
	p.Annotations[c4v1.AnnotationYieldBy] = start.Add(10 * time.Minute).Format(time.RFC3339)
	r, c := hostRunner(t, e, p)
	clock := start.Add(2 * time.Minute)
	r.Now, r.YieldTimeout = func() time.Time { return clock }, 0
	r.bgSince, r.bgLogged = map[types.UID]time.Time{}, map[types.UID]bool{}
	e.h.Now = func() time.Time { return clock }
	if err := r.yield(ctx, getPod(t, c), m); err != nil {
		t.Fatal(err)
	}
	if got := getPod(t, c); got.Status.Phase != corev1.PodRunning || len(e.fake.stops) != 0 {
		t.Fatalf("yielded over running background work: %s, %d stops", got.Status.Phase, len(e.fake.stops))
	}
	clock = start.Add(9*time.Minute + 30*time.Second) // past yield-by less a minute
	if err := r.yield(ctx, getPod(t, c), m); err != nil {
		t.Fatal(err)
	}
	got := getPod(t, c)
	if got.Status.Phase != corev1.PodSucceeded || got.Status.Reason != "Yielded" ||
		!strings.Contains(got.Status.Message, "1 background task(s) were still running and were stopped") || len(e.fake.stops) != 1 {
		t.Fatalf("pod %s %s %q, %d stops", got.Status.Phase, got.Status.Reason, got.Status.Message, len(e.fake.stops))
	}
}

// A block's turn that ends with background work running is not stopped
// until the work reports or its wait runs out; then the work is recorded.
func TestBlockTurnWaitsForBackground(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	start := e.clock
	m := e.register("lead", sess)
	e.transcript(sess, launchCommand("toolu_c", "bsuite", start.Add(time.Minute)))
	act := Active{SessionID: sess, Status: "idle", Kind: "background", StartedAt: start.UnixMilli()}
	p := hostPod()
	p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: EnvYieldBackgroundWait, Value: "5m"})
	r, _ := hostRunner(t, e, p)
	clock := start.Add(2 * time.Minute)
	r.Now = func() time.Time { return clock }
	r.bgSince, r.bgLogged = map[types.UID]time.Time{}, map[types.UID]bool{}
	if wait, err := r.awaitBackground(ctx, p, m, act); !wait || err != nil {
		t.Fatalf("did not wait: %v %v", wait, err)
	}
	clock = clock.Add(6 * time.Minute)
	if wait, err := r.awaitBackground(ctx, p, m, act); wait || err != nil {
		t.Fatalf("still waiting past its wait: %v %v", wait, err)
	}
	if last, _ := e.h.Store.Last("lead"); last == nil || len(last.Background) != 1 || last.Background[0].Task != "bsuite" {
		t.Fatalf("not recorded: %+v", last)
	}
}

// In a pod each turn is its own CLI process: a background agent a turn left
// running ended with it. The block records it (.c4/background-lost.json, in
// the unit's output) and its next turn is told, once.
func TestPodBlockRecordsBackgroundATurnLeft(t *testing.T) {
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(fakePrint), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, out := t.TempDir(), t.TempDir()
	prompts := filepath.Join(t.TempDir(), "prompts")
	t.Setenv("FAKE_SLUG", ProjectSlug(cwd))
	t.Setenv("FAKE_LOG", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("FAKE_PROMPTS", prompts)
	t.Setenv("FAKE_BG", "Survey the plates")
	t.Setenv("C4_JOB", "port")
	t.Setenv("C4_UNIT_NAME", "notes")
	t.Setenv(EnvBlock, "1")
	t.Setenv(EnvBlockPrompt, "Write notes.")
	t.Setenv("C4_OUTPUT", out)
	b := &PodBlock{Out: out, ConfigDir: t.TempDir(), Cwd: cwd, Claude: bin, HookCommand: "true", Env: os.Getenv, Logf: t.Logf}
	if code, err := b.Run(ctx); err != nil || code != PodBlockDone {
		t.Fatalf("exit %d, %v", code, err)
	}
	var lost []Background
	data, err := os.ReadFile(filepath.Join(out, ".c4", lostFile))
	if err != nil || json.Unmarshal(data, &lost) != nil || len(lost) != 1 || lost[0].Description != "Survey the plates" {
		t.Fatalf("record %s %v", data, err)
	}
	turns := strings.Split(string(mustRead(t, prompts)), "----")
	if len(turns) < 2 || !strings.Contains(turns[1], `agent "Survey the plates"`) || strings.Contains(turns[0], "Survey the plates") {
		t.Fatalf("prompts:\n%s", strings.Join(turns, "----"))
	}
}
