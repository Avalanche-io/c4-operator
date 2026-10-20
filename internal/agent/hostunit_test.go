package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	sp := "seq 1\nat " + at.Format(time.RFC3339Nano) + "\ncheckpoint " + sess + "#3:c4abc\nwait {\"server\":\"person:owner\",\"ask\":\"merge it?\"}\n"
	if err := os.WriteFile(filepath.Join(u.Out, ".c4", "safepoint"), []byte(sp), 0o644); err != nil {
		t.Fatal(err)
	}
	// the same file reads the same as the node daemon reads it
	ours, ok := readSafepoint(u.Out)
	theirs, ok2 := node.ReadSafepoint(u.Out)
	if !ok || !ok2 || ours.Seq != theirs.Seq || !ours.At.Equal(theirs.At) || ours.Checkpoint != theirs.Checkpoint || string(ours.Wait) != string(theirs.Wait) || ours.Wait == nil {
		t.Fatalf("safepoint %+v, node reads %+v", ours, theirs)
	}

	if err := r.relay(ctx, p, u); err != nil {
		t.Fatal(err)
	}
	p = getPod(t, c)
	var note node.SafepointNote
	if json.Unmarshal([]byte(p.Annotations[c4v1.AnnotationSafepoint]), &note) != nil || note.Seq != 1 || note.Checkpoint != sess+"#3:c4abc" ||
		string(note.Wait) != `{"server":"person:owner","ask":"merge it?"}` {
		t.Fatalf("safepoint annotation %q", p.Annotations[c4v1.AnnotationSafepoint])
	}
	info, _ := os.ReadFile(u.PodInfo)
	if !strings.Contains(string(info), `c4.avalanche.io/note="a \"quoted\" value"`) {
		t.Fatalf("podinfo %q", info)
	}
	if _, err := os.Stat(filepath.Join(u.Control, "safepoint.decision")); err == nil {
		t.Fatal("a decision before the queue answered")
	}

	// A message to the unit goes to control/steer; how far it read, back
	// to the pod.
	msgs := `[{"seq":1,"at":"2026-10-18T20:01:00Z","actor":"dana","text":"keep the old signature"}]`
	if err := r.annotate(ctx, p, map[string]string{c4v1.AnnotationUnitSteer: msgs}); err != nil {
		t.Fatal(err)
	}
	if err := r.relay(ctx, getPod(t, c), u); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(u.Control, "steer")); err != nil || string(b) != msgs {
		t.Fatalf("steer file %q %v", b, err)
	}
	os.WriteFile(filepath.Join(u.Out, ".c4", "steer-seen"), []byte("1\n"), 0o644)
	if err := r.relay(ctx, getPod(t, c), u); err != nil {
		t.Fatal(err)
	}
	if got := getPod(t, c).Annotations[c4v1.AnnotationSteerSeen]; got != "1" {
		t.Fatalf("steer-seen %q", got)
	}
	answers := `[{"seq":1,"ask":"2#1","answer":"tokenize/"}]`
	os.WriteFile(filepath.Join(u.Out, ".c4", "answers"), []byte(answers), 0o644)
	if err := r.relay(ctx, getPod(t, c), u); err != nil {
		t.Fatal(err)
	}
	if got := getPod(t, c).Annotations[c4v1.AnnotationAnswers]; got != answers {
		t.Fatalf("answers %q", got)
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

	// and a measure of the job's done
	measure := []byte(`{"value":12,"of":40,"name":"functions ported"}`)
	if err := os.WriteFile(filepath.Join(u.Out, ".c4", "measure.json"), measure, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.storeControlDocs(ctx, p, m, u); err != nil {
		t.Fatal(err)
	}
	if mid := c4.Identify(bytes.NewReader(measure)); getPod(t, c).Annotations[c4v1.AnnotationOutputMeasure] != mid.String() {
		t.Fatalf("output-measure %q, want %s", getPod(t, c).Annotations[c4v1.AnnotationOutputMeasure], mid)
	}

	// and a model member's token use
	usage := []byte(`{"models":[{"model":"opus","inputTokens":4,"outputTokens":500}]}`)
	if err := os.WriteFile(filepath.Join(u.Out, ".c4", "usage.json"), usage, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.storeControlDocs(ctx, p, m, u); err != nil {
		t.Fatal(err)
	}
	if uid := c4.Identify(bytes.NewReader(usage)); getPod(t, c).Annotations[c4v1.AnnotationOutputUsage] != uid.String() {
		t.Fatalf("output-usage %q, want %s", getPod(t, c).Annotations[c4v1.AnnotationOutputUsage], uid)
	}
}

// A pod the queue deletes (a disruption) keeps its finalizer until the
// queue records it: the runner stops the member at a turn boundary and
// gives the pod its phase, Yielded with its checkpoint, before deleting it.
// It marks the pod drained, as the queue would, so the queue reads its end
// as a yield and resumes it, not as the unit done.
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
	if got.Annotations[c4v1.AnnotationYield] != yieldDisruption {
		t.Fatalf("the deleted pod's yield is %q, want %q", got.Annotations[c4v1.AnnotationYield], yieldDisruption)
	}
}

// A transcript's token use, per model: a message written as several lines
// counts once, other lines are skipped, and only the bytes in range count.
func TestTranscriptUsage(t *testing.T) {
	turn := func(id, model string, in, out, read, write int) string {
		return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`+"\n", id, model, in, out, read, write)
	}
	first := `{"type":"user","message":{"content":"go"}}` + "\n" + turn("m1", "opus", 2, 200, 1000, 500) + turn("m1", "opus", 2, 200, 1000, 500) + turn("m2", "haiku", 10, 20, 0, 0)
	second := turn("m3", "opus", 3, 100, 2000, 0) + "not json\n"
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(first+second), 0o644); err != nil {
		t.Fatal(err)
	}
	all, err := TranscriptUsage(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Models) != 2 || all.Models[0].Model != "haiku" || all.Models[1] != (ModelUsage{Model: "opus", InputTokens: 5, OutputTokens: 300, CacheReadTokens: 3000, CacheWriteTokens: 500}) {
		t.Fatalf("all %+v", all.Models)
	}
	part, _ := TranscriptUsage(path, int64(len(first)), 0)
	if len(part.Models) != 1 || part.Models[0].OutputTokens != 100 {
		t.Fatalf("from the second turn %+v", part.Models)
	}
	head, _ := TranscriptUsage(path, 0, int64(len(first)))
	if head.Models[1].OutputTokens != 200 {
		t.Fatalf("up to the first turn's end %+v", head.Models)
	}
}

// A block's first attempt counts its tokens from the start of its session,
// not from where registration found the transcript; a member registered
// into a running session counts from its registration.
func TestAttemptStartBlock(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	for _, block := range []bool{true, false} {
		name := fmt.Sprintf("m-%v", block)
		if err := s.Put(&Member{Name: name, Session: "s", Block: block}); err != nil {
			t.Fatal(err)
		}
		s.appendLog(name, Checkpoint{Seq: 1, Event: "register", Session: "s", Offset: 4000})
		s.appendLog(name, Checkpoint{Seq: 2, Event: "stop", Session: "s", Offset: 9000})
		cp := Checkpoint{Seq: 2, Session: "s"}
		want := int64(4000)
		if block {
			want = 0
		}
		if got := s.AttemptStart(name, &cp); got != want {
			t.Errorf("block %v: from %d, want %d", block, got, want)
		}
		// a later attempt counts from where the last one stopped
		if got := s.AttemptStart(name, &Checkpoint{Seq: 3, Session: "s"}); got != 9000 {
			t.Errorf("block %v, later attempt: from %d", block, got)
		}
	}
}

// What an attempt reasoned with, from its transcript: the model that wrote
// most of its output, every effort level it ran at (the last one in
// force), its thinking mode, and the tools of the session's latest prompt
// snapshot, though that came before the attempt began; MCP servers are
// read from the tools' names.
func TestTranscriptReasoning(t *testing.T) {
	snapshot := `{"type":"attachment","attachment":{"type":"prompt_snapshot","tools":[{"name":"Read"},{"name":"Edit"},{"name":"mcp__docs__read"},{"name":"mcp__docs__write"},{"name":"mcp__git__log"}]}}` + "\n"
	turn := func(id, model, effort string, out int) string {
		return `{"type":"api-request","params":{"model":"` + model + `","thinking":{"type":"adaptive"}}}` + "\n" +
			fmt.Sprintf(`{"type":"assistant","effort":%q,"message":{"id":%q,"model":%q,"usage":{"input_tokens":1,"output_tokens":%d}}}`+"\n", effort, id, model, out)
	}
	before := snapshot + turn("a", "claude-haiku-5-5", "low", 999)
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(before+turn("b", "claude-opus-5-5", "medium", 400)+turn("c", "claude-haiku-5-5", "high", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := TranscriptUsage(path, int64(len(before)), 0)
	if err != nil {
		t.Fatal(err)
	}
	r := d.Reasoning
	if r == nil || r.Does != "model" || r.Model != "claude-opus-5-5" || r.Effort != "high" || r.Thinking != "adaptive" {
		t.Fatalf("reasoning %+v", r)
	}
	if strings.Join(r.Models, " ") != "claude-haiku-5-5 claude-opus-5-5" || strings.Join(r.Efforts, " ") != "medium high" {
		t.Fatalf("models %v, efforts %v", r.Models, r.Efforts)
	}
	if len(r.ToolNames) != 5 || strings.Join(r.MCPServers, " ") != "docs git" {
		t.Fatalf("tools %v, MCP servers %v", r.ToolNames, r.MCPServers)
	}
	// one model at one effort: no lists
	one, _ := TranscriptUsage(path, 0, int64(len(before)))
	if one.Reasoning.Models != nil || one.Reasoning.Efforts != nil || one.Reasoning.Effort != "low" {
		t.Fatalf("one turn %+v", one.Reasoning)
	}
}

// A block declares its effort (a flag its session starts with) and the
// name of its tool profile.
func TestBlockEffortAndProfile(t *testing.T) {
	p := hostPod()
	c := &p.Spec.Containers[0]
	if blockProfile(p) != "default" {
		t.Fatalf("profile %q without tools", blockProfile(p))
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: EnvBlockTools, Value: "Read"})
	if blockProfile(p) != "custom" {
		t.Fatalf("profile %q with tools", blockProfile(p))
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: EnvBlockProfile, Value: "read-only"}, corev1.EnvVar{Name: EnvBlockEffort, Value: "high"})
	if blockProfile(p) != "read-only" {
		t.Fatalf("profile %q", blockProfile(p))
	}
	flags, err := blockFlagsFor(p, "practice.j.u", "true")
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(flags, "--effort"); i < 0 || flags[i+1] != "high" {
		t.Fatalf("flags %v", flags)
	}
}

// A block resumed after the question it asked hears what was decided, by
// whom, and where the reply's files are; without a decision it hears that
// the farm resumed it.
func TestReworkPromptCarriesTheDecision(t *testing.T) {
	p := hostPod()
	u := UnitDirsAt(t.TempDir())
	if got := reworkPrompt(p, u); !strings.Contains(got, "The farm resumed your block") {
		t.Fatalf("no decision: %q", got)
	}
	os.MkdirAll(filepath.Join(u.Root, "reply"), 0o755)
	c := &p.Spec.Containers[0]
	c.Env = append(c.Env, corev1.EnvVar{Name: "C4_DECISION",
		Value: `{"safepoint":3,"do":"yes","note":"keep the C extension out of scope","actor":"joshua","at":"2026-10-20T05:00:00Z","view":"c4abc","reply":"/etc/c4/reply"}`},
		corev1.EnvVar{Name: "C4_REPLY", Value: "/etc/c4/reply"})
	got := reworkPrompt(p, u)
	for _, want := range []string{"joshua answered the question you asked: yes.", "keep the C extension out of scope", filepath.Join(u.Root, "reply")} {
		if !strings.Contains(got, want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, got)
		}
	}
}

// A block whose brief lacks information reports it in .c4/needs.json: the
// host runner fails its pod with reason NeedsInformation and the text as
// the message (what c4-unit needs writes), with the checkpoint the retry
// resumes from. A malformed report is refused by name.
func TestHostNeedsInformation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m := e.register("lead", sess)
	r, c := hostRunner(t, e, hostPod())
	p := getPod(t, c)
	u, err := r.prepareUnit(p, m)
	if err != nil {
		t.Fatal(err)
	}
	write := func(doc string) {
		if err := os.WriteFile(filepath.Join(u.Out, ".c4", "needs.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"needs": "the delivery frame rate", "why": "the brief gives 24 and 25"}`)
	msg, ok, err := takeNeeds(u)
	if !ok || err != nil || msg != "the delivery frame rate\nwhy: the brief gives 24 and 25" {
		t.Fatalf("takeNeeds: %q %v %v", msg, ok, err)
	}
	if _, ok, _ := takeNeeds(u); ok {
		t.Fatal("the report was not consumed")
	}
	write(`{"question": "a field the format does not have"}`)
	if _, ok, err := takeNeeds(u); !ok || err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("malformed: %v %v", ok, err)
	}

	if err := r.needsInformation(ctx, p, m, msg); err != nil {
		t.Fatal(err)
	}
	got := getPod(t, c)
	if got.Status.Phase != corev1.PodFailed || got.Status.Reason != "NeedsInformation" || got.Status.Message != msg {
		t.Fatalf("pod %s %s %q", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
	if got.Annotations[c4v1.AnnotationOutputCheckpoint] == "" {
		t.Fatal("no checkpoint for the retry to resume from")
	}
}

// The attempt that resumes a block after the information was supplied
// hears what it asked for and what came; that comes before any other
// reason it was resumed.
func TestReworkPromptCarriesWhatWasSupplied(t *testing.T) {
	p := hostPod()
	c := &p.Spec.Containers[0]
	c.Env = append(c.Env, corev1.EnvVar{Name: "C4_NEEDED", Value: "the delivery frame rate"},
		corev1.EnvVar{Name: "C4_SUPPLIED", Value: "24 fps; the 25 was a typo"})
	got := reworkPrompt(p, UnitDirsAt(t.TempDir()))
	for _, want := range []string{"your brief lacked:\nthe delivery frame rate", "Here it is:\n24 fps; the 25 was a typo"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, got)
		}
	}
}

// On a host, a block that asked another unit a question keeps its slot:
// the runner leaves its session stopped until the answer is among the
// unit's messages, then resumes it with the answer and marks it read.
// With no answer within its hold, a question to the job's answerer wakes
// it to decide within its brief.
func TestHostHoldsForAnswer(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m := e.register("lead", sess)
	r, c := hostRunner(t, e, hostPod())
	p := getPod(t, c)
	u, err := r.prepareUnit(p, m)
	if err != nil {
		t.Fatal(err)
	}
	r.since, r.waited = map[types.UID]time.Time{}, map[types.UID]bool{}
	now := time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	if err := WriteAsking(u.Out, Asking{ID: "0#1", To: "unit:plan", At: now}); err != nil {
		t.Fatal(err)
	}
	a, _ := ReadAsking(u.Out)
	if err := r.holdForAnswer(ctx, p, m, u, a); err != nil || len(e.fake.resumes) != 0 {
		t.Fatalf("woke before the answer: %v %d", err, len(e.fake.resumes))
	}
	os.WriteFile(filepath.Join(u.Control, "steer"), []byte(`[{"seq":4,"at":"2026-10-20T06:01:00Z","text":"answer to 0#1 from plan: the lexer package","answer":{"id":"0#1","from":"plan"}}]`), 0o644)
	if err := r.holdForAnswer(ctx, p, m, u, a); err != nil {
		t.Fatal(err)
	}
	if n := len(e.fake.resumes); n != 1 || !strings.Contains(e.fake.resumes[0].prompt, "answered by plan:\nanswer to 0#1 from plan: the lexer package") {
		t.Fatalf("resumes %+v", e.fake.resumes)
	}
	if _, ok := ReadAsking(u.Out); ok {
		t.Fatal("the answered question is still open")
	}
	if b, _ := os.ReadFile(filepath.Join(u.Out, ".c4", "steer-seen")); strings.TrimSpace(string(b)) != "4" {
		t.Fatalf("steer-seen %q", b)
	}

	// a question to the job's answerer that goes unanswered
	e.fake.active = map[string]Active{}
	WriteAsking(u.Out, Asking{ID: "0#2", At: now})
	a, _ = ReadAsking(u.Out)
	now = now.Add(DefaultAskHold + time.Second)
	if err := r.holdForAnswer(ctx, p, m, u, a); err != nil {
		t.Fatal(err)
	}
	if n := len(e.fake.resumes); n != 2 || !strings.Contains(e.fake.resumes[1].prompt, "No answer to your question 0#2") {
		t.Fatalf("resumes %+v", e.fake.resumes)
	}
}
