package authoring

import (
	"errors"
	"time"
)

// ToolRef is one published version of a tool.
type ToolRef struct {
	Name string `json:"name"`
	// Version is v<N>, counted per tool per scope.
	Version string `json:"version"`
	// View is the published tree's C4 ID, with its tool.lock.
	View string `json:"view"`
	// Image is the tool's image pinned by digest, <repo>@sha256:...,
	// from tool.lock (images[runs.image]).
	Image string `json:"image,omitempty"`
	// Scope is project or farm.
	Scope string `json:"scope"`
}

// ErrNoTool is what Farm.Tool wraps when a project has no such tool, or no
// such version of it.
var ErrNoTool = errors.New("no such tool")

// Farm is what checking and lowering look up: the HUD's view of the farm
// on the server, fixtures/farm.yaml in tests.
type Farm interface {
	// Tools are a project's tools at their current versions, its own
	// shadowing the farm's of the same name.
	Tools(project string) []ToolRef
	// Tool is one tool, parsed from its version's tool.yaml; version ""
	// is the current one.
	Tool(project, name, version string) (*Tool, ToolRef, error)
	// Jobs are the names of the jobs in a namespace.
	Jobs(namespace string) []string
	// Milestones are a namespace's Milestones by name, with their dues.
	Milestones(namespace string) map[string]time.Time
	// People, Roles and Parties are who may approve, wait or answer.
	People() []string
	Roles() []string
	Parties() []string
	// Root is where the project's trees mount (Project spec.root).
	Root(project string) string
	// Namespace is the project's namespace for a submission's jobs.
	Namespace(project string) string
	// Cut is a shot's frames as the farm knows them (a frame list), or
	// "": a frames job given no frames of its own takes them.
	Cut(project, shot string) string
	// Location is the project's time zone, which a due without one is
	// read in (nil: UTC).
	Location(project string) *time.Location
	// Policy is the project's policy as a submission may name and narrow
	// it, or nil when the farm keeps none to check against.
	Policy(project string) *FarmPolicy
}

// FarmPolicy is a project's policy as a submission sees it: the sets it
// may name, and the values it may narrow (only ever lower).
type FarmPolicy struct {
	Sets   []string               `json:"sets"`
	Values map[string]PolicyValue `json:"values"`
}

// PolicyValue is one value of a project's policy: a number, or a duration
// as written (2h).
type PolicyValue struct {
	Value      any  `json:"value"`
	Narrowable bool `json:"narrowable"`
}

func toolNames(refs []ToolRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Name
	}
	return out
}
