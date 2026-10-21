package farmcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/agent"
)

// noClaude is a Claude CLI with nothing running.
type noClaude struct{}

func (noClaude) Active(context.Context, string, bool) ([]agent.Active, error) { return nil, nil }
func (noClaude) Resume(context.Context, string, string, string, []string, string) (string, error) {
	return "", nil
}
func (noClaude) Stop(context.Context, string, string) error { return nil }

// agents add finds the session by the agent's name, registers it, and
// merges the Stop hook into settings.json once, keeping the person's
// keys and hooks in their order; a second add changes nothing.
func TestAddAgentRegistersAndInstallsTheHookOnce(t *testing.T) {
	cd := t.TempDir()
	session := "0b5e7c52-8a6f-4d0e-9b1c-3f2a4e6d8c10"
	proj := filepath.Join(cd, "projects", "-Users-noor-shows")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, session+".jsonl"), []byte(`{"type":"user","agentName":"scout","cwd":"/Users/noor/shows","sessionId":"`+session+`"}`+"\n"), 0o644)
	settings := filepath.Join(cd, "settings.json")
	before := `{"model": "opus", "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}]}, "env": {"X": "1"}}`
	os.WriteFile(settings, []byte(before), 0o644)

	a := AgentsArgs{Name: "scout", ConfigDir: cd, Yes: true, AgentBin: "/usr/local/bin/c4-agent",
		Harness: agent.New(&agent.Store{Dir: t.TempDir()}, noClaude{})}
	var out bytes.Buffer
	if err := AddAgent(context.Background(), Out{W: &out}, a); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "member    scout · session "+session) || !strings.Contains(out.String(), "installed the hook") {
		t.Errorf("output:\n%s", out.String())
	}
	b, _ := os.ReadFile(settings)
	s := string(b)
	model, hooks, env := strings.Index(s, `"model"`), strings.Index(s, `"hooks"`), strings.Index(s, `"env"`)
	if model < 0 || hooks < model || env < hooks || !strings.Contains(s, "guard.sh") ||
		!strings.Contains(s, `"command": "/usr/local/bin/c4-agent checkpoint --hook"`) || strings.Index(s, `"Stop"`) < strings.Index(s, `"PreToolUse"`) {
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
	if err := ListAgents(context.Background(), Out{W: &out}, a); err != nil || !strings.Contains(out.String(), "scout") {
		t.Errorf("ls: %v\n%s", err, out.String())
	}
}

// Declining the hook keeps the member, leaves settings.json as it was, and
// says how to install it later.
func TestAddAgentAsksBeforeTheHook(t *testing.T) {
	cd := t.TempDir()
	session := "1c6f8d63-9b70-4e1f-8c2d-4a3b5f7e9d21"
	proj := filepath.Join(cd, "projects", "-tmp")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, session+".jsonl"), []byte(`{"type":"user","cwd":"/tmp","sessionId":"`+session+`"}`+"\n"), 0o644)
	a := AgentsArgs{Name: "lab", Session: session, ConfigDir: cd, AgentBin: "/opt/c4-agent",
		Harness: agent.New(&agent.Store{Dir: t.TempDir()}, noClaude{})}
	var out bytes.Buffer
	err := AddAgent(context.Background(), Out{W: &out, In: strings.NewReader("n\n")}, a)
	if err == nil || !strings.Contains(err.Error(), "c4 farm agents add again installs it") || !strings.Contains(out.String(), "Install it? [Y/n]") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(cd, "settings.json")); !os.IsNotExist(err) {
		t.Error("settings.json was written after no")
	}
}
