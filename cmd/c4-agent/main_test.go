package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
