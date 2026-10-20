package api

import "time"

// Transition is one change of a job the work queue recorded, in the
// farm-wide order it recorded them (internal/hud/API.md, GET /book?since):
// what a client that was away reads to learn what it missed, in order.
// Order by Seq, never by At: At is when it happened, Seq when the queue saw
// it.
type Transition struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Project string    `json:"project"`
	// Job is namespace/name.
	Job string `json:"job"`
	// Unit is the unit it concerns, when it concerns one.
	Unit *int `json:"unit,omitempty"`
	// Kind is phase, started, finished, wait-opened, wait-decided,
	// verdict, ack, decision, yield, interrupted, needs, pressing,
	// supplied, needs-withdrawn or cause.
	Kind    string `json:"kind"`
	Actor   string `json:"actor,omitempty"`
	Do      string `json:"do,omitempty"`
	Record  string `json:"record,omitempty"`
	Message string `json:"message"`
	// Policy are the policy values that decided it, and where each came
	// from (a decision's).
	Policy []PolicyUse `json:"policy,omitempty"`
	// Key names the transition itself: one seen twice (re-recorded after
	// a crash) has the same Key.
	Key string `json:"key"`
}

// PolicyUse is a policy value that decided something: its key, value, and
// where it came from (scope farm, project, job or unit; ref the project's
// or job's name).
type PolicyUse struct {
	Key   string     `json:"key"`
	Value string     `json:"value"`
	From  PolicyFrom `json:"from"`
}

// PolicyFrom is where a policy value came from.
type PolicyFrom struct {
	Scope  string `json:"scope"`
	Ref    string `json:"ref,omitempty"`
	SetBy  string `json:"setBy,omitempty"`
	Record string `json:"record,omitempty"`
}

// Transitions answers GET /book?since=<seq>: transitions after since in
// the viewer's projects, in Seq order. Next is the cursor to send next
// (past transitions the viewer cannot see); More says there are more now.
type Transitions struct {
	Entries []Transition `json:"entries"`
	Next    int64        `json:"next"`
	More    bool         `json:"more"`
	// Head is the latest transition in the log: where a new client starts.
	Head int64 `json:"head"`
}
