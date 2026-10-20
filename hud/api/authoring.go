package api

// FieldError is one problem with a tool.yaml or a submission, at a dotted
// path (jobs.comp.after, params[1].type), with how to fix it.
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}
