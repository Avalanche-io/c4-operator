package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// --- c4_run_<tool> ---
//
// Each published tool is its own MCP tool, c4_run_<tool>, whose arguments
// are the tool's own parameters beside the submission's fields (shot,
// frames, due...). The set follows the farm while a session runs: it is
// read again on every tools/list, every runEvery on a stdio session (Serve),
// and at once after c4_publish_tool or a refusal that the tool changed;
// a tool added, changed or gone is added, replaced or removed, and the
// SDK tells the client (notifications/tools/list_changed).

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

func (c *Client) cacheKey() string { return c.Base + "\x00" + c.Token + "\x00" + c.As }

// forget drops the viewer's cached run tools.
func (c *Client) forget() {
	runMu.Lock()
	delete(runCache, c.cacheKey())
	runMu.Unlock()
}

// runTools are the viewer's published tools: from the cache when it is
// fresh, unless fresh asks to list them again, which reads only the
// detail of a tool whose version changed. An unreachable HUD gives the
// last list, else none: the server still serves its fixed tools.
func (c *Client) runTools(ctx context.Context, fresh bool) []runTool {
	key := c.cacheKey()
	runMu.Lock()
	e, ok := runCache[key]
	runMu.Unlock()
	if ok && !fresh && time.Since(e.at) < runEvery {
		return e.tools
	}
	prev := map[string]runTool{}
	for _, rt := range e.tools {
		prev[rt.name] = rt
	}
	ps, err := c.projectsFor(ctx, "")
	if err != nil {
		return e.tools
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
			if old, ok := prev[t.Name]; ok && old.detail.Version == t.Version && len(old.projects) > 0 && old.projects[0] == p {
				by[t.Name] = &runTool{name: t.Name, projects: []string{p}, detail: old.detail}
			} else {
				d, err := c.toolDetail(ctx, p, t.Name, "")
				if err != nil {
					continue
				}
				by[t.Name] = &runTool{name: t.Name, projects: []string{p}, detail: *d}
			}
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

// syncRunTools makes the server's run tools the viewer's published ones:
// adds the new, replaces the changed (another version, other projects),
// removes the gone. Each change notifies the client.
func (c *Client) syncRunTools(ctx context.Context, s *mcp.Server, fresh bool) {
	if c == nil || c.Base == "" || s == nil {
		return
	}
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	want := map[string]string{}
	var add []runTool
	for _, rt := range c.runTools(ctx, fresh) {
		def := runDef(rt)
		b, _ := json.Marshal(def)
		h := sha256.Sum256(b)
		fp := hex.EncodeToString(h[:8])
		want[def.Name] = fp
		if c.added[def.Name] != fp {
			add = append(add, rt)
		}
	}
	var gone []string
	for n := range c.added {
		if _, ok := want[n]; !ok {
			gone = append(gone, n)
		}
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		s.RemoveTools(gone...)
	}
	for _, rt := range add {
		s.AddTool(runDef(rt), c.runHandler(rt, s))
	}
	c.added = want
}

// hasRangeParam reports whether a tool declares a RANGE_EXPR parameter
// (a once tool's frames fill it).
func hasRangeParam(d api.ToolDetail) bool {
	for _, p := range params(d) {
		if p.Type == "RANGE_EXPR" {
			return true
		}
	}
	return false
}

type param struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// params are a tool's parameters in the order declared.
func params(d api.ToolDetail) []param {
	var ps []param
	_ = json.Unmarshal(d.Params, &ps)
	return ps
}

// fields are a run tool's own arguments: the submission's.
var fields = map[string]bool{"project": true, "shot": true, "frames": true, "due": true, "tree": true, "dry_run": true,
	"key": true, "explain": true}

// argName is the argument a tool's parameter is given as: its own name,
// or with_<name> where that is one of the submission's fields.
func argName(param string) string {
	if fields[param] || param == "with" {
		return "with_" + param
	}
	return param
}

// runSchema is a run tool's input: the tool's parameters, each an
// argument of its own, beside the submission's fields.
func runSchema(rt runTool) map[string]any {
	d := rt.detail
	props := map[string]any{
		"shot":    map[string]any{"type": "string", "description": "the shot the job belongs to, for example sm0140"},
		"due":     map[string]any{"type": "string", "description": "when it is due: a milestone (dailies), a time (fri 18:00, +3h, RFC 3339), a date, or suggested (the farm suggests one). Never a priority"},
		"tree":    map[string]any{"type": "string", "description": "what the job reads and writes: a view's C4 ID, or job:<name> for an earlier job's output"},
		"dry_run": map[string]any{"type": "boolean", "description": "check and plan only: return the estimate and create nothing. Do this first and show the person the estimate"},
		"key":     map[string]any{"type": "string", "description": "always pass one: a short name of your own for this run (the same one on every retry of it, a new one for the next): a retry after an error or a timeout is then answered with the job the farm made, never a second"},
		"explain": map[string]any{"type": "boolean", "description": "also return what the job lowers to in the operator, as text"},
	}
	var required []string
	pprops := asMap(d.ParamSchema["properties"])
	for name, ps := range pprops {
		props[argName(name)] = ps
	}
	if r, ok := d.ParamSchema["required"].([]any); ok {
		for _, n := range r {
			if s, ok := n.(string); ok {
				required = append(required, argName(s))
			}
		}
	}
	if r, ok := d.ParamSchema["required"].([]string); ok {
		for _, s := range r {
			required = append(required, argName(s))
		}
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
	sort.Strings(required)
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
	return fmt.Sprintf("%s (tool %s %s, %s). Its parameters are arguments of their own. Submits one job for the person you act for; call with dry_run first and show them the estimate.",
		strings.TrimSuffix(what, "."), d.Name, d.Version, d.Kind)
}

func runDef(rt runTool) *mcp.Tool {
	title := "Run " + firstNonEmpty(rt.detail.Title, rt.name)
	return &mcp.Tool{Name: runName(rt.name), Title: title, Description: runDescription(rt), InputSchema: runSchema(rt),
		Annotations: &mcp.ToolAnnotations{Title: title, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}}
}

// runIn is a run tool's submission fields.
type runIn struct {
	Project string `json:"project,omitempty"`
	Shot    string `json:"shot,omitempty"`
	Frames  string `json:"frames,omitempty"`
	Due     string `json:"due,omitempty"`
	Tree    string `json:"tree,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
	Explain bool   `json:"explain,omitempty"`
	Key     string `json:"key,omitempty"`
}

// runArgs splits a run tool's arguments into the submission's fields and
// the tool's parameters (by the parameters' own names). An argument that
// is neither is refused, with the nearest name it may have meant.
func runArgs(rt runTool, raw json.RawMessage) (runIn, map[string]json.RawMessage, []api.FieldError) {
	var in runIn
	var args map[string]json.RawMessage
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return in, nil, []api.FieldError{{Path: "arguments", Message: "the arguments are not an object: " + err.Error(), Fix: "give each as name: value"}}
		}
	}
	byArg := map[string]string{}
	for name := range asMap(rt.detail.ParamSchema["properties"]) {
		byArg[argName(name)] = name
	}
	own := map[string]json.RawMessage{}
	with := map[string]json.RawMessage{}
	var errs []api.FieldError
	known := make([]string, 0, len(fields)+len(byArg))
	for f := range fields {
		known = append(known, f)
	}
	for a := range byArg {
		known = append(known, a)
	}
	sort.Strings(known)
	for k, v := range args {
		switch {
		case fields[k]:
			own[k] = v
		case byArg[k] != "":
			with[byArg[k]] = v
		default:
			fe := api.FieldError{Path: k, Message: fmt.Sprintf("%s takes no argument %q", rt.name, k)}
			if n := nearest(k, known); n != "" {
				fe.Fix = fmt.Sprintf("did you mean %s? c4_describe_tool %s lists its parameters", n, rt.name)
			} else {
				fe.Fix = fmt.Sprintf("c4_describe_tool %s lists its parameters", rt.name)
			}
			if k == "with" {
				fe.Fix = "give the tool's parameters as arguments of their own (plates: ..., not with: {plates: ...})"
			}
			errs = append(errs, fe)
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	b, _ := json.Marshal(own)
	if err := json.Unmarshal(b, &in); err != nil {
		errs = append(errs, api.FieldError{Path: "arguments", Message: err.Error(), Fix: "check each argument's type against the tool's schema"})
	}
	return in, with, errs
}

// nearest is the known name closest to s, if one is close enough to have
// been meant: within a third of its length in edits, at least one.
func nearest(s string, known []string) string {
	best, bestD := "", 0
	for _, k := range known {
		d := editDistance(strings.ToLower(s), strings.ToLower(k))
		if best == "" || d < bestD {
			best, bestD = k, d
		}
	}
	if best == "" || bestD > max(1, len(s)/3) {
		return ""
	}
	return best
}

// editDistance counts the edits from a to b: an insertion, a deletion, a
// substitution, or two letters swapped (optimal string alignment).
func editDistance(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

// runSubmission is the one-job submission a run tool posts, its keys in
// the order a person writes them, and the tool's parameters in the order
// it declares them.
func runSubmission(rt runTool, project string, in runIn, with map[string]json.RawMessage) (json.RawMessage, error) {
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
	if len(with) > 0 {
		var order []string
		seen := map[string]bool{}
		for _, p := range params(rt.detail) {
			if _, ok := with[p.Name]; ok {
				order, seen[p.Name] = append(order, p.Name), true
			}
		}
		var rest []string
		for n := range with {
			if !seen[n] {
				rest = append(rest, n)
			}
		}
		sort.Strings(rest)
		var w strings.Builder
		for i, n := range append(order, rest...) {
			if i > 0 {
				w.WriteByte(',')
			}
			fmt.Fprintf(&w, "%s:%s", mustJSON(n), with[n])
		}
		job += `,"with":{` + w.String() + "}"
	}
	job += "}"
	fmt.Fprintf(&b, `,"jobs":{%s:%s}}`, mustJSON(key), job)
	return json.RawMessage(b.String()), nil
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// toolChanged says whether a refusal is the tool's form or version having
// moved since this run tool was drawn (its pin is stale).
func toolChanged(errs []api.FieldError) bool {
	for _, e := range errs {
		if strings.HasSuffix(e.Path, ".tool") || strings.HasSuffix(e.Path, ".version") {
			return true
		}
	}
	return false
}

// runHandler runs one published tool: its arguments as a one-job
// submission. A refusal is an answer (its errors say where and how to fix
// it); only a HUD that could not be reached is an error. A refusal that
// the tool changed draws it again.
func (c *Client) runHandler(rt runTool, s *mcp.Server) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		answer := func(out SubmitOut) (*mcp.CallToolResult, error) {
			b, _ := json.Marshal(out)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: out}, nil
		}
		in, with, errs := runArgs(rt, req.Params.Arguments)
		if len(errs) > 0 {
			return answer(SubmitOut{Errors: errs,
				Message: fmt.Sprintf("%s refused its arguments: %d problems; fix each and call it again", runName(rt.name), len(errs))})
		}
		project := in.Project
		if project == "" {
			project = rt.projects[0]
		}
		doc, err := runSubmission(rt, project, in, with)
		if err != nil {
			return toolError(err), nil
		}
		_, out, err := c.postSubmission(ctx, doc, nil, in.DryRun, in.Explain, in.Key)
		if err != nil {
			return toolError(err), nil
		}
		if !out.OK && toolChanged(out.Errors) {
			c.forget()
			c.syncRunTools(ctx, s, true)
			out.Message += fmt.Sprintf("; %s is drawn again with the tool's current form: read its arguments and call it again", runName(rt.name))
		}
		return answer(out)
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
