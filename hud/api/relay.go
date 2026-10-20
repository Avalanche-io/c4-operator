package api

// Relay is a decision a tracker's sync relays with POST
// /jobs/{ns}/{name}/decide (`via`, internal/hud/API.md, Relayed decisions):
// made in the tracker, by a tracker user the Tracker's people map names.
// The HUD checks it and signs the decision with its custody: relayed, or
// relayed-unattributed when the map names a party, not a person.
type Relay struct {
	// Tracker is the Tracker's name, in the job's namespace.
	Tracker string `json:"tracker"`
	// Source is the tracker entity that carries the decision (its URL and
	// ref: a pull request review, a Version's status change).
	Source string `json:"source"`
	// By is the tracker user who decided.
	By string `json:"by"`
	// Person is whom the Tracker's people map makes By: a C4 person, or a
	// party ("party:ml-registry").
	Person string `json:"person"`
	// Evidence is the C4 ID of the tracker's event or entity, stored in
	// the project's store.
	Evidence string `json:"evidence,omitempty"`
}
