package farmcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/integrations/dcc"
)

// The integration's answer is asked once, at a terminal, of each one found
// and unanswered; flags answer it in scripts; with no terminal nothing is
// asked or recorded, and the install is minimal.
func TestChooseAsksOnceAtATerminal(t *testing.T) {
	found := []Detected{{Name: Claude, Found: true, Question: IntegrateQuestion(true), DefaultYes: true}}
	var out bytes.Buffer
	cfg := &Config{}
	if on, err := Choose(cfg, found, "", false, false, Out{W: &out, In: strings.NewReader("y\n")}); err != nil || len(on) != 0 || len(cfg.Integrations) != 0 {
		t.Fatalf("no terminal: %v %v %v", on, err, cfg.Integrations)
	}
	if on, err := Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("\n")}); err != nil || len(on) != 1 ||
		!strings.Contains(out.String(), ".claude.json), and lets c4 farm agents add save") || !strings.Contains(out.String(), "a background service (") ||
		!strings.HasSuffix(out.String(), "c4 farm integrate claude off takes it all out. [Y/n] ") {
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
	if q := IntegrateQuestion(false); strings.Contains(q, "background service") {
		t.Errorf("a login's question names a service: %s", q)
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
	if err := in.On(context.Background(), Out{W: &out}); err != nil || !ran || !strings.Contains(out.String(), "has the farm's tools (c4-farm in ") {
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

// An application's integration is asked about with no as the default (it
// changes the application's own setup), Claude Code's with yes; one whose
// versions all have C4 already is not asked; integrations lists each
// version with its state.
func TestChooseAppsDefaultNo(t *testing.T) {
	found := []Detected{
		{Name: Claude, Found: true, Question: "claude?", DefaultYes: true},
		{Name: "blender", Found: true, Question: "Add C4 to Blender 5.2?", Versions: []dcc.Found{{Name: "blender", Version: "5.2", Title: "Blender 5.2", Status: dcc.StatusAvailable}}},
		{Name: "nuke", Found: true, Versions: []dcc.Found{{Name: "nuke", Version: "17.0", Title: "Nuke 17.0", Status: dcc.StatusInstalled}}},
	}
	cfg := &Config{}
	var out bytes.Buffer
	on, err := Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("\n\n")})
	if err != nil || len(on) != 1 || on[0] != Claude || cfg.Integrations["blender"] {
		t.Fatalf("enter, enter: %v %v %v", on, err, cfg.Integrations)
	}
	if s := out.String(); !strings.Contains(s, "claude? [Y/n]") || !strings.Contains(s, "Add C4 to Blender 5.2? [y/N]") || strings.Contains(s, "Nuke") {
		t.Errorf("asked:\n%s", s)
	}
	if _, asked := cfg.Integrations["nuke"]; asked {
		t.Error("nuke, with C4 in every version, was answered")
	}
	out.Reset()
	Integrations(cfg, found, Out{W: &out})
	if s := out.String(); !strings.Contains(s, "blender@5.2  available") || !strings.Contains(s, "nuke@17.0    installed") {
		t.Errorf("integrations:\n%s", s)
	}
}
