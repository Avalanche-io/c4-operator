package api

import (
	"encoding/json"
	"time"
)

// SubmissionRequest is the body of POST /submissions: a submission
// (c4: submit/1, the schema package) as JSON, checked and lowered to one job per
// key.
type SubmissionRequest struct {
	// Submission is the document as JSON, its keys in the order written
	// (the order the jobs are created in).
	Submission json.RawMessage `json:"submission"`
	// DryRun checks, lowers and plans every job; nothing is created.
	DryRun bool `json:"dryRun,omitempty"`
	// Explain adds what each job lowers to: its WorkJob's class, params,
	// inputs, template and deadline.
	Explain bool `json:"explain,omitempty"`
	// SourceRoots are the roots the submitter's paths were written under
	// (the folder the tree was uploaded from), given to each unit as
	// C4_SOURCE_ROOTS for the tool runner's path map.
	SourceRoots []SourceRoot `json:"sourceRoots,omitempty"`
}

// SourceRoot is a root on a submitter's machine: its operating system
// (linux, windows, macos) and path.
type SourceRoot struct {
	OS   string `json:"os"`
	Path string `json:"path"`
}

// SubmissionResponse answers POST /submissions: 201 when the jobs are
// created, 200 for a dry run, 422 with Errors when the submission is
// refused (nothing is created then).
type SubmissionResponse struct {
	OK     bool         `json:"ok"`
	DryRun bool         `json:"dryRun,omitempty"`
	Errors []FieldError `json:"errors,omitempty"`
	// Submission is the C4 ID of the document as sent (compact JSON), the
	// annotation c4.avalanche.io/submission on each of its jobs.
	Submission string          `json:"submission,omitempty"`
	Jobs       []SubmissionJob `json:"jobs,omitempty"`
	// Estimate is the submission's as a whole: when its last job is
	// expected to finish, against the earliest due among its jobs.
	Estimate *SubmissionEstimate `json:"estimate,omitempty"`
	// Warnings are what the farm did differently from what the file
	// says, until it can (a frame-by-frame after waits for the whole job).
	Warnings []string `json:"warnings,omitempty"`
	// Explain is the lowering in words, when asked for.
	Explain string `json:"explain,omitempty"`
	// Message says what happened, in a line.
	Message string `json:"message,omitempty"`
}

// SubmissionJob is one job of a submission, made or planned.
type SubmissionJob struct {
	// Key is the job's key in the file.
	Key       string `json:"key"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Tool and Version are the tool that runs, at the version resolved.
	Tool    string `json:"tool"`
	Version string `json:"version"`
	// What is the job in a line: "48 frames in 6 units, after
	// sm0140-proxies-v001".
	What string `json:"what"`
	// Estimate is the job's, as POST /jobs gives it: eta50 to eta95, the
	// deadline and where it came from, the slack.
	Estimate *SubmitEstimate `json:"estimate,omitempty"`
	// EstimateError says why there is no estimate.
	EstimateError string `json:"estimateError,omitempty"`
	// After are the names of the jobs it follows.
	After []string `json:"after,omitempty"`
	// Link is the job view's HUD route, as POST /jobs gives it.
	Link string `json:"link,omitempty"`
}

// SubmissionEstimate is a submission's estimate: the latest of its jobs'
// finishes, the earliest of their deadlines, and the least slack.
type SubmissionEstimate struct {
	ETA50        *time.Time `json:"eta50,omitempty"`
	ETA90        *time.Time `json:"eta90,omitempty"`
	Deadline     *time.Time `json:"deadline,omitempty"`
	SlackSeconds int64      `json:"slackSeconds"`
	// Feasible when every job is; AtRisk when any is.
	Feasible bool `json:"feasible"`
	AtRisk   bool `json:"atRisk"`
	// Jobs is how many of the jobs had an estimate.
	Jobs int `json:"jobs"`
}
