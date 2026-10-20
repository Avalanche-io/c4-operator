package api

// Tracker is a Tracker as GET /api/v1/trackers lists it: one connection
// from a project to a tracker site, in words a person reads, so they can
// choose one to let submit work for them (internal/hud/API.md, Grants).
type Tracker struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Project   string `json:"project"`
	// Kind is the adapter: github, flow, jira, linear, ...
	Kind string `json:"kind"`
	// Site is what it connects to: "github.com/petrelfinch/pipeline",
	// "Flow Production Tracking, Saltmarsh".
	Site string `json:"site"`
	// Syncs is what it turns into jobs, one entry per intake rule:
	// "pull requests", "issues labeled render".
	Syncs []string `json:"syncs,omitempty"`
	// Ready says it is connected; Why says why not.
	Ready bool   `json:"ready"`
	Why   string `json:"why,omitempty"`
	// Granted is how many people's grants to it serve now.
	Granted int `json:"granted"`
	// Mine is the viewer's own grant to it, if any, active or not.
	Mine *Grant `json:"mine,omitempty"`
}

// TrackerList answers GET /api/v1/trackers.
type TrackerList struct {
	Trackers []Tracker `json:"trackers"`
}
