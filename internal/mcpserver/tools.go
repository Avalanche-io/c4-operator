package mcpserver

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Avalanche-io/c4-operator/internal/hud/api"
)

// Version is the server's version, reported to clients.
var Version = "v2-dev"

// Instructions are sent to clients when they connect.
const Instructions = `This server operates a render, CI and ML farm on Kubernetes, the same farm the HUD shows. Jobs are named namespace/name.
Start with c4_farm_status, then c4_list_jobs (at_risk_only for trouble). There are no priorities: to make a job finish sooner, set its deadline (c4_set_deadline) or boost it (c4_boost_job); the queue works out the rest from estimates. Holding a job stops new units; c4_hold_job with stop_running also stops running ones.
Every change is made as you, checked against your Kubernetes permissions, and recorded as an Event. Costs: c4_approve_capacity provisions paid burst capacity.`

func boolPtr(b bool) *bool { return &b }

var (
	readOnly    = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}
	idempotent  = &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}
	destructive = &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false)}
)

// New builds the MCP server over a HUD client.
func New(c *Client) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "c4-farm", Title: "C4 farm", Version: Version}, &mcp.ServerOptions{Instructions: Instructions})
	for _, t := range Tools(c) {
		t.add(s)
	}
	return s
}

// Tool is one tool and how to register it. Tools are listed by Tools so
// documentation can be generated from the same definitions.
type Tool struct {
	Def *mcp.Tool
	add func(*mcp.Server)
}

func tool[In, Out any](name, title, desc string, ann *mcp.ToolAnnotations, h mcp.ToolHandlerFor[In, Out]) Tool {
	a := *ann
	a.Title = title
	def := &mcp.Tool{Name: name, Title: title, Description: desc, Annotations: &a}
	return Tool{Def: def, add: func(s *mcp.Server) { mcp.AddTool(s, def, h) }}
}

// Tools lists every tool.
func Tools(c *Client) []Tool {
	return []Tool{
		tool("c4_farm_status", "Farm status", "Slots in use, units waiting, throughput, failures, burst capacity, bytes the cache tiers saved, and the jobs at risk of missing their deadlines. Start here.",
			readOnly, c.farmStatus),
		tool("c4_list_jobs", "List jobs", "Jobs in dispatch order (rank 1 runs next), filtered by project, class (render, ci, ml), phase or at_risk_only. Each row has progress, deadline, ETA and slack. Paginated with limit and offset.",
			readOnly, c.listJobs),
		tool("c4_get_job", "Get a job", "One job: objective, estimate (ETA p50 and p90, required and attainable slots, slack, feasibility message), heavy frame ranges, inputs, output view, unit counts and, with include_units, each unit's state.",
			readOnly, c.getJob),
		tool("c4_get_unit", "Get a unit", "One unit of a job: state, attempts, node, run time against its estimate, the last lines of its log, its Kubernetes events and its output changeset.",
			readOnly, c.getUnit),
		tool("c4_list_nodes", "List nodes", "Worker nodes by pool: state (idle, busy, disabled, notReady, unprovisioned), slots used, cache size and hit rate, and the units of your projects running there.",
			readOnly, c.listNodes),
		tool("c4_cache_tiers", "Cache tiers", "The data path: hit rates and requests at the node, peer, regional and store tiers, bytes served, shed rate, and publish and capture latency.",
			readOnly, c.cacheTiers),
		tool("c4_capacity_and_cost", "Capacity and cost", "Pools and their cost per slot-hour, burst capacity requests (with what needs approval), burst spend against each project's budget, and metered use today: cluster-hours, container-hours and GPU-hours.",
			readOnly, c.capacityCost),
		tool("c4_dependency_graph", "Dependency graph", "Jobs and the jobs they depend on, with effective deadlines, ETAs and the critical path.",
			readOnly, c.graph),
		tool("c4_diff_views", "Diff two views", "The files that differ between two views (C4 IDs) of a project: added, modified and removed paths with sizes and IDs, computed from directory records without a checkout. Use it to choose which CI targets a change affects.",
			readOnly, c.diffViews),
		tool("c4_trace_file", "Trace a file to the task that made it", "The signed task record of the unit that wrote a file of a job's output (or of a unit): whether its signature verifies against the farm's keys, the node and container images it ran on, the input views it mounted and the jobs that produced them, its declared AI use, and the file's C2PA manifest definition and OMC asset.",
			readOnly, c.traceFile),
		tool("c4_hold_job", "Hold or release a job", "Hold a job (no new units start; running ones finish, or stop with stop_running) or release it (hold false).",
			idempotent, c.holdJob),
		tool("c4_boost_job", "Boost a job", "Multiply a job's required slots by 1 to 2, drawn from its own project's quota first. 1 removes the boost.",
			idempotent, c.boostJob),
		tool("c4_set_deadline", "Set a job's deadline", "Set or move a job's deadline (RFC 3339, for example 2026-10-23T18:00:00-07:00). The queue reorders work to meet it; this is what raising a priority means here.",
			idempotent, c.setDeadline),
		tool("c4_requeue_units", "Requeue units", "Run units of a job again, discarding their previous results: a unit set like \"0-9,14\".",
			destructive, c.requeue),
		tool("c4_avoid_node", "Avoid or allow a node for a job", "Keep a job's units off a node (a bad worker for that job), or allow it again.",
			idempotent, c.avoidNode),
		tool("c4_disable_node", "Disable or enable a node", "Take a node out of service for the whole farm (running units finish) or put it back.",
			idempotent, c.disableNode),
		tool("c4_approve_capacity", "Approve burst capacity", "Approve the burst capacity request waiting in a pool. This provisions paid capacity; check c4_capacity_and_cost first for its cost and the jobs it is for.",
			&mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)}, c.approveCapacity),
	}
}

// --- read-only tools

type Empty struct{}

type FarmStatus struct {
	Now              string   `json:"now"`
	Viewer           string   `json:"viewer"`
	SlotsUsed        int      `json:"slotsUsed"`
	SlotsTotal       int      `json:"slotsTotal"`
	QueuedUnits      int      `json:"queuedUnits"`
	UnitsPerHour     int      `json:"unitsPerHour"`
	FailuresLastHour int      `json:"failuresLastHour"`
	BurstSlots       int      `json:"burstSlots"`
	BurstCostPerHour float64  `json:"burstCostPerHour"`
	BytesAvoided     float64  `json:"bytesAvoided" jsonschema:"bytes mounted into pods that did not come from the store today"`
	Jobs             int      `json:"jobs"`
	Running          int      `json:"running"`
	AtRisk           []string `json:"atRisk" jsonschema:"jobs projected to miss their deadline"`
}

func (c *Client) snapshot(ctx context.Context) (*api.Snapshot, error) {
	var s api.Snapshot
	return &s, c.do(ctx, "GET", "/snapshot", nil, &s)
}

func (c *Client) farmStatus(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, FarmStatus, error) {
	s, err := c.snapshot(ctx)
	if err != nil {
		return nil, FarmStatus{}, err
	}
	f := s.Farm
	out := FarmStatus{Now: s.Now.Format(time.RFC3339), SlotsUsed: f.SlotsUsed, SlotsTotal: f.SlotsTotal, QueuedUnits: f.QueuedUnits,
		UnitsPerHour: f.UnitsPerHour, FailuresLastHour: f.FailuresLastHour, BurstSlots: f.BurstSlots, BurstCostPerHour: f.BurstCostPerHour,
		BytesAvoided: f.BytesAvoided, Jobs: len(s.Jobs), AtRisk: []string{}}
	if s.Viewer != nil {
		out.Viewer = s.Viewer.User
	}
	for _, j := range s.Jobs {
		if j.Phase == "Running" {
			out.Running++
		}
		if atRisk(j) {
			out.AtRisk = append(out.AtRisk, j.Namespace+"/"+j.Name)
		}
	}
	return nil, out, nil
}

func atRisk(j api.Job) bool {
	if j.Phase == "Succeeded" || j.Phase == "Failed" || j.Objective.Deadline == nil {
		return false
	}
	return !j.Estimate.Feasible || j.Estimate.SlackSeconds < 0
}

type ListJobsIn struct {
	Project    string `json:"project,omitempty" jsonschema:"only this project"`
	Class      string `json:"class,omitempty" jsonschema:"render, ci or ml"`
	Phase      string `json:"phase,omitempty" jsonschema:"Queued, Running, Blocked, Held, Succeeded or Failed"`
	AtRiskOnly bool   `json:"at_risk_only,omitempty" jsonschema:"only jobs projected to miss their deadline"`
	Limit      int    `json:"limit,omitempty" jsonschema:"rows to return, 1 to 100 (default 20)"`
	Offset     int    `json:"offset,omitempty" jsonschema:"rows to skip, for the next page"`
}

type JobRow struct {
	Job          string  `json:"job"`
	Project      string  `json:"project"`
	Class        string  `json:"class"`
	Phase        string  `json:"phase"`
	Rank         int     `json:"rank,omitempty"`
	Done         int     `json:"done"`
	Total        int     `json:"total"`
	Running      int     `json:"running"`
	Deadline     string  `json:"deadline,omitempty"`
	EtaP90       string  `json:"etaP90,omitempty"`
	SlackSeconds float64 `json:"slackSeconds"`
	Feasible     bool    `json:"feasible"`
	Message      string  `json:"message,omitempty"`
	Held         bool    `json:"held,omitempty"`
	Boost        float64 `json:"boost,omitempty"`
}

type ListJobsOut struct {
	Total      int      `json:"total"`
	Offset     int      `json:"offset"`
	Jobs       []JobRow `json:"jobs"`
	NextOffset int      `json:"nextOffset,omitempty" jsonschema:"set when more rows follow"`
}

func row(j api.Job) JobRow {
	r := JobRow{Job: j.Namespace + "/" + j.Name, Project: j.Project, Class: j.Class, Phase: j.Phase, Rank: j.Rank,
		Done: j.Units.Succeeded, Total: j.Units.Total, Running: j.Units.Running, SlackSeconds: j.Estimate.SlackSeconds,
		Feasible: j.Estimate.Feasible, Message: j.Estimate.Message, Held: j.Nudge.Hold}
	if j.Nudge.Boost > 1 {
		r.Boost = j.Nudge.Boost
	}
	if j.Objective.Deadline != nil {
		r.Deadline = j.Objective.Deadline.Format(time.RFC3339)
	}
	if !j.Estimate.EtaP90.IsZero() {
		r.EtaP90 = j.Estimate.EtaP90.Format(time.RFC3339)
	}
	return r
}

func (c *Client) listJobs(ctx context.Context, _ *mcp.CallToolRequest, in ListJobsIn) (*mcp.CallToolResult, ListJobsOut, error) {
	s, err := c.snapshot(ctx)
	if err != nil {
		return nil, ListJobsOut{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	var rows []JobRow
	for _, j := range s.Jobs {
		if in.Project != "" && j.Project != in.Project || in.Class != "" && j.Class != in.Class ||
			in.Phase != "" && !strings.EqualFold(j.Phase, in.Phase) || in.AtRiskOnly && !atRisk(j) {
			continue
		}
		rows = append(rows, row(j))
	}
	out := ListJobsOut{Total: len(rows), Offset: in.Offset, Jobs: []JobRow{}}
	if in.Offset < len(rows) {
		end := min(len(rows), in.Offset+limit)
		out.Jobs = rows[in.Offset:end]
		if end < len(rows) {
			out.NextOffset = end
		}
	}
	return nil, out, nil
}

type JobRef struct {
	Namespace string `json:"namespace" jsonschema:"the job's namespace"`
	Name      string `json:"name" jsonschema:"the job's name"`
}

func (r JobRef) path() (string, error) {
	if r.Namespace == "" || r.Name == "" {
		return "", fmt.Errorf("give the job's namespace and name; c4_list_jobs lists them as namespace/name")
	}
	return "/jobs/" + url.PathEscape(r.Namespace) + "/" + url.PathEscape(r.Name), nil
}

type GetJobIn struct {
	JobRef
	IncludeUnits bool   `json:"include_units,omitempty" jsonschema:"also list units (at most 500)"`
	UnitState    string `json:"unit_state,omitempty" jsonschema:"with include_units, only units in this state (queued, running, retrying, succeeded, failed, held, blocked)"`
}

type UnitRow struct {
	I        int    `json:"i"`
	Label    string `json:"label"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Node     string `json:"node,omitempty"`
}

type GetJobOut struct {
	Job          JobRow           `json:"job"`
	Objective    map[string]any   `json:"objective"`
	Estimate     map[string]any   `json:"estimate"`
	HeavyRegions []map[string]any `json:"heavyRegions"`
	Inputs       []map[string]any `json:"inputs"`
	Output       map[string]any   `json:"output,omitempty"`
	UnitStates   map[string]any   `json:"unitStates"`
	Pools        []string         `json:"pools"`
	DependsOn    []string         `json:"dependsOn"`
	Blacklist    []string         `json:"blacklist,omitempty"`
	Units        []UnitRow        `json:"units,omitempty"`
	UnitsShown   int              `json:"unitsShown,omitempty"`
}

func (c *Client) getJob(ctx context.Context, _ *mcp.CallToolRequest, in GetJobIn) (*mcp.CallToolResult, GetJobOut, error) {
	p, err := in.path()
	if err != nil {
		return nil, GetJobOut{}, err
	}
	var d map[string]any
	if err := c.do(ctx, "GET", p, nil, &d); err != nil {
		return nil, GetJobOut{}, err
	}
	var j api.Job
	if err := remarshal(d, &j); err != nil {
		return nil, GetJobOut{}, err
	}
	out := GetJobOut{Job: row(j), Objective: asMap(d["objective"]), Estimate: asMap(d["estimate"]),
		HeavyRegions: asMaps(d["heavyRegions"]), Inputs: asMaps(d["inputs"]), UnitStates: asMap(d["unitStates"]),
		Pools: j.Pools, DependsOn: j.DependsOn, Blacklist: j.Blacklist}
	if o := asMap(d["output"]); len(o) > 0 {
		out.Output = o
	}
	if in.IncludeUnits {
		for _, u := range asMaps(d["unitList"]) {
			r := UnitRow{I: toInt(u["i"]), Label: fmt.Sprint(u["label"]), State: fmt.Sprint(u["state"]), Attempts: toInt(u["attempts"])}
			if n, ok := u["node"].(string); ok {
				r.Node = n
			}
			if in.UnitState != "" && r.State != in.UnitState {
				continue
			}
			if len(out.Units) == 500 {
				break
			}
			out.Units = append(out.Units, r)
		}
		out.UnitsShown = len(out.Units)
	}
	return nil, out, nil
}

type GetUnitIn struct {
	JobRef
	Unit int `json:"unit" jsonschema:"the unit's index"`
}

func (c *Client) getUnit(ctx context.Context, _ *mcp.CallToolRequest, in GetUnitIn) (*mcp.CallToolResult, map[string]any, error) {
	p, err := in.path()
	if err != nil {
		return nil, nil, err
	}
	var d map[string]any
	if err := c.do(ctx, "GET", p+"/units/"+strconv.Itoa(in.Unit), nil, &d); err != nil {
		return nil, nil, err
	}
	return nil, d, nil
}

type ListNodesIn struct {
	Pool  string `json:"pool,omitempty" jsonschema:"only this pool"`
	State string `json:"state,omitempty" jsonschema:"idle, busy, disabled, notReady or unprovisioned"`
}

type ListNodesOut struct {
	Nodes []api.Node `json:"nodes"`
}

func (c *Client) listNodes(ctx context.Context, _ *mcp.CallToolRequest, in ListNodesIn) (*mcp.CallToolResult, ListNodesOut, error) {
	s, err := c.snapshot(ctx)
	if err != nil {
		return nil, ListNodesOut{}, err
	}
	out := ListNodesOut{Nodes: []api.Node{}}
	for _, n := range s.Nodes {
		if in.Pool != "" && n.Pool != in.Pool || in.State != "" && n.State != in.State {
			continue
		}
		out.Nodes = append(out.Nodes, n)
	}
	return nil, out, nil
}

func (c *Client) cacheTiers(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, api.Tiers, error) {
	s, err := c.snapshot(ctx)
	if err != nil {
		return nil, api.Tiers{}, err
	}
	return nil, s.Tiers, nil
}

type CapacityCostOut struct {
	Pools    []api.Pool       `json:"pools"`
	Requests []api.Capacity   `json:"requests"`
	Projects []map[string]any `json:"projects"`
	Usage    api.Usage        `json:"usage"`
}

func (c *Client) capacityCost(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, CapacityCostOut, error) {
	s, err := c.snapshot(ctx)
	if err != nil {
		return nil, CapacityCostOut{}, err
	}
	out := CapacityCostOut{Pools: s.Pools, Requests: s.Capacity, Usage: s.Usage, Projects: []map[string]any{}}
	for _, p := range s.Projects {
		out.Projects = append(out.Projects, map[string]any{"name": p.Name, "burstSpendToday": p.BurstSpendToday,
			"burstBudgetPerDay": p.BurstBudgetPerDay, "quotas": p.Quotas, "gpuHours": p.GPUHours})
	}
	sort.Slice(out.Projects, func(a, b int) bool { return out.Projects[a]["name"].(string) < out.Projects[b]["name"].(string) })
	return nil, out, nil
}

func (c *Client) graph(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, map[string]any, error) {
	var g map[string]any
	return nil, g, c.do(ctx, "GET", "/graph", nil, &g)
}

type DiffIn struct {
	Project string `json:"project" jsonschema:"the project whose store holds both views"`
	From    string `json:"from" jsonschema:"the earlier view's C4 ID"`
	To      string `json:"to" jsonschema:"the later view's C4 ID"`
	Limit   int    `json:"limit,omitempty" jsonschema:"most changes to return (default 1000)"`
}

func (c *Client) diffViews(ctx context.Context, _ *mcp.CallToolRequest, in DiffIn) (*mcp.CallToolResult, map[string]any, error) {
	q := url.Values{"from": {in.From}, "to": {in.To}}
	if in.Limit > 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	var d map[string]any
	return nil, d, c.do(ctx, "GET", "/views/"+url.PathEscape(in.Project)+"/diff?"+q.Encode(), nil, &d)
}

// --- controls

type Done struct {
	Message string `json:"message"`
}

func (c *Client) control(ctx context.Context, path string, body any) (*mcp.CallToolResult, Done, error) {
	var r struct {
		Message string `json:"message"`
	}
	if err := c.do(ctx, "POST", path, body, &r); err != nil {
		return nil, Done{}, err
	}
	return nil, Done{Message: r.Message}, nil
}

type HoldIn struct {
	JobRef
	Hold        bool `json:"hold" jsonschema:"true holds the job, false releases it"`
	StopRunning bool `json:"stop_running,omitempty" jsonschema:"with hold, also stop the units running now"`
}

func (c *Client) holdJob(ctx context.Context, _ *mcp.CallToolRequest, in HoldIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	body := map[string]any{"hold": in.Hold}
	if in.Hold && in.StopRunning {
		body["holdStop"] = true
	}
	return c.control(ctx, p+"/nudge", body)
}

type BoostIn struct {
	JobRef
	Boost float64 `json:"boost" jsonschema:"1 to 2; 1 removes the boost"`
}

func (c *Client) boostJob(ctx context.Context, _ *mcp.CallToolRequest, in BoostIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	if in.Boost < 1 || in.Boost > 2 {
		return nil, Done{}, fmt.Errorf("boost must be from 1 to 2 (1 removes it)")
	}
	return c.control(ctx, p+"/nudge", map[string]any{"boost": in.Boost})
}

type DeadlineIn struct {
	JobRef
	Deadline string `json:"deadline" jsonschema:"RFC 3339 time, for example 2026-10-23T18:00:00-07:00"`
}

func (c *Client) setDeadline(ctx context.Context, _ *mcp.CallToolRequest, in DeadlineIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	if _, err := time.Parse(time.RFC3339, in.Deadline); err != nil {
		return nil, Done{}, fmt.Errorf("deadline %q is not RFC 3339 (2026-10-23T18:00:00-07:00)", in.Deadline)
	}
	return c.control(ctx, p+"/nudge", map[string]any{"deadline": in.Deadline})
}

type RequeueIn struct {
	JobRef
	Units string `json:"units" jsonschema:"a unit set, for example 0-9,14"`
}

func (c *Client) requeue(ctx context.Context, _ *mcp.CallToolRequest, in RequeueIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	return c.control(ctx, p+"/requeue", map[string]any{"units": in.Units})
}

type AvoidIn struct {
	JobRef
	Node  string `json:"node" jsonschema:"the node's name"`
	Allow bool   `json:"allow,omitempty" jsonschema:"true lets the job use the node again"`
}

func (c *Client) avoidNode(ctx context.Context, _ *mcp.CallToolRequest, in AvoidIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	return c.control(ctx, p+"/blacklist", map[string]any{"node": in.Node, "remove": in.Allow})
}

type DisableIn struct {
	Node     string `json:"node" jsonschema:"the node's name"`
	Disabled bool   `json:"disabled" jsonschema:"true takes it out of service, false puts it back"`
}

func (c *Client) disableNode(ctx context.Context, _ *mcp.CallToolRequest, in DisableIn) (*mcp.CallToolResult, Done, error) {
	if in.Node == "" {
		return nil, Done{}, fmt.Errorf("give the node's name; c4_list_nodes lists them")
	}
	return c.control(ctx, "/nodes/"+url.PathEscape(in.Node)+"/disable", map[string]any{"disabled": in.Disabled})
}

type ApproveIn struct {
	Pool string `json:"pool" jsonschema:"the burst pool whose waiting request to approve"`
}

func (c *Client) approveCapacity(ctx context.Context, _ *mcp.CallToolRequest, in ApproveIn) (*mcp.CallToolResult, Done, error) {
	if in.Pool == "" {
		return nil, Done{}, fmt.Errorf("give the pool; c4_capacity_and_cost lists requests waiting for approval")
	}
	return c.control(ctx, "/capacity/"+url.PathEscape(in.Pool)+"/approve", map[string]any{})
}

type TraceIn struct {
	JobRef
	Path string `json:"path,omitempty" jsonschema:"a file the job wrote, relative to its output view, for example renders/sh010/sh010.1039.exr"`
	Unit *int   `json:"unit,omitempty" jsonschema:"or a unit index, for the unit's own record"`
}

func (c *Client) traceFile(ctx context.Context, _ *mcp.CallToolRequest, in TraceIn) (*mcp.CallToolResult, map[string]any, error) {
	p, err := in.path()
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	switch {
	case in.Path != "":
		q.Set("path", in.Path)
	case in.Unit != nil:
		q.Set("unit", strconv.Itoa(*in.Unit))
	default:
		return nil, nil, fmt.Errorf("give path (a file the job wrote) or unit (a unit index)")
	}
	var d map[string]any
	return nil, d, c.do(ctx, "GET", strings.Replace(p, "/jobs/", "/trace/", 1)+"?"+q.Encode(), nil, &d)
}
