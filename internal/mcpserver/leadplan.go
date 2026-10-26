package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// c4_plan (LEAD.md stage 3): a lead plans ahead in its own job, and
// changes the plan as it learns. One call carries a list of edits, applied
// together or not at all: workers added held, until a release names them;
// a unit that has not started split, merged with others, or withdrawn;
// the order among its ready units; edges added or removed. No unit is
// renumbered: a replaced unit stays, withdrawn, naming what replaced it.

// PlanWorker is a unit a plan adds: a worker, as c4_agent starts one, or
// a unit as an expansion document gives it.
type PlanWorker struct {
	AgentWorker
	Work float64        `json:"work,omitempty" jsonschema:"your estimate of its work, in seconds; the farm's forecast uses it until it has history"`
	Unit map[string]any `json:"unit,omitempty" jsonschema:"instead of a worker, a unit of an expansion document: {name, after, work, env, ...}, as a generator writes it"`
}

type PlanSplitIn struct {
	Unit string       `json:"unit" jsonschema:"the unit to split, by name or index; it must not have started"`
	Into []PlanWorker `json:"into" jsonschema:"the units that replace it; whatever followed it follows all of them"`
	Why  string       `json:"why,omitempty" jsonschema:"why, for its record"`
}

type PlanMergeIn struct {
	Units []string   `json:"units" jsonschema:"the units to merge, by name or index; none may have started"`
	Into  PlanWorker `json:"into" jsonschema:"the one unit that replaces them"`
	Why   string     `json:"why,omitempty" jsonschema:"why, for its record"`
}

type PlanEditIn struct {
	Add      *PlanWorker       `json:"add,omitempty" jsonschema:"a planned worker, held until a release names it"`
	Split    *PlanSplitIn      `json:"split,omitempty" jsonschema:"replace a unit that has not started with several"`
	Merge    *PlanMergeIn      `json:"merge,omitempty" jsonschema:"replace units that have not started with one"`
	Order    []string          `json:"order,omitempty" jsonschema:"the units to run first among your ready ones, first first; any other unit comes after them"`
	Withdraw *api.PlanWithdraw `json:"withdraw,omitempty" jsonschema:"retire a unit that has not started, with why; it stays in the job as withdrawn"`
	Release  []string          `json:"release,omitempty" jsonschema:"held units to let run"`
	Edge     *api.PlanEdge     `json:"edge,omitempty" jsonschema:"to runs after from; remove takes that away"`
}

type PlanIn struct {
	Job   string       `json:"job" jsonschema:"your job, namespace/name, as c4_agent returned it"`
	Edits []PlanEditIn `json:"edits" jsonschema:"the edits, each one of add, split, merge, order, withdraw, release or edge; applied together, or none if the farm refuses one"`
}

type PlanOut struct {
	Job string `json:"job"`
	// Units are the units the call added, with their indexes once
	// applied, and how to wait for them.
	Units    []AgentUnit `json:"units,omitempty"`
	State    string      `json:"state"`
	Record   string      `json:"record,omitempty"`
	Warnings []string    `json:"warnings,omitempty"`
	Message  string      `json:"message"`
}

// plan is c4_plan.
func (c *Client) plan(ctx context.Context, _ *mcp.CallToolRequest, in PlanIn) (*mcp.CallToolResult, PlanOut, error) {
	var none PlanOut
	if inBlock(os.Getenv) {
		return nil, none, errors.New("a worker may not plan: only a lead calls c4_plan. Do the work yourself, or say in your summary what should be split off")
	}
	l := readLead(os.Getenv, os.Getppid())
	if l.Session == "" {
		return nil, none, errors.New("c4_plan plans a lead's job, a person's own Claude Code session's, and none was found here")
	}
	ns, name, ok := strings.Cut(in.Job, "/")
	if !ok || ns == "" || name == "" {
		return nil, none, errors.New("job: your job as namespace/name, as c4_agent returned it")
	}
	req := api.PlanRequest{Key: callKey(), Root: repoRoot(ctx, l.Cwd)}
	var warnings []string
	dirtySaid := false
	unit := func(w PlanWorker) (api.PlanUnit, error) {
		if w.Unit != nil {
			b, err := json.Marshal(w.Unit)
			return api.PlanUnit{Unit: b}, err
		}
		sha, dirty, err := leadBase(ctx, l.Cwd, w.Base)
		if err != nil {
			return api.PlanUnit{}, err
		}
		if len(dirty) > 0 && !dirtySaid {
			warnings = append(warnings, fmt.Sprintf("you have changes you have not committed (%s): the workers start from %s and will not see them; commit first if they need them",
				strings.Join(dirty, ", "), sha[:12]))
			dirtySaid = true
		}
		return api.PlanUnit{Work: w.Work, Worker: &api.LeadWorker{Name: w.Name, Description: w.Description, Prompt: w.Prompt,
			Agent: w.Agent, Model: w.Model, Effort: w.Effort, Base: sha, After: w.After}}, nil
	}
	for _, e := range in.Edits {
		out := api.PlanEdit{Order: e.Order, Withdraw: e.Withdraw, Release: e.Release, Edge: e.Edge}
		switch {
		case e.Add != nil:
			u, err := unit(*e.Add)
			if err != nil {
				return nil, none, err
			}
			out.Add = &u
		case e.Split != nil:
			out.Split = &api.PlanSplit{Unit: e.Split.Unit, Why: e.Split.Why}
			for _, w := range e.Split.Into {
				u, err := unit(w)
				if err != nil {
					return nil, none, err
				}
				out.Split.Into = append(out.Split.Into, u)
			}
		case e.Merge != nil:
			u, err := unit(e.Merge.Into)
			if err != nil {
				return nil, none, err
			}
			out.Merge = &api.PlanMerge{Units: e.Merge.Units, Into: u, Why: e.Merge.Why}
		}
		req.Edits = append(req.Edits, out)
	}
	ctx = asAgent(ctx, l.Session)
	// the same call again until the queue has applied it, so each unit
	// comes back with its index
	path := "/jobs/" + url.PathEscape(ns) + "/" + url.PathEscape(name) + "/plan"
	var ans api.PlanResponse
	applied := time.Now().Add(agentAddedWithin)
	for {
		ans = api.PlanResponse{}
		if err := c.do(ctx, "POST", path, req, &ans); err != nil {
			return nil, none, err
		}
		if ans.State != "pending" || !time.Now().Before(applied) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, none, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if ans.State == "refused" {
		return nil, none, fmt.Errorf("the farm refused the plan, and made none of its edits: %s", ans.Error)
	}
	out := PlanOut{Job: in.Job, State: ans.State, Record: ans.Record, Warnings: warnings, Message: ans.Message}
	for _, u := range ans.Units {
		out.Units = append(out.Units, AgentUnit{Name: u.Name, Index: u.Index, Ref: in.Job + "/" + u.Name})
	}
	if len(out.Units) > 0 {
		out.Message += "; held units run once a release names them; wait for released ones with " + waitCommand(out.Units)
	}
	return nil, out, nil
}
