package authoring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Avalanche-io/c4"
	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/plan"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// Agent work lowers to a graph job whose unit table is stored with it
// (class.TableParam): a plan's blocks, or a single task as a table of one
// unit. Each unit is a block, a fresh agent session the host runner (or a
// block pod) starts for it; the job's template is the tool's, and what
// differs by block (its brief, its tools, its model and effort) is the
// unit's environment in the table.

// HostLabel is the label a host runner puts on its Node; a pool whose
// node selector names it runs on host runners.
const HostLabel = "c4.avalanche.io/host"

// HostPool is a pool of host runners: machines that run blocks as their
// owners' own agent sessions. Owners are whose jobs it takes (none: any).
type HostPool struct {
	Name   string
	Owners []string
}

// Inputs are what a submission carries beside its document.
type Inputs struct {
	// Plans are the plan files its jobs name, by the path as written, as
	// JSON. The farm never reads a plan from the tree.
	Plans map[string][]byte
	// Owner is who submits: whose host pools agent work may use.
	Owner string
}

// blockEnv is where an agent tool's parameters reach a block: the brief,
// the tools it may use, the model and the effort (docs/agents/c4-agent.md,
// "Blocks").
var blockEnv = []struct{ param, env string }{
	{"task", "C4_BLOCK_PROMPT"},
	{"allow", "C4_BLOCK_TOOLS"},
	{"model", "C4_BLOCK_MODEL"},
	{"effort", "C4_BLOCK_EFFORT"},
	{"env", "C4_AGENT_ENV"},
}

// LowerWith is Lower with what the request carries beside the document.
func LowerWith(sub *Submission, farm Farm, in Inputs) (*Plan, []Error) {
	c, ok := newSubCtx(sub, farm)
	if !ok {
		return nil, sortErrors(c.errs)
	}
	c.in = in
	c.check()
	if len(c.errs) > 0 {
		return nil, sortErrors(c.errs)
	}
	if errs := c.refusals(); len(errs) > 0 {
		return nil, sortErrors(errs)
	}
	return c.lower()
}

// agentWork reports whether a job lowers to a stored table: it runs an
// agent tool, or a plan.
func (c *subCtx) agentWork(j *Job) bool {
	r, ok := c.tools[j.Key]
	return j.Plan != "" || ok && r.tool.Kind == "agent"
}

// checkAgentAnswerer refuses an answers: unit that is not a unit of an
// agent job: a block of its plan, or for a single task the job's key. A
// plan not sent, or one that does not read, is refused with the plan.
func (c *subCtx) checkAgentAnswerer(p string, j *Job, name string) {
	units := []string{j.Key}
	if j.Plan != "" {
		b, ok := c.in.Plans[j.Plan]
		if !ok {
			return
		}
		pl, err := plan.Parse(b, j.Plan)
		if err != nil {
			return
		}
		units = pl.Names()
	}
	if slices.Contains(units, name) {
		return
	}
	fix := "name a unit of the job, or leave answers: out"
	if s, ok := closest(name, units); ok {
		fix = fmt.Sprintf("did you mean %q?", s)
	}
	c.add(errf(p+".answers.by", fix, "no unit %q in %s", name, j.Key))
}

// planRefusals are what about a job's plan this farm cannot run yet: a
// plan not sent with the submission, one that does not read, blocks that
// run two different tools (a job has one template so far), and plans of
// tools other than agents.
func (c *subCtx) planRefusals(j *Job) []Error {
	p := "jobs." + j.Key
	if j.Plan == "" {
		return nil
	}
	b, ok := c.in.Plans[j.Plan]
	if !ok {
		return []Error{errf(p+".plan", "send it with the submission (plans: in the request); c4 farm submit does",
			"%s was not sent with the submission", j.Plan)}
	}
	pl, err := plan.Parse(b, j.Plan)
	if err != nil {
		return []Error{errf(p+".plan", "check it with c4-plan import --format file", "%s does not read as a plan: %v", j.Plan, err)}
	}
	doc, err := pl.Doc()
	if err != nil {
		return []Error{errf(p+".plan", "check it with c4-plan import --format file", "%s: %v", j.Plan, err)}
	}
	r := c.tools[j.Key]
	var errs []Error
	// (Other kinds are refused as tools, ToolRefusals.)
	if r.tool != nil && (r.tool.Kind == "frames" || r.tool.Kind == "once") {
		errs = append(errs, errf(p+".plan", "run the plan's blocks with an agent tool for now", "plans of %s tools cannot run on this farm yet", r.tool.Kind))
	}
	if j.Tool != "" && j.Each != nil && splitName(j.Tool) != splitName(j.Each.Tool) {
		planned, other := false, false
		for _, u := range doc.Units {
			if u.Wait != nil {
				continue
			}
			planned, other = planned || u.Planned, other || !u.Planned
		}
		if planned && other {
			errs = append(errs, errf(p+".each.tool", "run every block with one tool for now",
				"blocks running two tools (%s and %s) in one job cannot run on this farm yet", splitName(j.Tool), splitName(j.Each.Tool)))
		}
	}
	return errs
}

func splitName(s string) string { name, _ := splitTool(s); return name }

// hostPools are the farm's host pools the submitter may use.
func (c *subCtx) hostPools() []string {
	var out []string
	for _, p := range c.farm.HostPools() {
		if len(p.Owners) == 0 || slices.Contains(p.Owners, c.in.Owner) {
			out = append(out, p.Name)
		}
	}
	return out
}

// lowerAgent lowers a job of agent work: class graph with its unit table
// (returned, for the front end to store before it creates the job), the
// tool's template once, and each block's settings in its unit's env.
// Paths in errors are relative to the job.
func (c *subCtx) lowerAgent(j *Job, tj toolJob) (*api.SubmitRequest, []byte, []Error) {
	t := tj.tool
	required := j.Plan == ""
	params, errs := resolve(t, tj.with, tj.root, "", required)
	if len(errs) > 0 {
		return nil, nil, errs
	}
	doc, errs := c.agentTable(j, t, params)
	if len(errs) > 0 {
		return nil, nil, errs
	}
	table, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, []Error{errf("", "", "the job's unit table cannot be written: %v", err)}
	}
	id := c4.Identify(bytes.NewReader(table)).String()
	pj, err := json.Marshal(params)
	if err != nil {
		return nil, nil, []Error{errf("with", "", "the parameters cannot be written: %v", err)}
	}
	env := []corev1.EnvVar{{Name: "C4_TOOLS", Value: ToolsMount}, {Name: "C4_PARAMS", Value: string(pj)},
		{Name: "C4_AGENT_BLOCK", Value: "1"}, {Name: "C4_BLOCK_PROFILE", Value: t.Name}}
	if r := c.sub.Root; r != "" {
		// the repository on the developer's machine, where a host runner
		// gives each block its own worktree
		env = append(env, corev1.EnvVar{Name: "C4_BLOCK_REPO", Value: r})
	}
	for _, b := range blockEnv {
		if v := paramString(params[b.param]); v != "" && b.param != "task" {
			env = append(env, corev1.EnvVar{Name: b.env, Value: v})
		}
	}
	if v := paramString(params["env"]); v == "" {
		// the farm's clean environment, unless the job names another
		if e := c.farm.AgentEnv(); e != "" {
			env = append(env, corev1.EnvVar{Name: "C4_AGENT_ENV", Value: e})
		}
	}
	if tj.shot != "" {
		env = append(env, corev1.EnvVar{Name: "C4_SHOT", Value: tj.shot})
	}
	env = append(env, tj.env...)
	res, rerrs := requests(t, tj.size, tj.needs)
	if len(rerrs) > 0 {
		return nil, nil, rerrs
	}
	pools := tj.pools
	if len(pools) == 0 && t.Runs.Host != nil {
		pools = c.hostPools()
		if len(pools) == 0 && t.Runs.Image == "" {
			return nil, nil, []Error{errf("tool", "run c4 farm up on your machine, which makes it a runner for your agents",
				"no machine of yours runs agents on this farm yet")}
		}
	}
	// A host runner runs a block as a session of its own and starts no
	// container; a block pod runs the tool's pinned image.
	image := tj.ref.Image
	if image == "" {
		if t.Runs.Image != "" {
			return nil, nil, []Error{errf("tool", "publish the tool again, which pins its image", "%s@%s has no image pinned in its tool.lock, so this farm cannot run it", t.Name, tj.ref.Version)}
		}
		image = "c4-agent"
	}
	cp := map[string]string{class.TableParam: id, "key": t.Name}
	if tj.answerer != "" {
		cp["answerer"] = tj.answerer
	}
	ct := corev1.Container{Name: "unit", Image: image, Command: append([]string(nil), RunnerCommand...), Env: env, Resources: res}
	spec := c4v1.WorkJobSpec{Class: "graph", Params: cp, Pools: pools, MaxParallel: int32(tj.maxPar), Measure: tj.measure,
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{ct}}}}
	retries := tj.retries
	if retries == nil {
		retries = t.Retries
	}
	if retries != nil {
		r := int32(*retries)
		spec.Retries = &r
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, []Error{errf("", "", "the job cannot be written: %v", err)}
	}
	inputs := []api.SubmitInput{{View: tj.ref.View, MountPath: ToolsMount, ReadOnly: true}}
	inputs = append(inputs, tj.inputs...)
	out := &api.SubmitRequest{Project: tj.project, Namespace: tj.ns, Name: tj.name, Type: t.Name, Stage: tj.stage, Shot: tj.shot,
		Spec: raw, Inputs: inputs}
	if len(t.AIUsage) > 0 {
		out.AIUsage = fillAIUsage(t.AIUsage, params)
	}
	return out, table, nil
}

// agentTable is a job's unit table: its plan's blocks, or its task as one
// unit. A block's params named for the tool's parameters are its with:
// its reasoning, then its params, then the job's with, then the tool's
// defaults. Other params stay facts of the plan.
func (c *subCtx) agentTable(j *Job, t *Tool, job map[string]any) (*class.Doc, []Error) {
	if j.Plan == "" {
		u := class.DocUnit{Name: j.Key, Label: brief(paramString(job["task"])), Key: t.Name, Env: map[string]string{}}
		blockSettings(&u, t, job, nil)
		return &class.Doc{Version: 1, Units: []class.DocUnit{u}}, nil
	}
	pl, err := plan.Parse(c.in.Plans[j.Plan], j.Plan)
	if err != nil {
		return nil, []Error{errf("plan", "check it with c4-plan import --format file", "%s does not read as a plan: %v", j.Plan, err)}
	}
	doc, err := pl.Doc()
	if err != nil {
		return nil, []Error{errf("plan", "check it with c4-plan import --format file", "%s: %v", j.Plan, err)}
	}
	var errs []Error
	for i := range doc.Units {
		u := &doc.Units[i]
		if u.Wait != nil {
			continue
		}
		if u.Key == "" {
			u.Key = t.Name
		}
		if u.Env == nil {
			u.Env = map[string]string{}
		}
		for k := range u.Env {
			if strings.HasPrefix(k, "C4_") {
				errs = append(errs, errf("plan", "give a block's settings as params named for the tool's parameters",
					"block %s sets %s, which only the farm sets", u.Name, k))
			}
		}
		blockSettings(u, t, job, u.Params)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return doc, nil
}

// blockSettings puts a block's resolved settings in its env and its
// reasoning: the block's reasoning, then its params, then the job's.
func blockSettings(u *class.DocUnit, t *Tool, job map[string]any, own map[string]string) {
	val := func(name string) string {
		if r := u.Reasoning; r != nil {
			switch name {
			case "model":
				if r.Model != "" {
					return r.Model
				}
			case "effort":
				if r.Effort != "" {
					return r.Effort
				}
			}
		}
		if v, ok := own[name]; ok && hasParam(t, name) {
			return v
		}
		return paramString(job[name])
	}
	for _, b := range blockEnv {
		if !hasParam(t, b.param) {
			continue
		}
		if v := val(b.param); v != "" {
			u.Env[b.env] = v
		}
	}
	r := class.Reasoning{Does: class.DoesModel}
	if u.Reasoning != nil {
		r = *u.Reasoning
		if r.Does == "" {
			r.Does = class.DoesModel
		}
	}
	if hasParam(t, "model") {
		r.Model = val("model")
	}
	if hasParam(t, "effort") {
		r.Effort = val("effort")
	}
	u.Reasoning = &r
	// A block's settings are its env now; params left are the plan's
	// facts, carried for the HUD and the record.
	for name := range own {
		if hasParam(t, name) {
			delete(u.Params, name)
		}
	}
	if len(u.Params) == 0 {
		u.Params = nil
	}
	if len(u.Env) == 0 {
		u.Env = nil
	}
}

func hasParam(t *Tool, name string) bool {
	for _, p := range t.Params {
		if p.Name == name {
			return true
		}
	}
	return false
}

// paramString is a resolved parameter as a block reads it.
func paramString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// brief is a task's first line, short, as a unit's label.
func brief(task string) string {
	s, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:57]) + "..."
	}
	return s
}

// tableUnits is how many units a stored table starts with.
func tableUnits(table []byte) int {
	var d class.Doc
	if json.Unmarshal(table, &d) != nil {
		return 0
	}
	return len(d.Units)
}
