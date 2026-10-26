package main

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

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/agent"
)

// fakeClaude is a shell script that stands in for the claude CLI. It keeps
// its running sessions in a file: one "<session> <status>" line each.
const fakeClaude = `#!/bin/sh
# Models the real CLI (Claude Code 2.1.302, checked 2026-10-18): running
# sessions in $state as "<full id> <status>", known background sessions in
# $state.known; --bg --resume prints "backgrounded · <short id> · <name>";
# a running session, or a known one resumed with extra flags, gets a copy;
# stop takes only the short ID.
state="$FAKE_CLAUDE_STATE"
touch "$state" "$state.known"
case "$1" in
agents)
  printf '['
  sep=''
  while read -r id st; do
    [ -n "$id" ] || continue
    printf '%s{"sessionId":"%s","status":"%s","kind":"background","pid":99,"name":"x","cwd":"/"}' "$sep" "$id" "$st"
    sep=','
  done < "$state"
  if [ "$3" = --all ]; then
    while read -r id; do
      [ -n "$id" ] || continue
      grep -q "^$id " "$state" && continue
      printf '%s{"sessionId":"%s","status":"stopped","kind":"background","pid":0,"name":"x","cwd":"/"}' "$sep" "$id"
      sep=','
    done < "$state.known"
  fi
  printf ']\n'
  ;;
--bg)
  id="$3"; shift 3
  echo "$id $*" >> "$state.calls"
  if grep -q "^$id " "$state" || { grep -q "^$id$" "$state.known" && [ "$#" -gt 1 ]; }; then
    n=$(wc -l < "$state.calls" | tr -d ' ')
    copy="9999999$n-0000-4000-8000-00000000000$n"
    echo "$copy busy" >> "$state"; echo "$copy" >> "$state.known"
    echo "backgrounded · $(echo $copy | cut -c1-8) · copy"
    exit 0
  fi
  echo "$id idle" >> "$state"
  grep -q "^$id$" "$state.known" || echo "$id" >> "$state.known"
  echo "backgrounded · $(echo $id | cut -c1-8) · x"
  ;;
stop)
  if [ "${#2}" -ne 8 ]; then echo "No job matching $2"; exit 1; fi
  grep -v "^$2" "$state" > "$state.tmp"; mv "$state.tmp" "$state"
  echo "stopped $2"
  ;;
esac
`

const sid = "11111111-2222-3333-4444-555555555555"

func setup(t *testing.T) (state, work string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(root, "state")
	work = filepath.Join(root, "work")
	cfg := filepath.Join(root, "cfg")
	os.MkdirAll(work, 0o755)
	tr := filepath.Join(cfg, "projects", agent.ProjectSlug(work), sid+".jsonl")
	os.MkdirAll(filepath.Dir(tr), 0o755)
	os.WriteFile(tr, []byte(`{"agentName":"lead","turn":1}`+"\n"), 0o644)
	t.Setenv("C4_AGENT_HOME", filepath.Join(root, "agents"))
	t.Setenv("C4_AGENT_CLAUDE", bin)
	t.Setenv("C4_AGENT_WORKSPACE", "none")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("FAKE_CLAUDE_STATE", state)
	return state, work
}

func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLIRegisterResumeYield(t *testing.T) {
	state, work := setup(t)
	if code, out, errs := runCLI(t, "", "find", "--agent-name", "lead"); code != 0 || !strings.Contains(out, sid) {
		t.Fatalf("find: %d %q %q", code, out, errs)
	}
	if code, _, errs := runCLI(t, "", "register", "--name", "lead", "--session", sid, "--cwd", work, "--flag", "-n", "--flag", "lead"); code != 0 {
		t.Fatalf("register: %d %s", code, errs)
	}
	code, out, errs := runCLI(t, "", "resume", "lead", "carry", "on")
	if code != 0 || !strings.Contains(out, "epoch 1") {
		t.Fatalf("resume: %d %q %q", code, out, errs)
	}
	calls, _ := os.ReadFile(state + ".calls")
	if got := strings.TrimSpace(string(calls)); got != sid+" -n lead carry on" {
		t.Fatalf("claude was called with %q", got)
	}
	// Running now: a second resume is refused with exit code 3, and the CLI
	// is not called again (no copy is started).
	if code, _, errs := runCLI(t, "", "resume", "lead"); code != 3 || !strings.Contains(errs, "would start a copy") {
		t.Fatalf("second resume: %d %q", code, errs)
	}
	if calls2, _ := os.ReadFile(state + ".calls"); !bytes.Equal(calls, calls2) {
		t.Fatal("a second resume reached the CLI")
	}
	if code, out, _ := runCLI(t, "", "ls"); code != 0 || !strings.Contains(out, "lead") || !strings.Contains(out, "idle") {
		t.Fatalf("ls: %d %q", code, out)
	}
	if code, out, errs := runCLI(t, "", "yield", "lead", "--timeout", "1s"); code != 0 || !strings.Contains(out, "stopped") {
		t.Fatalf("yield: %d %q %q", code, out, errs)
	}
	if code, out, _ := runCLI(t, "", "ls"); code != 0 || !strings.Contains(out, "stopped") {
		t.Fatalf("ls after yield: %q", out)
	}
	// Resumed again, it is a known background session: no flags, so the
	// CLI continues it under the same ID instead of starting a copy.
	if code, out, errs := runCLI(t, "", "resume", "lead"); code != 0 || !strings.Contains(out, "epoch 2") {
		t.Fatalf("second resume: %d %q %q", code, out, errs)
	}
	lines := strings.Split(strings.TrimSpace(readFile(state+".calls")), "\n")
	if last := lines[len(lines)-1]; last != sid {
		t.Fatalf("second resume called claude with %q, want the ID alone", last)
	}
	if strings.Contains(readFile(state), "9999999") {
		t.Fatal("a copy was started")
	}
	if code, out, errs := runCLI(t, "", "yield", "lead", "--timeout", "1s"); code != 0 || !strings.Contains(out, "stopped") {
		t.Fatalf("second yield: %d %q %q", code, out, errs)
	}
	if code, out, _ := runCLI(t, "", "verify", "lead"); code != 0 || !strings.Contains(out, "verified") {
		t.Fatalf("verify: %q", out)
	}
	// The hook never fails the agent, even on bad input.
	if code, _, _ := runCLI(t, "garbage", "checkpoint", "--hook"); code != 0 {
		t.Fatalf("hook exit %d", code)
	}
}

func TestCLIDetectsCopy(t *testing.T) {
	state, work := setup(t)
	runCLI(t, "", "register", "--name", "lead", "--session", sid, "--cwd", work)
	// A race: the session is running, but started after the liveness
	// check, so agents --json (reading an empty state) does not list it,
	// while --bg (reading the real state) finds it and starts a copy. The
	// harness sees the copy's ID, stops it, and fails.
	os.WriteFile(state, []byte(sid+" idle\n"), 0o644)
	hidden := filepath.Join(filepath.Dir(state), "hidden")
	os.WriteFile(hidden, nil, 0o644)
	t.Setenv("FAKE_CLAUDE_STATE", hidden)
	wrapper := filepath.Join(filepath.Dir(state), "claude-race")
	real := os.Getenv("C4_AGENT_CLAUDE")
	// agents reads the hidden state; --bg and stop act on the real one.
	os.WriteFile(wrapper, []byte("#!/bin/sh\nif [ \"$1\" != agents ]; then FAKE_CLAUDE_STATE="+state+" exec "+real+" \"$@\"; fi\nexec "+real+" \"$@\"\n"), 0o755)
	t.Setenv("C4_AGENT_CLAUDE", wrapper)
	code, out, errs := runCLI(t, "", "resume", "lead")
	if code != 4 || !strings.Contains(errs, "started 9999999") || !strings.Contains(out, "stopped") {
		t.Fatalf("copy: %d %q %q", code, out, errs)
	}
	if strings.Contains(readFile(state), "9999999") {
		t.Fatal("the copy was left running")
	}
}

func TestReorder(t *testing.T) {
	got := strings.Join(reorder([]string{"lead", "--timeout", "5m"}), " ")
	if got != "--timeout 5m lead" {
		t.Fatal(got)
	}
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

// fakeUnit stands in for c4-unit: it records its arguments and exits with
// $FAKE_UNIT_CODE (10 is "stop"); steer prints $FAKE_STEER, the unread
// messages as JSON lines (exit 1 when there are none), and is not
// recorded; ask prints $FAKE_ASK_ID, the question's id.
const fakeUnit = `#!/bin/sh
if [ "$1" = ask ]; then
  echo "$@" >> "$FAKE_UNIT_LOG"
  echo "${FAKE_ASK_ID:-3#1}"
  exit 0
fi
if [ "$1" = steer ]; then
  [ -n "$FAKE_STEER" ] || exit 1
  printf '%s\n' "$FAKE_STEER"
  exit 0
fi
echo "$@" >> "$FAKE_UNIT_LOG"
exit "${FAKE_UNIT_CODE:-0}"
`

// In a farm unit the Stop hook announces the turn's end as a safe point,
// for a member (with its checkpoint as the ref) and for a session that is
// not one (with its session ID); when the farm asks the unit to stop, the
// hook tells Claude Code to end the session. Outside a unit it announces
// nothing, and the hook always exits 0.
func TestHookSafepoint(t *testing.T) {
	_, work := setup(t)
	root := t.TempDir()
	unit, log := filepath.Join(root, "c4-unit"), filepath.Join(root, "unit.log")
	if err := os.WriteFile(unit, []byte(fakeUnit), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("C4_UNIT_BIN", unit)
	t.Setenv("FAKE_UNIT_LOG", log)
	if code, _, e := runCLI(t, "", "register", "--name", "lead", "--session", sid, "--cwd", work); code != 0 {
		t.Fatalf("register: %d %s", code, e)
	}
	member := `{"session_id":"` + sid + `","hook_event_name":"Stop"}`
	stranger := `{"session_id":"99999999-0000-0000-0000-000000000000","hook_event_name":"Stop"}`

	// outside a unit: no safe point
	if code, out, _ := runCLI(t, member, "checkpoint", "--hook"); code != 0 || out != "" {
		t.Fatalf("outside a unit: %d %q", code, out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("c4-unit ran outside a unit")
	}

	t.Setenv("C4_PODINFO", filepath.Join(root, "annotations"))
	// a member: continue
	if code, out, _ := runCLI(t, member, "checkpoint", "--hook"); code != 0 || out != "" {
		t.Fatalf("member, continue: %d %q", code, out)
	}
	// not a member, asked to stop
	t.Setenv("FAKE_UNIT_CODE", "10")
	code, out, _ := runCLI(t, stranger, "checkpoint", "--hook")
	if code != 0 || !strings.Contains(out, `"continue":false`) {
		t.Fatalf("stop: %d %q", code, out)
	}
	// c4-unit failing otherwise never fails the hook
	t.Setenv("FAKE_UNIT_CODE", "1")
	if code, out, _ := runCLI(t, stranger, "checkpoint", "--hook"); code != 0 || out != "" {
		t.Fatalf("c4-unit error: %d %q", code, out)
	}

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) != 3 {
		t.Fatalf("calls %q", calls)
	}
	if !strings.HasPrefix(calls[0], "safepoint --checkpoint "+sid+"#") || !strings.Contains(calls[0], ":c4") {
		t.Fatalf("member ref %q", calls[0])
	}
	if calls[1] != "safepoint --checkpoint 99999999-0000-0000-0000-000000000000 --wait 8s" {
		t.Fatalf("stranger ref %q", calls[1])
	}
}

// fakeUnitEnv logs the unit paths c4-unit was given.
const fakeUnitEnv = `#!/bin/sh
echo "out=$C4_OUTPUT control=$C4_CONTROL podinfo=$C4_PODINFO $*" >> "$FAKE_UNIT_LOG"
exit "${FAKE_UNIT_CODE:-0}"
`

// A member a host runner runs has no $C4_PODINFO: the hook finds the unit
// directory the runner recorded on the member and points c4-unit there; a
// stop decision ends the session the same way.
func TestHookSafepointHostUnit(t *testing.T) {
	_, work := setup(t)
	root := t.TempDir()
	unit, log := filepath.Join(root, "c4-unit"), filepath.Join(root, "unit.log")
	if err := os.WriteFile(unit, []byte(fakeUnitEnv), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("C4_UNIT_BIN", unit)
	t.Setenv("FAKE_UNIT_LOG", log)
	t.Setenv("C4_PODINFO", "")
	if code, _, e := runCLI(t, "", "register", "--name", "lead", "--session", sid, "--cwd", work); code != 0 {
		t.Fatalf("register: %d %s", code, e)
	}
	st := &agent.Store{Dir: os.Getenv("C4_AGENT_HOME")}
	m, err := st.Get("lead")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "units", "pod-uid-1")
	m.UnitDir = dir
	if err := st.Put(m); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_UNIT_CODE", "10")
	code, out, _ := runCLI(t, `{"session_id":"`+sid+`","hook_event_name":"Stop"}`, "checkpoint", "--hook")
	if code != 0 || !strings.Contains(out, `"continue":false`) {
		t.Fatalf("stop: %d %q", code, out)
	}
	b, _ := os.ReadFile(log)
	u := agent.UnitDirsAt(dir)
	want := "out=" + u.Out + " control=" + u.Control + " podinfo=" + u.PodInfo + " safepoint --checkpoint " + sid + "#"
	if !strings.HasPrefix(string(b), want) {
		t.Fatalf("c4-unit called with %q, want prefix %q", b, want)
	}
}

// In a unit, the Stop hook writes the attempt's token use to the unit's
// .c4/usage.json, per model, from the transcript: rewritten whole each
// turn, from where the attempt began.
func TestHookUsage(t *testing.T) {
	_, work := setup(t)
	root := t.TempDir()
	unit := filepath.Join(root, "c4-unit")
	if err := os.WriteFile(unit, []byte(fakeUnit), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	t.Setenv("C4_UNIT_BIN", unit)
	t.Setenv("FAKE_UNIT_LOG", filepath.Join(root, "unit.log"))
	t.Setenv("C4_PODINFO", filepath.Join(root, "annotations"))
	t.Setenv("C4_OUTPUT", out)
	if code, _, e := runCLI(t, "", "register", "--name", "lead", "--session", sid, "--cwd", work); code != 0 {
		t.Fatalf("register: %d %s", code, e)
	}
	tr := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", agent.ProjectSlug(work), sid+".jsonl")
	add := func(id string, outTokens int) {
		f, err := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(f, `{"type":"assistant","message":{"id":%q,"model":"claude-opus-5-5","usage":{"input_tokens":2,"output_tokens":%d,"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}}`+"\n", id, outTokens)
		f.Close()
	}
	read := func() agent.UsageDoc {
		var d agent.UsageDoc
		b, err := os.ReadFile(filepath.Join(out, ".c4", "usage.json"))
		if err != nil || json.Unmarshal(b, &d) != nil {
			t.Fatalf("usage.json: %v %s", err, b)
		}
		return d
	}
	hook := `{"session_id":"` + sid + `","transcript_path":"` + tr + `","hook_event_name":"Stop"}`
	add("a", 300)
	if code, _, e := runCLI(t, hook, "checkpoint", "--hook"); code != 0 {
		t.Fatalf("hook: %d %s", code, e)
	}
	if d := read(); len(d.Models) != 1 || d.Models[0].OutputTokens != 300 || d.Models[0].CacheWriteTokens != 50 {
		t.Fatalf("first turn %+v", d)
	}
	add("b", 200)
	add("b", 200) // the same message, written twice
	runCLI(t, hook, "checkpoint", "--hook")
	runCLI(t, hook, "checkpoint", "--hook") // a hook that runs twice
	if d := read(); d.Models[0].OutputTokens != 500 || d.Models[0].InputTokens != 4 {
		t.Fatalf("two turns %+v", d)
	}
	if r := read().Reasoning; r == nil || r.Does != "model" || r.Model != "claude-opus-5-5" {
		t.Fatalf("reasoning %+v", r)
	}
}

// At the end of a turn that goes on, the hook gives the session what
// people sent it while it worked, keeping it from stopping; a turn the
// farm stops leaves the messages for the attempt that resumes it.
func TestHookSteer(t *testing.T) {
	_, work := setup(t)
	root := t.TempDir()
	unit := filepath.Join(root, "c4-unit")
	if err := os.WriteFile(unit, []byte(fakeUnit), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	os.MkdirAll(filepath.Join(out, ".c4"), 0o755)
	t.Setenv("C4_UNIT_BIN", unit)
	t.Setenv("FAKE_UNIT_LOG", filepath.Join(root, "unit.log"))
	t.Setenv("C4_PODINFO", filepath.Join(root, "annotations"))
	t.Setenv("C4_OUTPUT", out)
	if code, _, e := runCLI(t, "", "register", "--name", "lead", "--session", sid, "--cwd", work); code != 0 {
		t.Fatalf("register: %d %s", code, e)
	}
	hook := `{"session_id":"` + sid + `","hook_event_name":"Stop"}`

	// nothing new: the turn ends as it would
	if _, stdout, _ := runCLI(t, hook, "checkpoint", "--hook"); stdout != "" {
		t.Fatalf("no messages: %q", stdout)
	}
	// a message: the session carries on with it
	t.Setenv("FAKE_STEER", `{"seq":1,"at":"2026-10-20T05:40:00Z","actor":"dana","text":"use the v3 plates"}`)
	_, stdout, _ := runCLI(t, hook, "checkpoint", "--hook")
	var got struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal([]byte(stdout), &got) != nil || got.Decision != "block" || !strings.Contains(got.Reason, "use the v3 plates") {
		t.Fatalf("a message: %q", stdout)
	}
	// the farm stops the turn: no message is delivered, so none is lost
	t.Setenv("FAKE_UNIT_CODE", "10")
	_, stdout, _ = runCLI(t, hook, "checkpoint", "--hook")
	if strings.Contains(stdout, "v3 plates") || !strings.Contains(stdout, `"continue":false`) {
		t.Fatalf("a stop: %q", stdout)
	}
}

// After each tool call, what was sent to a block's unit while it worked
// reaches it at once, not only at the end of its turn: the PostToolUse
// hook hands it to the model, which is told its tool call ran as usual.
// The hook starts c4-unit only when the control directory holds a message
// past what the unit has seen, so a tool call with nothing new costs no
// process. A session outside a unit hears nothing, and a stop the farm
// asked for at that step leaves the message for the attempt that resumes
// it.
func TestHookSteerAtToolCall(t *testing.T) {
	_, work := setup(t)
	root := t.TempDir()
	unit, log := filepath.Join(root, "c4-unit"), filepath.Join(root, "unit.log")
	if err := os.WriteFile(unit, []byte(fakeUnit), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("C4_UNIT_BIN", unit)
	t.Setenv("FAKE_UNIT_LOG", log)
	t.Setenv("C4_PODINFO", "")
	// the fake prints this whenever it is run, so the steps below say
	// whether the hook ran it at all
	t.Setenv("FAKE_STEER", `{"seq":1,"at":"2026-10-26T10:40:00Z","actor":"noor","text":"keep the C extension out of scope"}`)
	hook := `{"session_id":"` + sid + `","hook_event_name":"PostToolUse","tool_name":"Bash"}`
	if code, _, e := runCLI(t, "", "register", "--name", "lead", "--session", sid, "--cwd", work); code != 0 {
		t.Fatalf("register: %d %s", code, e)
	}
	// a session that is not a unit's member hears nothing
	if code, stdout, _ := runCLI(t, hook, "checkpoint", "--hook"); code != 0 || stdout != "" {
		t.Fatalf("outside a unit: %d %q", code, stdout)
	}

	st := &agent.Store{Dir: os.Getenv("C4_AGENT_HOME")}
	m, err := st.Get("lead")
	if err != nil {
		t.Fatal(err)
	}
	m.UnitDir = filepath.Join(root, "units", "pod-uid-1")
	if err := st.Put(m); err != nil {
		t.Fatal(err)
	}
	u := agent.UnitDirsAt(m.UnitDir)
	for _, d := range []string{filepath.Join(u.Out, ".c4"), u.Control} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	steer := func(msgs string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(u.Control, "steer"), []byte(msgs), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// in a unit never steered: no message, and no c4-unit
	if _, stdout, _ := runCLI(t, hook, "checkpoint", "--hook"); stdout != "" {
		t.Fatalf("no steer file: %q", stdout)
	}

	// a message: it reaches the model after this tool call
	steer(`[{"seq":1,"at":"2026-10-26T10:40:00Z","actor":"noor","text":"keep the C extension out of scope"}]`)
	code, stdout, _ := runCLI(t, hook, "checkpoint", "--hook")
	var got struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &got) != nil || got.Decision != "block" ||
		!strings.Contains(got.Reason, "noor: keep the C extension out of scope") || !strings.Contains(got.Reason, "Your last tool call ran as usual") {
		t.Fatalf("a message at a tool call: %d %q", code, stdout)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Fatalf("a tool call announced a safe point: %q", b)
	}

	// seen (c4-unit marks it): the next tool call says nothing
	if err := os.WriteFile(filepath.Join(u.Out, ".c4", "steer-seen"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stdout, _ := runCLI(t, hook, "checkpoint", "--hook"); stdout != "" {
		t.Fatalf("a message already seen: %q", stdout)
	}

	// the farm asked the unit to stop: the step ends the turn, and the
	// new message waits
	steer(`[{"seq":1,"at":"2026-10-26T10:40:00Z","actor":"noor","text":"keep the C extension out of scope"},{"seq":2,"at":"2026-10-26T10:41:00Z","actor":"noor","text":"use the v3 plates"}]`)
	t.Setenv("FAKE_STEER", `{"seq":2,"at":"2026-10-26T10:41:00Z","actor":"noor","text":"use the v3 plates"}`)
	if err := os.WriteFile(u.PodInfo, []byte(c4v1.AnnotationYield+`="capacity: a deadline needs the slot"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stdout, _ = runCLI(t, hook, "checkpoint", "--hook")
	if strings.Contains(stdout, "v3 plates") || !strings.Contains(stdout, "end your turn now") {
		t.Fatalf("a stop at a tool call: %q", stdout)
	}
}

// A turn's question to another unit goes to the farm (c4-unit ask) and is
// recorded open; a turn's answer to a question it was asked goes back
// (c4-unit answer). A question that reaches the block comes with how to
// answer it, and the answer to its own question closes it.
func TestHookAsksAndAnswers(t *testing.T) {
	_, work := setup(t)
	root := t.TempDir()
	unit, log := filepath.Join(root, "c4-unit"), filepath.Join(root, "unit.log")
	if err := os.WriteFile(unit, []byte(fakeUnit), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	os.MkdirAll(filepath.Join(out, ".c4"), 0o755)
	t.Setenv("C4_UNIT_BIN", unit)
	t.Setenv("FAKE_UNIT_LOG", log)
	t.Setenv("C4_PODINFO", filepath.Join(root, "annotations"))
	t.Setenv("C4_OUTPUT", out)
	if code, _, e := runCLI(t, "", "register", "--name", "lead", "--session", sid, "--cwd", work); code != 0 {
		t.Fatalf("register: %d %s", code, e)
	}
	hook := `{"session_id":"` + sid + `","hook_event_name":"Stop"}`
	put := func(name, doc string) {
		if err := os.WriteFile(filepath.Join(out, ".c4", name), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logged := func() string { b, _ := os.ReadFile(log); return string(b) }

	put("ask.json", `{"to": "unit:plan", "ask": "Which module owns the tokenizer?"}`)
	t.Setenv("FAKE_ASK_ID", "4#1")
	runCLI(t, hook, "checkpoint", "--hook")
	if !strings.Contains(logged(), "ask --ask Which module owns the tokenizer? --to unit:plan") {
		t.Fatalf("c4-unit ran %q", logged())
	}
	a, ok := agent.ReadAsking(out)
	if !ok || a.ID != "4#1" || a.To != "unit:plan" {
		t.Fatalf("open question %+v %v", a, ok)
	}

	put("answer.json", `{"ask": "2#3", "answer": "the lexer package"}`)
	runCLI(t, hook, "checkpoint", "--hook")
	if !strings.Contains(logged(), "answer --ask 2#3 --answer the lexer package") {
		t.Fatalf("c4-unit ran %q", logged())
	}

	t.Setenv("FAKE_STEER", `{"seq":2,"at":"2026-10-20T06:00:00Z","text":"question 2#4 from 2: which encoding?\n(answer with: c4-unit answer --ask 2#4 --answer TEXT, or --cannot WHY)","ask":{"id":"2#4","from":"2"}}`+"\n"+
		`{"seq":3,"at":"2026-10-20T06:01:00Z","text":"answer to 4#1 from plan: the lexer package","answer":{"id":"4#1","from":"plan"}}`)
	_, stdout, _ := runCLI(t, hook, "checkpoint", "--hook")
	var got struct{ Reason string }
	json.Unmarshal([]byte(stdout), &got)
	if !strings.Contains(got.Reason, `answer.json`) || !strings.Contains(got.Reason, `"ask": "2#4"`) || strings.Contains(got.Reason, "c4-unit answer") {
		t.Fatalf("the question's message: %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "answer to 4#1 from plan") {
		t.Fatalf("the answer's message: %q", got.Reason)
	}
	if _, ok := agent.ReadAsking(out); ok {
		t.Fatal("the answered question is still open")
	}
}

// The text view says what each session has used, and that a session with
// no transcript has written nothing yet.
func TestPrintObserved(t *testing.T) {
	var b strings.Builder
	printObserved(&b, []agent.Observed{
		{Name: "lead", Word: agent.WordNeedsYou, NeedsYou: "it asks: Which pool?", Transcript: "t", Usage: &agent.ObservedUse{Models: []agent.ModelUsage{{Model: "opus", InputTokens: 12000, OutputTokens: 400}}}},
		{Name: "new", Word: agent.WordIdle},
		{Name: "big", Word: agent.WordWorking, Transcript: "t", TurnAge: "5 minutes", BackgroundOpen: 2, Usage: &agent.ObservedUse{Models: []agent.ModelUsage{{Model: "opus", InputTokens: 400_000, OutputTokens: 2_400_000, CacheReadTokens: 1_400_000_000}}}},
	})
	for _, want := range []string{"it asks: Which pool? · wrote 400, read 12k tokens", "no transcript yet", "last wrote 5 minutes ago, 2 background tasks running · wrote 2.4M, read 1.4B tokens"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "Nothing needs you") {
		t.Error("said nothing needs you with a question waiting")
	}
}

// unregister takes a member out and says so in one line; its checkpoint
// log is kept aside, so registering the name again starts a new chain.
// An unknown name is an error naming it. No settings file is touched.
func TestCLIUnregister(t *testing.T) {
	_, work := setup(t)
	if code, _, errs := runCLI(t, "", "register", "--name", "lead", "--session", sid, "--cwd", work); code != 0 {
		t.Fatalf("register: %d %s", code, errs)
	}
	settings := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
	os.WriteFile(settings, []byte(`{"hooks":{}}`), 0o644)
	code, out, errs := runCLI(t, "", "unregister", "--name", "lead")
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "unregistered lead") {
		t.Fatalf("unregister: %d %q %q", code, out, errs)
	}
	if _, out, _ := runCLI(t, "", "ls"); strings.Contains(out, "lead") {
		t.Errorf("still listed:\n%s", out)
	}
	if b, _ := os.ReadFile(settings); string(b) != `{"hooks":{}}` {
		t.Errorf("a settings file was touched: %s", b)
	}
	kept, _ := filepath.Glob(filepath.Join(os.Getenv("C4_AGENT_HOME"), "members", "lead.unregistered-*.checkpoints.jsonl"))
	if len(kept) != 1 {
		t.Errorf("the checkpoint log was not kept aside: %v", kept)
	}
	if code, _, errs := runCLI(t, "", "register", "--name", "lead", "--session", sid, "--cwd", work); code != 0 {
		t.Fatalf("registering the name again: %d %s", code, errs)
	}
	if code, _, errs := runCLI(t, "", "unregister", "--name", "nobody"); code == 0 || !strings.Contains(errs, "no member named nobody") {
		t.Errorf("an unknown name: %d %q", code, errs)
	}
}

// A runner a service starts with the system's bare PATH finds its tools:
// its own folder first, then the usual install folders that exist, each
// once.
func TestServicePath(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755)
	t.Setenv("PATH", "/usr/bin:/bin")
	servicePath(home)
	parts := filepath.SplitList(os.Getenv("PATH"))
	exe, _ := os.Executable()
	if parts[0] != filepath.Dir(exe) {
		t.Errorf("its own folder is not first: %v", parts)
	}
	if !slices.Contains(parts, filepath.Join(home, ".local", "bin")) || slices.Contains(parts, filepath.Join(home, ".claude", "local")) {
		t.Errorf("install folders: %v", parts)
	}
	servicePath(home)
	if again := filepath.SplitList(os.Getenv("PATH")); len(again) != len(parts) {
		t.Errorf("a second call added folders again: %v", again)
	}
}
