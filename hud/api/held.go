package api

import "time"

// HeldWrite is a write to a tracker that c4-sync holds (integrations GAPS
// 11): it was interrupted, and nothing the tracker offers can tell whether
// it happened, so it is neither sent again nor dropped until a person says
// which. Look in the tracker (Entity's URL) and confirm.
type HeldWrite struct {
	Namespace string `json:"namespace"`
	Project   string `json:"project"`
	// Link is the TrackerLink holding it; Field the write (comment, check,
	// status, ...). Together they name it for POST /held.
	Link  string `json:"link"`
	Field string `json:"field"`
	// Tracker is the Tracker's name; Kind its kind (github, flow, ...).
	Tracker string `json:"tracker"`
	Kind    string `json:"kind"`
	// Entity is what it was written to: an issue, a task.
	Entity HeldEntity `json:"entity"`
	// Job is the job the link serves (namespace/name), and Owner its
	// owner.
	Job   string `json:"job,omitempty"`
	Owner string `json:"owner,omitempty"`
	// State is the work state the write carried (running, needs-person,
	// ...), when it carried one.
	State string    `json:"state,omitempty"`
	Since time.Time `json:"since"`
	// Says is the write in words: "the GitHub comment on Issue 7, saying
	// running".
	Says string `json:"says"`
	// Yours: the job is the viewer's, so it is theirs to confirm.
	Yours bool `json:"yours"`
}

// HeldEntity is a tracker entity.
type HeldEntity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	URL  string `json:"url,omitempty"`
}

// HeldList answers GET /api/v1/held.
type HeldList struct {
	Held []HeldWrite `json:"held"`
}

// ConfirmRequest is POST /api/v1/held/{ns}/{link}/{field}: a person says
// whether a held write happened. Happened: c4-sync takes it as written.
// Not: c4-sync writes it again on its next pass.
type ConfirmRequest struct {
	Happened *bool  `json:"happened"`
	Note     string `json:"note,omitempty"`
}
