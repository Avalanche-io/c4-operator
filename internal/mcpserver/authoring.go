package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

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
	if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity && json.Unmarshal(ae.Body, &r) == nil && len(r.Errors) > 0 {
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
	return nil, out, nil
}

// --- c4_run_<tool> ---

var mcpName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// runName is the MCP tool that runs a published tool.
func runName(tool string) string { return "c4_run_" + mcpName.ReplaceAllString(tool, "_") }

// runTool is a published tool as its c4_run_ tool: the projects that
// publish it, and its detail in the first of them.
type runTool struct {
	name     string
	projects []string
	detail   api.ToolDetail
}

// runCache keeps each viewer's run tools for runEvery: the streamable
// HTTP handler builds a server for every request.
var (
	runMu    sync.Mutex
	runCache = map[string]runCacheEntry{}
)

const runEvery = 30 * time.Second

type runCacheEntry struct {
	at    time.Time
	tools []runTool
}

// runTools are the viewer's published tools, from the cache when fresh.
// An unreachable HUD gives none: the server still serves its fixed tools.
func (c *Client) runTools(ctx context.Context) []runTool {
	key := c.Base + "\x00" + c.Token + "\x00" + c.As
	runMu.Lock()
	e, ok := runCache[key]
	runMu.Unlock()
	if ok && time.Since(e.at) < runEvery {
		return e.tools
	}
	ps, err := c.projectsFor(ctx, "")
	if err != nil {
		return nil
	}
	by := map[string]*runTool{}
	var names []string
	for _, p := range ps {
		ts, err := c.toolList(ctx, p)
		if err != nil {
			continue
		}
		for _, t := range ts {
			if rt, ok := by[t.Name]; ok {
				rt.projects = append(rt.projects, p)
				continue
			}
			d, err := c.toolDetail(ctx, p, t.Name, "")
			if err != nil {
				continue
			}
			by[t.Name] = &runTool{name: t.Name, projects: []string{p}, detail: *d}
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	var out []runTool
	for _, n := range names {
		out = append(out, *by[n])
	}
	runMu.Lock()
	runCache[key] = runCacheEntry{at: time.Now(), tools: out}
	runMu.Unlock()
	return out
}

// hasRangeParam reports whether a tool declares a RANGE_EXPR parameter
// (a once tool's frames fill it).
func hasRangeParam(d api.ToolDetail) bool {
	var ps []struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(d.Params, &ps)
	for _, p := range ps {
		if p.Type == "RANGE_EXPR" {
			return true
		}
	}
	return false
}

// runSchema is a run tool's input: the tool's parameters as `with`, and
// the submission's own fields.
func runSchema(rt runTool) map[string]any {
	d := rt.detail
	with := d.ParamSchema
	if with == nil {
		with = map[string]any{"type": "object"}
	}
	w := map[string]any{}
	for k, v := range with {
		if k != "$schema" {
			w[k] = v
		}
	}
	w["description"] = "the tool's parameters (c4_describe_tool shows them as written)"
	props := map[string]any{
		"with":    w,
		"shot":    map[string]any{"type": "string", "description": "the shot the job belongs to, for example sm0140"},
		"due":     map[string]any{"type": "string", "description": "when it is due: a milestone (dailies), a time (fri 18:00, +3h, RFC 3339), a date, or suggested (the farm suggests one). Never a priority"},
		"tree":    map[string]any{"type": "string", "description": "what the job reads and writes: a view's C4 ID, or job:<name> for an earlier job's output"},
		"dry_run": map[string]any{"type": "boolean", "description": "check and plan only: return the estimate and create nothing. Do this first and show the person the estimate"},
		"explain": map[string]any{"type": "boolean", "description": "also return what the job lowers to in the operator, as text"},
	}
	var required []string
	if r, ok := with["required"]; ok && r != nil {
		required = append(required, "with")
	}
	if d.Kind == "frames" || (d.Kind == "once" && hasRangeParam(d)) {
		props["frames"] = map[string]any{"type": "string", "description": "the frames, in OpenJD's range grammar: 1001-1240, 1001-1240:2, 1001,1050-1100"}
		if d.Kind == "frames" {
			required = append(required, "frames")
		}
	}
	if len(rt.projects) > 1 {
		props["project"] = map[string]any{"type": "string", "enum": rt.projects, "description": "the project to run it in"}
		required = append(required, "project")
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func runDescription(rt runTool) string {
	d := rt.detail
	what := d.Summary
	if what == "" {
		what = d.Title
	}
	if what == "" {
		what = "Run the tool " + d.Name
	}
	return fmt.Sprintf("%s (tool %s %s, %s). Submits one job for the person you act for; call with dry_run first and show them the estimate.",
		strings.TrimSuffix(what, "."), d.Name, d.Version, d.Kind)
}

// runIn is a run tool's arguments.
type runIn struct {
	Project string          `json:"project,omitempty"`
	With    json.RawMessage `json:"with,omitempty"`
	Shot    string          `json:"shot,omitempty"`
	Frames  string          `json:"frames,omitempty"`
	Due     string          `json:"due,omitempty"`
	Tree    string          `json:"tree,omitempty"`
	DryRun  bool            `json:"dry_run,omitempty"`
	Explain bool            `json:"explain,omitempty"`
}

// runSubmission is the one-job submission a run tool posts, its keys in
// the order a person writes them.
func runSubmission(rt runTool, project string, in runIn) (json.RawMessage, error) {
	var b strings.Builder
	field := func(k string, v any) error {
		j, err := json.Marshal(v)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, ",%q:%s", k, j)
		return nil
	}
	b.WriteString(`{"c4":"submit/1"`)
	for _, f := range []struct {
		k, v string
	}{{"project", project}, {"shot", in.Shot}, {"due", in.Due}, {"tree", in.Tree}} {
		if f.v != "" {
			if err := field(f.k, f.v); err != nil {
				return nil, err
			}
		}
	}
	key := mcpName.ReplaceAllString(rt.name, "-")
	job := `{"tool":` + mustJSON(rt.name+"@"+rt.detail.Version)
	if in.Frames != "" {
		job += `,"frames":` + mustJSON(in.Frames)
	}
	if len(in.With) > 0 && string(in.With) != "null" {
		job += `,"with":` + string(in.With)
	}
	job += "}"
	fmt.Fprintf(&b, `,"jobs":{%s:%s}}`, mustJSON(key), job)
	return json.RawMessage(b.String()), nil
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// addRunTools adds a c4_run_<tool> for each of the viewer's published
// tools.
func (c *Client) addRunTools(ctx context.Context, s *mcp.Server) {
	for _, rt := range c.runTools(ctx) {
		rt := rt
		def := &mcp.Tool{Name: runName(rt.name), Title: "Run " + firstNonEmpty(rt.detail.Title, rt.name), Description: runDescription(rt),
			InputSchema: runSchema(rt), Annotations: &mcp.ToolAnnotations{Title: "Run " + firstNonEmpty(rt.detail.Title, rt.name),
				DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}}
		s.AddTool(def, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in runIn
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return toolError(fmt.Errorf("arguments: %v", err)), nil
			}
			project := in.Project
			if project == "" {
				project = rt.projects[0]
			}
			doc, err := runSubmission(rt, project, in)
			if err != nil {
				return toolError(err), nil
			}
			_, out, err := c.postSubmission(ctx, doc, in.DryRun, in.Explain)
			if err != nil {
				return toolError(err), nil
			}
			b, _ := json.Marshal(out)
			res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: out}
			if !out.OK {
				res.IsError = true
			}
			return res, nil
		})
	}
}

func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
