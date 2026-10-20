package api

import "time"

// Transition is one change of a job the work queue recorded, in the
// farm-wide order it recorded them (integrations GAPS 5): what a client
// that was away reads to learn what it missed, in order. Order by Seq,
// never by At: At is when it happened, Seq when the queue saw it.
type Transition struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Project string    `json:"project"`
	// Job is namespace/name.
	Job string `json:"job"`
	// Unit is the unit it concerns, when it concerns one.
	Unit *int `json:"unit,omitempty"`
	// Kind is phase, started, finished, wait-opened, wait-decided,
	// verdict, ack, decision, yield, interrupted, needs, supplied or
	// cause.
	Kind    string `json:"kind"`
	Actor   string `json:"actor,omitempty"`
	Do      string `json:"do,omitempty"`
	Record  string `json:"record,omitempty"`
	Message string `json:"message"`
	// Key names the transition itself: one seen twice (re-recorded after
	// a crash) has the same Key.
	Key string `json:"key"`
}

// Transitions answers GET /book?since=<seq>: transitions after since in
// the viewer's projects, in Seq order. Next is the cursor to send next
// (past transitions the viewer cannot see); More says there are more now.
type Transitions struct {
	Entries []Transition `json:"entries"`
	Next    int64        `json:"next"`
	More    bool         `json:"more"`
}
