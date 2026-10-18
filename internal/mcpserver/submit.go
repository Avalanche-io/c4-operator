package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/jobspec"
)

// SubmitInput is one input of a submitted job.
type SubmitInput struct {
	View      string `json:"view,omitempty" jsonschema:"the C4 ID of a view in the project's store (c4_get_job shows output views)"`
	FromJob   string `json:"from_job,omitempty" jsonschema:"a job in the same namespace whose output this job reads; the job waits for it"`
	MountPath string `json:"mount_path" jsonschema:"where the input appears in every unit, for example /show"`
	ReadOnly  bool   `json:"read_only,omitempty" jsonschema:"mount without capturing writes"`
}

type SubmitIn struct {
	Project  string            `json:"project,omitempty" jsonschema:"the project; may be omitted when you see only one"`
	Name     string            `json:"name,omitempty" jsonschema:"the job's name; default <shot>-<type>-vNNN, the next version"`
	Type     string            `json:"type,omitempty" jsonschema:"the job type it is (render, usd, sim, comp, transcode, ci, lora, infer, agent)"`
	Shot     string            `json:"shot,omitempty" jsonschema:"the shot it belongs to, for example sm0140"`
	Class    string            `json:"class" jsonschema:"render (a frame range), ci (test shards) or ml (a training run)"`
	Params   map[string]string `json:"params,omitempty" jsonschema:"the class's parameters: render frames (1001-1048), chunk, order (binary); ci shards, target; ml ranks, steps, checkpointEvery"`
	Deadline string            `json:"deadline,omitempty" jsonschema:"RFC 3339; omit to have the farm suggest one"`
	Pools    []string          `json:"pools,omitempty" jsonschema:"pools the units may use; default the class's"`
	Image    string            `json:"image" jsonschema:"the container image each unit runs"`
	Command  []string          `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty" jsonschema:"environment for each unit (C4_ variables are set by the queue)"`
	Requests map[string]string `json:"requests,omitempty" jsonschema:"resource requests per unit, for example cpu 2, memory 4Gi"`
	Inputs   []SubmitInput     `json:"inputs,omitempty"`
	DryRun   bool              `json:"dry_run,omitempty" jsonschema:"plan only: return the estimate and whether it makes its deadline, create nothing. Do this first and show the person the estimate"`
}

type SubmitOut struct {
	Job            string `json:"job"`
	Submitted      bool   `json:"submitted"`
	Message        string `json:"message"`
	Units          int    `json:"units,omitempty"`
	ETA50          string `json:"eta50,omitempty"`
	ETA90          string `json:"eta90,omitempty"`
	Deadline       string `json:"deadline,omitempty"`
	DeadlineSource string `json:"deadlineSource,omitempty" jsonschema:"spec (given), suggested (by the farm) or class (the class's turnaround)"`
	SlackSeconds   int64  `json:"slackSeconds,omitempty"`
	OnTrack        bool   `json:"onTrack" jsonschema:"whether it makes its deadline at current capacity"`
	Warning        string `json:"warning,omitempty"`
	EstimateError  string `json:"estimateError,omitempty"`
}

func rfc(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (c *Client) submitJob(ctx context.Context, _ *mcp.CallToolRequest, in SubmitIn) (*mcp.CallToolResult, SubmitOut, error) {
	if in.Class == "" {
		return nil, SubmitOut{}, fmt.Errorf("class is required: render, ci or ml")
	}
	pod, err := jobspec.Pod(jobspec.Container{Image: in.Image, Command: in.Command, Args: in.Args, Env: in.Env, Requests: in.Requests})
	if err != nil {
		return nil, SubmitOut{}, err
	}
	spec := c4v1.WorkJobSpec{Class: strings.ToLower(in.Class), Params: in.Params, Pools: in.Pools, Template: pod}
	if in.Deadline != "" {
		t, err := time.Parse(time.RFC3339, in.Deadline)
		if err != nil {
			return nil, SubmitOut{}, fmt.Errorf("deadline %q is not RFC 3339 (2026-10-23T18:00:00-07:00)", in.Deadline)
		}
		spec.Deadline = &metav1.Time{Time: t}
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, SubmitOut{}, err
	}
	req := api.SubmitRequest{Project: in.Project, Name: in.Name, Type: in.Type, Shot: in.Shot, Spec: raw, DryRun: in.DryRun}
	for _, x := range in.Inputs {
		req.Inputs = append(req.Inputs, api.SubmitInput{View: x.View, FromJob: x.FromJob, MountPath: x.MountPath, ReadOnly: x.ReadOnly})
	}
	var r api.SubmitResponse
	if err := c.do(ctx, "POST", "/jobs", req, &r); err != nil {
		return nil, SubmitOut{}, err
	}
	out := SubmitOut{Job: r.Namespace + "/" + r.Name, Submitted: !r.DryRun, Message: r.Message, EstimateError: r.EstimateError}
	if e := r.Estimate; e != nil {
		out.Units, out.ETA50, out.ETA90, out.Deadline = e.Units, rfc(e.ETA50), rfc(e.ETA90), rfc(e.Deadline)
		out.DeadlineSource, out.SlackSeconds, out.OnTrack, out.Warning = e.DeadlineSource, e.SlackSeconds, e.Feasible && !e.AtRisk, e.Message
	}
	return nil, out, nil
}
