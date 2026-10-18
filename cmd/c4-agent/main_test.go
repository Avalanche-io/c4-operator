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
state="$FAKE_CLAUDE_STATE"
touch "$state"
case "$1" in
agents)
  printf '['
  sep=''
  while read -r id st; do
    [ -n "$id" ] || continue
    printf '%s{"sessionId":"%s","status":"%s","kind":"background","pid":99,"name":"x","cwd":"/"}' "$sep" "$id" "$st"
    sep=','
  done < "$state"
  printf ']\n'
  ;;
--bg)
  id="$3"
  if grep -q "^$id " "$state"; then
    echo "Session $id is already running; started a copy as 99999999-0000-0000-0000-000000000000"
    exit 0
  fi
  echo "$id idle" >> "$state"
  echo "$*" >> "$state.calls"
  echo "Resumed in the background: $id"
  ;;
stop)
  grep -v "^$2 " "$state" > "$state.tmp"; mv "$state.tmp" "$state"
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
	if got := strings.TrimSpace(string(calls)); got != "--bg --resume "+sid+" -n lead carry on" {
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
	// harness sees the copy's ID and says so.
	os.WriteFile(state, []byte(sid+" idle\n"), 0o644)
	hidden := filepath.Join(filepath.Dir(state), "hidden")
	os.WriteFile(hidden, nil, 0o644)
	t.Setenv("FAKE_CLAUDE_STATE", hidden)
	wrapper := filepath.Join(filepath.Dir(state), "claude-race")
	real := os.Getenv("C4_AGENT_CLAUDE")
	os.WriteFile(wrapper, []byte("#!/bin/sh\nif [ \"$1\" = --bg ]; then FAKE_CLAUDE_STATE="+state+" exec "+real+" \"$@\"; fi\nexec "+real+" \"$@\"\n"), 0o755)
	t.Setenv("C4_AGENT_CLAUDE", wrapper)
	code, out, errs := runCLI(t, "", "resume", "lead")
	if code != 4 || !strings.Contains(out, "99999999-0000-0000-0000-000000000000") {
		t.Fatalf("copy: %d %q %q", code, out, errs)
	}
}

func TestReorder(t *testing.T) {
	got := strings.Join(reorder([]string{"lead", "--timeout", "5m"}), " ")
	if got != "--timeout 5m lead" {
		t.Fatal(got)
	}
}
