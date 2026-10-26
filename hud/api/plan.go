package api

import (
	"encoding/json"
	"time"
)

// PlanRequest is the body of POST /jobs/{ns}/{name}/plan: a lead's edits
// to its own job's plan (c4_plan), applied together or not at all.
type PlanRequest struct {
	// Edits are applied as one: added, then split, merged and withdrawn,
	// then edges added and removed, units released, and the order given.
	Edits []PlanEdit `json:"edits"`
	// Key makes the call idempotent: a call again with the same key
	// changes nothing more and answers as the first did.
	Key string `json:"key"`
	// Root is the lead's repository, where a host gives each worker its
	// worktree, as POST /lead/workers takes it (for workers only).
	Root string `json:"root,omitempty"`
}

// PlanEdit is one edit: exactly one of its fields. A unit is named by its
// name or its index.
type PlanEdit struct {
	// Add is a planned unit, held until a release names it.
	Add *PlanUnit `json:"add,omitempty"`
	// Split replaces a unit that has not started with several.
	Split *PlanSplit `json:"split,omitempty"`
	// Merge replaces units that have not started with one.
	Merge *PlanMerge `json:"merge,omitempty"`
	// Order is the order among the job's ready units, first first: the
	// units it names before every other, which keep no order key.
	Order []string `json:"order,omitempty"`
	// Withdraw retires a unit that has not started, with why.
	Withdraw *PlanWithdraw `json:"withdraw,omitempty"`
	// Release lets held units run.
	Release []string `json:"release,omitempty"`
	// Edge adds an ordering, or removes one.
	Edge *PlanEdge `json:"edge,omitempty"`
}

// PlanUnit is a unit an edit adds: a worker, as c4_agent starts one, or a
// unit of an expansion document (version 1), as a generator writes it.
type PlanUnit struct {
	Worker *LeadWorker     `json:"worker,omitempty"`
	Unit   json.RawMessage `json:"unit,omitempty"`
	// Work is a worker's estimate, in seconds (a unit gives its own).
	Work float64 `json:"work,omitempty"`
}

// PlanSplit replaces Unit with Into. Whatever followed it follows all of
// them; each waits on what it waited on, and is held if it was.
type PlanSplit struct {
	Unit string     `json:"unit"`
	Into []PlanUnit `json:"into"`
	Why  string     `json:"why,omitempty"`
}

// PlanMerge replaces Units with Into.
type PlanMerge struct {
	Units []string `json:"units"`
	Into  PlanUnit `json:"into"`
	Why   string   `json:"why,omitempty"`
}

// PlanWithdraw retires Unit: kept in the job as withdrawn, with Why.
type PlanWithdraw struct {
	Unit string `json:"unit"`
	Why  string `json:"why"`
}

// PlanEdge is an ordering: To runs after From. Remove takes it away.
type PlanEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Remove bool   `json:"remove,omitempty"`
}

// PlanResponse answers POST /jobs/{ns}/{name}/plan.
type PlanResponse struct {
	// Units are the units the edits add (each add, and each split's or
	// merge's replacements), with their indexes once applied.
	Units []AppendedUnit `json:"units,omitempty"`
	// Record is the C4 ID of the signed control record of the call.
	Record string `json:"record,omitempty"`
	// Document is the C4 ID of the stored plan document.
	Document string `json:"document"`
	// State is pending (the queue has not read it yet), applied, or
	// refused (the queue refused the edits; Error says why, and it made
	// none of them).
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message"`
}

// LeadPlan is what a lead's edits to its job's plan did (Job.Plan).
type LeadPlan struct {
	// Edits counts the calls applied, refused ones included.
	Edits int `json:"edits"`
	// Recent are the latest calls, oldest first (at most 16); Log is the
	// C4 ID of the stored log of every one.
	Recent []PlanCall `json:"recent,omitempty"`
	Log    string     `json:"log,omitempty"`
	// Withdrawn maps a withdrawn unit's index to why and its replacements
	// (the newest 256; every one is in unitStates.withdrawn).
	Withdrawn map[string]WithdrawnUnit `json:"withdrawn,omitempty"`
	// Order is each ordered unit's key, "index:key" pairs: among the
	// job's ready units the lower key runs first.
	Order string `json:"order,omitempty"`
}

// PlanCall is one call applied: it added Units units from First, or was
// refused (Error), with its signed records.
type PlanCall struct {
	Key      string    `json:"key"`
	Document string    `json:"document"`
	First    int       `json:"first"`
	Units    int       `json:"units"`
	At       time.Time `json:"at"`
	Control  string    `json:"control,omitempty"`
	Record   string    `json:"record,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// WithdrawnUnit is why a unit was withdrawn, the units that replace it
// (an index set, none for a plain withdraw) and the call that did it.
type WithdrawnUnit struct {
	Why          string `json:"why"`
	Replacements string `json:"replacements,omitempty"`
	Edit         string `json:"edit"`
}
