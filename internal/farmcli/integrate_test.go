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
		!strings.HasSuffix(out.String(), "c4 farm integrate off takes it all out. [Y/n] ") {
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

// An application already said yes to is asked once, at a terminal, about
// the versions C4 isn't fully in, and a yes covers only those; a no is
// kept, so up doesn't ask again. With no terminal or inside an agent
// nothing is asked or changed, and a line says how to bring it up to date.
func TestChooseOffersUpdates(t *testing.T) {
	found := []Detected{{Name: "blender", Found: true, Question: "Add the C4 Farm panel?", Versions: []dcc.Found{
		{Name: "blender", Version: "4.5", Title: "Blender 4.5", Status: dcc.StatusOlder},
		{Name: "blender", Version: "5.0", Title: "Blender 5.0", Status: dcc.StatusAvailable},
		{Name: "blender", Version: "5.1", Title: "Blender 5.1", Status: dcc.StatusIncomplete},
		{Name: "blender", Version: "5.2", Title: "Blender 5.2", Status: dcc.StatusInstalled},
	}}}
	var out bytes.Buffer
	cfg := &Config{Integrations: map[string]bool{"blender": true}}
	on, err := Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("y\n")})
	if err != nil || strings.Join(on, " ") != "blender@4.5 blender@5.0 blender@5.1" {
		t.Fatalf("yes: %v %v", on, err)
	}
	if s := out.String(); s != "C4 Farm panel: update it in Blender 4.5 (an earlier c4 release's), finish it in Blender 5.1 (an install was interrupted) and add it to Blender 5.0? c4 farm integrate blender --show says each file first. [y/N] " {
		t.Errorf("asked: %q", s)
	}

	// enter is no, kept per version: not asked again
	out.Reset()
	if on, _ := Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("\n")}); len(on) != 0 || cfg.Integrations["blender@4.5"] || !cfg.Integrations["blender"] {
		t.Fatalf("enter: %v %v", on, cfg.Integrations)
	}
	out.Reset()
	if on, _ := Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("y\n")}); len(on) != 0 || out.Len() != 0 {
		t.Errorf("asked again after a no: %v\n%s", on, out.String())
	}

	// the input ended: nothing recorded, asked next time
	cfg = &Config{Integrations: map[string]bool{"blender": true}}
	if on, _ := Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("")}); len(on) != 0 || len(cfg.Integrations) != 1 {
		t.Errorf("no answer: %v %v", on, cfg.Integrations)
	}

	// no terminal, or inside an agent: nothing asked or changed, one line
	for _, agent := range []string{"", "1"} {
		t.Setenv("CLAUDECODE", agent)
		out.Reset()
		on, _ := Choose(cfg, found, "", false, agent != "", Out{W: &out, In: strings.NewReader("y\n")})
		if len(on) != 0 || len(cfg.Integrations) != 1 || out.String() != "Blender 4.5, Blender 5.0 and Blender 5.1: c4 farm integrate blender brings C4's panel up to date there\n" {
			t.Errorf("agent %q: %v %v %q", agent, on, cfg.Integrations, out.String())
		}
	}
	t.Setenv("CLAUDECODE", "")

	// said no to the application: never asked about its versions
	cfg = &Config{Integrations: map[string]bool{"blender": false}}
	out.Reset()
	if on, _ := Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("y\n")}); len(on) != 0 || out.Len() != 0 {
		t.Errorf("said no: %v %q", on, out.String())
	}
	// --integrate=blender turns it on, every version
	if on, _ := Choose(cfg, found, "blender", false, true, Out{W: &out}); strings.Join(on, " ") != "blender" {
		t.Errorf("--integrate=blender: %v", on)
	}
}

// These tests run as a person would, whatever shell runs them (an agent's
// included); the ones about an agent set CLAUDECODE themselves.
func init() { os.Unsetenv("CLAUDECODE") }

// Nothing that changes this machine runs from inside an agent, even with
// --yes, nor without a terminal unless --yes; each refusal names the
// command for the person to run.
func TestConsent(t *testing.T) {
	if err := Consent(false, true, "c4 farm integrate on"); err != nil {
		t.Errorf("at a terminal: %v", err)
	}
	if err := Consent(true, false, "c4 farm integrate on"); err != nil {
		t.Errorf("--yes in a script: %v", err)
	}
	if err := Consent(false, false, "c4 farm integrate on"); err == nil || !strings.Contains(err.Error(), "run c4 farm integrate on at one, or pass --yes") {
		t.Errorf("no terminal: %v", err)
	}
	t.Setenv("CLAUDECODE", "1")
	if err := Consent(true, true, "c4 farm integrate on"); err == nil || !strings.Contains(err.Error(), "you run it, not an agent: in your own terminal, c4 farm integrate on") {
		t.Errorf("inside an agent with --yes: %v", err)
	}
	cfg := &Config{}
	found := []Detected{{Name: Claude, Found: true, Question: "q?", DefaultYes: true, Runner: true}}
	if on, err := Choose(cfg, found, "", false, true, Out{W: &bytes.Buffer{}, In: strings.NewReader("y\n")}); err != nil || len(on) != 0 || len(cfg.Integrations) != 0 {
		t.Errorf("an agent at up was asked or turned something on: %v %v %v", on, err, cfg.Integrations)
	}
	if _, err := Choose(cfg, found, "claude", false, true, Out{W: &bytes.Buffer{}}); err == nil {
		t.Error("an agent gave itself --integrate=claude")
	}
	cd, folder := t.TempDir(), t.TempDir()
	bin, calls := fakeAgent(t, cd, folder, "0b5e7c52-8a6f-4d0e-9b1c-3f2a4e6d8c10")
	if err := AddAgent(context.Background(), Out{W: &bytes.Buffer{}}, AgentsArgs{Name: "scout", ConfigDir: cd, Yes: true, AgentBin: bin}); err == nil ||
		!strings.Contains(err.Error(), "you run it, not an agent") {
		t.Errorf("agents add --yes inside an agent: %v", err)
	}
	if c, _ := os.ReadFile(calls); strings.Contains(string(c), "register") {
		t.Error("registered from inside an agent")
	}
}

// A yes given at login names no service, so up asks about the runner on
// its own; a yes at up covers both.
func TestRunnerHasItsOwnYes(t *testing.T) {
	cfg := &Config{Integrations: map[string]bool{Claude: true}}
	found := []Detected{{Name: Claude, Found: true, Question: "q?", DefaultYes: true, Runner: true}}
	var out bytes.Buffer
	Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("n\n")})
	if on, asked := cfg.Integrations[RunnerKey]; !asked || on || !strings.Contains(out.String(), "Run your agents on this machine too?") {
		t.Errorf("runner after login's yes: %v %v\n%s", on, asked, out.String())
	}
	cfg = &Config{}
	Choose(cfg, found, "", false, true, Out{W: &out, In: strings.NewReader("\n")})
	if !cfg.Integrations[Claude] || !cfg.Integrations[RunnerKey] {
		t.Errorf("up's yes: %v", cfg.Integrations)
	}
}

// Off takes out the MCP entry only if c4 added it; one the person added
// stays, and says so.
func TestOffKeepsAnMCPEntryC4DidNotAdd(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	claude := filepath.Join(dir, "claude")
	os.WriteFile(claude, []byte("#!/bin/sh\necho \"$@\" >>"+calls+"\n"), 0o755)
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	os.WriteFile(filepath.Join(cfgDir, ".claude.json"), []byte(`{"mcpServers":{"c4-farm":{"command":"/somewhere/else/c4-farm"}}}`), 0o644)
	svc := &AgentService{FarmBin: "/f", AgentBin: "/a", Home: t.TempDir(), OS: "linux", Ctl: func(context.Context, string, ...string) error { return nil }}
	in := Integration{FarmBin: "/f", ClaudeBin: claude, AgentBin: "", Service: svc, Record: map[string]bool{}}
	var out bytes.Buffer
	if err := in.Off(context.Background(), Out{W: &out}); err != nil {
		t.Fatal(err)
	}
	if c, _ := os.ReadFile(calls); strings.Contains(string(c), "mcp remove") || !strings.Contains(out.String(), "c4 did not add it, so it is left as it was") {
		t.Errorf("off removed the person's entry: %s\n%s", c, out.String())
	}
}

// When the input ends (a script, /dev/null, a closed pipe), nobody has
// answered: nothing is turned on, and nothing is recorded, so the person is
// asked when they are there.
func TestChooseEndOfInputIsNoAnswer(t *testing.T) {
	cfg := &Config{}
	found := []Detected{
		{Name: Claude, Found: true, Question: "q?", DefaultYes: true, Runner: true},
		{Name: "blender", Found: true, Question: "panel?"},
	}
	on, err := Choose(cfg, found, "", false, true, Out{W: &bytes.Buffer{}, In: strings.NewReader("")})
	if err != nil || len(on) != 0 || len(cfg.Integrations) != 0 {
		t.Errorf("end of input: %v %v %v", on, err, cfg.Integrations)
	}
}
