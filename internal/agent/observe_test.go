package agent

import (
	"context"
	"fmt"
	"github.com/Avalanche-io/c4-operator/work/tokens"
	"os"
	"path/filepath"
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
	e.transcript(waiting, `{"type":"assistant","message":{"id":"m1","model":"opus","usage":{"input_tokens":100,"output_tokens":20},"content":[{"type":"text","text":"The plates are in.\nWhich pool should the comp run in?"}]}}`+"\n")
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

// Who needs their person, read from the transcript as the CLI does not say
// it for every session: a tool call waiting at the end (a permission
// prompt), or a turn that ends on a question, in an interactive session as
// in a background one. A background session the CLI calls blocked that
// asks nothing is idle (a finished task reads as blocked too). A limit hit
// followed by turns that went through has passed. A failed session is
// listed without --all.
func TestObserveWhoNeedsYou(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
	start := now.Add(-time.Hour)
	n := 0
	add := func(name, kind, status, state, transcript string) {
		n++
		id := fmt.Sprintf("cccccccc-0000-4000-8000-%012d", n)
		e.transcript(id, transcript)
		e.fake.active[id] = Active{SessionID: id, Name: name, Kind: kind, Status: status, State: state, Cwd: e.cwd, StartedAt: start.UnixMilli()}
	}
	asst := func(id, content string) string {
		return `{"type":"assistant","message":{"id":"` + id + `","model":"opus","content":` + content + `}}` + "\n"
	}
	add("permission", "interactive", "idle", "", asst("m1", `[{"type":"text","text":"I will run the tests."},{"type":"tool_use","id":"t1","name":"Bash"}]`))
	add("asked", "interactive", "idle", "", asst("m2", `[{"type":"text","text":"Should I squash the commits?"}]`))
	add("answered", "interactive", "idle", "", asst("m3", `[{"type":"text","text":"Should I squash?"}]`)+`{"type":"user","message":{"role":"user","content":"yes, squash"}}`+"\n")
	add("finished", "background", "idle", "blocked", asst("m4", `[{"type":"text","text":"All done; the branch is pushed."}]`))
	add("recovered", "interactive", "idle", "", limitHit("You've hit your session limit · resets 1:10pm (America/Los_Angeles)", now.Add(-30*time.Minute))+asst("m5", `[{"type":"text","text":"Back at it."}]`))
	add("broken", "background", "", "failed", asst("m6", `[{"type":"text","text":"panic: nil map"}]`))
	add("gone", "background", "", "done", asst("m7", `[{"type":"text","text":"done"}]`))

	obs, err := e.h.Observe(context.Background(), e.configDir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Observed{}
	for _, o := range obs {
		got[o.Name] = o
	}
	for name, want := range map[string]struct{ word, needs string }{
		"permission": {WordNeedsYou, "it is waiting for your permission to run Bash"},
		"asked":      {WordNeedsYou, "it asks: Should I squash the commits?"},
		"answered":   {WordIdle, ""},
		"finished":   {WordIdle, ""},
		"recovered":  {WordIdle, ""},
		"broken":     {WordFailed, "it stopped on an error: panic: nil map"},
	} {
		if o := got[name]; o.Word != want.word || o.NeedsYou != want.needs {
			t.Errorf("%s: %q %q, want %q %q", name, o.Word, o.NeedsYou, want.word, want.needs)
		}
	}
	if _, ok := got["gone"]; ok {
		t.Error("a finished session that is not running was listed without --all")
	}
	if got["recovered"].Limit != nil {
		t.Error("a limit followed by turns that went through is still shown")
	}
	if all, _ := e.h.Observe(context.Background(), e.configDir, true, now); len(all) != 7 {
		t.Errorf("with --all: %d sessions", len(all))
	}
	if got["asked"].TurnAge == "" {
		t.Error("no age for the last turn")
	}
}

func TestAgeWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now",
		time.Minute:      "1 minute",
		5 * time.Minute:  "5 minutes",
		3 * time.Hour:    "3 hours",
		47 * time.Hour:   "47 hours",
		72 * time.Hour:   "3 days",
	} {
		if got := ageWords(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

// A failed session says the error it stopped on, from the CLI's own API
// error record; the CLI's filler messages (model <synthetic>) are neither
// the model's last words nor usage. A failure older than a day that no
// longer runs is at rest: listed with --all only.
func TestObserveFailures(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	start := now.Add(-time.Hour)
	add := func(id, name, text string, age time.Duration) {
		e.transcript(id, text)
		if age > 0 {
			os.Chtimes(e.transcriptPath(id), now.Add(-age), now.Add(-age))
		}
		e.fake.active[id] = Active{SessionID: id, Name: name, Kind: "background", State: "failed", Cwd: e.cwd, StartedAt: start.UnixMilli()}
	}
	broke := `{"type":"assistant","message":{"id":"m1","model":"opus","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"Fetching the plates."}]}}` + "\n" +
		`{"type":"assistant","isApiErrorMessage":true,"error":"server_error","message":{"id":"e1","model":"<synthetic>","content":[{"type":"text","text":"API Error: Can't reach the API server (ENOTFOUND)\nretry later"}]}}` + "\n" +
		`{"type":"assistant","message":{"id":"s1","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0},"content":[{"type":"text","text":"No response requested."}]}}` + "\n"
	add("dddddddd-0000-4000-8000-000000000001", "fresh", broke, 0)
	add("dddddddd-0000-4000-8000-000000000002", "old", broke, 53*24*time.Hour)

	obs, err := e.h.Observe(context.Background(), e.configDir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Name != "fresh" {
		t.Fatalf("listed %+v, want the fresh failure alone", obs)
	}
	o := obs[0]
	if o.NeedsYou != "it stopped on an error: API Error: Can't reach the API server (ENOTFOUND)" {
		t.Errorf("needsYou %q", o.NeedsYou)
	}
	if o.Usage == nil || len(o.Usage.Models) != 1 || o.Usage.Models[0].Model != "opus" {
		t.Errorf("usage %+v: no <synthetic> entries", o.Usage)
	}
	if want := tokens.Weighted("opus", tokens.Counts{Input: 10, Output: 5}); o.Weighted != want || want == 0 {
		t.Errorf("weighted %v, want %v", o.Weighted, want)
	}
	if all, _ := e.h.Observe(context.Background(), e.configDir, true, now); len(all) != 2 {
		t.Errorf("with --all: %d", len(all))
	}
}

// A session's workers are rows of their own, read from its subagents
// folder: each with the session (or worker) that started it, its
// description, and its state. A foreground worker is done once its call
// has its result; a background one while its launch is open; a nested
// one is under the worker that started it. A finished worker older than
// FinishedWorkersShownFor is counted on its session's row, not listed.
func TestObserveWorkers(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	start := now.Add(-time.Hour)
	const lead = "eeeeeeee-0000-4000-8000-000000000001"
	e.transcript(lead, `{"type":"assistant","message":{"id":"m1","model":"opus","content":[{"type":"tool_use","id":"t1","name":"Agent"}]}}`+"\n"+
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"read"}]}}`+"\n"+
		launchAgent("t2", "a2", "Port the parser", start.Add(time.Minute)))
	e.fake.active[lead] = Active{SessionID: lead, Name: "shop-ui", Kind: "background", Status: "busy", State: "working", Cwd: e.cwd, StartedAt: start.UnixMilli()}
	e.fake.busyFor[lead] = 1
	dir := filepath.Join(strings.TrimSuffix(e.transcriptPath(lead), ".jsonl"), "subagents")
	os.MkdirAll(dir, 0o755)
	worker := func(id, meta, transcript string, age time.Duration) {
		os.WriteFile(filepath.Join(dir, "agent-"+id+".meta.json"), []byte(meta), 0o644)
		p := filepath.Join(dir, "agent-"+id+".jsonl")
		os.WriteFile(p, []byte(transcript), 0o644)
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	used := `{"type":"assistant","message":{"id":"w","model":"opus","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"on it"}]}}` + "\n"
	worker("a1", `{"agentType":"Explore","description":"Read the spec","toolUseId":"t1","spawnDepth":1}`, used, time.Minute)
	worker("a2", `{"agentType":"general-purpose","description":"Port the parser","toolUseId":"t2","spawnDepth":1,"requestShape":"background"}`,
		used+launchAgent("t3", "a3", "Check the tests", start.Add(2*time.Minute)), 0)
	worker("a3", `{"agentType":"general-purpose","description":"Check the tests","toolUseId":"t3","spawnDepth":2,"requestShape":"background","parentAgentId":"a2"}`, used, 0)
	worker("a4", `{"agentType":"Explore","description":"An old survey","toolUseId":"t0","spawnDepth":1,"finished":true}`, used, 3*time.Hour)

	obs, err := e.h.Observe(context.Background(), e.configDir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 4 || obs[0].Session != lead || obs[0].WorkersDone != 1 {
		t.Fatalf("rows %+v", obs)
	}
	got := map[string]Observed{}
	for _, o := range obs[1:] {
		got[o.Session] = o
	}
	for id, want := range map[string]struct{ parent, word, desc string }{
		"a1": {lead, WordDone, "Read the spec"},
		"a2": {lead, WordWorking, "Port the parser"},
		"a3": {"a2", WordWorking, "Check the tests"},
	} {
		o := got[id]
		if o.Parent != want.parent || o.Word != want.word || o.Description != want.desc || o.Kind != KindSubagent || o.Weighted == 0 {
			t.Errorf("%s: %+v", id, o)
		}
	}
	if all, _ := e.h.Observe(context.Background(), e.configDir, true, now); len(all) != 5 {
		t.Errorf("with --all: %d rows", len(all))
	}
}

// A background session a lead started with `claude --bg` through its Bash
// tool is listed after the lead, its link claimed: the launch is within
// seconds before it started, and names it. One started long before any
// launch has no parent.
func TestObserveClaimsTheLeadOfABackgroundSession(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	launch := now.Add(-10 * time.Minute)
	const lead, port, other = "ffffffff-0000-4000-8000-000000000001", "ffffffff-0000-4000-8000-000000000002", "ffffffff-0000-4000-8000-000000000003"
	e.transcript(lead, fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":"m1","model":"opus","content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"cd ~/src/shop && claude --bg -n port-notes \"port the notes\""}}]}}`+"\n", launch.UTC().Format(time.RFC3339Nano)))
	e.transcript(port, `{"type":"assistant","message":{"id":"p1","model":"opus","content":[{"type":"text","text":"porting"}]}}`+"\n")
	e.transcript(other, `{"type":"assistant","message":{"id":"o1","model":"opus","content":[{"type":"text","text":"earlier"}]}}`+"\n")
	e.fake.active[lead] = Active{SessionID: lead, Name: "shop-lead", Kind: "interactive", Status: "idle", Cwd: e.cwd, StartedAt: now.Add(-time.Hour).UnixMilli()}
	e.fake.active[port] = Active{SessionID: port, Name: "port-notes", Kind: "background", Status: "idle", Cwd: e.cwd, StartedAt: launch.Add(3 * time.Second).UnixMilli()}
	e.fake.active[other] = Active{SessionID: other, Name: "older", Kind: "background", Status: "idle", Cwd: e.cwd, StartedAt: now.Add(-2 * time.Hour).UnixMilli()}

	obs, err := e.h.Observe(context.Background(), e.configDir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, o := range obs {
		pos[o.Session] = i
	}
	if p := obs[pos[port]]; p.Parent != lead || !p.ParentClaimed {
		t.Errorf("port-notes: parent %q claimed %v", p.Parent, p.ParentClaimed)
	}
	if pos[port] != pos[lead]+1 {
		t.Errorf("port-notes is not right after its lead: %v", pos)
	}
	if o := obs[pos[other]]; o.Parent != "" {
		t.Errorf("an older session was given a parent: %q", o.Parent)
	}
	if got := claudeLaunches([]byte(`[{"type":"tool_use","name":"Bash","input":{"command":"claude --bg --name='a b' x; echo claude"}}]`), launch); len(got) != 1 || got[0].Name != "a b" {
		t.Errorf("launches %+v", got)
	}
}

// A transcript is read as it grows: each scan reads only the lines written
// since the last, a line still being written waits for the next, and the
// result is what one scan of the whole file gives. A shorter file is read
// again from its start.
func TestScanReadsOnlyWhatIsNew(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	turn := func(id string, in int64) string {
		return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"model":"opus","usage":{"input_tokens":%d,"output_tokens":1},"content":[{"type":"text","text":"turn %s"}]}}`+"\n", id, in, id)
	}
	scan := func() sessionScan {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return scanSessionCached(p, time.Time{}, info)
	}
	os.WriteFile(p, []byte(turn("a", 10)), 0o644)
	if s := scan(); s.usage[0].InputTokens != 10 {
		t.Fatalf("first %+v", s.usage)
	}
	whole := turn("b", 20)
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(whole[:30]) // half a line
	f.Close()
	if s := scan(); s.usage[0].InputTokens != 10 || s.lastText != "turn a" {
		t.Fatalf("a half-written line was read: %+v %q", s.usage, s.lastText)
	}
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(whole[30:] + turn("b", 20)) // the rest, and the same message again
	f.Close()
	got := scan()
	if want := scanSession(p, time.Time{}); got.usage[0] != want.usage[0] || got.lastText != want.lastText || got.usage[0].InputTokens != 30 {
		t.Fatalf("grown %+v %q, whole file %+v %q", got.usage, got.lastText, want.usage, want.lastText)
	}
	os.WriteFile(p, []byte(turn("c", 5)), 0o644)
	if s := scan(); s.usage[0].InputTokens != 5 {
		t.Fatalf("a shorter file was not read again: %+v", s.usage)
	}
}
