package api

import "encoding/json"

// AppendUnitsRequest is the body of POST /jobs/{ns}/{name}/units: units
// added to a running graph job by its owner, after the units it has.
type AppendUnitsRequest struct {
	// Units are an expansion document's units (version 1), the same a
	// generator writes to .c4/expand.json: a JSON array of {name, label,
	// key, after, work, env, inputs, slots, group, params, reasoning, ...}.
	// They are read strictly, as the queue reads a generator's.
	Units json.RawMessage `json:"units"`
	// Key makes the request idempotent: a request again with the same key
	// adds nothing more and answers as the first did.
	Key string `json:"key"`
	// Turn is the lead's turn that started these units, if any: each unit
	// without a group of its own is grouped as "turn N", and carries
	// param turn.
	Turn *int `json:"turn,omitempty"`
}

// AppendedUnit is one unit a request added. Index is the unit's place in
// the job's table, set once the queue has added it (within a tick or two
// of the request; the same request again reads it).
type AppendedUnit struct {
	Name  string `json:"name"`
	Index *int   `json:"index,omitempty"`
}

// AppendUnitsResponse answers POST /jobs/{ns}/{name}/units.
type AppendUnitsResponse struct {
	Units []AppendedUnit `json:"units"`
	// Record is the C4 ID of the signed control record of the request.
	Record string `json:"record,omitempty"`
	// Document is the C4 ID of the stored expansion document.
	Document string `json:"document"`
	// State is pending (the queue has not read it yet), added, or refused
	// (the queue refused the units; Error says why, and none were added).
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message"`
}
