package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Observing a machine's sessions reads, and only reads: each session's
// word from the CLI's own state (blocked is "needs you", with what it last
// said), its open background work, what it has used, and a usage window it
// hit that has not reset; what needs its person comes first. Nothing is
// stopped or resumed.
func TestObserveReadsSessions(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 10, 20, 20, 0, 0, 0, time.UTC)
	start := now.Add(-time.Hour)
	const (
		waiting = "aaaaaaaa-0000-4000-8000-000000000001"
		busy    = "aaaaaaaa-0000-4000-8000-000000000002"
		limited = "aaaaaaaa-0000-4000-8000-000000000003"
	)
	e.transcript(waiting, `{"type":"assistant","message":{"id":"m1","model":"opus","usage":{"input_tokens":100,"output_tokens":20},"content":[{"type":"text","text":"Which pool should the comp run in?\nI can wait."}]}}`+"\n")
	e.transcript(busy, launchAgent("toolu_a", "aresearch", "Audience evidence research", start.Add(time.Minute)))
	e.transcript(limited, limitHit("You've hit your session limit · resets 1:10pm (America/Los_Angeles)", now.Add(-time.Minute)))
	e.fake.active[waiting] = Active{SessionID: waiting, Name: "lead", Kind: "background", Status: "idle", State: "blocked", Cwd: e.cwd, StartedAt: start.UnixMilli()}
	e.fake.active[busy] = Active{SessionID: busy, Name: "research", Kind: "background", Status: "busy", State: "working", Cwd: e.cwd, StartedAt: start.UnixMilli()}
	e.fake.busyFor[busy] = 1
	e.fake.active[limited] = Active{SessionID: limited, Name: "port", Kind: "interactive", Status: "idle", Cwd: e.cwd, StartedAt: start.UnixMilli()}

	obs, err := e.h.Observe(context.Background(), e.configDir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Observed{}
	var order []string
	for _, o := range obs {
		got[o.Name], order = o, append(order, o.Name)
	}
	if strings.Join(order, ",") != "lead,port,research" {
		t.Errorf("order %v: what needs its person first, then a limit, then work", order)
	}
	if o := got["lead"]; o.Word != WordNeedsYou || o.NeedsYou != "it asks: Which pool should the comp run in?" || o.Usage == nil || o.Usage.Models[0].InputTokens != 100 {
		t.Errorf("lead %+v", o)
	}
	if o := got["research"]; o.Word != WordWorking || o.BackgroundOpen != 1 || !o.Running {
		t.Errorf("research %+v", o)
	}
	if o := got["port"]; o.Word != WordAtLimit || o.Limit == nil || o.Limit.Kind != "session" || o.Limit.Resets == nil {
		t.Errorf("port %+v", o)
	}
	if len(e.fake.stops) != 0 || len(e.fake.resumes) != 0 {
		t.Errorf("observing changed something: %d stops, %d resumes", len(e.fake.stops), len(e.fake.resumes))
	}
}
