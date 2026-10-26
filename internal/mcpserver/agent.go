package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// c4_agent (LEAD.md stage 2): a lead starts workers on the farm as it would
// start subagents, and gets back what a subagent gives it: the worker's
// result when it ends within a bounded wait, else its unit and how to
// wait for it. The farm starts and owns the worker: a fresh session from
// its brief alone, a branch of its own from the lead's commit, its record.

// agentWaitFor is how long c4_agent waits for its workers before it
// answers that they are running, well inside an MCP client's timeout.
const agentWaitFor = 3 * time.Minute

// agentAddedWithin bounds the wait for the queue to add a call's workers
// (a tick or two), so the lead gets units it can name.
const agentAddedWithin = 30 * time.Second

type AgentWorker struct {
	Prompt      string   `json:"prompt" jsonschema:"the worker's whole brief: what to do, where, and what done looks like; it sees nothing of this conversation"`
	Description string   `json:"description" jsonschema:"a few words the HUD shows for it"`
	Name        string   `json:"name,omitempty" jsonschema:"its unit name in your job (default: made from the description)"`
	Agent       string   `json:"agent,omitempty" jsonschema:"the agent environment it runs in (default: the farm's)"`
	Model       string   `json:"model,omitempty" jsonschema:"the model, when not the default"`
	Effort      string   `json:"effort,omitempty" jsonschema:"low, medium, high, xhigh or max, when not the default"`
	Base        string   `json:"base,omitempty" jsonschema:"the commit or branch its work starts from (default: your HEAD); sent as the commit's full ID"`
	After       []string `json:"after,omitempty" jsonschema:"other workers of your job it waits for, by name"`
}

type AgentIn struct {
	AgentWorker
	// Workers start several at once, in place of the one above.
	Workers    []AgentWorker `json:"workers,omitempty" jsonschema:"several workers started together (instead of prompt and description), as a fan-out of reviewers"`
	Background bool          `json:"background,omitempty" jsonschema:"answer at once with the units, without waiting for them"`
	Project    string        `json:"project,omitempty" jsonschema:"the project your job is in, when you can submit to more than one"`
}

type AgentUnit struct {
	Name  string `json:"name"`
	Index *int   `json:"index,omitempty"`
	// Ref is the unit for c4 farm wait: job/unit.
	Ref    string          `json:"ref"`
	State  string          `json:"state,omitempty"`
	Result *api.UnitResult `json:"result,omitempty"`
}

type AgentOut struct {
	Job      string      `json:"job"`
	Created  bool        `json:"created,omitempty"`
	Units    []AgentUnit `json:"units"`
	Running  bool        `json:"running"`
	Warnings []string    `json:"warnings,omitempty"`
	Message  string      `json:"message"`
}

// agent is c4_agent.
func (c *Client) agent(ctx context.Context, _ *mcp.CallToolRequest, in AgentIn) (*mcp.CallToolResult, AgentOut, error) {
	var none AgentOut
	if inBlock(os.Getenv) {
		return nil, none, errInBlock
	}
	l := readLead(os.Getenv, os.Getppid())
	if l.Session == "" {
		return nil, none, errors.New("c4_agent starts workers for a lead, a person's own Claude Code session, and none was found here")
	}
	workers := in.Workers
	if len(workers) == 0 {
		workers = []AgentWorker{in.AgentWorker}
	} else if in.Prompt != "" {
		return nil, none, errors.New("give either prompt and description, for one worker, or workers, not both")
	}
	req := api.LeadWorkersRequest{Project: in.Project, Root: repoRoot(ctx, l.Cwd), Key: callKey()}
	var warnings []string
	dirtySaid := false
	for _, w := range workers {
		sha, dirty, err := leadBase(ctx, l.Cwd, w.Base)
		if err != nil {
			return nil, none, err
		}
		if len(dirty) > 0 && !dirtySaid {
			warnings = append(warnings, fmt.Sprintf("you have changes you have not committed (%s): the workers start from %s and will not see them; commit first if they need them",
				strings.Join(dirty, ", "), sha[:12]))
			dirtySaid = true
		}
		req.Workers = append(req.Workers, api.LeadWorker{Name: w.Name, Description: w.Description, Prompt: w.Prompt,
			Agent: w.Agent, Model: w.Model, Effort: w.Effort, Base: sha, After: w.After})
	}
	ctx = asAgent(ctx, l.Session)
	// the same call again until the queue has added the workers, so each
	// comes back with its index
	var ans api.LeadWorkersResponse
	added := time.Now().Add(agentAddedWithin)
	for {
		ans = api.LeadWorkersResponse{}
		if err := c.do(ctx, "POST", "/lead/workers", req, &ans); err != nil {
			return nil, none, err
		}
		if ans.State != "pending" || !time.Now().Before(added) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, none, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if ans.State == "refused" {
		return nil, none, fmt.Errorf("the farm refused the workers: %s", ans.Error)
	}
	out := AgentOut{Job: ans.Job, Created: ans.Created, Warnings: append(warnings, ans.Warnings...)}
	for _, u := range ans.Units {
		out.Units = append(out.Units, AgentUnit{Name: u.Name, Index: u.Index, Ref: ans.Job + "/" + u.Name})
	}
	if in.Background {
		out.Running = true
		out.Message = "started; wait for them with a background command: " + waitCommand(out.Units)
		return nil, out, nil
	}
	return nil, c.awaitWorkers(ctx, out), nil
}

// awaitWorkers waits a bounded time for the workers to settle, and
// answers with their results, or that they are running and how to wait.
func (c *Client) awaitWorkers(ctx context.Context, out AgentOut) AgentOut {
	ns, name, ok := strings.Cut(out.Job, "/")
	if !ok {
		return out
	}
	q := url.Values{}
	for _, u := range out.Units {
		q.Add("unit", u.Name)
	}
	deadline := time.Now().Add(agentWaitFor)
	for {
		within := min(25*time.Second, time.Until(deadline))
		if within <= 0 {
			break
		}
		q.Set("within", within.Round(time.Second).String())
		var ans api.WaitAnswer
		err := c.do(ctx, "GET", "/jobs/"+url.PathEscape(ns)+"/"+url.PathEscape(name)+"/wait?"+q.Encode(), nil, &ans)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for i := range out.Units {
			for k := range ans.Units {
				if ans.Units[k].Name == out.Units[i].Name {
					r := ans.Units[k]
					out.Units[i].State, out.Units[i].Result = r.State, &r
				}
			}
		}
		if ans.Done {
			out.Message = "settled: each unit's result is its own words (summary) and its branch; answer a question with c4_supply"
			return out
		}
	}
	out.Running = true
	out.Message = "running; wait for them with a background command: " + waitCommand(out.Units)
	return out
}

// waitCommand is how a lead waits for units.
func waitCommand(units []AgentUnit) string {
	refs := make([]string, len(units))
	for i, u := range units {
		refs[i] = u.Ref
	}
	return "c4 farm wait " + strings.Join(refs, " ") + " (add --any for the first)"
}

// callKey is a call's key: a retry of the same call adds nothing twice.
func callKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// repoRoot is the top of the repository cwd is in, or cwd.
func repoRoot(ctx context.Context, cwd string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return cwd
	}
	return strings.TrimSpace(string(out))
}

type CloseLeadIn struct {
	JobRef
	Why string `json:"why,omitempty" jsonschema:"why the job is closed, for its record"`
}

// closeLead is c4_close_lead: the lead closes its job, which then takes no
// more workers and finishes once its workers have.
func (c *Client) closeLead(ctx context.Context, _ *mcp.CallToolRequest, in CloseLeadIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	if l := readLead(os.Getenv, os.Getppid()); l.Session != "" {
		ctx = asAgent(ctx, l.Session)
	}
	return c.control(ctx, p+"/close", map[string]any{"why": in.Why})
}
