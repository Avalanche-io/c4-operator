package farmcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAgent is a c4-agent that answers find, register, hooks and ls as the
// real one does, and records what it was asked in calls.
func fakeAgent(t *testing.T, session string) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin, calls = filepath.Join(dir, "c4-agent"), filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$@" >>` + calls + `
case "$1" in
find) if [ "$3" = scout ]; then printf '` + session + `\t/cfg/projects/x/` + session + `.jsonl\n'; fi ;;
register) echo "$3: session $5, phase 0, epoch 0, transcript /cfg/projects/x/$5.jsonl" ;;
hooks) printf '{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + "BIN" + ` checkpoint --hook","timeout":30}]}]}}\n' ;;
ls) printf 'MEMBER  STATE  SESSION\nscout   idle   ` + session + `\n' ;;
esac
`
	script = strings.ReplaceAll(script, "BIN", bin)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

// agents add finds the session by the agent's name, registers it through
// c4-agent, and merges c4-agent's Stop hook into settings.json once,
// keeping the person's keys and hooks in their order; a second add
// changes nothing.
func TestAddAgentRegistersAndInstallsTheHookOnce(t *testing.T) {
	session := "0b5e7c52-8a6f-4d0e-9b1c-3f2a4e6d8c10"
	bin, calls := fakeAgent(t, session)
	cd := t.TempDir()
	settings := filepath.Join(cd, "settings.json")
	before := `{"model": "opus", "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}]}, "env": {"X": "1"}}`
	os.WriteFile(settings, []byte(before), 0o644)

	a := AgentsArgs{Name: "scout", ConfigDir: cd, Yes: true, AgentBin: bin}
	var out bytes.Buffer
	if err := AddAgent(context.Background(), Out{W: &out}, a); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "member    scout · session "+session) || !strings.Contains(out.String(), "installed the hook") {
		t.Errorf("output:\n%s", out.String())
	}
	if c, _ := os.ReadFile(calls); !strings.Contains(string(c), "find --agent-name scout --config-dir "+cd) ||
		!strings.Contains(string(c), "register --name scout --session "+session+" --config-dir "+cd) {
		t.Errorf("c4-agent was asked:\n%s", c)
	}
	b, _ := os.ReadFile(settings)
	s := string(b)
	model, hooks, env := strings.Index(s, `"model"`), strings.Index(s, `"hooks"`), strings.Index(s, `"env"`)
	if model < 0 || hooks < model || env < hooks || !strings.Contains(s, "guard.sh") ||
		!strings.Contains(s, `"command": "`+bin+` checkpoint --hook"`) || strings.Index(s, `"Stop"`) < strings.Index(s, `"PreToolUse"`) {
		t.Errorf("settings.json:\n%s", s)
	}
	if bk, _ := os.ReadFile(settings + ".c4-backup"); string(bk) != before {
		t.Errorf("backup %q", bk)
	}

	out.Reset()
	if err := AddAgent(context.Background(), Out{W: &out}, a); err != nil || !strings.Contains(out.String(), "hook      already in") {
		t.Fatalf("second add: %v\n%s", err, out.String())
	}
	if again, _ := os.ReadFile(settings); string(again) != s {
		t.Error("a second add changed settings.json")
	}
	out.Reset()
	if err := ListAgents(context.Background(), Out{W: &out}, a); err != nil || !strings.Contains(out.String(), "scout   idle") {
		t.Errorf("ls: %v\n%s", err, out.String())
	}
}

// Declining the hook keeps the member, leaves settings.json as it was, and
// says how to install it later; an agent no session names is refused with
// the flag that names one.
func TestAddAgentAsksBeforeTheHook(t *testing.T) {
	session := "1c6f8d63-9b70-4e1f-8c2d-4a3b5f7e9d21"
	bin, _ := fakeAgent(t, session)
	cd := t.TempDir()
	a := AgentsArgs{Name: "lab", Session: session, ConfigDir: cd, AgentBin: bin}
	var out bytes.Buffer
	err := AddAgent(context.Background(), Out{W: &out, In: strings.NewReader("n\n")}, a)
	if err == nil || !strings.Contains(err.Error(), "c4 farm agents add again installs it") || !strings.Contains(out.String(), "Install it? [Y/n]") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(cd, "settings.json")); !os.IsNotExist(err) {
		t.Error("settings.json was written after no")
	}
	a.Session = ""
	if err := AddAgent(context.Background(), Out{W: &out}, a); err == nil || !strings.Contains(err.Error(), "--session ID") {
		t.Errorf("no session: %v", err)
	}
}
