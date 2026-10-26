package agent

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// After each tool call, a member whose unit the farm has asked to stop is
// told to finish the step and end its turn (a PostToolUse hook's block
// decision); one with no stop asked, or a session that is not a member, is
// told nothing.
func TestStopAskedWithinATurn(t *testing.T) {
	e := newEnv(t)
	m := e.register("lead", sess)
	r, c := hostRunner(t, e, hostPod())
	u, err := r.prepareUnit(getPod(t, c), m)
	if err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"session_id":"` + sess + `","hook_event_name":"PostToolUse","tool_name":"Bash"}`)
	if HookEvent(input) != "PostToolUse" {
		t.Fatal("event")
	}
	if _, ok, err := e.h.StopAsked(input); ok || err != nil {
		t.Fatalf("told to stop with no stop asked: %v", err)
	}
	if err := os.WriteFile(u.PodInfo, []byte(c4v1.AnnotationYield+`="capacity: a deadline needs the slot"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	why, ok, err := e.h.StopAsked(input)
	if !ok || err != nil || !strings.Contains(why, "asked this session to stop (capacity: a deadline needs the slot)") || !strings.Contains(why, "end your turn now") {
		t.Fatalf("%q %v %v", why, ok, err)
	}
	var d map[string]string
	if json.Unmarshal(HookDecision(why), &d) != nil || d["decision"] != "block" || d["reason"] != why {
		t.Fatalf("decision %s", HookDecision(why))
	}
	stranger := []byte(`{"session_id":"99999999-0000-0000-0000-000000000000","hook_event_name":"PostToolUse"}`)
	if _, ok, _ := e.h.StopAsked(stranger); ok {
		t.Error("a session that is not a member was told to stop")
	}
}

// The settings C4 gives a member's sessions run the hook after each tool
// call as well as at each turn's end.
func TestHooksRunAfterEachTool(t *testing.T) {
	for name, b := range map[string][]byte{"hooks": mustJSON(t, HookSettings), "block": mustJSON(t, HookSettingsCommand)} {
		var s struct {
			Hooks map[string][]struct {
				Matcher string `json:"matcher"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		if len(s.Hooks["Stop"]) != 1 || len(s.Hooks["PostToolUse"]) != 1 || s.Hooks["PostToolUse"][0].Matcher != "*" {
			t.Errorf("%s: %s", name, b)
		}
	}
}

// A block's session, on a host or in a pod, has C4_AGENT_BLOCK=1 in its
// environment from the settings it starts with, so the CLI and MCP inside
// it can refuse agent work that only a lead may start.
func TestABlockSessionKnowsItIsABlock(t *testing.T) {
	flags, err := blockFlagsFor(hostPod(), "practice.j.u", "c4-agent")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(flags, "--settings")
	if i < 0 || i+1 >= len(flags) {
		t.Fatalf("no --settings in %v", flags)
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(flags[i+1]), &s); err != nil {
		t.Fatal(err)
	}
	if s.Env[EnvBlock] != "1" {
		t.Fatalf("the block's settings set no %s: %s", EnvBlock, flags[i+1])
	}
	m, err := HookSettings("c4-agent")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(m), EnvBlock) {
		t.Fatalf("a member's settings mark it a block: %s", m)
	}
}

func mustJSON(t *testing.T, f func(string) ([]byte, error)) []byte {
	t.Helper()
	b, err := f("c4-agent")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A member's Yielded pod says how long the stop took from the farm's
// request, so every yield in the book carries its latency.
func TestYieldReportsItsLatency(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m := e.register("lead", sess)
	e.fake.active[sess] = Active{SessionID: sess, Status: "idle", Kind: "background", StartedAt: e.clock.UnixMilli()}
	p := hostPod()
	p.Annotations[c4v1.AnnotationYield] = "capacity"
	r, c := hostRunner(t, e, p)
	asked := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	now := asked.Add(4*time.Minute + 30*time.Second)
	r.Now = func() time.Time { return now }
	r.bgSince, r.bgLogged = map[types.UID]time.Time{}, map[types.UID]bool{}
	r.asked[p.UID] = asked
	if err := r.yield(ctx, getPod(t, c), m); err != nil {
		t.Fatal(err)
	}
	if got := getPod(t, c); got.Status.Reason != "Yielded" || !strings.Contains(got.Status.Message, "stopped 4m30s after it was asked") {
		t.Fatalf("%s %q", got.Status.Reason, got.Status.Message)
	}
}

// While a block pod waits to start, the runner looks again within a
// second rather than at its ordinary pace, so a lead's worker starts as
// soon as the farm places it; a pod waiting longer than soonFor waits for
// something else, and the pace goes back. Running pods, members that are
// not blocks, and pods being deleted never hurry it.
func TestRunnerLooksSoonWhileABlockWaits(t *testing.T) {
	r := &Runner{}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	block := func(uid string, phase corev1.PodPhase) corev1.Pod {
		p := hostPod()
		p.UID = types.UID(uid)
		p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: EnvBlock, Value: "1"})
		p.Status.Phase = phase
		return *p
	}
	member := hostPod()
	member.UID, member.Status.Phase = "m", corev1.PodPending
	if r.blocksStarting([]corev1.Pod{block("a", corev1.PodRunning), *member}, now) {
		t.Fatal("a running block or a member hurried the runner")
	}
	if r.nextWait(5*time.Second) != 5*time.Second {
		t.Fatal("no hurry, yet a short wait")
	}
	r.soon = r.blocksStarting([]corev1.Pod{block("b", corev1.PodPending)}, now)
	if !r.soon || r.nextWait(5*time.Second) != time.Second {
		t.Fatalf("a block waiting to start: soon %v, wait %s", r.soon, r.nextWait(5*time.Second))
	}
	if r.SoonEvery = 300 * time.Millisecond; r.nextWait(5*time.Second) != 300*time.Millisecond {
		t.Fatal("SoonEvery")
	}
	if r.nextWait(200*time.Millisecond) != 200*time.Millisecond {
		t.Fatal("an ordinary pace already faster")
	}
	// still waiting after a minute: it waits for something else
	if r.blocksStarting([]corev1.Pod{block("b", corev1.PodPending)}, now.Add(soonFor)) {
		t.Fatal("a pod waiting past soonFor still hurries the runner")
	}
	// one being deleted does not, and a pod gone is forgotten
	gone := block("c", corev1.PodPending)
	gone.DeletionTimestamp = &metav1.Time{Time: now}
	if r.blocksStarting([]corev1.Pod{gone}, now) || len(r.pendingSince) != 0 {
		t.Fatalf("a deleted pod: %v", r.pendingSince)
	}
}
