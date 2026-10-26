package mcpserver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The server finds the lead it serves in Claude Code's own session file,
// by the CLI's pid or its parent's, and falls back on the session's
// environment when the file is missing or says nothing it knows.
func TestReadLead(t *testing.T) {
	cfg := t.TempDir()
	os.MkdirAll(filepath.Join(cfg, "sessions"), 0o755)
	os.WriteFile(filepath.Join(cfg, "sessions", "4242.json"), []byte(`{"pid":4242,"sessionId":"aaaaaaaa-0000-4000-8000-000000000001","cwd":"/src/textkit","kind":"interactive"}`), 0o644)
	os.WriteFile(filepath.Join(cfg, "sessions", "5151.json"), []byte(`{"pid":5151,"id":"a new shape"}`), 0o644)
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if l := readLead(env(map[string]string{"CLAUDE_CONFIG_DIR": cfg}), 4242); l.Session != "aaaaaaaa-0000-4000-8000-000000000001" || l.Cwd != "/src/textkit" {
		t.Fatalf("by the parent: %+v", l)
	}
	if l := readLead(env(map[string]string{"CLAUDE_CONFIG_DIR": cfg, "CLAUDE_PID": "4242"}), 9999); l.Session != "aaaaaaaa-0000-4000-8000-000000000001" {
		t.Fatalf("by CLAUDE_PID: %+v", l)
	}
	l := readLead(env(map[string]string{"CLAUDE_CONFIG_DIR": cfg, "CLAUDE_CODE_SESSION_ID": "bbbbbbbb-0000-4000-8000-000000000002"}), 5151)
	if wd, _ := os.Getwd(); l.Session != "bbbbbbbb-0000-4000-8000-000000000002" || l.Cwd != wd {
		t.Fatalf("a file in a new shape: %+v", l)
	}
	if l := readLead(env(map[string]string{"CLAUDE_CONFIG_DIR": cfg, "CLAUDE_PID": "../../etc/passwd"}), 1); l.Session != "" {
		t.Fatalf("a pid that is a path: %+v", l)
	}
	if !inBlock(env(map[string]string{"C4_AGENT_BLOCK": "1"})) || inBlock(env(nil)) {
		t.Fatal("the block marker")
	}
}

// A worker's base is the lead's commit as a full ID, never a ref that can
// move; what the lead has not committed is named, so the lead can commit
// it first; a base that is not a commit, or looks like an option, is
// refused.
func TestLeadBase(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if _, _, err := leadBase(ctx, repo, ""); err == nil {
		t.Fatal("not a repository")
	}
	git("init", "-q", "-b", "main")
	if _, _, err := leadBase(ctx, repo, ""); err == nil || !strings.Contains(err.Error(), "commit first") {
		t.Fatalf("no commit yet: %v", err)
	}
	os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "start")
	head := git("rev-parse", "HEAD")
	sha, dirty, err := leadBase(ctx, repo, "")
	if err != nil || sha != head || len(sha) != 40 || len(dirty) != 0 {
		t.Fatalf("HEAD: %q %v %v", sha, dirty, err)
	}
	if sha, _, err := leadBase(ctx, repo, "main"); err != nil || sha != head {
		t.Fatalf("a branch name, resolved: %q %v", sha, err)
	}
	os.MkdirAll(filepath.Join(repo, "docs", "designs"), 0o755)
	os.WriteFile(filepath.Join(repo, "docs", "designs", "parser.md"), []byte("# parser\n"), 0o644)
	if _, dirty, _ := leadBase(ctx, repo, ""); len(dirty) != 1 || !strings.Contains(dirty[0], "docs/") {
		t.Fatalf("an uncommitted design: %v", dirty)
	}
	for _, bad := range []string{"nope", "--output=/tmp/x", "main extra"} {
		if _, _, err := leadBase(ctx, repo, bad); err == nil || !strings.Contains(err.Error(), "is not a commit") {
			t.Errorf("base %q: %v", bad, err)
		}
	}
}
