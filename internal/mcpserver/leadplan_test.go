package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// c4_plan sends a lead's edits to its job as the lead's session, each
// worker's base resolved to the lead's HEAD, and sends the call again with
// its key until the queue has applied it, so the units it added come back
// with their indexes. A refusal makes none of the edits and says why in
// the farm's words; inside a block it is refused before anything is sent.
func TestPlanTool(t *testing.T) {
	repo, head := leadRepo(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "aaaaaaaa-0000-4000-8000-000000000001")
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	t.Setenv("C4_AGENT_BLOCK", "")
	var posts atomic.Int32
	var keys []string
	var last api.PlanRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("X-C4-Agent"); a != "aaaaaaaa-0000-4000-8000-000000000001" {
			t.Errorf("%s names agent %q", r.URL.Path, a)
		}
		if r.URL.Path != "/api/v1/jobs/show-a/lead-1a2b3c4d5e/plan" {
			http.NotFound(w, r)
			return
		}
		var req api.PlanRequest
		json.NewDecoder(r.Body).Decode(&req)
		keys, last = append(keys, req.Key), req
		if len(req.Edits) > 0 && req.Edits[0].Withdraw != nil && req.Edits[0].Withdraw.Unit == "scan" {
			json.NewEncoder(w).Encode(api.PlanResponse{State: "refused", Error: "lead-1a2b3c4d5e/1 (scan) is running; withdraw it after it ends, or steer it"})
			return
		}
		ans := api.PlanResponse{State: "pending", Units: []api.AppendedUnit{{Name: "port"}}, Message: "lead-1a2b3c4d5e: plan to be applied"}
		if posts.Add(1) > 1 {
			i := 2
			ans = api.PlanResponse{State: "applied", Units: []api.AppendedUnit{{Name: "port", Index: &i}}, Message: "lead-1a2b3c4d5e: plan applied, adding port"}
		}
		json.NewEncoder(w).Encode(ans)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	ctx := context.Background()

	_, out, err := c.plan(ctx, nil, PlanIn{Job: "show-a/lead-1a2b3c4d5e", Edits: []PlanEditIn{
		{Add: &PlanWorker{AgentWorker: AgentWorker{Name: "port", Description: "Port the parser", Prompt: "Port each module.", After: []string{"scan"}}, Work: 1800}},
		{Release: []string{"port"}},
		{Edge: &api.PlanEdge{From: "scan", To: "docs", Remove: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 2 || len(keys) != 2 || keys[0] != keys[1] || keys[0] == "" {
		t.Fatalf("sent %d times with keys %v", posts.Load(), keys)
	}
	if len(last.Edits) != 3 || last.Edits[0].Add == nil || last.Edits[0].Add.Worker == nil || last.Edits[0].Add.Worker.Base != head ||
		last.Edits[0].Add.Work != 1800 || last.Edits[1].Release[0] != "port" || !last.Edits[2].Edge.Remove || last.Root == "" {
		t.Fatalf("the call %+v", last)
	}
	if out.State != "applied" || len(out.Units) != 1 || out.Units[0].Index == nil || *out.Units[0].Index != 2 ||
		out.Units[0].Ref != "show-a/lead-1a2b3c4d5e/port" || !strings.Contains(out.Message, "c4 farm wait show-a/lead-1a2b3c4d5e/port") {
		t.Fatalf("the answer: %+v", out)
	}

	_, _, err = c.plan(ctx, nil, PlanIn{Job: "show-a/lead-1a2b3c4d5e", Edits: []PlanEditIn{{Withdraw: &api.PlanWithdraw{Unit: "scan", Why: "done by hand"}}}})
	if err == nil || err.Error() != "the farm refused the plan, and made none of its edits: lead-1a2b3c4d5e/1 (scan) is running; withdraw it after it ends, or steer it" {
		t.Fatalf("refused: %v", err)
	}

	sent := len(keys)
	if _, _, err := c.plan(ctx, nil, PlanIn{Job: "lead-1a2b3c4d5e", Edits: []PlanEditIn{{Release: []string{"port"}}}}); err == nil || !strings.Contains(err.Error(), "namespace/name") {
		t.Fatalf("a job without its namespace: %v", err)
	}
	t.Setenv("C4_AGENT_BLOCK", "1")
	if _, _, err := c.plan(ctx, nil, PlanIn{Job: "show-a/lead-1a2b3c4d5e", Edits: []PlanEditIn{{Release: []string{"port"}}}}); err == nil || !strings.Contains(err.Error(), "only a lead calls c4_plan") {
		t.Fatalf("in a block: %v", err)
	}
	if len(keys) != sent {
		t.Fatal("a refused call was sent")
	}
}
