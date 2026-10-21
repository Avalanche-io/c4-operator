package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// The agent tool as the release ships it.
const agentToolYAML = `c4: tool/1
name: agent
title: Claude Code
summary: A Claude Code session works one task in your repository and hands back its change as a branch.
kind: agent
runs:
  host:
    has: [claude-code]
    exe: {macos: c4-agent, linux: c4-agent, windows: c4-agent.exe}
  image: c4/block
  command: ["{{Run.Exe}}", block]
params:
- {name: task, type: STRING, userInterface: {control: MULTILINE_EDIT, label: Task}}
- {name: model, type: STRING, default: "", show: advanced}
- {name: effort, type: STRING, allowedValues: ["", low, medium, high, xhigh, max], default: "", show: advanced}
- {name: allow, type: STRING, default: "Read Edit Write Glob Grep Bash(git:*)", show: advanced}
needs: {network: [model-api]}
reuse: never
`

// A plan of agent blocks through POST /submissions: the plan travels in
// the request; a dry run plans its table without storing it; a real
// submit stores the table, then creates a graph job naming it, on the
// farm's host pool, with the plan in the submission's record.
func TestSubmissionAgentPlan(t *testing.T) {
	s := submissionFarm(t)
	ctx := context.Background()
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	view := putTree(t, s, "show-a", map[string]string{"tool.yaml": agentToolYAML}).String()
	tool := &c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "agent"}, Spec: c4v1.ToolSpec{View: view}}
	if err := s.Client.Create(ctx, tool); err != nil {
		t.Fatal(err)
	}
	image := "registry.example/block@sha256:" + strings.Repeat("b", 64)
	tool.Status = c4v1.ToolStatus{Version: "v1", Image: image, Versions: []c4v1.ToolVersion{{Version: "v1", View: view, Image: image,
		Test: c4v1.ToolTest{State: c4v1.ToolTestOK}}}}
	if err := s.Client.Status().Update(ctx, tool); err != nil {
		t.Fatal(err)
	}
	s.m.pools = append(s.m.pools, c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "laptop"},
		Spec: c4v1.PoolSpec{NodeSelector: map[string]string{authoring.HostLabel: "laptop"}, SlotsPerNode: 4}})
	var tables [][]byte
	s.DryRunAll = func(_ context.Context, jobs []*c4v1.WorkJob, _ string, _ map[string]string, given map[string][]byte) ([]api.SubmitEstimate, error) {
		out := make([]api.SubmitEstimate, len(jobs))
		for i, j := range jobs {
			b, ok := given[j.Spec.Params[class.TableParam]]
			if !ok {
				t.Errorf("the dry run was not given %s's table", j.Name)
			}
			tables = append(tables, b)
			out[i] = api.SubmitEstimate{Units: 2}
		}
		return out, nil
	}
	plan := json.RawMessage(`{"version": 1, "blocks": [
	  {"name": "scan", "planned": false, "params": {"task": "port scan"}},
	  {"name": "format", "planned": false, "after": ["scan"], "params": {"task": "port format"}}]}`)
	doc := []byte(`{"c4": "submit/1", "project": "show-a", "shot": "port", "root": "/Users/dev/textkit",
	  "jobs": {"port": {"tool": "agent", "plan": "port.plan.json", "with": {"effort": "high"}}}}`)

	code, out := submit(t, s, api.SubmissionRequest{Submission: doc, DryRun: true})
	if code != http.StatusUnprocessableEntity || len(out.Errors) != 1 || out.Errors[0].Path != "jobs.port.plan" ||
		out.Errors[0].Message != "port.plan.json was not sent with the submission" {
		t.Fatalf("a plan not sent: %d %+v", code, out)
	}
	code, out = submit(t, s, api.SubmissionRequest{Submission: doc, DryRun: true, Plans: map[string]json.RawMessage{"port.plan.json": plan}})
	if code != http.StatusOK || !out.OK || len(out.Jobs) != 1 || out.Jobs[0].What != "2 units" || len(tables) != 1 {
		t.Fatalf("dry run: %d %+v", code, out)
	}
	id := c4.Identify(bytes.NewReader(tables[0]))
	st, _ := s.Stores(ctx, "show-a")
	if _, err := st.Head(ctx, id); err == nil {
		t.Fatal("a dry run stored the table")
	}

	code, out = submit(t, s, api.SubmissionRequest{Submission: doc, Plans: map[string]json.RawMessage{"port.plan.json": plan}})
	if code != http.StatusCreated || !out.OK || len(out.Jobs) != 1 {
		t.Fatalf("submit: %d %+v", code, out)
	}
	var j c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: out.Jobs[0].Name}, &j); err != nil {
		t.Fatal(err)
	}
	if j.Spec.Class != "graph" || j.Spec.Params[class.TableParam] != id.String() || len(j.Spec.Pools) != 1 || j.Spec.Pools[0] != "laptop" {
		t.Fatalf("the job: %+v", j.Spec)
	}
	if _, err := st.Head(ctx, id); err != nil {
		t.Fatalf("the table was not stored: %v", err)
	}
	r, err := st.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	p, err := class.TablePlan(&j, buf.Bytes())
	if err != nil || len(p.Units) != 2 {
		t.Fatalf("the stored table does not plan: %v", err)
	}
	// The submission's record names the plan it was read with.
	rid, err := c4.Parse(out.Record)
	if err != nil {
		t.Fatalf("record %q: %v", out.Record, err)
	}
	rr, err := st.Open(rid)
	if err != nil {
		t.Fatalf("the record is not in the store: %v", err)
	}
	defer rr.Close()
	var env struct {
		DSSE struct {
			Payload []byte `json:"payload"`
		} `json:"dsseEnvelope"`
	}
	if err := json.NewDecoder(rr).Decode(&env); err != nil {
		t.Fatal(err)
	}
	// the plan as the server received it (a RawMessage travels compacted)
	var sent bytes.Buffer
	_ = json.Compact(&sent, plan)
	if pid := c4.Identify(bytes.NewReader(sent.Bytes())).String(); !bytes.Contains(env.DSSE.Payload, []byte(pid)) {
		t.Fatalf("the record does not name the plan %s: %s", pid, env.DSSE.Payload)
	}
}
