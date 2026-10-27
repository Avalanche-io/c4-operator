package mcpserver

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Version is the server's version, reported to clients.
var Version = "dev"

// Instructions are sent to clients when they connect.
const Instructions = `This server operates a farm on Kubernetes that runs render, CI, ML and agents' work, the same farm the HUD shows. Jobs are named namespace/name.
Start with c4_farm_status, then c4_list_jobs (at_risk_only for trouble). New work goes in through a published tool: c4_list_tools, c4_describe_tool, then c4_run_<tool> for one job or c4_submit_job for a submission of several (dry_run first, either way). Your own Claude Code agents run as a bundle: c4_run_bundle (dry_run first), then c4_list_bundles and c4_stop_bundle. A lead starts workers as it would start subagents with c4_agent (each a fresh session on a branch of its own from your commit), waits for them with c4 farm wait as a background command, plans ahead in its job with c4_plan (planned workers held until released, split, merged or withdrawn as it learns), and closes its job with c4_close_lead. There are no priorities: to make a job finish sooner, set its deadline (c4_set_deadline) or boost it (c4_boost_job); the queue works out the rest from estimates. A job with needsPerson waits on someone: requeue its failed units (c4_requeue_units), take or accept them (c4_acknowledge_units), or decide what waits for a person, a ruling or an escalated unit (c4_decide). Before choosing a time, c4_deadline_options says what it would take (boost, burst cost, jobs made late); a job without a deadline gets one suggested by the farm (c4_suggest_deadline). Holding a job stops new units; c4_hold_job with stop_running also stops running ones.
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
	// Each published tool is its own tool too (runtools.go), when the
	// server has a HUD to ask, read again whenever a client lists them.
	if c != nil && c.Base != "" {
		c.server = s
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c.syncRunTools(ctx, s, false)
		cancel()
		s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if method == "tools/list" {
					c.syncRunTools(ctx, s, true)
				}
				return next(ctx, method, req)
			}
		})
	}
	return s
}

// Serve runs the MCP server over one long-lived transport (stdio): the
// run tools are read again every runEvery too, so a tool published while
// the session runs appears without the client asking.
func Serve(ctx context.Context, c *Client, t mcp.Transport) error {
	s := New(c)
	if c != nil && c.Base != "" {
		go func() {
			tick := time.NewTicker(runEvery)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
					c.syncRunTools(sctx, s, true)
					cancel()
				}
			}
		}()
	}
	return s.Run(ctx, t)
}

// Tool is one tool and how to register it. Tools are listed by Tools so
// documentation can be generated from the same definitions.
type Tool struct {
	Def *mcp.Tool
	add func(*mcp.Server)
}

// withSchema gives a tool its input schema instead of the one inferred
// from its input type.
func (t Tool) withSchema(s any) Tool {
	t.Def.InputSchema = s
	return t
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
		tool("c4_get_job", "Get a job", "One job: objective, estimate (ETA p50 and p90, required and attainable slots, slack, feasibility message), heavy frame ranges, inputs, output view, unit counts, the class's summary (CI pipelines, ML checkpoints and yields) and, with include_units, each unit's state.",
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
		tool("c4_read_book", "Read the farm's book", "The farm's history, newest last: units captured and failed on which node, retries, expansions, yields and resumes, capacity requested, approved and provisioned, and every hold, boost, deadline and requeue a person or agent made, each with who did it, which entity signed it and the C4 ID of its signed record. Filter to one job with namespace and name. Use it to answer why a unit retried, why a run yielded or who moved a deadline.",
			readOnly, c.readBook),
		tool("c4_diff_views", "Diff two views", "The files that differ between two views (C4 IDs) of a project: added, modified and removed paths with sizes and IDs, computed from directory records without a checkout. Use it to choose which CI targets a change affects.",
			readOnly, c.diffViews),
		tool("c4_trace_file", "Trace a file to the task that made it", "The signed task record of the unit that wrote a file of a job's output (or of a unit): whether it verifies against the farm's signing roots and who signed it, the node and container images it ran on, the input views it mounted and the jobs that produced them, its declared AI use, and the file's C2PA manifest definition and OMC asset.",
			readOnly, c.traceFile),
		tool("c4_verify_custody", "Verify a file's chain of custody", "Every signed record between a file a job wrote and where its inputs came from, each verified with its signer: the task record (signed by the node that ran the unit), each input's resolution (the controller), the input's origin (an import or ingest signed by a workload, or the upstream job's record signed by the queue, with its scheduling decisions, the HUD and MCP actions on it, its own task records and inputs), and the job's own record. Reports verified (every record checks out and was signed by whom the farm's policy wants), complete (no step lacks a record), the distinct signers, and any gaps.",
			readOnly, c.verifyCustody),
		tool("c4_hold_job", "Hold or release a job", "Hold a job (no new units start; running ones finish, or stop with stop_running) or release it (hold false).",
			idempotent, c.holdJob),
		tool("c4_boost_job", "Boost a job", "Multiply a job's required slots by 1 to 2, drawn from its own project's quota first. 1 removes the boost.",
			idempotent, c.boostJob),
		tool("c4_suggest_deadline", "Suggested deadline", "A job's deadline and who owns it (pinned by a person, suggested by the farm, or set by production tracking), and the farm's suggested deadline: generous on purpose (met 95% of the time by default), never moved earlier by the farm, moved later only when it is itself at risk (each such move listed with reason at-risk and its signed record). Also its state (estimating, settled), whether it is at risk now, the finish band (ready by, at 50, 80, 90 and 95%), and, for a job that will miss its review session, the next session. To make it sooner, see c4_deadline_options.",
			readOnly, c.suggestDeadline),
		tool("c4_deadline_options", "Deadline options", "What tighter deadlines would take for a job, without changing anything. With no times given, it offers the choices itself: the project's upcoming review sessions (dailies and the like) before the job's deadline, and the earliest time it makes at 80% without making another job late. For each time: the finish band and which percentile is on time, the smallest boost that makes it, the burst capacity and its cost (and whether it needs approval), how many other jobs it would make late (named only in your projects), and, when nothing makes it, the earliest the job can finish. Suggested deadlines are generous; tightening one is your choice, made with c4_set_deadline.",
			readOnly, c.deadlineOptions),
		tool("c4_set_deadline", "Set a job's deadline", "Set or move a job's deadline (RFC 3339, for example 2026-10-23T18:00:00-07:00), which pins it. The queue reorders work to meet it; this is what raising a priority means here. With mode, choose who owns the deadline instead: pin (keep the one it has now), suggested (follow the farm's suggestion) or tracking (production tracking sets it). Check c4_deadline_options first for what a time would take.",
			idempotent, c.setDeadline),
		tool("c4_move_to_next_session", "Move to the next session", "Move a job that will miss its review session to the next one the farm offers (c4_suggest_deadline shows it), and pin it there.",
			idempotent, c.moveToNextSession),
		tool("c4_requeue_units", "Requeue units", "Run units of a job again, discarding their previous results: a unit set like \"0-9,14\".",
			destructive, c.requeue),
		tool("c4_acknowledge_units", "Take, assign or accept failed units", "Say a person has failed units in hand so the farm stops asking: acknowledge (you take them), assign (to assignee), or accept (the failure stands; the units count as done, so nothing waits on them). Recorded with who and when, and signed in the book. A requeue clears it. Units is a set like \"14\" or \"0-9,14\"; only failed units are taken.",
			idempotent, c.acknowledge),
		tool("c4_needs_me", "What needs me", "Only what waits for the person you act for, most urgent first: questions units of their jobs stopped on (raised, or pressing once the plan says an answer is due, with answer_by, the latest an answer keeps the job on time), decisions a wait addresses to them or a party they speak for, and burst capacity waiting for their approval. Each item names the control that answers it: c4_supply or c4_withdraw_needs for a question, c4_decide with one of its choices for a decision, c4_approve_capacity for a burst. Use it to tell the person what is waiting for them; answer only what they tell you to.",
			readOnly, c.needsMe),
		tool("c4_decide", "Decide what waits for a person", "Decide a job's wait for a person (a ruling, an approval, an outside party's answer): do is approve, one of the wait's choices, or fail. A wait that asks for a reply (reply in c4_get_job's waits) takes one with the decision, reply_text or reply_view, which the units it holds up read. Or decide a unit a review escalated: accept (it stands), rework (it runs again) or fail. The job's waits, and whether each holds up work, are in c4_get_job. Signed and recorded in the book.",
			idempotent, c.decide),
		tool("c4_steer", "Send a message to a running unit", "Send a message to one unit of a job while it runs, without stopping it: a change of direction, a hint, a correction. The unit reads it at its next safe point (an agent block after its next tool call, or at the end of its turn if that comes first). A unit not running yet gets it when it starts; a finished one takes none. Delivered and read status are in c4_get_job's steers.",
			idempotent, c.steer),
		tool("c4_agent", "Start workers on the farm", "Start a worker, or several at once (workers), as you would start subagents: the farm runs each as a fresh Claude Code session from its brief alone (it sees nothing of this conversation), on a branch of its own from your HEAD (base), in your own job, which the first call makes and later calls add to. It waits up to 3 minutes and returns each worker's result: its summary in its own words, its branch, how it ended, or the question it stopped on (answer with c4_supply). Workers still running come back with their units: wait for them with c4 farm wait as a background command. background answers at once. Commit what the workers need first: they start from a commit. Only a lead, a person's own session, starts workers; a worker cannot.",
			&mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}, c.agent),
		tool("c4_plan", "Plan ahead in your job", "Plan your job ahead and change the plan as you learn, in one call of edits applied together (or none, if the farm refuses one, in its words): add a planned worker (held: it does not run until a release names it; work is your estimate), split a unit that has not started into several or merge several into one (whatever followed them follows the replacements), withdraw one with why, release held ones, give the order among your ready units, or add or remove an edge (to runs after from). A unit keeps its index: a replaced or withdrawn one stays in the job as withdrawn, naming what replaced it. A running unit is not changed: steer it (c4_steer). job is the one c4_agent returned; units are named by name or index. It answers with the forecast, before the edits and with them: when the plan is ready (p50, p90) against its deadline, its critical path, how many workers can run at once on your machines, and each unit with no history yet and where its estimate comes from (agent-env: the same agent environment and tool; work: your estimate; class-default: a guess). Signed and recorded in the book. Only a lead plans; a worker cannot.",
			&mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}, c.plan),
		tool("c4_close_lead", "Close your lead job", "Close the job c4_agent made for this session: it takes no more workers and finishes once its workers have. The next c4_agent makes a new job. The farm also closes an idle one after a day. Signed and recorded in the book.",
			idempotent, c.closeLead),
		tool("c4_supply", "Supply what a unit stopped for", "Give a unit that stopped because its brief lacked information only a person could give (needsPerson reason needsinformation; the question is in c4_get_job's needs) what it asked for. It runs again, resuming where it stopped, with the text. Signed and recorded in the book.",
			idempotent, c.supply),
		tool("c4_withdraw_needs", "Withdraw a unit's question", "Withdraw the question a unit stopped for (needsPerson reason needsinformation; c4_get_job's needs) once it no longer applies, with why. The unit runs again without an answer, told why. Supplied or withdrawn questions take none. Signed and recorded in the book.",
			idempotent, c.withdrawNeeds),
		tool("c4_avoid_node", "Avoid or allow a node for a job", "Keep a job's units off a node (a bad worker for that job), or allow it again.",
			idempotent, c.avoidNode),
		tool("c4_disable_node", "Disable or enable a node", "Take a node out of service for the whole farm (running units finish) or put it back.",
			idempotent, c.disableNode),
		tool("c4_run_bundle", "Run your agents on the farm", "Run your own Claude Code agents on the farm as one bundle, by the names they were added under (c4 farm agents add): your runner resumes each at a turn's end when there is room, and the bundle's deadline orders them against the farm's other work. The bundle is yours. dry_run first: it says which agents no runner of yours has, and refuses in the farm's words when no pool runs agents for you yet.",
			idempotent, c.runBundle),
		tool("c4_list_bundles", "List agent bundles", "The agent bundles in your projects: each one's owner, phase, deadline and members with their state (queued, running, parked at a turn's end, done, failed), and why its members cannot start, if they cannot.",
			readOnly, c.listBundles),
		tool("c4_stop_bundle", "Hold, resume or remove an agent bundle", "hold stops a bundle's agents at their next turn's end, resume lets them run again, remove stops them and deletes the bundle. Signed and recorded in the book.",
			destructive, c.stopBundle),
		tool("c4_list_tools", "List tools", "The tools published to your projects: each one's name, title, summary, kind (frames, once, shards, train, agent), version and scope, and the c4_run_<tool> that runs it. Work goes in through a tool: pick one here, read its parameters with c4_describe_tool, then submit.",
			readOnly, c.listTools),
		tool("c4_describe_tool", "Describe a tool", "One published tool: its parameters as its tool.yaml writes them and as the JSON Schema of a submission's with, its frames, its needs, its test on the farm and its versions.",
			readOnly, c.describeTool),
		tool("c4_publish_tool", "Publish a tool", "Publish a tool folder (a tree with tool.yaml at its root, already uploaded to the project's store) to a project, or to every project with farm. The farm checks tool.yaml, pins its image by digest into tool.lock and runs its test as a job; the version is offered only once that test passes, so poll c4_describe_tool with the version. Call with dry_run first to see the errors or the tool.lock.",
			&mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}, c.publishTool),
		tool("c4_submit_job", "Submit a submission", "Submit a submission (c4: submit/1): a project, a shot, when it is due, the tree it works on, and jobs by key, each a published tool with its frames and with, following others with after. Give it as an object (submission) or as YAML (submission_yaml). Call with dry_run first: it returns each job's estimate (units, ETA p50 and p90, slack, on track or not), or the errors at their paths with the fix, and creates nothing; show it to the person, then submit. explain adds what each job lowers to. The jobs are owned by the person you act for. For one tool, c4_run_<tool> writes the submission for you.",
			&mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)}, c.submitJob).withSchema(submitSchema()),
		tool("c4_get_capacity_forecast", "Capacity forecast", "What the farm's forecast jobs (work a tracker expects, from a bid or a schedule, not yet submitted) would need among the real work, as of the last forecast pass: burst slots per pool with the forecasts and as planned now, what the difference would cost an hour, and the real jobs the forecast work would put at more risk. Nothing is requested for a forecast; use it to plan capacity and budget before the work arrives.",
			readOnly, c.capacityForecast),
		tool("c4_approve_capacity", "Approve burst capacity", "Approve the burst capacity request waiting in a pool. This provisions paid capacity; check c4_capacity_and_cost first for its cost and the jobs it is for.",
			&mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)}, c.approveCapacity),
		tool("c4_grant", "Plan a grant to a tracker", "Plan a grant that would let a production tracker's sync (a Tracker: GitHub, Flow, ...) submit work for the person you act for, so jobs it makes from their issues or tasks are theirs and what is theirs (a lease of their own sign-in) serves them. It plans, never performs: an agent cannot grant for a person. It checks the tracker and returns what the grant would allow and the exact c4 farm grant command; show both to the person, who grants it as themselves, in the HUD or with that command. Optionally limit it to pools and job classes; it ends at until (30d by default, at most 90d). Granting again renews it.",
			readOnly, c.grant),
		tool("c4_list_held_writes", "List held tracker writes", "Writes to a production tracker (a comment, a status) that were interrupted where nothing can tell whether they happened, so they are held: never sent again on a guess. Each names the tracker entity (with its URL), the job, what it said and since when, and whether it is yours to confirm. Filter by job (namespace/name) or mine. Look in the tracker, then answer with c4_confirm_write.",
			readOnly, c.listHeld),
		tool("c4_confirm_write", "Confirm a held tracker write", "Say whether a held tracker write happened, after looking in the tracker: happened true and it is taken as written; false and it is written again on the sync's next pass (so say false only when it is not there, or it is made twice). The job's owner, or one allowed to change the job, answers; signed and recorded in the book.",
			&mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)}, c.confirmWrite),
		tool("c4_list_people_map", "List tracker logins and who they are", "The people maps of the trackers in projects you can see: each tracker login the project offers as a C4 person (or party), whether that person has confirmed it, and what confirming lets the login do (its decisions in the tracker relayed as that person's). Filter by tracker (namespace/name) or mine. Only the person confirms their own login, in the HUD, on their own page; an agent never confirms or accepts one for them, so tell the person what is offered and where to confirm it.",
			readOnly, c.listPeople),
		tool("c4_list_trackers", "List trackers", "The trackers (GitHub, Flow, ...) in the projects you can see: what each connects to, what it turns into jobs, whether it is connected and why not, how many people have let it submit work for them, and the person you act for's own grant to it. Use it to find the tracker to name in c4_grant.",
			readOnly, c.listTrackers),
		tool("c4_list_grants", "List grants", "Grants to trackers: by default the person you act for's own, or with tracker (namespace/name) who has let that tracker submit for them. Each shows its pools, classes, end, record, and whether it serves now and why not.",
			readOnly, c.listGrants),
		tool("c4_revoke_grant", "Revoke a grant", "Take back a grant to a tracker: the tracker submits nothing more for that person, and units of their jobs not yet started do not get their lease. Units running now finish. The person who gave it, or a project admin, revokes it; recorded in the book.",
			destructive, c.revokeGrant),
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

// NeedsMeOut is c4_needs_me's answer: the HUD's needs-me list.
type NeedsMeOut struct {
	Now   string            `json:"now"`
	For   string            `json:"for,omitempty"`
	Items []api.NeedsMeItem `json:"items"`
}

func (c *Client) needsMe(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, NeedsMeOut, error) {
	var n api.NeedsMe
	if err := c.do(ctx, "GET", "/needs-me", nil, &n); err != nil {
		return nil, NeedsMeOut{}, err
	}
	out := NeedsMeOut{Now: n.Now.Format(time.RFC3339), Items: n.Items}
	if n.Viewer != nil {
		out.For = n.Viewer.User
	}
	if out.Items == nil {
		out.Items = []api.NeedsMeItem{}
	}
	return nil, out, nil
}

func atRisk(j api.Job) bool {
	if j.Phase == "Succeeded" || j.Phase == "Failed" || j.Objective.Deadline == nil {
		return false
	}
	if r := j.Estimate.Risk; r != "" {
		return r != c4v1.RiskNone
	}
	return !j.Estimate.Feasible || j.Estimate.SlackSeconds < 0
}

type ListJobsIn struct {
	Project    string `json:"project,omitempty" jsonschema:"only this project"`
	Class      string `json:"class,omitempty" jsonschema:"render, ci or ml"`
	Phase      string `json:"phase,omitempty" jsonschema:"Queued, Running, Blocked, Held, Succeeded or Failed"`
	AtRiskOnly bool   `json:"at_risk_only,omitempty" jsonschema:"only jobs projected to miss their deadline"`
	Shot       string `json:"shot,omitempty" jsonschema:"only the jobs of this shot (sim, render, comp, transcode)"`
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
	NoETA        string  `json:"noEta,omitempty" jsonschema:"when set, the job has no ETA and this says why (held, suspended, waiting for capacity, no deadline); etaP90 is then absent"`
	SlackSeconds float64 `json:"slackSeconds"`
	Feasible     bool    `json:"feasible"`
	Message      string  `json:"message,omitempty"`
	Held         bool    `json:"held,omitempty"`
	Boost        float64 `json:"boost,omitempty"`
	Shot         string  `json:"shot,omitempty"`
	// Risk is none, atRisk, willMiss or missed.
	Risk        string       `json:"risk,omitempty" jsonschema:"deadline risk: none, atRisk (projected late, still fixable), willMiss (late even with every slot it can get) or missed"`
	Moving      int          `json:"moving,omitempty" jsonschema:"units done whose output is still moving (not yet counted in done)"`
	Failed      int          `json:"failed,omitempty" jsonschema:"units failed after their retries that nobody has accepted"`
	Accepted    int          `json:"accepted,omitempty" jsonschema:"failed units a person accepted as they are"`
	DonePct     float64      `json:"donePercent,omitempty" jsonschema:"share of the work done, never decreasing"`
	NeedsPerson *NeedsPerson `json:"needsPerson,omitempty" jsonschema:"set while the job waits on a person: why and since when"`
}

// NeedsPerson says why a job waits on a person.
type NeedsPerson struct {
	Reason  string `json:"reason" jsonschema:"failed, stuck, approval, quota or permission"`
	Message string `json:"message"`
	Since   string `json:"since"`
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
		Feasible: j.Estimate.Feasible, Message: j.Estimate.Message, Held: j.Nudge.Hold, Shot: j.Shot}
	if j.Nudge.Boost > 1 {
		r.Boost = j.Nudge.Boost
	}
	r.Running += j.Units.Retrying + j.Units.Preflight
	r.Risk, r.Moving, r.Failed, r.Accepted = j.Estimate.Risk, j.Units.Egress, j.Units.Failed, j.Units.Accepted
	if f := j.Work.DoneFraction; f != nil {
		r.DonePct = math.Round(*f*1000) / 10
	}
	if np := j.NeedsPerson; np != nil {
		r.NeedsPerson = &NeedsPerson{Reason: np.Reason, Message: np.Message, Since: np.Since.Format(time.RFC3339)}
	}
	if j.Objective.Deadline != nil {
		r.Deadline = j.Objective.Deadline.Format(time.RFC3339)
	}
	r.NoETA = j.Estimate.NoETA
	if !j.Estimate.EtaP90.IsZero() && r.NoETA == "" {
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
			in.Phase != "" && !strings.EqualFold(j.Phase, in.Phase) || in.AtRiskOnly && !atRisk(j) || in.Shot != "" && j.Shot != in.Shot {
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
	UnitState    string `json:"unit_state,omitempty" jsonschema:"with include_units, only units in this state (queued, blocked, held, preflight, running, retrying, backoff, egress, succeeded, failed, accepted)"`
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
	// Work is progress weighted by work in slot-seconds: done, running,
	// the running units' remainder, and W and W90 still to do.
	Work       api.Work  `json:"work"`
	Pools      []string  `json:"pools"`
	DependsOn  []string  `json:"dependsOn"`
	Blacklist  []string  `json:"blacklist,omitempty"`
	Units      []UnitRow `json:"units,omitempty"`
	UnitsShown int       `json:"unitsShown,omitempty"`
	// Lens is the class's own summary: passes for render; pipelines,
	// time to green and flaky targets for CI; ranks, checkpoints and
	// yields for ML.
	Lens map[string]any `json:"lens,omitempty"`
	// Waits are what its units wait for (a person's decision, a party's
	// answer), Needs the questions units stopped on, and Steers the
	// messages sent to running units, as the HUD's job detail has them.
	Waits  []api.Wait  `json:"waits,omitempty"`
	Needs  []api.Needs `json:"needs,omitempty"`
	Steers []api.Steer `json:"steers,omitempty"`
	// Plan is a lead's job's plan, with its latest forecast.
	Plan *api.LeadPlan `json:"plan,omitempty"`
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
		Pools: j.Pools, DependsOn: j.DependsOn, Blacklist: j.Blacklist, Work: j.Work,
		Waits: j.Waits, Needs: j.Needs, Steers: j.Steers, Plan: j.Plan}
	if o := asMap(d["output"]); len(o) > 0 {
		out.Output = o
	}
	if l := asMap(d["classSummary"]); len(l) > 0 {
		out.Lens = l
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

type SuggestOut struct {
	Job string `json:"job"`
	// Deadline is the effective deadline; Source where it comes from
	// (spec, suggested, class) and Mode who owns it.
	Deadline string `json:"deadline,omitempty"`
	Source   string `json:"source,omitempty" jsonschema:"spec (a person, a tool or tracking), suggested (the farm) or class (the class's objective)"`
	Mode     string `json:"mode,omitempty" jsonschema:"Pinned, Suggested or Tracking"`
	// Band is when the job is ready, at 50, 80, 90 and 95%.
	Band        map[string]string `json:"band,omitempty"`
	NoETA       string            `json:"noEta,omitempty" jsonschema:"when set, the job has no ETA and this says why; band is then absent"`
	Calibrated  bool              `json:"calibrated" jsonschema:"the band comes from finished jobs' outcomes"`
	Suggestion  *api.Suggestion   `json:"suggestion,omitempty"`
	NextSession string            `json:"nextSession,omitempty" jsonschema:"the next review session, offered when the job will miss its own; c4_move_to_next_session moves it there"`
}

func (c *Client) suggestDeadline(ctx context.Context, _ *mcp.CallToolRequest, in JobRef) (*mcp.CallToolResult, SuggestOut, error) {
	p, err := in.path()
	if err != nil {
		return nil, SuggestOut{}, err
	}
	var j api.Job
	if err := c.do(ctx, "GET", p, nil, &j); err != nil {
		return nil, SuggestOut{}, err
	}
	o, e := j.Objective, j.Estimate
	out := SuggestOut{Job: j.Namespace + "/" + j.Name, Source: o.Source, Mode: o.Mode, Calibrated: e.Calibrated, Suggestion: o.Suggestion}
	if o.Deadline != nil {
		out.Deadline = o.Deadline.Format(time.RFC3339)
	}
	out.NoETA = e.NoETA
	if !e.EtaP50.IsZero() && e.NoETA == "" {
		out.Band = map[string]string{"p50": e.EtaP50.Format(time.RFC3339), "p90": e.EtaP90.Format(time.RFC3339)}
		if e.EtaP80 != nil {
			out.Band["p80"] = e.EtaP80.Format(time.RFC3339)
		}
		if e.EtaP95 != nil {
			out.Band["p95"] = e.EtaP95.Format(time.RFC3339)
		}
	}
	if sg := o.Suggestion; sg != nil && sg.NextSession != nil {
		out.NextSession = sg.NextSession.Format(time.RFC3339)
	}
	return nil, out, nil
}

type OptionsIn struct {
	JobRef
	At []string `json:"at" jsonschema:"candidate deadlines, up to 8 RFC 3339 times (for example 2026-10-23T18:00:00-07:00); none: the tighter choices the farm offers"`
}

func (c *Client) deadlineOptions(ctx context.Context, _ *mcp.CallToolRequest, in OptionsIn) (*mcp.CallToolResult, api.DeadlineOptions, error) {
	p, err := in.path()
	if err != nil {
		return nil, api.DeadlineOptions{}, err
	}
	if len(in.At) > 8 {
		return nil, api.DeadlineOptions{}, fmt.Errorf("give at most 8 candidate times in at")
	}
	q := url.Values{}
	for _, a := range in.At {
		if _, err := time.Parse(time.RFC3339, a); err != nil {
			return nil, api.DeadlineOptions{}, fmt.Errorf("at %q is not RFC 3339 (2026-10-23T18:00:00-07:00)", a)
		}
		q.Add("at", a)
	}
	var out api.DeadlineOptions
	path := p + "/deadline-options"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if err := c.do(ctx, "GET", path, nil, &out); err != nil {
		return nil, api.DeadlineOptions{}, err
	}
	if out.Options == nil {
		out.Options = []api.DeadlineOption{}
	}
	return nil, out, nil
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
	Deadline string `json:"deadline,omitempty" jsonschema:"RFC 3339 time, for example 2026-10-23T18:00:00-07:00"`
	Mode     string `json:"mode,omitempty" jsonschema:"who owns the deadline: pin, suggested or tracking (default pin when deadline is given)"`
}

var deadlineModes = map[string]string{"pin": "Pinned", "pinned": "Pinned", "suggested": "Suggested", "tracking": "Tracking"}

func (c *Client) setDeadline(ctx context.Context, _ *mcp.CallToolRequest, in DeadlineIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	body := map[string]any{}
	if in.Deadline != "" {
		if _, err := time.Parse(time.RFC3339, in.Deadline); err != nil {
			return nil, Done{}, fmt.Errorf("deadline %q is not RFC 3339 (2026-10-23T18:00:00-07:00)", in.Deadline)
		}
		body["deadline"] = in.Deadline
	}
	if in.Mode != "" {
		m, ok := deadlineModes[strings.ToLower(in.Mode)]
		if !ok {
			return nil, Done{}, fmt.Errorf("mode %q: use pin, suggested or tracking", in.Mode)
		}
		if m != "Pinned" && in.Deadline != "" {
			return nil, Done{}, fmt.Errorf("a deadline is pinned: give deadline alone (or with mode pin), or mode %s alone", in.Mode)
		}
		body["deadlineMode"] = m
	}
	if len(body) == 0 {
		return nil, Done{}, fmt.Errorf("give deadline (RFC 3339), mode (pin, suggested or tracking), or both")
	}
	return c.control(ctx, p+"/nudge", body)
}

func (c *Client) moveToNextSession(ctx context.Context, _ *mcp.CallToolRequest, in JobRef) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	return c.control(ctx, p+"/nudge", map[string]any{"nextSession": true})
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

type AckIn struct {
	JobRef
	Units    string `json:"units" jsonschema:"a unit set, for example 14 or 0-9,14"`
	Kind     string `json:"kind,omitempty" jsonschema:"acknowledge (default), assign or accept"`
	Assignee string `json:"assignee,omitempty" jsonschema:"with assign, who has them"`
	Note     string `json:"note,omitempty" jsonschema:"why, for the book"`
}

func (c *Client) acknowledge(ctx context.Context, _ *mcp.CallToolRequest, in AckIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	body := map[string]any{"units": in.Units, "kind": in.Kind}
	if in.Assignee != "" {
		body["assignee"] = in.Assignee
	}
	if in.Note != "" {
		body["note"] = in.Note
	}
	return c.control(ctx, p+"/ack", body)
}

type DecideIn struct {
	JobRef
	Unit string `json:"unit" jsonschema:"the waiting unit's name or index"`
	Do   string `json:"do" jsonschema:"approve, one of the wait's choices, or fail; for an escalated unit accept, rework or fail"`
	Note string `json:"note,omitempty" jsonschema:"why, for the book"`
	// A wait that asks for a reply completes only with one.
	ReplyText string `json:"reply_text,omitempty" jsonschema:"the decision's content, for a wait that asks for a reply: stored as reply.md, which the units it holds up read"`
	ReplyView string `json:"reply_view,omitempty" jsonschema:"the decision's content as a view already in the project's store (its C4 ID), instead of reply_text"`
}

func (c *Client) decide(ctx context.Context, _ *mcp.CallToolRequest, in DecideIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	body := map[string]any{"unit": in.Unit, "do": in.Do}
	if in.Note != "" {
		body["note"] = in.Note
	}
	if in.ReplyText != "" || in.ReplyView != "" {
		body["reply"] = map[string]string{"text": in.ReplyText, "view": in.ReplyView}
	}
	return c.control(ctx, p+"/decide", body)
}

type SteerIn struct {
	JobRef
	Unit string `json:"unit" jsonschema:"the unit's name or index"`
	Text string `json:"text" jsonschema:"the message, up to 8 KiB"`
}

func (c *Client) steer(ctx context.Context, _ *mcp.CallToolRequest, in SteerIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	return c.control(ctx, p+"/steer", map[string]any{"unit": in.Unit, "text": in.Text})
}

type SupplyIn struct {
	JobRef
	Unit string `json:"unit" jsonschema:"the unit's name or index"`
	Text string `json:"text" jsonschema:"the information it asked for"`
}

func (c *Client) supply(ctx context.Context, _ *mcp.CallToolRequest, in SupplyIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	return c.control(ctx, p+"/supply", map[string]any{"unit": in.Unit, "text": in.Text})
}

type WithdrawNeedsIn struct {
	JobRef
	Unit string `json:"unit" jsonschema:"the unit's name or index"`
	Why  string `json:"why" jsonschema:"why the question no longer applies"`
}

func (c *Client) withdrawNeeds(ctx context.Context, _ *mcp.CallToolRequest, in WithdrawNeedsIn) (*mcp.CallToolResult, Done, error) {
	p, err := in.path()
	if err != nil {
		return nil, Done{}, err
	}
	return c.control(ctx, p+"/needs/"+url.PathEscape(in.Unit)+"/withdraw", map[string]any{"why": in.Why})
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
	return c.provenance(ctx, in, "/trace/")
}

func (c *Client) verifyCustody(ctx context.Context, _ *mcp.CallToolRequest, in TraceIn) (*mcp.CallToolResult, map[string]any, error) {
	return c.provenance(ctx, in, "/custody/")
}

func (c *Client) provenance(ctx context.Context, in TraceIn, route string) (*mcp.CallToolResult, map[string]any, error) {
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
	return nil, d, c.do(ctx, "GET", strings.Replace(p, "/jobs/", route, 1)+"?"+q.Encode(), nil, &d)
}

type BookIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"with name, only this job's entries"`
	Name      string `json:"name,omitempty" jsonschema:"with namespace, only this job's entries"`
	Limit     int    `json:"limit,omitempty" jsonschema:"newest entries to return, 1 to 1000 (default 50)"`
}

func (c *Client) readBook(ctx context.Context, _ *mcp.CallToolRequest, in BookIn) (*mcp.CallToolResult, api.Book, error) {
	q := url.Values{}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	q.Set("limit", strconv.Itoa(min(limit, 1000)))
	if in.Namespace != "" || in.Name != "" {
		if _, err := (JobRef{Namespace: in.Namespace, Name: in.Name}).path(); err != nil {
			return nil, api.Book{}, err
		}
		q.Set("job", in.Namespace+"/"+in.Name)
	}
	var b api.Book
	if err := c.do(ctx, "GET", "/book?"+q.Encode(), nil, &b); err != nil {
		return nil, api.Book{}, err
	}
	if b.Entries == nil {
		b.Entries = []api.BookEntry{}
	}
	return nil, b, nil
}

type GrantIn struct {
	Project   string   `json:"project,omitempty" jsonschema:"the project the tracker is in (one you can see may be left out)"`
	Namespace string   `json:"namespace,omitempty" jsonschema:"or the namespace the tracker is in"`
	Tracker   string   `json:"tracker" jsonschema:"the tracker's name"`
	Pools     []string `json:"pools,omitempty" jsonschema:"only jobs in these pools"`
	Classes   []string `json:"classes,omitempty" jsonschema:"only jobs of these classes"`
	Until     string   `json:"until,omitempty" jsonschema:"when it ends: an RFC 3339 time or a span like 30d (default 30d, at most 90d)"`
}

// GrantPlan is a grant planned, never performed: what it would allow and
// the command the person runs to grant it.
type GrantPlan struct {
	// Performed is always false: an agent plans a grant; the person grants.
	Performed bool     `json:"performed"`
	Project   string   `json:"project"`
	Namespace string   `json:"namespace"`
	Tracker   string   `json:"tracker"`
	Site      string   `json:"site,omitempty"`
	Pools     []string `json:"pools,omitempty"`
	Classes   []string `json:"classes,omitempty"`
	Until     string   `json:"until"`
	// Renews is the person's own grant to the tracker this one renews.
	Renews *api.Grant `json:"renews,omitempty"`
	// Allows is what the grant would let the tracker do.
	Allows string `json:"allows"`
	// Command is what the person runs to grant it.
	Command string `json:"command"`
	Message string `json:"message"`
}

// grant plans a grant: it finds the tracker and says what the grant would
// allow and how the person grants it. It never sends the grant; the HUD
// refuses one from an agent.
func (c *Client) grant(ctx context.Context, _ *mcp.CallToolRequest, in GrantIn) (*mcp.CallToolResult, GrantPlan, error) {
	if in.Tracker == "" {
		return nil, GrantPlan{}, fmt.Errorf("name the tracker to grant (from c4_list_trackers)")
	}
	var l api.TrackerList
	if err := c.do(ctx, "GET", "/trackers", nil, &l); err != nil {
		return nil, GrantPlan{}, err
	}
	var found []api.Tracker
	for _, t := range l.Trackers {
		if t.Name == in.Tracker && (in.Project == "" || t.Project == in.Project) && (in.Namespace == "" || t.Namespace == in.Namespace) {
			found = append(found, t)
		}
	}
	switch {
	case len(found) == 0:
		return nil, GrantPlan{}, fmt.Errorf("no tracker %s in the projects you can see (c4_list_trackers lists them)", in.Tracker)
	case len(found) > 1:
		return nil, GrantPlan{}, fmt.Errorf("tracker %s is in more than one project: name the project", in.Tracker)
	}
	t := found[0]
	until := in.Until
	if until == "" {
		until = "30d"
	}
	p := GrantPlan{Project: t.Project, Namespace: t.Namespace, Tracker: t.Name, Site: t.Site, Pools: in.Pools, Classes: in.Classes, Until: until}
	if t.Mine != nil && t.Mine.Revoked == nil {
		p.Renews = t.Mine
	}
	var scope []string
	if len(in.Pools) > 0 {
		scope = append(scope, "only jobs in pools "+strings.Join(in.Pools, ", "))
	}
	if len(in.Classes) > 0 {
		scope = append(scope, "only jobs of class "+strings.Join(in.Classes, ", "))
	}
	if len(scope) == 0 {
		scope = append(scope, "any of its jobs in project "+t.Project)
	}
	where := t.Kind
	if t.Site != "" {
		where = t.Site
	}
	p.Allows = fmt.Sprintf("tracker %s (%s, project %s) would submit work for the person you act for, as them: jobs it makes from their issues or tasks are theirs, and what is theirs (a lease of their own sign-in) serves them; %s; for %s from when they grant it",
		t.Name, where, t.Project, strings.Join(scope, "; "), until)
	if p.Renews != nil {
		p.Allows += fmt.Sprintf("; it renews their grant %s/%s", p.Renews.Namespace, p.Renews.Name)
	}
	cmd := []string{"c4", "farm", "grant", shellWord(t.Name), "--project", shellWord(t.Project)}
	if len(in.Pools) > 0 {
		cmd = append(cmd, "--pools", shellWord(strings.Join(in.Pools, ",")))
	}
	if len(in.Classes) > 0 {
		cmd = append(cmd, "--classes", shellWord(strings.Join(in.Classes, ",")))
	}
	if in.Until != "" {
		cmd = append(cmd, "--until", shellWord(in.Until))
	}
	p.Command = strings.Join(cmd, " ")
	p.Message = "planned, not granted: an agent cannot grant for you. Ask the person to grant it in the HUD or run: " + p.Command
	return nil, p, nil
}

// shellWord quotes s for a shell when it needs it.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-,:/@+=") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type ListGrantsIn struct {
	Tracker string `json:"tracker,omitempty" jsonschema:"namespace/name: who has granted this tracker"`
	Person  string `json:"person,omitempty" jsonschema:"another person's grants, in projects you can see"`
}

func (c *Client) listGrants(ctx context.Context, _ *mcp.CallToolRequest, in ListGrantsIn) (*mcp.CallToolResult, api.GrantList, error) {
	q := url.Values{}
	if in.Tracker != "" {
		q.Set("tracker", in.Tracker)
	}
	if in.Person != "" {
		q.Set("person", in.Person)
	}
	p := "/grants"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var out api.GrantList
	err := c.do(ctx, "GET", p, nil, &out)
	return nil, out, err
}

type RevokeGrantIn struct {
	Namespace string `json:"namespace" jsonschema:"the grant's namespace (from c4_list_grants)"`
	Name      string `json:"name" jsonschema:"the grant's name (from c4_list_grants)"`
}

func (c *Client) revokeGrant(ctx context.Context, _ *mcp.CallToolRequest, in RevokeGrantIn) (*mcp.CallToolResult, Done, error) {
	if in.Namespace == "" || in.Name == "" {
		return nil, Done{}, fmt.Errorf("name the grant: namespace and name, from c4_list_grants")
	}
	var r struct {
		Message string `json:"message"`
	}
	if err := c.do(ctx, "DELETE", "/grants/"+url.PathEscape(in.Namespace)+"/"+url.PathEscape(in.Name), nil, &r); err != nil {
		return nil, Done{}, err
	}
	return nil, Done{Message: r.Message}, nil
}

func (c *Client) listTrackers(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, api.TrackerList, error) {
	var out api.TrackerList
	err := c.do(ctx, "GET", "/trackers", nil, &out)
	return nil, out, err
}

func (c *Client) capacityForecast(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, api.CapacityForecast, error) {
	var out api.CapacityForecast
	err := c.do(ctx, "GET", "/capacity/forecast", nil, &out)
	return nil, out, err
}

type ListHeldIn struct {
	Job  string `json:"job,omitempty" jsonschema:"namespace/name: one job's held writes"`
	Mine bool   `json:"mine,omitempty" jsonschema:"only writes for jobs you own"`
}

func (c *Client) listHeld(ctx context.Context, _ *mcp.CallToolRequest, in ListHeldIn) (*mcp.CallToolResult, api.HeldList, error) {
	q := url.Values{}
	if in.Job != "" {
		q.Set("job", in.Job)
	}
	if in.Mine {
		q.Set("mine", "1")
	}
	p := "/held"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var out api.HeldList
	err := c.do(ctx, "GET", p, nil, &out)
	return nil, out, err
}

type ConfirmWriteIn struct {
	Namespace string `json:"namespace" jsonschema:"the held write's namespace (from c4_list_held_writes)"`
	Link      string `json:"link" jsonschema:"the held write's link (from c4_list_held_writes)"`
	Field     string `json:"field" jsonschema:"the held write's field (from c4_list_held_writes)"`
	Happened  bool   `json:"happened" jsonschema:"whether the write is there in the tracker"`
	Note      string `json:"note,omitempty" jsonschema:"what you saw, for the record"`
}

func (c *Client) confirmWrite(ctx context.Context, _ *mcp.CallToolRequest, in ConfirmWriteIn) (*mcp.CallToolResult, Done, error) {
	var out Done
	err := c.do(ctx, "POST", "/held/"+url.PathEscape(in.Namespace)+"/"+url.PathEscape(in.Link)+"/"+url.PathEscape(in.Field),
		api.ConfirmRequest{Happened: &in.Happened, Note: in.Note}, &out)
	return nil, out, err
}

type ListPeopleIn struct {
	Tracker string `json:"tracker,omitempty" jsonschema:"namespace/name: one tracker's people map"`
	Mine    bool   `json:"mine,omitempty" jsonschema:"only the logins offered as the person you act for"`
}

func (c *Client) listPeople(ctx context.Context, _ *mcp.CallToolRequest, in ListPeopleIn) (*mcp.CallToolResult, api.PeopleList, error) {
	q := url.Values{}
	if in.Tracker != "" {
		q.Set("tracker", in.Tracker)
	}
	if in.Mine {
		q.Set("mine", "1")
	}
	p := "/people"
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var out api.PeopleList
	err := c.do(ctx, "GET", p, nil, &out)
	return nil, out, err
}
