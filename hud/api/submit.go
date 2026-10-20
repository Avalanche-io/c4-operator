package api

import (
	"encoding/json"
	"time"
)

// MissingRequest is the body of POST /objects/{project}/missing.
type MissingRequest struct {
	IDs []string `json:"ids"`
}

// MissingResponse lists the IDs the project's store lacks.
type MissingResponse struct {
	Missing []string `json:"missing"`
}

// ViewFile is one file of a view: its path in the tree, its C4 ID and size.
type ViewFile struct {
	Path string `json:"path"`
	ID   string `json:"id"`
	Size int64  `json:"size"`
	Exec bool   `json:"exec,omitempty"`
}

// ViewRequest is the body of POST /views/{project}: a tree to record as a
// view. Every file's object must already be in the project's store.
type ViewRequest struct {
	Files []ViewFile `json:"files"`
	// Dirs are the tree's empty directories (one with anything in it is
	// implied by its paths), and Links its symbolic links.
	Dirs  []string   `json:"dirs,omitempty"`
	Links []ViewLink `json:"links,omitempty"`
}

// ViewLink is a symbolic link of a view, recorded as given and never
// followed by the farm.
type ViewLink struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

// ViewResponse is a recorded view, or the objects it still needs.
type ViewResponse struct {
	View    string   `json:"view,omitempty"`
	Files   int      `json:"files"`
	Bytes   int64    `json:"bytes"`
	Missing []string `json:"missing,omitempty"`
}

// ViewListing is GET /views/{project}/{id}/files.
type ViewListing struct {
	View      string     `json:"view"`
	Files     []ViewFile `json:"files"`
	Dirs      []string   `json:"dirs,omitempty"`
	Links     []ViewLink `json:"links,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

// SubmitInput is one input of a submitted job: a view by C4 ID (the HUD
// makes its Materialization), or the output of another job.
type SubmitInput struct {
	View      string `json:"view,omitempty"`
	FromJob   string `json:"fromJob,omitempty"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// SubmitRequest is the body of POST /jobs.
type SubmitRequest struct {
	// Project, or Namespace (one of the project's namespaces). With only
	// Project, the job goes in the project's first namespace.
	Project   string `json:"project,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// Name of the job; empty makes <shot>-<stage>-vNNN, one version past
	// the latest such job.
	Name string `json:"name,omitempty"`
	// Type is the prebuilt job type it came from (render, usd, comp, ...).
	Type string `json:"type,omitempty"`
	// Stage is the pipeline stage the job is (light, layout, fx, comp,
	// review, train, ...), the middle of its default name; default Type.
	Stage string `json:"stage,omitempty"`
	Shot  string `json:"shot,omitempty"`
	// Spec is the WorkJob's spec (c4.avalanche.io/v1alpha1 WorkJobSpec)
	// as JSON. Its inputs may name Materializations already in the
	// namespace; Inputs below add views and jobs.
	Spec   json.RawMessage `json:"spec"`
	Inputs []SubmitInput   `json:"inputs,omitempty"`
	// AIUsage is the job's declared AI use (DESIGN.md section 9.4), as JSON.
	AIUsage map[string]any `json:"aiUsage,omitempty"`
	// DryRun plans the job and returns its estimate; nothing is created.
	DryRun bool `json:"dryRun,omitempty"`
	// TrackerRef is the tracker entity the job is made for,
	// <tracker>/<type>/<id>: the job carries it (annotation
	// c4.avalanche.io/tracker-ref, label c4.avalanche.io/tracker) and its
	// record names it.
	TrackerRef string `json:"trackerRef,omitempty"`
	// Follows names the job in the namespace this one is the next version
	// of (spec.follows).
	Follows string `json:"follows,omitempty"`
	// Due is the job's deadline as a date ("2026-10-23"), at the time of
	// day the project's due-time convention gives (18:00 UTC when it has
	// none), or as an RFC 3339 time. It pins spec.deadline.
	Due string `json:"due,omitempty"`
	// For is the person the job is submitted for, by a tracker's sync
	// identity under the grant that person gave the Tracker TrackerRef
	// names: the person owns the job (internal/hud/API.md, Grants).
	For string `json:"for,omitempty"`
	// Replaces names the forecast job in the namespace this real job
	// replaces (spec.forecast, DESIGN.md 10.12): it is deleted once
	// this one is made, and the book says so.
	Replaces string `json:"replaces,omitempty"`
}

// SubmitEstimate is what the farm expects of a job before it runs.
type SubmitEstimate struct {
	Units int `json:"units"`
	// Remaining work in slot-seconds at the 50th and 90th percentile.
	Work50 float64 `json:"work50"`
	Work90 float64 `json:"work90"`
	// ETA50 to ETA95 are the finish shown to people.
	ETA50 *time.Time `json:"eta50,omitempty"`
	ETA80 *time.Time `json:"eta80,omitempty"`
	ETA90 *time.Time `json:"eta90,omitempty"`
	ETA95 *time.Time `json:"eta95,omitempty"`
	// Deadline is the effective deadline the plan would use, and where it
	// came from: spec, suggested or class.
	Deadline       *time.Time        `json:"deadline,omitempty"`
	DeadlineSource string            `json:"deadlineSource,omitempty"`
	Suggestion     *SubmitSuggestion `json:"suggestion,omitempty"`
	SlackSeconds   int64             `json:"slackSeconds"`
	Feasible       bool              `json:"feasible"`
	AtRisk         bool              `json:"atRisk"`
	Calibrated     bool              `json:"calibrated,omitempty"`
	Message        string            `json:"message,omitempty"`
}

// SubmitSuggestion is the deadline the farm would suggest for a job whose
// deadline mode is Suggested.
type SubmitSuggestion struct {
	Deadline time.Time `json:"deadline"`
	// State is Estimating, Settling or Settled; Policy is the deadline
	// policy (achievable, sessions, turnaround) and Quantile the finish
	// percentile it meets.
	State    string `json:"state"`
	Policy   string `json:"policy"`
	Quantile string `json:"quantile"`
	// NextSession is the following session when the job is not expected
	// to make this one.
	NextSession *time.Time `json:"nextSession,omitempty"`
}

// SubmitResponse answers POST /jobs.
type SubmitResponse struct {
	OK        bool            `json:"ok"`
	DryRun    bool            `json:"dryRun,omitempty"`
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Project   string          `json:"project"`
	Estimate  *SubmitEstimate `json:"estimate,omitempty"`
	// EstimateError says why there is no estimate (the queue has not
	// planned yet); the job is still created.
	EstimateError    string   `json:"estimateError,omitempty"`
	Materializations []string `json:"materializations,omitempty"`
	Record           string   `json:"record,omitempty"`
	// Link is the job view's HUD route.
	Link    string `json:"link"`
	Message string `json:"message"`
}
