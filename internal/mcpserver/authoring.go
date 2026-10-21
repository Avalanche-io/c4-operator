package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Tools (API.md, tools and submissions). A tool is a folder with a tool.yaml
// published to a project (POST /tools): c4_list_tools and
// c4_describe_tool read them, c4_publish_tool publishes one, and every
// published tool is also its own MCP tool, c4_run_<tool>, whose input is
// the tool's parameters and which submits a one-job submission.

type ListToolsIn struct {
	Project string `json:"project,omitempty" jsonschema:"one project; omit for every project you see"`
}

// ToolRow is one published tool.
type ToolRow struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	Kind    string `json:"kind" jsonschema:"frames (a chunk of frames per unit), once (one run), shards, train, agent, generator or check"`
	Version string `json:"version" jsonschema:"the version in use (v<N>)"`
	Scope   string `json:"scope" jsonschema:"project, or farm (published to every project)"`
	Image   string `json:"image,omitempty" jsonschema:"the image it runs, pinned by digest"`
	Run     string `json:"run,omitempty" jsonschema:"the MCP tool that runs it (c4_run_<tool>)"`
}

type ListToolsOut struct {
	Tools []ToolRow `json:"tools"`
}

type DescribeToolIn struct {
	Project string `json:"project,omitempty" jsonschema:"the project; may be omitted when only one of yours has the tool"`
	Tool    string `json:"tool" jsonschema:"the tool's name"`
	Version string `json:"version,omitempty" jsonschema:"a version (v<N>); default the one in use"`
}

type PublishToolIn struct {
	Project string `json:"project,omitempty" jsonschema:"the project to publish to; may be omitted when you see only one"`
	View    string `json:"view" jsonschema:"the C4 ID of the tool's folder (a tree with tool.yaml at its root), already uploaded to the project's store"`
	Farm    bool   `json:"farm,omitempty" jsonschema:"publish to every project (farm admins only)"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"check the tool and pin its image, and return the tool.lock it would write; publish nothing and run no test"`
}

type PublishToolOut struct {
	OK      bool               `json:"ok"`
	Errors  []api.FieldError   `json:"errors,omitempty" jsonschema:"each problem in tool.yaml at its dotted path, with the fix"`
	Tool    string             `json:"tool,omitempty"`
	Version string             `json:"version,omitempty"`
	View    string             `json:"view,omitempty" jsonschema:"the published tree, with its tool.lock"`
	Image   string             `json:"image,omitempty" jsonschema:"the image pinned by digest"`
	Scope   string             `json:"scope,omitempty"`
	Lock    json.RawMessage    `json:"lock,omitempty" jsonschema:"the tool.lock written, or that would be (dry run)"`
	Test    *api.ToolTestState `json:"test,omitempty" jsonschema:"the tool's test on the farm: running, ok or failed"`
	Message string             `json:"message"`
}

// projectsFor is project, or every project the viewer sees.
func (c *Client) projectsFor(ctx context.Context, project string) ([]string, error) {
	if project != "" {
		return []string{project}, nil
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if s.Viewer == nil || len(s.Viewer.Projects) == 0 {
		return nil, errors.New("you see no project; give project, or ask for access to one")
	}
	ps := append([]string(nil), s.Viewer.Projects...)
	sort.Strings(ps)
	return ps, nil
}

func (c *Client) toolList(ctx context.Context, project string) ([]api.ToolSummary, error) {
	var r api.ToolListResponse
	if err := c.do(ctx, "GET", "/tools/"+url.PathEscape(project), nil, &r); err != nil {
		return nil, err
	}
	return r.Tools, nil
}

func (c *Client) listTools(ctx context.Context, _ *mcp.CallToolRequest, in ListToolsIn) (*mcp.CallToolResult, ListToolsOut, error) {
	ps, err := c.projectsFor(ctx, in.Project)
	if err != nil {
		return nil, ListToolsOut{}, err
	}
	out := ListToolsOut{Tools: []ToolRow{}}
	for _, p := range ps {
		ts, err := c.toolList(ctx, p)
		if err != nil {
			return nil, ListToolsOut{}, err
		}
		for _, t := range ts {
			out.Tools = append(out.Tools, ToolRow{Project: p, Name: t.Name, Title: t.Title, Summary: t.Summary, Kind: t.Kind,
				Version: t.Version, Scope: t.Scope, Image: t.Image, Run: runName(t.Name)})
		}
	}
	return nil, out, nil
}

// projectOf is the project a tool is described or run in: project, or
// the one of the viewer's projects that has it.
func (c *Client) projectOf(ctx context.Context, project, tool string) (string, error) {
	if project != "" {
		return project, nil
	}
	ps, err := c.projectsFor(ctx, "")
	if err != nil {
		return "", err
	}
	var has []string
	for _, p := range ps {
		ts, err := c.toolList(ctx, p)
		if err != nil {
			return "", err
		}
		for _, t := range ts {
			if t.Name == tool {
				has = append(has, p)
			}
		}
	}
	switch len(has) {
	case 0:
		return "", fmt.Errorf("no tool %q in your projects; c4_list_tools lists them", tool)
	case 1:
		return has[0], nil
	}
	return "", fmt.Errorf("tool %q is in projects %s; give project", tool, strings.Join(has, ", "))
}

func (c *Client) toolDetail(ctx context.Context, project, tool, version string) (*api.ToolDetail, error) {
	path := "/tools/" + url.PathEscape(project) + "/" + url.PathEscape(tool)
	if version != "" {
		path += "?version=" + url.QueryEscape(version)
	}
	var d api.ToolDetail
	return &d, c.do(ctx, "GET", path, nil, &d)
}

func (c *Client) describeTool(ctx context.Context, _ *mcp.CallToolRequest, in DescribeToolIn) (*mcp.CallToolResult, map[string]any, error) {
	if in.Tool == "" {
		return nil, nil, errors.New("tool is required; c4_list_tools lists them")
	}
	p, err := c.projectOf(ctx, in.Project, in.Tool)
	if err != nil {
		return nil, nil, err
	}
	d, err := c.toolDetail(ctx, p, in.Tool, in.Version)
	if err != nil {
		return nil, nil, err
	}
	var out map[string]any
	if err := remarshal(d, &out); err != nil {
		return nil, nil, err
	}
	out["project"], out["run"] = p, runName(d.Name)
	return nil, out, nil
}

func (c *Client) publishTool(ctx context.Context, _ *mcp.CallToolRequest, in PublishToolIn) (*mcp.CallToolResult, PublishToolOut, error) {
	if in.View == "" {
		return nil, PublishToolOut{}, errors.New("view is required: the C4 ID of the tool's folder, uploaded to the project's store")
	}
	p := in.Project
	if p == "" {
		ps, err := c.projectsFor(ctx, "")
		if err != nil {
			return nil, PublishToolOut{}, err
		}
		if len(ps) != 1 {
			return nil, PublishToolOut{}, fmt.Errorf("you see projects %s; give project", strings.Join(ps, ", "))
		}
		p = ps[0]
	}
	var r api.ToolPublishResponse
	err := c.do(ctx, "POST", "/tools/"+url.PathEscape(p), api.ToolPublishRequest{View: in.View, Farm: in.Farm, DryRun: in.DryRun}, &r)
	var ae *APIError
	if errors.As(err, &ae) && json.Unmarshal(ae.Body, &r) == nil && len(r.Errors) > 0 {
		err = nil
	}
	if err != nil {
		return nil, PublishToolOut{}, err
	}
	out := PublishToolOut{OK: r.OK, Errors: r.Errors, Tool: r.Tool, Version: r.Version, View: r.View, Image: r.Image, Scope: r.Scope,
		Lock: r.Lock, Test: r.Test}
	switch {
	case !r.OK:
		out.Message = fmt.Sprintf("the farm refused the tool: %d problems in tool.yaml; fix each at its path and publish again", len(r.Errors))
	case in.DryRun:
		out.Message = fmt.Sprintf("%s checks out; this is the tool.lock it would write. Publish without dry_run to test it on the farm", r.Tool)
	case r.Test != nil && r.Test.State == "running":
		out.Message = fmt.Sprintf("%s %s is published, and its test runs on the farm as %s. It is offered only once the test passes: "+
			"call c4_describe_tool with version %s until its test is ok or failed", r.Tool, r.Version, r.Test.Job, r.Version)
	default:
		out.Message = fmt.Sprintf("%s %s is published", r.Tool, r.Version)
	}
	if r.OK && !in.DryRun {
		// The run tools show it as soon as the farm offers it.
		c.forget()
		c.syncRunTools(ctx, c.server, true)
	}
	return nil, out, nil
}
