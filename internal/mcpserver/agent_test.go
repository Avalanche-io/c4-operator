package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// leadRepo is a lead's repository with one commit, as the lead's folder.
func leadRepo(t *testing.T) (dir, head string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "start")
	return dir, git("rev-parse", "HEAD")
}

// c4_agent starts workers as the lead's session: the agent header names
// it, each worker's base is the lead's HEAD as a full commit, and the call
// is sent again with its key until the queue has added the workers. It
// waits for them and answers with their results; with background, at once
// with how to wait. Uncommitted work is warned of; inside a block it is
// refused before anything is sent.
func TestAgentTool(t *testing.T) {
	repo, head := leadRepo(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "aaaaaaaa-0000-4000-8000-000000000001")
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	t.Setenv("C4_AGENT_BLOCK", "")
	var posts, waits atomic.Int32
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("X-C4-Agent"); a != "aaaaaaaa-0000-4000-8000-000000000001" {
			t.Errorf("%s names agent %q", r.URL.Path, a)
		}
		switch {
		case r.URL.Path == "/api/v1/lead/workers":
			var req api.LeadWorkersRequest
			json.NewDecoder(r.Body).Decode(&req)
			keys = append(keys, req.Key)
			if len(req.Workers) == 0 || req.Workers[0].Base != head || req.Root == "" {
				t.Errorf("workers %+v root %q", req.Workers, req.Root)
			}
			units := []api.AppendedUnit{}
			state := "pending"
			if posts.Add(1) > 1 {
				state = "added"
			}
			for i, wk := range req.Workers {
				u := api.AppendedUnit{Name: wk.Name}
				if state == "added" {
					u.Index = &i
				}
				units = append(units, u)
			}
			json.NewEncoder(w).Encode(api.LeadWorkersResponse{Job: "show-a/lead-1a2b3c4d5e", Units: units, State: state, Message: "ok"})
		case strings.HasSuffix(r.URL.Path, "/wait"):
			waits.Add(1)
			json.NewEncoder(w).Encode(api.WaitAnswer{Done: true, Units: []api.UnitResult{{Job: "show-a/lead-1a2b3c4d5e", Unit: 0, Name: "critic", State: "succeeded", Settled: "ended",
				Summary: &api.UnitSummary{Text: "The design holds; one naming nit.", By: "unit"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	ctx := context.Background()

	_, out, err := c.agent(ctx, nil, AgentIn{AgentWorker: AgentWorker{Name: "critic", Description: "Design critic", Prompt: "Review docs/designs/parser.md."}})
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 2 || len(keys) != 2 || keys[0] != keys[1] || keys[0] == "" {
		t.Fatalf("sent %d times with keys %v", posts.Load(), keys)
	}
	if out.Running || len(out.Units) != 1 || out.Units[0].Ref != "show-a/lead-1a2b3c4d5e/critic" || out.Units[0].Index == nil ||
		out.Units[0].Result == nil || out.Units[0].Result.Summary.Text != "The design holds; one naming nit." || len(out.Warnings) != 0 {
		t.Fatalf("the answer: %+v", out)
	}

	// background: at once, with how to wait; uncommitted work warned of
	os.WriteFile(filepath.Join(repo, "draft.md"), []byte("# draft\n"), 0o644)
	posts.Store(1)
	before := waits.Load()
	_, out, err = c.agent(ctx, nil, AgentIn{Background: true, Workers: []AgentWorker{
		{Name: "r1", Description: "Reviewer one", Prompt: "Review it."}, {Name: "r2", Description: "Reviewer two", Prompt: "Review it again."}}})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Running || waits.Load() != before || len(out.Units) != 2 || !strings.Contains(out.Message, "c4 farm wait show-a/lead-1a2b3c4d5e/r1 show-a/lead-1a2b3c4d5e/r2") {
		t.Fatalf("background: %+v", out)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "draft.md") || !strings.Contains(out.Warnings[0], head[:12]) {
		t.Fatalf("the uncommitted warning: %v", out.Warnings)
	}

	// both forms at once, and inside a block: refused, nothing sent
	sent := posts.Load()
	if _, _, err := c.agent(ctx, nil, AgentIn{AgentWorker: AgentWorker{Prompt: "x", Description: "y"}, Workers: []AgentWorker{{Prompt: "z", Description: "w"}}}); err == nil {
		t.Fatal("both forms at once")
	}
	t.Setenv("C4_AGENT_BLOCK", "1")
	if _, _, err := c.agent(ctx, nil, AgentIn{AgentWorker: AgentWorker{Prompt: "x", Description: "y"}}); err != errInBlock {
		t.Fatalf("in a block: %v", err)
	}
	if posts.Load() != sent {
		t.Fatal("a refused call was sent")
	}
}
