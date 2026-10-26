package api

// LeadWorkersRequest is POST /lead/workers: a lead (a person's own Claude
// Code session, named in the agent header X-C4-Agent) puts workers on the
// farm. The first call makes the lead's job; later calls add to it.
type LeadWorkersRequest struct {
	// Project is the project the lead's job is in; with one project to
	// submit to, it may be left out.
	Project string `json:"project,omitempty"`
	// Root is the lead's repository on its machine, where each worker gets
	// a worktree of its own.
	Root string `json:"root,omitempty"`
	// Tool is the agent tool the workers run (default "agent").
	Tool    string       `json:"tool,omitempty"`
	Workers []LeadWorker `json:"workers"`
	// Turn is the lead's turn that started them, so the HUD hangs them
	// under it.
	Turn *int `json:"turn,omitempty"`
	// Key makes the call idempotent: a call again with the same key adds
	// nothing more and answers as the first did.
	Key string `json:"key"`
}

// LeadWorker is one worker: its brief, and how it runs.
type LeadWorker struct {
	// Name is the worker's unit name in the lead's job (default: made
	// from its description).
	Name string `json:"name,omitempty"`
	// Description is what the HUD shows for it; Prompt is its whole brief.
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	// Agent names the agent environment it runs in (default: the farm's).
	Agent  string `json:"agent,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// Base is the full ID of the commit its branch starts from.
	Base string `json:"base,omitempty"`
	// After names other workers of the lead's job it waits for.
	After []string `json:"after,omitempty"`
}

// LeadWorkersResponse answers POST /lead/workers.
type LeadWorkersResponse struct {
	// Job is the lead's job, namespace/name; Created is true when this
	// call made it.
	Job     string `json:"job"`
	Created bool   `json:"created,omitempty"`
	// Units are the workers, with their indexes once the queue has added
	// them; State is pending, added or refused (Error says why).
	Units    []AppendedUnit `json:"units"`
	State    string         `json:"state"`
	Error    string         `json:"error,omitempty"`
	Record   string         `json:"record,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
	Message  string         `json:"message"`
}
