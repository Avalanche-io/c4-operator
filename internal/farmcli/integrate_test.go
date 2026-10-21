package farmcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The integration's answer is asked once, at a terminal, of each one found
// and unanswered; flags answer it in scripts; with no terminal nothing is
// asked or recorded, and the install is minimal.
func TestChooseAsksOnceAtATerminal(t *testing.T) {
	found := []Detected{{Name: Claude, Found: true}}
	var out bytes.Buffer
	cfg := &Config{}
	if on, err := Choose(cfg, found, "", false, false, Out{W: &out, In: strings.NewReader("y\n")}); err != nil || len(on) != 0 || len(cfg.Integrations) != 0 {
		t.Fatalf("no terminal: %v %v %v", on, err, cfg.Integrations)
	}
	if on, err := Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("\n")}); err != nil || len(on) != 1 ||
		!strings.Contains(out.String(), IntegrateQuestion+" [Y/n]") {
		t.Fatalf("terminal, enter: %v %v\n%s", on, err, out.String())
	}
	out.Reset()
	if on, _ := Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("n\n")}); len(on) != 1 || out.Len() != 0 {
		t.Errorf("answered once, asked again: %v\n%s", on, out.String())
	}
	if on, _ := Choose(cfg, found, "", true, true, Out{W: &out}); len(on) != 0 || cfg.Integrations[Claude] {
		t.Errorf("--no-integrate: %v", on)
	}
	if on, _ := Choose(cfg, found, "claude", false, false, Out{W: &out}); len(on) != 1 {
		t.Errorf("--integrate=claude: %v", on)
	}
	if _, err := Choose(cfg, found, "blender", false, false, Out{W: &out}); err == nil || !strings.Contains(err.Error(), "c4 farm integrations lists them") {
		t.Errorf("unknown: %v", err)
	}
	// Claude Code not found is not asked about
	cfg = &Config{}
	if on, _ := Choose(cfg, []Detected{{Name: Claude}}, "", false, true, Out{W: &out, In: strings.NewReader("y\n")}); len(on) != 0 || len(cfg.Integrations) != 0 {
		t.Errorf("not found: %v %v", on, cfg.Integrations)
	}
}

// On adds the farm's MCP server to Claude Code at user scope and runs the
// runner; Off removes the server, stops the service, and takes c4-agent's
// hooks out of every member's folder, keeping the person's own.
func TestIntegrationOnAndOff(t *testing.T) {
	dir, cd, folder := t.TempDir(), t.TempDir(), t.TempDir()
	calls := filepath.Join(dir, "calls")
	claude := filepath.Join(dir, "claude")
	os.WriteFile(claude, []byte("#!/bin/sh\necho \"$@\" >>"+calls+"\n"), 0o755)
	session := "0b5e7c52-8a6f-4d0e-9b1c-3f2a4e6d8c10"
	agent, _ := fakeAgent(t, cd, folder, session)
	settings := filepath.Join(folder, ".claude", "settings.local.json")
	var out bytes.Buffer
	if err := AddAgent(context.Background(), Out{W: &out}, AgentsArgs{Name: "scout", ConfigDir: cd, Yes: true, AgentBin: agent}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(settings)
	os.WriteFile(settings, bytes.Replace(b, []byte(`"hooks": {`), []byte(`"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": "guard.sh"}]}], `), 1), 0o644)

	ran := false
	svc := &AgentService{FarmBin: "/f", AgentBin: agent, Home: t.TempDir(), OS: "linux", Ctl: func(context.Context, string, ...string) error { return nil }}
	in := Integration{FarmBin: "/usr/local/bin/c4-farm", ClaudeBin: claude, AgentBin: agent, ConfigDir: cd, Service: svc,
		Runner: func(context.Context, Out) error { ran = true; return nil }}
	out.Reset()
	if err := in.On(context.Background(), Out{W: &out}); err != nil || !ran || !strings.Contains(out.String(), "has the farm's tools") {
		t.Fatalf("on: %v %v\n%s", err, ran, out.String())
	}
	if c, _ := os.ReadFile(calls); !strings.Contains(string(c), "mcp add --scope user c4-farm -- /usr/local/bin/c4-farm mcp") {
		t.Errorf("claude was asked:\n%s", c)
	}
	out.Reset()
	if err := in.Off(context.Background(), Out{W: &out}); err != nil {
		t.Fatal(err)
	}
	if c, _ := os.ReadFile(calls); !strings.Contains(string(c), "mcp remove --scope user c4-farm") {
		t.Errorf("claude was asked:\n%s", c)
	}
	b, _ = os.ReadFile(settings)
	if strings.Contains(string(b), "checkpoint") || !strings.Contains(string(b), "guard.sh") || !strings.Contains(out.String(), "loses PostToolUse and Stop") {
		t.Errorf("off:\n%s\n%s", out.String(), b)
	}
}
