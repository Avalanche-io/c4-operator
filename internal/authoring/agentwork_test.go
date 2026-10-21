package authoring

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// The agent tool as the release ships it (the tool format's farm agent).
const agentTool = `c4: tool/1
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
- {name: env, type: STRING, default: "", show: advanced}
needs: {network: [model-api]}
reuse: never
`

// agentFarm is the fixtures' farm with the agent tool published in it, a
// host-only variant, and a host pool of dev's.
func agentFarm(t *testing.T) *fixtureFarm {
	t.Helper()
	f := loadFarm(t)
	for name, doc := range map[string]string{
		"agent":      agentTool,
		"agent-host": strings.Replace(strings.Replace(agentTool, "name: agent", "name: agent-host", 1), "  image: c4/block\n", "", 1),
	} {
		tl, errs := ParseTool([]byte(doc))
		if len(errs) > 0 {
			t.Fatalf("%s: %v", name, errs)
		}
		f.ToolsV[name], f.tools[name] = "v1", tl
	}
	f.Env = "c4farmenv"
	f.Hosts = []HostPool{{Name: "laptop", Owners: []string{"dev"}}, {Name: "theirs", Owners: []string{"someone"}}}
	return f
}

func lowerAgentDoc(t *testing.T, f Farm, doc string, in Inputs) (*Plan, []Error) {
	t.Helper()
	sub, perrs := ParseSubmission([]byte(doc))
	if len(perrs) > 0 {
		t.Fatalf("parse: %v", perrs)
	}
	return LowerWith(sub, f, in)
}

func agentSpec(t *testing.T, j PlannedJob) c4v1.WorkJobSpec {
	t.Helper()
	var spec c4v1.WorkJobSpec
	if err := json.Unmarshal(j.Request.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func envMap(spec c4v1.WorkJobSpec) map[string]string {
	out := map[string]string{}
	for _, e := range spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

func tableOf(t *testing.T, j PlannedJob) class.Doc {
	t.Helper()
	var d class.Doc
	if err := json.Unmarshal(j.Table, &d); err != nil {
		t.Fatalf("table %s: %v", j.Table, err)
	}
	return d
}

// A single agent task is a graph job with a stored table of one block:
// the brief in the unit's env, the tool's settings in the template, the
// developer's folder as the repository, and their host pool.
func TestLowerAgentTask(t *testing.T) {
	plan, errs := lowerAgentDoc(t, agentFarm(t), `c4: submit/1
project: textkit
shot: parser
root: /Users/dev/textkit
jobs:
  fix:
    tool: agent
    with: {task: "port the parser to the new API\nkeep the tests green", effort: high}
`, Inputs{Owner: "dev"})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	j := plan.Jobs[0]
	spec := agentSpec(t, j)
	id := c4.Identify(bytes.NewReader(j.Table)).String()
	if spec.Class != "graph" || spec.Params[class.TableParam] != id || spec.Params["key"] != "agent" {
		t.Fatalf("class and params: %s %v (table %s)", spec.Class, spec.Params, id)
	}
	if len(spec.Pools) != 1 || spec.Pools[0] != "laptop" {
		t.Fatalf("pools: %v", spec.Pools)
	}
	env := envMap(spec)
	for k, v := range map[string]string{"C4_AGENT_BLOCK": "1", "C4_BLOCK_PROFILE": "agent", "C4_BLOCK_REPO": "/Users/dev/textkit",
		"C4_BLOCK_TOOLS": "Read Edit Write Glob Grep Bash(git:*)", "C4_BLOCK_EFFORT": "high", "C4_AGENT_ENV": "c4farmenv"} {
		if env[k] != v {
			t.Errorf("template %s = %q, want %q", k, env[k], v)
		}
	}
	if _, ok := env["C4_BLOCK_PROMPT"]; ok {
		t.Error("the brief is in the template, not the block's own env")
	}
	d := tableOf(t, j)
	if len(d.Units) != 1 {
		t.Fatalf("table: %s", j.Table)
	}
	u := d.Units[0]
	if u.Name != "fix" || u.Label != "port the parser to the new API" || u.Env["C4_BLOCK_PROMPT"] != "port the parser to the new API\nkeep the tests green" ||
		u.Reasoning == nil || u.Reasoning.Does != class.DoesModel || u.Reasoning.Effort != "high" {
		t.Fatalf("the block: %+v %+v", u, u.Reasoning)
	}
	if j.Units != 1 || j.What() != "1 unit" {
		t.Fatalf("what: %d %q", j.Units, j.What())
	}
}

// A plan's blocks are the table: each block's params named for the tool's
// parameters are its settings (its reasoning first, then its params, then
// the job's with), other params stay facts of the plan, and a wait stays
// a wait.
func TestLowerAgentPlan(t *testing.T) {
	planFile := []byte(`version: 1
blocks:
- {name: scan, params: {task: "port scan", model: m1, functions: "14"}}
- {name: format, after: [scan], planned: false, reasoning: {effort: low}, params: {task: "port format", effort: max, env: c4blockenv}}
- {name: api-break, wait: {server: "person:owner", ask: "Keep the old signature?"}, before: [format]}
`)
	plan, errs := lowerAgentDoc(t, agentFarm(t), `c4: submit/1
project: textkit
shot: port
root: /Users/dev/textkit
jobs:
  port:
    tool: agent
    plan: port.plan.yaml
    with: {model: m0, effort: high, env: c4jobenv}
`, Inputs{Owner: "dev", Plans: map[string][]byte{"port.plan.yaml": planFile}})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	j := plan.Jobs[0]
	if j.PlanFile != c4.Identify(bytes.NewReader(planFile)).String() {
		t.Fatalf("the plan file's ID: %q", j.PlanFile)
	}
	d := tableOf(t, j)
	by := map[string]class.DocUnit{}
	for _, u := range d.Units {
		by[u.Name] = u
	}
	scan, format, wait := by["scan"], by["format"], by["api-break"]
	if scan.Env["C4_BLOCK_PROMPT"] != "port scan" || scan.Env["C4_BLOCK_MODEL"] != "m1" || scan.Env["C4_BLOCK_EFFORT"] != "high" ||
		scan.Params["functions"] != "14" || scan.Params["model"] != "" || !scan.Planned {
		t.Fatalf("scan: %+v", scan)
	}
	if format.Env["C4_BLOCK_PROMPT"] != "port format" || format.Env["C4_BLOCK_MODEL"] != "m0" || format.Env["C4_BLOCK_EFFORT"] != "low" ||
		format.Reasoning.Effort != "low" || format.Reasoning.Model != "m0" || format.Planned {
		t.Fatalf("format: %+v %+v", format, format.Reasoning)
	}
	// The job's with.env replaces the farm's environment; a block's own
	// env param replaces the job's, for that block.
	if env := envMap(agentSpec(t, j)); env["C4_AGENT_ENV"] != "c4jobenv" {
		t.Errorf("the job's environment: %q", env["C4_AGENT_ENV"])
	}
	if format.Env["C4_AGENT_ENV"] != "c4blockenv" || scan.Env["C4_AGENT_ENV"] != "c4jobenv" {
		t.Errorf("blocks' environments: %q %q", format.Env["C4_AGENT_ENV"], scan.Env["C4_AGENT_ENV"])
	}
	if wait.Wait == nil || wait.Env != nil || wait.Reasoning != nil {
		t.Fatalf("the wait: %+v", wait)
	}
	if j.Units != 3 || j.What() != "3 units" {
		t.Fatalf("what: %d %q", j.Units, j.What())
	}
	// The table plans as the queue will plan it.
	job := &c4v1.WorkJob{Spec: agentSpec(t, j)}
	p, err := class.TablePlan(job, j.Table)
	if err != nil || len(p.Units) != 3 {
		t.Fatalf("the table does not plan: %v", err)
	}
}

// What the farm refuses about agent work, in its words.
func TestLowerAgentRefusals(t *testing.T) {
	f := agentFarm(t)
	for _, c := range []struct {
		name, doc string
		in        Inputs
		want      Error
	}{
		{"plan not sent", "c4: submit/1\nproject: textkit\nshot: port\njobs:\n  port: {tool: agent, plan: port.plan.yaml}\n",
			Inputs{Owner: "dev"},
			Error{Path: "jobs.port.plan", Message: "port.plan.yaml was not sent with the submission", Fix: "send it with the submission (plans: in the request); c4 farm submit does"}},
		{"no host of yours", "c4: submit/1\nproject: textkit\nshot: port\njobs:\n  fix: {tool: agent-host, with: {task: go}}\n",
			Inputs{Owner: "nobody"},
			Error{Path: "jobs.fix.tool", Message: "no machine of yours runs agents on this farm yet", Fix: "run c4 farm up on your machine, which makes it a runner for your agents"}},
		{"a block sets the farm's env", "c4: submit/1\nproject: textkit\nshot: port\njobs:\n  port: {tool: agent, plan: p.yaml}\n",
			Inputs{Owner: "dev", Plans: map[string][]byte{"p.yaml": []byte("version: 1\nblocks:\n- {name: a, env: {C4_BLOCK_PROMPT: x}}\n")}},
			Error{Path: "jobs.port.plan", Message: "block a sets C4_BLOCK_PROMPT, which only the farm sets", Fix: "give a block's settings as params named for the tool's parameters"}},
		{"two tools", "c4: submit/1\nproject: textkit\nshot: port\njobs:\n  port: {tool: agent, each: {tool: agent-host}, plan: p.yaml}\n",
			Inputs{Owner: "dev", Plans: map[string][]byte{"p.yaml": []byte("version: 1\nblocks:\n- {name: a}\n- {name: b, planned: false}\n")}},
			Error{Path: "jobs.port.each.tool", Message: "blocks running two tools (agent and agent-host) in one job cannot run on this farm yet", Fix: "run every block with one tool for now"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, errs := lowerAgentDoc(t, f, c.doc, c.in)
			if len(errs) != 1 || errs[0] != c.want {
				t.Fatalf("\n got  %v\n want %v", errs, c.want)
			}
		})
	}
	// An agent job's answerer is one of its own units: a block of its plan.
	leadPlan := map[string][]byte{"p.yaml": []byte("version: 1\nblocks:\n- {name: lead, planned: false}\n- {name: scan, planned: false}\n")}
	if _, errs := lowerAgentDoc(t, f, "c4: submit/1\nproject: textkit\nshot: port\njobs:\n  port: {tool: agent, plan: p.yaml, answers: {by: unit:lead}}\n",
		Inputs{Owner: "dev", Plans: leadPlan}); len(errs) > 0 {
		t.Fatalf("a block answering: %v", errs)
	}
	want := Error{Path: "jobs.port.answers.by", Message: "no unit \"laed\" in port", Fix: "did you mean \"lead\"?"}
	if _, errs := lowerAgentDoc(t, f, "c4: submit/1\nproject: textkit\nshot: port\njobs:\n  port: {tool: agent, plan: p.yaml, answers: {by: unit:laed}}\n",
		Inputs{Owner: "dev", Plans: leadPlan}); len(errs) != 1 || errs[0] != want {
		t.Fatalf("\n got  %v\n want %v", errs, want)
	}
	// A host-and-image tool with no host of yours runs as block pods on
	// the class's pools.
	plan, errs := lowerAgentDoc(t, f, "c4: submit/1\nproject: textkit\nshot: port\njobs:\n  fix: {tool: agent, with: {task: go}}\n", Inputs{Owner: "nobody"})
	if len(errs) > 0 || len(agentSpec(t, plan.Jobs[0]).Pools) != 0 {
		t.Fatalf("a block pod job: %v %+v", errs, plan)
	}
}
