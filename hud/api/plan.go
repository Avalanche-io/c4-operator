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
	// Forecast is what the scheduler forecasts for the job with the edits
	// applied; none while it cannot be answered at once (Message says the
	// forecast follows on the next tick, and the job's plan.forecast has
	// it then), or for a refused call.
	Forecast *PlanForecast `json:"forecast,omitempty"`
}

// ForecastLater is what a plan call's answer says when the queue cannot
// forecast the plan at once.
const ForecastLater = "the forecast follows on the next tick"

// PlanForecast is what the scheduler forecasts for a lead's job: a dry
// run of the job with the edits applied, as of the farm's last look
// (AsOf). A held unit counts as released: it is the plan's forecast.
type PlanForecast struct {
	AsOf time.Time `json:"asOf"`
	// Before is the job before the call's edits and After with them: for
	// a call the queue has not applied, the job as it is with the calls
	// before it and then its own; once applied, After is the job as it
	// is, and there is no Before.
	Before *PlanEstimate `json:"before,omitempty"`
	After  PlanEstimate  `json:"after"`
	// Change says what the edits do to when the plan is ready, against
	// its deadline, in plain words: "withdrawing docs brings ready to Mon
	// 14:20 PDT (21:20 UTC), 40 min earlier (p90 Mon 15:05 PDT (22:05
	// UTC)), 2 h 55 min before its deadline Mon 18:00 PDT (Tue 01:00 UTC)".
	Change string `json:"change,omitempty"`
}

// PlanEstimate is one forecast of a lead's job.
type PlanEstimate struct {
	// Ready50 and Ready90 are when the plan is ready, at the 50th and
	// 90th percentile.
	Ready50 *time.Time `json:"ready50,omitempty"`
	Ready90 *time.Time `json:"ready90,omitempty"`
	// Deadline is the deadline it is planned for, and SlackSeconds the
	// time from Ready90 to it (0 without one, or with NoHistory).
	Deadline     *time.Time `json:"deadline,omitempty"`
	SlackSeconds int64      `json:"slackSeconds"`
	Feasible     bool       `json:"feasible"`
	AtRisk       bool       `json:"atRisk"`
	// NoHistory says the estimate rests on the class default: the ready
	// times are a guess, and Message says the first workers set it.
	NoHistory bool   `json:"noHistory,omitempty"`
	Message   string `json:"message,omitempty"`
	// CriticalPath is the chain of units left whose work decides when the
	// plan is ready, first first.
	CriticalPath []PlanUnitEstimate `json:"criticalPath"`
	// Capacity is how many of its workers can run at once.
	Capacity PlanCapacity `json:"capacity"`
	// Unmeasured are its units left with no history of their own in the
	// job, each with where its estimate comes from (PlanUnitEstimate.From).
	Unmeasured []PlanUnitEstimate `json:"unmeasured,omitempty"`
}

// PlanUnitEstimate is one unit of a forecast: its name, its index (none
// for a unit a call the queue has not applied adds), its predicted time
// left in seconds, and, for an unmeasured unit, where that comes from:
// agent-env (the runs of the same agent environment and tool), work (the
// lead's estimate), estimate (the job's estimateSeconds), history (the
// class's runs in the project), job-median (the job's other units),
// profile (the shot's previous version) or class-default (a guess).
type PlanUnitEstimate struct {
	Name    string  `json:"name,omitempty"`
	Index   *int    `json:"index,omitempty"`
	Seconds float64 `json:"seconds"`
	From    string  `json:"from,omitempty"`
}

// PlanCapacity is how many of a lead's job's workers can run at once on
// the machines and pools it uses.
type PlanCapacity struct {
	// Workers is how many can run at once: the slots the scheduler can
	// give the job, in workers.
	Workers int `json:"workers"`
	// Slots are its pools' slots, and Pools the pools.
	Slots int      `json:"slots"`
	Pools []string `json:"pools,omitempty"`
	// Machines are the machines of those pools that run its work, with
	// their slots, and Power why a machine's slots are lowered, where it
	// says ("power: 38 of 40 W, battery 48% on a 39 W adapter").
	Machines []PlanMachine `json:"machines,omitempty"`
}

// PlanMachine is one machine a lead's job runs on.
type PlanMachine struct {
	Name  string `json:"name"`
	Pool  string `json:"pool"`
	Slots int    `json:"slots"`
	Power string `json:"power,omitempty"`
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
	// Forecast is the latest forecast of the plan, the job as it is with
	// the calls the queue has not applied yet: on the job's detail (GET
	// /jobs/{ns}/{name}) only, when the queue answers at once.
	Forecast *PlanForecast `json:"forecast,omitempty"`
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
