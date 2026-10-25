package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A submission's plans travel with it: each plan file its jobs name is
// read beside the submission, as JSON, under the path as written, once
// however many jobs name it, on the plan and on the submit; a plan that is
// not there is said at its job before the farm is asked.
func TestSubmitSendsPlans(t *testing.T) {
	var sent []api.SubmissionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.SubmissionRequest
		json.NewDecoder(r.Body).Decode(&req)
		sent = append(sent, req)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.SubmissionResponse{})
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "noor"})
	ctx := context.Background()

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "plans"), 0o755)
	os.WriteFile(filepath.Join(dir, "plans", "port.plan.yaml"), []byte("version: 1\nblocks:\n- {name: scan}\n- {name: port, after: [scan]}\n"), 0o644)
	file := filepath.Join(dir, "port.yaml")
	os.WriteFile(file, []byte("c4: submit/1\nproject: textkit\nshot: port\njobs:\n"+
		"  port: {tool: agent, plan: plans/port.plan.yaml}\n"+
		"  again: {tool: agent, plan: plans/port.plan.yaml}\n"+
		"  review: {tool: agent, with: {task: read the diff}}\n"), 0o644)
	var out bytes.Buffer
	if _, err := SubmitFile(ctx, c, Out{W: &out}, SubmitFileArgs{Path: file, Yes: true}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(sent) != 2 {
		t.Fatalf("%d requests", len(sent))
	}
	for i, req := range sent {
		if len(req.Plans) != 1 {
			t.Fatalf("request %d's plans: %v", i, req.Plans)
		}
		var p struct {
			Blocks []struct{ Name string } `json:"blocks"`
		}
		if err := json.Unmarshal(req.Plans["plans/port.plan.yaml"], &p); err != nil || len(p.Blocks) != 2 || p.Blocks[1].Name != "port" {
			t.Errorf("request %d's plan: %s (%v)", i, req.Plans["plans/port.plan.yaml"], err)
		}
	}

	// no plan files named, none sent
	os.WriteFile(file, []byte("c4: submit/1\nproject: textkit\nshot: port\njobs:\n  review: {tool: agent, with: {task: read the diff}}\n"), 0o644)
	sent = nil
	SubmitFile(ctx, c, Out{W: &out}, SubmitFileArgs{Path: file, Yes: true})
	if len(sent) == 0 || sent[0].Plans != nil {
		t.Errorf("plans without a plan: %v", sent)
	}

	// a plan that is not there
	os.WriteFile(file, []byte("c4: submit/1\nproject: textkit\nshot: port\njobs:\n  port: {tool: agent, plan: gone.plan.yaml}\n"), 0o644)
	sent = nil
	_, err := SubmitFile(ctx, c, Out{W: &out}, SubmitFileArgs{Path: file, Yes: true})
	if err == nil || !strings.HasPrefix(err.Error(), "jobs.port.plan: ") || !strings.Contains(err.Error(), "gone.plan.yaml") || len(sent) != 0 {
		t.Errorf("a missing plan: %v, sent %d", err, len(sent))
	}
}
