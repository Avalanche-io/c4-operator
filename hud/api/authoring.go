package api

// FieldError is one problem with a tool.yaml or a submission, at a dotted
// path (jobs.comp.after, params[1].type), with how to fix it.
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
	// Reason is which refusal it is, when a client acts on it: a
	// submission for a person refused for its grant carries POST /jobs's
	// reason (no-grant, grant-lapsed, grant-uncovered, no-tracker,
	// not-sync, not-installed).
	Reason string `json:"reason,omitempty"`
}
