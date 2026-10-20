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
}

func toolNames(refs []ToolRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Name
	}
	return out
}
