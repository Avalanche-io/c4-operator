package api

import "encoding/json"

// Tools on the farm (DESIGN.md 10.6, Tools; the format is schema/README.md):
// a tool is a folder with a tool.yaml (c4: tool/1), published into a
// project or the whole farm, and offered to submissions and agents.

// ToolPublishRequest is POST /tools/{project}: a folder's tree, already
// stored through PUT /objects and POST /views.
type ToolPublishRequest struct {
	View string `json:"view"`
	// Farm publishes to every project (the farm admin role).
	Farm bool `json:"farm,omitempty"`
	// DryRun checks the tool and pins its images, writing nothing.
	DryRun bool `json:"dryRun,omitempty"`
	// TestedIn is ignored: whether a tree is one of the release's own
	// tools, tested in it, is the server's to decide from the tree's ID.
	TestedIn string `json:"testedIn,omitempty"`
}

// ToolTestState is a version's test run on the farm.
type ToolTestState struct {
	// Job is the test job, <namespace>/<name>; empty for a tool with no
	// test.
	Job string `json:"job,omitempty"`
	// State is running, ok or failed; or untested for a person's tool
	// with no test, offered at once.
	State   string `json:"state"`
	Seconds int64  `json:"seconds,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// TestedIn is the release whose tests ran it (a release's own tool).
	TestedIn string `json:"testedIn,omitempty"`
}

// ToolPublishResponse answers a publish.
type ToolPublishResponse struct {
	OK     bool         `json:"ok"`
	Errors []FieldError `json:"errors,omitempty"`
	// Tool and Version name what was published (v<N>, per tool and
	// scope); View is the published tree, the folder with tool.lock, and
	// Image the unit image pinned by digest.
	Tool    string `json:"tool,omitempty"`
	Version string `json:"version,omitempty"`
	View    string `json:"view,omitempty"`
	Image   string `json:"image,omitempty"`
	Scope   string `json:"scope,omitempty"`
	// Lock is the tool.lock written (or, with DryRun, that would be).
	Lock json.RawMessage `json:"lock,omitempty"`
	// Test is the version's test; publish returns once its job exists,
	// and GET /tools/{project}/{tool}?version= says how it went.
	Test   *ToolTestState `json:"test,omitempty"`
	DryRun bool           `json:"dryRun,omitempty"`
}

// ToolSummary is a published tool in GET /tools/{project}.
type ToolSummary struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	Kind    string `json:"kind"`
	// Version is the version in use; View its tree; Image its unit image.
	Version string `json:"version"`
	View    string `json:"view"`
	Image   string `json:"image,omitempty"`
	// Scope is project, or farm for a farm-wide tool. A project's tool
	// shadows a farm tool of the same name.
	Scope string `json:"scope"`
	// Untested marks a version offered with no test that passed (a
	// person's tool with no test); the Submit panel says so.
	Untested bool `json:"untested,omitempty"`
}

// ToolListResponse is GET /tools/{project}.
type ToolListResponse struct {
	Tools []ToolSummary `json:"tools"`
}

// ToolVersionInfo is one publish of a tool.
type ToolVersionInfo struct {
	Version string        `json:"version"`
	View    string        `json:"view"`
	Image   string        `json:"image,omitempty"`
	By      string        `json:"by"`
	At      string        `json:"at"`
	Test    ToolTestState `json:"test"`
}

// ToolDetail is GET /tools/{project}/{tool}[?version=]: the version asked
// for (the one in use by default), as its tool.yaml declares it.
type ToolDetail struct {
	ToolSummary
	// Tool is the version's tool.yaml as JSON, keys in the order written,
	// for a form to draw from (tool.params, frames, needs.sizes, kind).
	Tool json.RawMessage `json:"tool,omitempty"`
	// Params are the tool.yaml's params as written, and ParamSchema the
	// JSON Schema of a submission's `with` for this tool.
	Params      json.RawMessage `json:"params,omitempty"`
	ParamSchema map[string]any  `json:"paramSchema"`
	Frames      json.RawMessage `json:"frames,omitempty"`
	Needs       json.RawMessage `json:"needs,omitempty"`
	// Test is this version's test on the farm.
	Test *ToolTestState `json:"test,omitempty"`
	// Versions are the tool's publishes in this scope, newest last.
	Versions []ToolVersionInfo `json:"versions,omitempty"`
	// Earlier are the versions older than those, by tree.
	Earlier []ToolVersionInfo `json:"earlier,omitempty"`
}
