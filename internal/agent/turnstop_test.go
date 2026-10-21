package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

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
