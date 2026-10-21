package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A person's own agents run on the farm as a bundle (POST /bundles):
// c4_run_bundle makes one, c4_list_bundles lists them, c4_stop_bundle
// holds, resumes or removes one.

type RunBundleIn struct {
	Project string   `json:"project,omitempty" jsonschema:"the project; empty, your only one"`
	Name    string   `json:"name,omitempty" jsonschema:"the bundle's name; empty, agents (numbered if taken)"`
	Members []string `json:"members" jsonschema:"the agents to run, by the names they were added under (c4 farm agents add)"`
	Tasks   []string `json:"tasks,omitempty" jsonschema:"members among those that have one job (task) rather than living as long as the bundle"`
	Max     int32    `json:"max,omitempty" jsonschema:"how many may run at once; 0 runs them all"`
	Due     string   `json:"due,omitempty" jsonschema:"the deadline: an RFC 3339 time, a date, or words like friday 18:00"`
	Pools   []string `json:"pools,omitempty" jsonschema:"where they run; empty, the farm's agent pools that serve you"`
	DryRun  bool     `json:"dry_run,omitempty" jsonschema:"check it and say what would run, making nothing"`
}

type BundleOut struct {
	OK       bool                   `json:"ok"`
	Message  string                 `json:"message"`
	Bundle   string                 `json:"bundle,omitempty"`
	Owner    string                 `json:"owner,omitempty"`
	Link     string                 `json:"link,omitempty"`
	Phase    string                 `json:"phase,omitempty"`
	Due      string                 `json:"due,omitempty"`
	Members  []api.BundleMemberInfo `json:"members,omitempty"`
	Warnings []string               `json:"warnings,omitempty"`
	Errors   []api.FieldError       `json:"errors,omitempty"`
}

func (c *Client) oneProject(ctx context.Context, project string) (string, error) {
	ps, err := c.projectsFor(ctx, project)
	if err != nil {
		return "", err
	}
	if len(ps) > 1 {
		return "", fmt.Errorf("you see %d projects; give project (one of %v)", len(ps), ps)
	}
	return ps[0], nil
}

func (c *Client) runBundle(ctx context.Context, _ *mcp.CallToolRequest, in RunBundleIn) (*mcp.CallToolResult, BundleOut, error) {
	p, err := c.oneProject(ctx, in.Project)
	if err != nil {
		return nil, BundleOut{}, err
	}
	task := map[string]bool{}
	for _, t := range in.Tasks {
		task[t] = true
	}
	req := api.BundleRequest{Name: in.Name, MaxRunning: in.Max, Due: in.Due, Pools: in.Pools, DryRun: in.DryRun}
	for _, m := range in.Members {
		bm := api.BundleMember{Name: m}
		if task[m] {
			bm.Kind = "task"
		}
		req.Members = append(req.Members, bm)
	}
	var r api.BundleResponse
	err = c.do(ctx, "POST", "/bundles/"+url.PathEscape(p), req, &r)
	var ae *APIError
	if errors.As(err, &ae) && json.Unmarshal(ae.Body, &r) == nil && len(r.Errors) > 0 {
		err = nil
	}
	if err != nil {
		return nil, BundleOut{}, err
	}
	out := bundleOut(r)
	switch {
	case !r.OK:
		out.Message = fmt.Sprintf("the farm refused the bundle: %d problems; fix each at its path and run again", len(r.Errors))
	case r.DryRun:
		out.Message = fmt.Sprintf("%s would run %d agents; nothing made (dry run)", r.Name, len(r.Members))
	default:
		out.Message = fmt.Sprintf("%s runs %d agents, as %s's work", r.Name, len(r.Members), r.Owner)
	}
	return nil, out, nil
}

func bundleOut(r api.BundleResponse) BundleOut {
	out := BundleOut{OK: r.OK, Bundle: r.Namespace + "/" + r.Name, Owner: r.Owner, Link: r.Link, Phase: r.Phase,
		Members: r.Members, Warnings: r.Warnings, Errors: r.Errors}
	if r.Name == "" {
		out.Bundle = ""
	}
	if r.Due != nil {
		out.Due = rfc(r.Due)
	}
	return out
}

type ListBundlesIn struct {
	Project string `json:"project,omitempty" jsonschema:"the project; empty, every project you see"`
}

type ListBundlesOut struct {
	Bundles []BundleOut `json:"bundles"`
}

func (c *Client) listBundles(ctx context.Context, _ *mcp.CallToolRequest, in ListBundlesIn) (*mcp.CallToolResult, ListBundlesOut, error) {
	ps, err := c.projectsFor(ctx, in.Project)
	if err != nil {
		return nil, ListBundlesOut{}, err
	}
	out := ListBundlesOut{Bundles: []BundleOut{}}
	for _, p := range ps {
		var rs []api.BundleResponse
		if err := c.do(ctx, "GET", "/bundles/"+url.PathEscape(p), nil, &rs); err != nil {
			return nil, ListBundlesOut{}, err
		}
		for _, r := range rs {
			out.Bundles = append(out.Bundles, bundleOut(r))
		}
	}
	return nil, out, nil
}

type StopBundleIn struct {
	Project string `json:"project,omitempty" jsonschema:"the project; empty, your only one"`
	Name    string `json:"name" jsonschema:"the bundle's name"`
	Do      string `json:"do" jsonschema:"hold (members stop at their next turn's end), resume, or remove (stop and delete the bundle)"`
}

func (c *Client) stopBundle(ctx context.Context, _ *mcp.CallToolRequest, in StopBundleIn) (*mcp.CallToolResult, Done, error) {
	p, err := c.oneProject(ctx, in.Project)
	if err != nil {
		return nil, Done{}, err
	}
	path := "/bundles/" + url.PathEscape(p) + "/" + url.PathEscape(in.Name)
	switch in.Do {
	case "hold", "resume":
		return c.control(ctx, path+"/suspend", map[string]bool{"suspend": in.Do == "hold"})
	case "remove":
		var r Done
		if err := c.do(ctx, "DELETE", path, nil, &r); err != nil {
			return nil, Done{}, err
		}
		return nil, r, nil
	}
	return nil, Done{}, fmt.Errorf("do %q: hold, resume or remove", in.Do)
}
