package api

import (
	"encoding/json"
	"time"
)

// BundleRequest is POST /bundles/{project}: a bundle of the person's own
// agents, made without kubectl (`c4 farm agents run`, MCP c4_run_bundle).
type BundleRequest struct {
	// Name is the bundle's name; empty, "agents", numbered if taken.
	Name string `json:"name,omitempty"`
	// Members are the agents, by the names a runner registered them under
	// (c4 farm agents add); a member may also say its kind and its next
	// prompt.
	Members []BundleMember `json:"members"`
	// MaxRunning is how many members may run at once (0: all).
	MaxRunning int32 `json:"maxRunning,omitempty"`
	// Due is the bundle's deadline: an RFC 3339 time, a date, or words
	// ("friday 18:00") read on the farm's clock.
	Due string `json:"due,omitempty"`
	// Pools are where its members run; empty, the agent class's pools.
	Pools []string `json:"pools,omitempty"`
	// Suspend makes it held: nothing runs until it is started.
	Suspend bool `json:"suspend,omitempty"`
	DryRun  bool `json:"dryRun,omitempty"`
}

// BundleMember is one agent of a bundle request. In JSON it is either the
// member's name or an object.
type BundleMember struct {
	Name   string `json:"name"`
	Kind   string `json:"kind,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

// UnmarshalJSON takes a bare name as a member with that name.
func (m *BundleMember) UnmarshalJSON(b []byte) error {
	var name string
	if json.Unmarshal(b, &name) == nil {
		*m = BundleMember{Name: name}
		return nil
	}
	type plain BundleMember
	return json.Unmarshal(b, (*plain)(m))
}

// BundleResponse answers POST /bundles/{project} and describes a bundle
// in GET /bundles/{project}/{name}.
type BundleResponse struct {
	OK        bool   `json:"ok"`
	DryRun    bool   `json:"dryRun,omitempty"`
	Name      string `json:"name,omitempty"`
	Project   string `json:"project,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Owner     string `json:"owner,omitempty"`
	// Link is the bundle's job in the HUD.
	Link     string             `json:"link,omitempty"`
	Phase    string             `json:"phase,omitempty"`
	Due      *time.Time         `json:"due,omitempty"`
	Members  []BundleMemberInfo `json:"members,omitempty"`
	Warnings []string           `json:"warnings,omitempty"`
	Errors   []FieldError       `json:"errors,omitempty"`
}

// BundleMemberInfo is a member as the farm sees it.
type BundleMemberInfo struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Node  string `json:"node,omitempty"`
}
