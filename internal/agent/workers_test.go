package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bashCall(id, command string, at time.Time) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"opus","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q}}]}}`+"\n",
		at.UTC().Format(time.RFC3339Nano), "m-"+id, id, command)
}

func bashResult(id string, at time.Time) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"backgrounded"}]}}`+"\n",
		at.UTC().Format(time.RFC3339Nano), id)
}

func byName(obs []Observed) map[string]Observed {
	out := map[string]Observed{}
	for _, o := range obs {
		out[o.Name] = o
	}
	return out
}

// A launch names a command that is claude itself, with --bg of its own,
// its -n from that command alone and the folder it cd'd into; claude in an
// echo or a grep is no launch.
func TestClaudeLaunches(t *testing.T) {
	got := claudeLaunches([]byte(`[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"cd ~/src/shop && claude --bg -n \"port notes\" 'go' ; echo claude --bg -n fake | grep claude --bg; FOO=1 claude --bg --name=x; claude -p hi -n y"}}]`), time.Now())
	if len(got) != 2 || got[0].Name != "port notes" || got[0].Dir != "~/src/shop" || got[1].Name != "x" || got[1].ID != "b1" {
		t.Fatalf("launches %+v", got)
	}
}

// Several unnamed launches link their sessions one to one, in the order
// they started; a launch claims only sessions in its own folder, started
// between its call and its result, and never a session the farm runs.
func TestClaimsAFanOut(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	at := now.Add(-10 * time.Minute)
	const lead, other = "11111111-0000-4000-8000-000000000001", "11111111-0000-4000-8000-000000000002"
	e.transcript(lead, bashCall("b1", "claude --bg 'a'", at)+bashResult("b1", at.Add(2*time.Second))+
		bashCall("b2", "claude --bg 'b'", at.Add(3*time.Second))+bashResult("b2", at.Add(5*time.Second)))
	e.transcript(other, `{"type":"assistant","message":{"id":"o","model":"opus","content":[{"type":"text","text":"idle"}]}}`+"\n")
	e.fake.active[lead] = Active{SessionID: lead, Name: "lead", Kind: "interactive", Status: "idle", Cwd: e.cwd, StartedAt: now.Add(-time.Hour).UnixMilli()}
	e.fake.active[other] = Active{SessionID: other, Name: "other-lead", Kind: "interactive", Status: "idle", Cwd: e.cwd, StartedAt: now.Add(-time.Hour).UnixMilli()}
	add := func(n int, name, cwd string, started time.Time) {
		id := fmt.Sprintf("22222222-0000-4000-8000-%012d", n)
		e.transcript(id, `{"type":"assistant","message":{"id":"x","model":"opus","content":[{"type":"text","text":"on it"}]}}`+"\n")
		e.fake.active[id] = Active{SessionID: id, Name: name, Kind: "background", Status: "idle", Cwd: cwd, StartedAt: started.UnixMilli()}
	}
	add(1, "first", e.cwd, at.Add(time.Second))
	add(2, "second", e.cwd, at.Add(4*time.Second))
	add(3, "elsewhere", t.TempDir(), at.Add(4*time.Second))
	add(4, "late", e.cwd, at.Add(time.Minute))
	obs, err := e.h.Observe(context.Background(), e.configDir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(obs)
	for name, parent := range map[string]string{"first": lead, "second": lead, "elsewhere": "", "late": ""} {
		if got[name].Parent != parent || got[name].ParentClaimed != (parent != "") {
			t.Errorf("%s: parent %q claimed %v, want %q", name, got[name].Parent, got[name].ParentClaimed, parent)
		}
	}
}

// A nested worker whose starter is old and not listed is still judged by
// its starter's transcript; a worker launched before its session's process
// started is stopped, not done; a teammate is told by its records and is
// idle, or waiting on its lead after a question; a foreground worker at a
// permission prompt is waiting on its person; and the session's own count
// of background work leaves out its workers listed.
func TestWorkerStates(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	start := now.Add(-time.Hour)
	const lead = "33333333-0000-4000-8000-000000000001"
	e.transcript(lead, launchAgent("t-bg", "bg", "Port the parser", start.Add(time.Minute))+
		`{"type":"assistant","message":{"id":"m2","model":"opus","content":[{"type":"tool_use","id":"t-fg","name":"Agent"}]}}`+"\n")
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
	stamp := func(at time.Time, rest string) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"opus","content":%s}}`+"\n", at.UTC().Format(time.RFC3339Nano), at.String(), rest)
	}
	// an old finished parent, not listed, with a foreground child whose call has its result
	worker("old", `{"description":"An old survey","toolUseId":"t0","spawnDepth":1,"finished":true}`,
		stamp(start.Add(time.Minute), `[{"type":"tool_use","id":"t-child","name":"Agent"}]`)+
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t-child","content":"ok"}]}}`+"\n", 3*time.Hour)
	worker("child", `{"description":"Its child","toolUseId":"t-child","spawnDepth":2,"parentAgentId":"old"}`, stamp(start.Add(2*time.Minute), `[{"type":"text","text":"done"}]`), time.Minute)
	// launched before the lead's process started: the last run's
	worker("before", `{"description":"Before the resume","toolUseId":"t-old","spawnDepth":1,"requestShape":"background"}`,
		stamp(start.Add(-time.Hour), `[{"type":"text","text":"working"}]`), time.Minute)
	// a teammate, by its records, idle after a question
	worker("mate", `{"description":"A teammate","name":"reviewer","toolUseId":"t-mate","spawnDepth":1}`,
		`{"type":"user","teamName":"shop","agentName":"reviewer","timestamp":"`+start.Add(time.Minute).UTC().Format(time.RFC3339Nano)+`","message":{"role":"user","content":"review it"}}`+"\n"+
			stamp(start.Add(2*time.Minute), `[{"type":"text","text":"Should I look at the tests too?"}]`), time.Minute)
	// a foreground worker at a permission prompt
	worker("fg", `{"description":"Run the build","toolUseId":"t-fg","spawnDepth":1}`,
		stamp(start.Add(3*time.Minute), `[{"type":"tool_use","id":"x1","name":"Bash"}]`), time.Minute)
	// the background worker, under way
	worker("bg", `{"description":"Port the parser","toolUseId":"t-bg","spawnDepth":1,"requestShape":"background"}`, stamp(start.Add(time.Minute), `[{"type":"text","text":"porting"}]`), 0)

	obs, err := e.h.Observe(context.Background(), e.configDir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(obs)
	if l := got["shop-ui"]; l.BackgroundOpen != 0 || l.WorkersDone != 1 {
		t.Errorf("lead: background %d, workers done %d", l.BackgroundOpen, l.WorkersDone)
	}
	for name, want := range map[string]struct{ word, waitsOn, parent string }{
		"Its child":         {WordDone, "", "old"},
		"Before the resume": {WordStopped, "", lead},
		"reviewer":          {WordWaiting, "lead", lead},
		"Run the build":     {WordWaiting, "person", lead},
		"Port the parser":   {WordWorking, "", lead},
	} {
		o, ok := got[name]
		if !ok || o.Word != want.word || o.WaitsOn != want.waitsOn || o.Parent != want.parent {
			t.Errorf("%s: %q waits on %q under %q (listed %v), want %+v", name, o.Word, o.WaitsOn, o.Parent, ok, want)
		}
	}
	if got["reviewer"].Kind != KindTeammate || got["Port the parser"].Depth != 1 || got["Its child"].Depth != 2 {
		t.Errorf("kinds and depths: %+v %+v", got["reviewer"], got["Its child"])
	}
}

// A transcript replaced by another file, not shorter, is read again from
// its start, not from where the old one ended.
func TestScanKnowsAReplacedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	line := func(id string, in int64) string {
		return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"model":"opus","usage":{"input_tokens":%d,"output_tokens":1},"content":[{"type":"text","text":"x"}]}}`+"\n", id, in)
	}
	os.WriteFile(p, []byte(line("a", 10)), 0o644)
	info, _ := os.Stat(p)
	scanSessionCached(p, time.Time{}, info)
	tmp := p + ".new"
	os.WriteFile(tmp, []byte(line("b", 20)+line("c", 30)), 0o644)
	os.Rename(tmp, p)
	info, _ = os.Stat(p)
	if s := scanSessionCached(p, time.Time{}, info); s.usage[0].InputTokens != 50 {
		t.Fatalf("a replaced file read from mid-way: %+v", s.usage)
	}
}

// Free text that leaves the machine carries no home path.
func TestHomeless(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"it stopped on an error: open " + home + "/src/app/x.go": "it stopped on an error: open ~/src/app/x.go",
		"read /Users/noor/notes.txt and /home/lee/a":             "read ~/notes.txt and ~/a",
	} {
		if got := homeless(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
