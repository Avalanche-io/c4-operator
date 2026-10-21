package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/jobspec"
	"github.com/Avalanche-io/c4-operator/schema"
)

// SubmitIn is a submission (c4: submit/1), as an object or as YAML; or
// one job in the raw form, an image and its command, which goes to POST
// /jobs as it is.
type SubmitIn struct {
	Submission     json.RawMessage `json:"submission,omitempty" jsonschema:"the submission (c4: submit/1) as a JSON object: project, shot, due, tree, and jobs by key, each a tool with its frames and with; GET /api/v1/schema/submit is its schema"`
	SubmissionYAML string          `json:"submission_yaml,omitempty" jsonschema:"the same submission as YAML text, as a person writes it; jobs keep the order written"`
	DryRun         bool            `json:"dry_run,omitempty" jsonschema:"check and plan only: return each job's estimate (units, ETA p50 and p90, slack, on track or not), create nothing. Do this first and show the person the estimate"`
	Explain        bool            `json:"explain,omitempty" jsonschema:"also return what each job lowers to in the operator (class, inputs, image, units), as text"`
	Key            string          `json:"key,omitempty" jsonschema:"makes a retry safe: send the key an earlier answer gave (or one of your own) when you submit the same thing again after an error or a timeout, and the farm answers with the jobs it made then rather than making them twice"`

	// The raw form: one job, an image and its command, instead of a
	// submission.
	Project  string            `json:"project,omitempty" jsonschema:"raw form: the project; may be omitted when you see only one"`
	Name     string            `json:"name,omitempty" jsonschema:"raw form: the job's name; default <shot>-<stage>-vNNN, the next version"`
	Type     string            `json:"type,omitempty" jsonschema:"raw form: the job type it is (render, usd, sim, comp, transcode, ci, lora, infer, agent)"`
	Stage    string            `json:"stage,omitempty" jsonschema:"raw form: the pipeline stage (light, layout, fx, comp, review, train), the middle of the default name"`
	Shot     string            `json:"shot,omitempty" jsonschema:"raw form: the shot it belongs to, for example sm0140"`
	Class    string            `json:"class,omitempty" jsonschema:"raw form: render (a frame range), ci (test shards) or ml (a training run)"`
	Params   map[string]string `json:"params,omitempty" jsonschema:"raw form: the class's parameters: render frames (1001-1048), chunk, order (binary); ci shards, target; ml ranks, steps, checkpointEvery"`
	Deadline string            `json:"deadline,omitempty" jsonschema:"raw form: RFC 3339; omit to have the farm suggest one"`
	Pools    []string          `json:"pools,omitempty" jsonschema:"raw form: pools the units may use; default the class's"`
	Image    string            `json:"image,omitempty" jsonschema:"raw form: the container image each unit runs"`
	Command  []string          `json:"command,omitempty" jsonschema:"raw form: the command each unit runs"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty" jsonschema:"raw form: environment for each unit (C4_ variables are set by the queue)"`
	Requests map[string]string `json:"requests,omitempty" jsonschema:"raw form: resource requests per unit, for example cpu 2, memory 4Gi"`
	Inputs   []SubmitInput     `json:"inputs,omitempty" jsonschema:"raw form: the views and jobs each unit reads"`
}

// SubmitInput is one input of a raw-form job.
type SubmitInput struct {
	View      string `json:"view,omitempty" jsonschema:"the C4 ID of a view in the project's store (c4_get_job shows output views)"`
	FromJob   string `json:"from_job,omitempty" jsonschema:"a job in the same namespace whose output this job reads; the job waits for it"`
	MountPath string `json:"mount_path" jsonschema:"where the input appears in every unit, for example /show"`
	ReadOnly  bool   `json:"read_only,omitempty" jsonschema:"mount without capturing writes"`
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
	raw := in.Image != "" || in.Class != ""
	switch {
	case raw && (len(doc) > 0 || in.SubmissionYAML != ""):
		return nil, SubmitOut{}, errors.New("give a submission or one job in the raw form (image and class), not both")
	case raw:
		return c.submitRaw(ctx, in)
	case len(doc) > 0 && in.SubmissionYAML != "":
		return nil, SubmitOut{}, errors.New("give the submission once: as submission (an object) or as submission_yaml (text), not both")
	case in.SubmissionYAML != "":
		b, err := schema.ToJSON([]byte(in.SubmissionYAML))
		if err != nil {
			return nil, SubmitOut{}, fmt.Errorf("submission_yaml is not YAML: %v", err)
		}
		doc = b
	case len(doc) == 0:
		return nil, SubmitOut{}, errors.New("a submission is required: submission (an object, c4: submit/1) or submission_yaml (text); or one job in the raw form, class and image")
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

// submitRaw submits one job in the raw form, an image and its command,
// through POST /jobs (LAYERS 3.4 keeps it for whoever has an image and no
// tool). A refusal is an answer, as a submission's is.
func (c *Client) submitRaw(ctx context.Context, in SubmitIn) (*mcp.CallToolResult, SubmitOut, error) {
	if in.Class == "" || in.Image == "" {
		return nil, SubmitOut{Errors: []api.FieldError{{Path: map[bool]string{true: "class", false: "image"}[in.Class == ""],
			Message: "a raw-form job needs a class and an image", Fix: "give class (render, ci or ml) and image, or a submission instead"}},
			Message: "the raw form needs a class and an image"}, nil
	}
	pod, err := jobspec.Pod(jobspec.Container{Image: in.Image, Command: in.Command, Args: in.Args, Env: in.Env, Requests: in.Requests})
	if err != nil {
		return nil, SubmitOut{Errors: []api.FieldError{{Path: "image", Message: err.Error()}}, Message: "the job's container: " + err.Error()}, nil
	}
	spec := c4v1.WorkJobSpec{Class: strings.ToLower(in.Class), Params: in.Params, Pools: in.Pools, Template: pod}
	if in.Deadline != "" {
		t, err := time.Parse(time.RFC3339, in.Deadline)
		if err != nil {
			return nil, SubmitOut{Errors: []api.FieldError{{Path: "deadline", Message: fmt.Sprintf("%q is not RFC 3339", in.Deadline),
				Fix: "for example 2026-10-23T18:00:00-07:00"}}, Message: "the deadline is not RFC 3339"}, nil
		}
		spec.Deadline = &metav1.Time{Time: t}
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, SubmitOut{}, err
	}
	req := api.SubmitRequest{Project: in.Project, Name: in.Name, Type: in.Type, Stage: in.Stage, Shot: in.Shot, Spec: b, DryRun: in.DryRun}
	for _, x := range in.Inputs {
		req.Inputs = append(req.Inputs, api.SubmitInput{View: x.View, FromJob: x.FromJob, MountPath: x.MountPath, ReadOnly: x.ReadOnly})
	}
	var r api.SubmitResponse
	err = c.do(ctx, "POST", "/jobs", req, &r)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusUnauthorized {
		return nil, SubmitOut{Errors: []api.FieldError{{Path: "job", Message: ae.Msg}}, Message: "the farm refused the job: " + ae.Msg}, nil
	}
	if err != nil {
		return nil, SubmitOut{}, err
	}
	out := SubmitOut{OK: true, Submitted: !r.DryRun, Message: r.Message}
	sj := SubmittedJob{Job: r.Namespace + "/" + r.Name, EstimateError: r.EstimateError, Link: r.Link}
	if e := r.Estimate; e != nil {
		sj.Units, sj.ETA50, sj.ETA90, sj.Deadline = e.Units, rfc(e.ETA50), rfc(e.ETA90), rfc(e.Deadline)
		sj.SlackSeconds, sj.OnTrack, sj.Warning = e.SlackSeconds, e.Feasible && !e.AtRisk, e.Message
		out.ETA50, out.ETA90, out.Deadline, out.OnTrack = sj.ETA50, sj.ETA90, sj.Deadline, sj.OnTrack
	}
	out.Jobs = []SubmittedJob{sj}
	return nil, out, nil
}
