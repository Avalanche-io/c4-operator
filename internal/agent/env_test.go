package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleanEnv is the environment C4 gives every member, as shipped.
const cleanEnv = "../../config/agent-env/base"

// The version of an environment is the C4 ID of its listing, content only:
// the shipped clean environment is the version the prototype recorded for
// it (c42dfDuBy...), and copying or touching it changes nothing.
func TestEnvIDOfTheCleanEnvironment(t *testing.T) {
	const want = "c42dfDuByUVzHRPgg1cs1Q8G98YA5NZ6kigBjqqaqqaS7uH1Az4Pm5fanH3AHr1ChbwtB5sCGwrxJ7i82TcHV9UKBw"
	if id, err := EnvID(cleanEnv); err != nil || id != want {
		t.Fatalf("clean environment %s %v, want %s", id, err, want)
	}
	moved := filepath.Join(t.TempDir(), "copy")
	if err := envCopyTree(cleanEnv, moved, nil); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(moved, "CLAUDE.md"), 0o600)
	if id, _ := EnvID(moved); id != want {
		t.Fatalf("a copy is %s", id)
	}
	os.WriteFile(filepath.Join(moved, "CLAUDE.md"), []byte("changed\n"), 0o644)
	if id, _ := EnvID(moved); id == want {
		t.Fatal("a change kept the version")
	}
}

// Filling a member's config directory copies the environment (not its own
// c4-member.json), marks the version, and gives the member the environment's
// MCP configuration alone. A version other than the one asked for, and a
// person's own ~/.claude*, are refused.
func TestFillEnv(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "members", "lead", "config")
	env, err := FillEnv(cleanEnv, dst, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"settings.json", "CLAUDE.md", "mcp.json"} {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Errorf("%s not filled: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, envMeta)); err == nil {
		t.Error("the environment's own file was copied into the config directory")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, envMark)); strings.TrimSpace(string(b)) != env.ID {
		t.Errorf("mark %q, want %s", b, env.ID)
	}
	if got := strings.Join(env.Flags, " "); got != "--mcp-config "+filepath.Join(dst, "mcp.json")+" --strict-mcp-config" {
		t.Errorf("flags %q", got)
	}
	if _, err := FillEnv(cleanEnv, t.TempDir(), "c4notthisone"); err == nil || !strings.Contains(err.Error(), "not c4notthisone") {
		t.Errorf("a different version: %v", err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, personal := range []string{".claude", ".claude-b"} {
		if _, err := FillEnv(cleanEnv, filepath.Join(home, personal), ""); err == nil || !strings.Contains(err.Error(), "person's own") {
			t.Errorf("%s: %v", personal, err)
		}
	}
}

// A member registered with an environment runs in its own config
// directory, filled from it; an existing session is imported into it with
// its subagents, the source left as it was, and its version is on the member
// and each checkpoint. A session running where it came from is not imported.
func TestRegisterInAnEnvironmentWithImport(t *testing.T) {
	e := newEnv(t)
	e.transcript(sess, turn(1))
	sub := filepath.Join(strings.TrimSuffix(e.transcriptPath(sess), ".jsonl"), "subagents", "agent-a1.jsonl")
	os.MkdirAll(filepath.Dir(sub), 0o755)
	os.WriteFile(sub, []byte(turn(1)), 0o644)
	before, _ := os.ReadFile(e.transcriptPath(sess))
	ctx := context.Background()

	e.fake.active[sess] = Active{SessionID: sess, Status: "idle", Kind: "interactive"}
	_, err := e.h.Register(ctx, RegisterOptions{Name: "lead", Session: sess, Cwd: e.cwd, Env: cleanEnv, Import: e.configDir})
	if !errors.Is(err, ErrLive) {
		t.Fatalf("imported a running session: %v", err)
	}
	delete(e.fake.active, sess)

	m, err := e.h.Register(ctx, RegisterOptions{Name: "lead", Session: sess, Cwd: e.cwd, Env: cleanEnv, Import: e.configDir})
	if err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(e.h.Store.Dir, "members", "lead", "config")
	if m.ConfigDir != own || !strings.HasPrefix(m.Transcript, own) || m.Env == "" {
		t.Fatalf("member %+v", m)
	}
	if got, _ := os.ReadFile(m.Transcript); string(got) != string(before) {
		t.Fatalf("imported transcript %q", got)
	}
	if _, err := os.Stat(filepath.Join(strings.TrimSuffix(m.Transcript, ".jsonl"), "subagents", "agent-a1.jsonl")); err != nil {
		t.Errorf("subagents not imported: %v", err)
	}
	if after, _ := os.ReadFile(e.transcriptPath(sess)); string(after) != string(before) {
		t.Error("the source transcript changed")
	}
	if cp, _ := e.h.Store.Last("lead"); cp == nil || cp.Env != m.Env {
		t.Fatalf("checkpoint %+v, want env %s", cp, m.Env)
	}
	if !strings.Contains(strings.Join(m.Flags, " "), "--strict-mcp-config") {
		t.Errorf("flags %v", m.Flags)
	}
	// registering again with the same environment keeps the imported session
	if _, err := e.h.Register(ctx, RegisterOptions{Name: "lead", Session: sess, Cwd: e.cwd, Env: cleanEnv, Import: e.configDir}); err != nil {
		t.Fatalf("again: %v", err)
	}
}
