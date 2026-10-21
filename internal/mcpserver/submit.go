package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/schema"
)

// SubmitIn is a submission (c4: submit/1), as an object or as YAML.
type SubmitIn struct {
	Submission     json.RawMessage `json:"submission,omitempty" jsonschema:"the submission (c4: submit/1) as a JSON object: project, shot, due, tree, and jobs by key, each a tool with its frames and with; GET /api/v1/schema/submit is its schema"`
	SubmissionYAML string          `json:"submission_yaml,omitempty" jsonschema:"the same submission as YAML text, as a person writes it; jobs keep the order written"`
	DryRun         bool            `json:"dry_run,omitempty" jsonschema:"check and plan only: return each job's estimate (units, ETA p50 and p90, slack, on track or not), create nothing. Do this first and show the person the estimate"`
	Explain        bool            `json:"explain,omitempty" jsonschema:"also return what each job lowers to in the operator (class, inputs, image, units), as text"`
	Key            string          `json:"key,omitempty" jsonschema:"makes a retry safe: send the key an earlier answer gave (or one of your own) when you submit the same thing again after an error or a timeout, and the farm answers with the jobs it made then rather than making them twice"`
}

// SubmittedJob is one job of a submission as the farm planned or made it.
type SubmittedJob struct {
	Key           string   `json:"key" jsonschema:"the job's key in the submission"`
	Job           string   `json:"job" jsonschema:"namespace/name"`
	Tool          string   `json:"tool"`
	Version       string   `json:"version"`
	What          string   `json:"what,omitempty"`
	After         []string `json:"after,omitempty" jsonschema:"the jobs it follows"`
	Units         int      `json:"units,omitempty"`
	ETA50         string   `json:"eta50,omitempty"`
	ETA90         string   `json:"eta90,omitempty"`
	Deadline      string   `json:"deadline,omitempty"`
	SlackSeconds  int64    `json:"slackSeconds,omitempty"`
	OnTrack       bool     `json:"onTrack" jsonschema:"whether it makes its deadline at current capacity"`
	Warning       string   `json:"warning,omitempty"`
	EstimateError string   `json:"estimateError,omitempty"`
	Link          string   `json:"link,omitempty" jsonschema:"the job's page in the HUD"`
}

type SubmitOut struct {
	OK         bool             `json:"ok" jsonschema:"false when the farm refused the submission; then errors say where and how to fix it"`
	Submitted  bool             `json:"submitted" jsonschema:"true when the jobs were created (not a dry run)"`
	Errors     []api.FieldError `json:"errors,omitempty" jsonschema:"each problem at its dotted path (jobs.comp.after), with the fix"`
	Submission string           `json:"submission,omitempty" jsonschema:"the C4 ID of the submission as sent"`
	Jobs       []SubmittedJob   `json:"jobs,omitempty"`
	ETA50      string           `json:"eta50,omitempty" jsonschema:"when the last job is expected to finish, p50"`
	ETA90      string           `json:"eta90,omitempty" jsonschema:"the same, p90"`
	Deadline   string           `json:"deadline,omitempty" jsonschema:"the earliest due among its jobs"`
	OnTrack    bool             `json:"onTrack" jsonschema:"whether every job makes its deadline at current capacity"`
	Warnings   []string         `json:"warnings,omitempty" jsonschema:"what the farm does differently from what the submission says"`
	Explain    string           `json:"explain,omitempty"`
	Message    string           `json:"message,omitempty"`
	Key        string           `json:"key,omitempty" jsonschema:"the key this submission was sent with: send it again with the same submission if you retry, so nothing is made twice"`
}

// submitSchema is SubmitIn's schema, with submission an object: the
// document's bytes are kept as sent (json.RawMessage), so its jobs keep
// the order written.
func submitSchema() *jsonschema.Schema {
	s, err := jsonschema.For[SubmitIn](nil)
	if err != nil {
		panic(err)
	}
	desc := ""
	if p := s.Properties["submission"]; p != nil {
		desc = p.Description
	}
	s.Properties["submission"] = &jsonschema.Schema{Type: "object", Description: desc}
	return s
}

func rfc(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (c *Client) submitJob(ctx context.Context, _ *mcp.CallToolRequest, in SubmitIn) (*mcp.CallToolResult, SubmitOut, error) {
	doc := in.Submission
	switch {
	case len(doc) > 0 && in.SubmissionYAML != "":
		return nil, SubmitOut{}, errors.New("give the submission once: as submission (an object) or as submission_yaml (text), not both")
	case in.SubmissionYAML != "":
		b, err := schema.ToJSON([]byte(in.SubmissionYAML))
		if err != nil {
			return nil, SubmitOut{}, fmt.Errorf("submission_yaml is not YAML: %v", err)
		}
		doc = b
	case len(doc) == 0:
		return nil, SubmitOut{}, errors.New("a submission is required: submission (an object, c4: submit/1) or submission_yaml (text)")
	}
	return c.postSubmission(ctx, doc, in.DryRun, in.Explain, in.Key)
}

// postSubmission sends a submission document (JSON, keys in the order
// written) to POST /submissions; a refusal with errors (422, or 403 and
// 404 for a job the farm would not create) is an answer, not an error:
// its errors say where and how to fix it.
//
// A submission is sent with a key, the caller's or one made here, which
// the answer carries: sent again on a retry, it gets the jobs the first
// made rather than a second set.
func (c *Client) postSubmission(ctx context.Context, doc json.RawMessage, dryRun, explain bool, key string) (*mcp.CallToolResult, SubmitOut, error) {
	if !dryRun && key == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		key = hex.EncodeToString(b)
	}
	var r api.SubmissionResponse
	err := c.do(ctx, "POST", "/submissions", api.SubmissionRequest{Submission: doc, DryRun: dryRun, Explain: explain, Key: key}, &r)
	var ae *APIError
	if errors.As(err, &ae) && json.Unmarshal(ae.Body, &r) == nil && len(r.Errors) > 0 {
		err = nil
	}
	if err != nil {
		return nil, SubmitOut{}, err
	}
	out := SubmitOut{OK: r.OK, Submitted: r.OK && !r.DryRun, Errors: r.Errors, Submission: r.Submission, Warnings: r.Warnings,
		Explain: r.Explain, Message: r.Message}
	if !dryRun {
		out.Key = key
	}
	if e := r.Estimate; e != nil {
		out.ETA50, out.ETA90, out.Deadline, out.OnTrack = rfc(e.ETA50), rfc(e.ETA90), rfc(e.Deadline), e.Feasible && !e.AtRisk
	}
	if !r.OK && out.Message == "" {
		out.Message = fmt.Sprintf("the farm refused the submission: %d problems; fix each at its path and submit again", len(r.Errors))
	}
	for _, j := range r.Jobs {
		sj := SubmittedJob{Key: j.Key, Job: j.Namespace + "/" + j.Name, Tool: j.Tool, Version: j.Version, What: j.What, After: j.After,
			EstimateError: j.EstimateError, Link: j.Link}
		if e := j.Estimate; e != nil {
			sj.Units, sj.ETA50, sj.ETA90, sj.Deadline = e.Units, rfc(e.ETA50), rfc(e.ETA90), rfc(e.Deadline)
			sj.SlackSeconds, sj.OnTrack, sj.Warning = e.SlackSeconds, e.Feasible && !e.AtRisk, e.Message
		}
		out.Jobs = append(out.Jobs, sj)
	}
	return nil, out, nil
}
