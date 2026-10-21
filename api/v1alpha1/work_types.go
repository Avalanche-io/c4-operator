package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Labels and annotations of the work system.
const (
	// LabelJob names the WorkJob a unit pod belongs to.
	LabelJob = "c4.avalanche.io/job"
	// LabelUnit is the unit's index within its job.
	LabelUnit = "c4.avalanche.io/unit"
	// LabelPool is the pool a unit was released into.
	LabelPool = "c4.avalanche.io/pool"
	// AnnotationAttempt is the unit's attempt number, from 1.
	AnnotationAttempt = "c4.avalanche.io/attempt"
	// AnnotationRequeue on a WorkJob asks the queue to run units again: a
	// JSON list of WorkRequeueRequest, each naming its signed record. On a
	// farm with signing off, "TOKEN:SET" (SET a run-length index set,
	// TOKEN making each request distinct) is still read.
	AnnotationRequeue = "c4.avalanche.io/requeue"
	// AnnotationAllowNodes on a WorkJob takes nodes off the job's
	// automatic blacklist: a JSON list of WorkAllowNodeRequest. On a farm
	// with signing off, a comma-separated list of nodes is still read.
	AnnotationAllowNodes = "c4.avalanche.io/allow-nodes"
	// AnnotationYield asks a unit to stop at its next safe point (a
	// checkpoint, a frame or chunk boundary, the end of an agent turn): a
	// soft stop. Its value is the reason.
	AnnotationYield = "c4.avalanche.io/yield"
	// AnnotationYieldBy is the soft stop's deadline (RFC 3339): a unit that
	// has not stopped by then is stopped (the hard stop), losing what it
	// did since its last safe point.
	AnnotationYieldBy = "c4.avalanche.io/yield-by"
	// AnnotationSafepoint on a running unit's pod is the last safe point
	// it reached and asked a decision for, written by the node daemon:
	// {"seq", "at", "checkpoint"?} (JSON).
	AnnotationSafepoint = "c4.avalanche.io/safepoint"
	// AnnotationSafepointDecision is the queue's answer at that safe point:
	// {"seq", "do": "stop" | "continue", "why", "at"} (JSON). The node
	// daemon passes it to the waiting unit (c4-unit safepoint).
	AnnotationSafepointDecision = "c4.avalanche.io/safepoint-decision"
	// AnnotationSafepointNext is what the queue would decide at the unit's
	// next safe point as things stand: stop, continue or undecided.
	AnnotationSafepointNext = "c4.avalanche.io/safepoint-next"
	// AnnotationOutputCheckpoint on a unit pod that yielded names where the
	// next attempt resumes, for classes whose state is not a captured view:
	// an agent member's turn checkpoint ("<session>#<seq>:<head>"). Written
	// by whatever ran the unit (the node daemon, or a host runner).
	AnnotationOutputCheckpoint = "c4.avalanche.io/output-checkpoint"
	// AnnotationOutputEpoch on an agent member's pod is the member's lease
	// number as its runner last resumed it.
	AnnotationOutputEpoch = "c4.avalanche.io/output-epoch"
	// AnnotationOutputExpand on a generator unit's finished pod is the C4
	// ID of the expansion document it wrote (.c4/expand.json), stored by
	// the node daemon at capture (DESIGN 10.6).
	AnnotationOutputExpand = "c4.avalanche.io/output-expand"
	// AnnotationOutputVerdict on a review unit's finished pod is the C4 ID
	// of the verdict it wrote (.c4/verdict.json), stored by the node daemon
	// at capture.
	AnnotationOutputVerdict = "c4.avalanche.io/output-verdict"
	// AnnotationOutputMeasure on a finished unit pod is the C4 ID of the
	// measure reading it wrote (.c4/measure.json), stored at capture.
	AnnotationOutputMeasure = "c4.avalanche.io/output-measure"
	// AnnotationOutputUsage on a finished model unit's pod is the C4 ID of
	// its model usage (.c4/usage.json: tokens and their cost), stored at
	// capture.
	AnnotationOutputUsage = "c4.avalanche.io/output-usage"
	// FinalizerUnit is on every unit pod, so a pod deleted while it runs
	// (a drain, an eviction, a preemption) stays readable until the queue
	// has recorded how it ended.
	FinalizerUnit = "c4.avalanche.io/unit"
	// AnnotationOwner on a WorkJob is the user who submitted it, set at
	// admission from the request's user (or, for jobs the HUD creates for
	// a signed-in viewer, by the HUD) and never changed afterwards.
	AnnotationOwner = "c4.avalanche.io/owner"
	// AnnotationAcceptValidated on a WorkJob (a JSON list of
	// WorkAcceptValidatedRequest; "true" on a farm with signing off)
	// accepts a validated
	// recompute of its inputs in place of moving them (staging,
	// experimental): the recomputed output differs from the recorded one,
	// so only the consuming job may choose it. Exact recomputes and
	// identical outputs need no consent; volatile ones are never offered.
	AnnotationAcceptValidated = "c4.avalanche.io/accept-validated"
	// AnnotationSubmittedVia says how the job was submitted: hud, mcp,
	// c4-sync (a tracker's, for a person under their grant), or api
	// (anything else that wrote the WorkJob directly).
	AnnotationSubmittedVia = "c4.avalanche.io/submitted-via"
	// AnnotationSubmittedByUnit names the unit (<namespace>/<job>/<unit>)
	// that submitted a job as part of its own work. Its owner is the owner
	// of the unit's job, who is accountable for it; only the farm sets it.
	AnnotationSubmittedByUnit = "c4.avalanche.io/submitted-by-unit"
	// LabelShot groups a shot's jobs (the HUD's shot view).
	LabelShot = "c4.avalanche.io/shot"
	// LabelMilestone on a WorkJob joins it to the Milestone of that name
	// in its namespace, whatever the Milestone's selector: a submission
	// due at a milestone (due: dailies) is labelled so.
	LabelMilestone = "c4.avalanche.io/milestone"
	// LabelJobType names the prebuilt job type a job was made from
	// (render, usd, sim, comp, transcode, ci, lora, infer, ...).
	LabelJobType = "c4.avalanche.io/job-type"
	// LabelStage names the pipeline stage a job is (light, depth, comp,
	// review, ...): with the shot and the job type it keys the shot
	// history a job's estimate starts from.
	LabelStage = "c4.avalanche.io/stage"
	// AnnotationAcks on a WorkJob carries people's acknowledgements of
	// units that need a person: a JSON list of WorkAckRequest, newest last.
	// The queue carries out each request once (status.acked) and records
	// it in status.acks.
	AnnotationAcks = "c4.avalanche.io/acks"
	// AnnotationDecide on a WorkJob carries people's decisions on what
	// waits for them: its wait units (a ruling, an approval, an outside
	// party's answer) and units a review escalated. A JSON list of
	// WorkDecideRequest; the queue carries out each once, in token order
	// (status.decided), and records it in status.waits.
	AnnotationDecide = "c4.avalanche.io/decide"
	// AnnotationReplaces on a real WorkJob names the forecast job in its
	// namespace it replaced (DESIGN.md 10.12). The HUD deletes that
	// forecast when it submits the job, and again until it is gone.
	AnnotationReplaces = "c4.avalanche.io/replaces"
	// AnnotationSteer on a WorkJob carries messages people send to its
	// units while they run, without ending them: a JSON list of
	// WorkSteerRequest. The queue carries out each once, in token order
	// (status.steered), and delivers it to the unit (AnnotationUnitSteer).
	AnnotationSteer = "c4.avalanche.io/steer"
	// AnnotationUnitSteer on a unit pod holds the messages its unit has
	// not read yet: a JSON list of {seq, at, actor, text}. The node daemon
	// (or the host runner) writes them to the pod's control directory as
	// steer, where c4-unit steer reads them.
	AnnotationUnitSteer = "c4.avalanche.io/unit-steer"
	// AnnotationSteerSeen on a unit pod is the number of the last message
	// its unit read, relayed from the unit's .c4/steer-seen.
	AnnotationSteerSeen = "c4.avalanche.io/steer-seen"
	// AnnotationAsks on a unit pod is the questions its unit asked other
	// units (c4-unit ask), and AnnotationAnswers the answers it gave
	// (c4-unit answer): JSON lists relayed by the node daemon (or the host
	// runner) from the unit's .c4/asks and .c4/answers. The queue routes
	// each to the unit it is for, as a steering message.
	AnnotationAsks    = "c4.avalanche.io/asks"
	AnnotationAnswers = "c4.avalanche.io/answers"
	// AnnotationWake on a unit pod says it is an extra attempt of a unit
	// that had finished, woken only to answer these questions (comma-
	// separated ids; $C4_ANSWER_ONLY). Its end changes nothing about the
	// unit, and nothing it writes is kept.
	AnnotationWake = "c4.avalanche.io/wake"
	// AnnotationSupply on a WorkJob carries information people supply to
	// units that stopped for it (NeedsInformation): a JSON list of
	// WorkSupplyRequest. The queue carries out each once, in token order
	// (status.supplied): the unit runs again with the text in
	// $C4_SUPPLIED, resuming where it stopped.
	AnnotationSupply = "c4.avalanche.io/supply"
	// AnnotationWithdrawNeeds on a WorkJob carries the lead's withdrawals
	// of units' questions (NeedsInformation): a JSON list of
	// WorkWithdrawRequest, carried out once each in token order
	// (status.withdrawn). The unit runs again without an answer, with the
	// reason in $C4_WITHDRAWN.
	AnnotationWithdrawNeeds = "c4.avalanche.io/withdraw-needs"
	// AnnotationRecheck on a unit pod says it is an extra attempt of a unit
	// that stopped for information, started only to re-check its question
	// ($C4_RECHECK): the value is why. Exit 0 withdraws the question, with
	// the termination message as the why; exit NeedsExitCode keeps it.
	// Nothing it writes is kept, and no retry is spent.
	AnnotationRecheck = "c4.avalanche.io/recheck"
	// AnnotationPreflight on a unit pod reports the step it is preparing
	// before the work starts: JSON {"step": "stage" | "fetch" | "image" |
	// "license", "bytes": n, "of": n}. The node daemon and the class
	// drivers write it; "step": "done" (or no annotation once the
	// container runs) ends preflight.
	AnnotationPreflight = "c4.avalanche.io/preflight"
	// AnnotationEgress on a finished unit pod reports its output's
	// capture: JSON {"bytes": n, "of": n}.
	AnnotationEgress = "c4.avalanche.io/egress"
	// TaintUnprovisioned marks a node that stands in for capacity not yet
	// provisioned (the kind capacity simulator, section 10.12).
	TaintUnprovisioned = "c4.avalanche.io/unprovisioned"
)

// WorkJobSpec declares a job: its class, its units' parameters, its
// objective, where it may run, its inputs and outputs, and the pod each
// unit runs.
type WorkJobSpec struct {
	// Class names the work class: render, ci, ml, agent or graph.
	// +kubebuilder:validation:Enum=render;ci;ml;agent;graph
	Class string `json:"class"`

	// Params are the class's parameters, for example frames and chunk for
	// render, shards for ci, steps for ml.
	// +optional
	Params map[string]string `json:"params,omitempty"`

	// Deadline is the job's own deadline. Without one, the class's
	// objective applies (a latency target from the time the job became
	// ready, or a default turnaround).
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`

	// DeadlineMode says who owns the deadline. Pinned: Deadline, set by a
	// person or a tool (the default when Deadline is set). Suggested: the
	// farm suggests one from the deadline policy (the project's, else the
	// class's), the job's estimate and the first units' measurements; it
	// revises the suggestion at most a few times and then settles it, and
	// never moves a settled one (the default when Deadline is not set and
	// the policy is not a latency SLO). Tracking: production tracking
	// writes Deadline; the farm never suggests one.
	// +kubebuilder:validation:Enum=Pinned;Suggested;Tracking
	// +optional
	DeadlineMode string `json:"deadlineMode,omitempty"`

	// Pools the job's units may run in. Empty means the class's defaults.
	// +optional
	Pools []string `json:"pools,omitempty"`

	// Measure is the job's own measure of done (DESIGN.md 10.6): a number a
	// unit reports (.c4/measure.json, c4-unit measure), such as rules
	// sited of 379, shown as the job's progress.
	// +optional
	Measure *WorkMeasureSpec `json:"measure,omitempty"`
	// DependsOn names jobs in this namespace that must succeed first.
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

	// Follows names the job in this namespace this one is the next
	// version of (v008 answers the notes on v007). The job record names
	// it, so lineage links the versions.
	// +optional
	Follows string `json:"follows,omitempty"`

	// Tool is the published tool the job's units run under: a tool job's
	// (POST /submissions, and a tool's test when it is published). Its
	// units are labelled with it and reach the networks it names and
	// nothing else; with none they are sealed (DESIGN.md 10.6, tools).
	// +optional
	Tool *WorkTool `json:"tool,omitempty"`

	// Review is a person's (or party's) review of the whole job's work:
	// a final wait, unit "@review", after every other unit, including
	// those expansions add later, so the job is Succeeded only once it is
	// decided ("done means approved", DESIGN.md 10.6). Any choice but
	// fail completes it; fail fails the job. Set at creation only.
	// +optional
	Review *WorkReviewSpec `json:"review,omitempty"`

	// Forecast makes the job a forecast only (DESIGN.md 10.12): work a
	// tracker expects (a shot not yet turned over, its bid) that the farm
	// plans for. It is estimated and placed among the real work, never
	// released: no pods, no quota, no burst capacity bought for it, and no
	// real job's ETA moves for it. It needs a deadline, its own or a
	// Milestone's. A real job replaces it when the work is submitted. A
	// job is a forecast, or not, from its creation; its size may change
	// as the bid does.
	// +optional
	Forecast *WorkForecast `json:"forecast,omitempty"`

	// Inputs are C4 views mounted into every unit.
	// +optional
	Inputs []WorkInput `json:"inputs,omitempty"`

	// Nudge holds or boosts the job. There are no priorities.
	// +optional
	Nudge WorkNudge `json:"nudge,omitempty"`

	// Retries per unit after the first attempt: 0 for none. Unset is 2.
	// +kubebuilder:default=2
	// +optional
	Retries *int32 `json:"retries,omitempty"`

	// NodeErrorLimit blacklists a node for this job after this many unit
	// failures on it.
	// +kubebuilder:default=3
	// +optional
	NodeErrorLimit int32 `json:"nodeErrorLimit,omitempty"`

	// AvoidNodes are nodes a person took out of this job's use; the queue
	// adds nodes that failed its units NodeErrorLimit times.
	// +optional
	AvoidNodes []string `json:"avoidNodes,omitempty"`

	// MaxParallel caps the job's running units (0: no cap).
	// +optional
	MaxParallel int32 `json:"maxParallel,omitempty"`

	// Template is the pod every unit runs. The queue adds the unit's
	// environment (C4_UNIT, and the class's variables), the pool's node
	// selector and tolerations, and the input volumes. The API server
	// validates it when the queue creates each pod; the CRD does not
	// repeat the pod schema (it would exceed what kubectl apply can carry).
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	Template corev1.PodTemplateSpec `json:"template"`
}

// WorkInput mounts a view into every unit: a Materialization, or the
// output view of another job in the namespace.
type WorkInput struct {
	// Materialization in the job's namespace.
	// +optional
	Materialization string `json:"materialization,omitempty"`
	// FromJob mounts the output view of that job (in this namespace) once
	// it has succeeded. List it in dependsOn too.
	// +optional
	FromJob string `json:"fromJob,omitempty"`
	// MountPath in every container of the unit.
	MountPath string `json:"mountPath"`
	// ReadOnly mounts without capture.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// WorkTool is the tool a job's units run under, and the networks the
// version declares (its tool.yaml's needs.network).
type WorkTool struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Network names the networks the units may reach, as the farm admin
	// defines them (the c4-networks ConfigMap in the operator's
	// namespace). Empty: the units are sealed.
	// +optional
	Network []string `json:"network,omitempty"`
}

// WorkNudge is a person's bounded override.
type WorkNudge struct {
	// Hold releases no new units; running units finish.
	// +optional
	Hold bool `json:"hold,omitempty"`
	// HoldStop holds and stops running units.
	// +optional
	HoldStop bool `json:"holdStop,omitempty"`
	// Boost multiplies the job's required slots, from 1 to 2, drawn from
	// its own project's quota first.
	// +kubebuilder:validation:Pattern=`^(1(\.[0-9]+)?|2(\.0+)?)$`
	// +optional
	Boost string `json:"boost,omitempty"`
}

// WorkJobStatus is the queue's view of a job. Per-unit state is kept as
// run-length index sets, so its size grows with state changes along the
// unit range, not with the number of units.
type WorkJobStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is Queued, Running, Blocked, Held, Succeeded or Failed; or
	// Forecast for a forecast job (spec.forecast), which never runs.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Order is the dispatch order over the unit axis.
	// +optional
	Order string `json:"order,omitempty"`
	// Units counts units by state.
	// +optional
	Units UnitCounts `json:"units,omitempty"`
	// UnitStates holds run-length index sets by state, each unit in one:
	// queued (or blocked), preflight (its pod is preparing: placement,
	// inputs, cache, image, license), running (the first attempt is
	// working), retrying (a later attempt is working), backoff (an attempt
	// failed and the next waits for a slot), egress (the work is done and
	// its output is being captured and added to the job's output),
	// succeeded (the output is added and recorded), failed (failed after
	// its retries: a person must act) and accepted (failed, and a person
	// accepted it, so nothing waits on it).
	// +optional
	UnitStates map[string]string `json:"unitStates,omitempty"`
	// Attempts per unit index, for units tried more than once
	// ("index:attempts" pairs, comma-separated).
	// +optional
	Attempts string `json:"attempts,omitempty"`
	// UnitMillis is each finished unit's run time in milliseconds, by
	// index (0: not finished). Jobs above 50,000 units keep it in the
	// project store instead (DESIGN.md section 10.5).
	// +optional
	UnitMillis []int64 `json:"unitMillis,omitempty"`
	// Estimate is the remaining-work estimate and objective state.
	// +optional
	Estimate *WorkEstimate `json:"estimate,omitempty"`
	// Deadline is the effective deadline.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
	// DeadlineSource says where Deadline comes from: spec (Pinned or
	// Tracking), milestone (the earliest Milestone selecting the job),
	// suggested, or class (the class's objective).
	// +optional
	DeadlineSource string `json:"deadlineSource,omitempty"`
	// Milestone names the Milestone the deadline comes from.
	// +optional
	Milestone string `json:"milestone,omitempty"`
	// Suggestion is the farm's suggested deadline and how it got there.
	// +optional
	Suggestion *WorkSuggestion `json:"suggestion,omitempty"`
	// Derivation is the record of the job's process. Experimental: it
	// will change with C4's derivation-record design.
	// +optional
	Derivation *WorkDerivation `json:"derivation,omitempty"`
	// InputSubstitutes are the inputs the job's units read a validated
	// recompute's output in place of, at a site (experimental, staging;
	// AnnotationAcceptValidated).
	// +optional
	InputSubstitutes []WorkInputSubstitute `json:"inputSubstitutes,omitempty"`
	// Procedural lists model units whose work a procedure might do: run
	// again on the same inputs, they wrote the same bytes every time
	// (DESIGN.md 10.6). A report, for a person to weigh.
	// +optional
	Procedural []WorkProceduralCandidate `json:"procedural,omitempty"`
	// Rank is the job's position in dispatch order (1 is next).
	// +optional
	Rank int32 `json:"rank,omitempty"`
	// Blacklist holds nodes this job will not use.
	// +optional
	Blacklist []string `json:"blacklist,omitempty"`
	// Output is the job's output view: every unit's captured changeset
	// folded in as the unit finishes.
	// +optional
	Output *WorkOutput `json:"output,omitempty"`
	// Resume maps a unit index to the view its last yielded attempt
	// captured; the next attempt mounts that view and continues from its
	// checkpoint.
	// +optional
	Resume map[string]string `json:"resume,omitempty"`
	// Yields lists the most recent yields of checkpointing units.
	// +optional
	Yields []WorkYield `json:"yields,omitempty"`
	// Decisions lists the most recent preemptions and yields the queue
	// decided for this job's units, each with its signed decision record.
	// +optional
	Decisions []WorkDecision `json:"decisions,omitempty"`
	// Expansions lists every generator unit evaluated, in the order its
	// children were added: the unit table is the class's plan with these
	// applied, so a restart rebuilds it from the documents in the store
	// (DESIGN 10.6).
	// +optional
	Expansions []WorkExpansion `json:"expansions,omitempty"`
	// UnitOutputs maps the index of a unit another unit reads (fromUnit)
	// to its captured output view.
	// +optional
	UnitOutputs map[string]string `json:"unitOutputs,omitempty"`
	// UnitBranches maps a unit's index to the git branch its work is on,
	// as its last attempt's runner recorded it: a block on a host works
	// in its own worktree of the developer's repository, and its commits
	// are its output.
	// +optional
	UnitBranches map[string]WorkUnitBranch `json:"unitBranches,omitempty"`
	// Verdicts lists the most recent verdicts review units wrote, each
	// carried out once.
	// +optional
	Verdicts []WorkVerdict `json:"verdicts,omitempty"`
	// Reviews maps the index of each unit a verdict did more than pass
	// (reworked, escalated, accepted with a note) to what it decided last.
	// +optional
	Reviews map[string]WorkReview `json:"reviews,omitempty"`
	// Asks are the latest questions the job's units asked one another,
	// and their answers.
	// +optional
	Asks []WorkAsk `json:"asks,omitempty"`
	// Needs holds, by unit index, what each unit that stopped for
	// information its brief lacked asked for (NeedsInformation).
	// +optional
	Needs map[string]WorkNeeds `json:"needs,omitempty"`
	// Supplied is the token of the last supply carried out
	// (AnnotationSupply).
	// +optional
	Supplied string `json:"supplied,omitempty"`
	// Withdrawn is the token of the last withdrawal carried out
	// (AnnotationWithdrawNeeds).
	// +optional
	Withdrawn string `json:"withdrawn,omitempty"`
	// Requeued is the token of the last requeue request carried out.
	// +optional
	Requeued string `json:"requeued,omitempty"`
	// Acked is the token of the last acknowledgement request carried out
	// (AnnotationAcks).
	// +optional
	Acked string `json:"acked,omitempty"`
	// Acks are the standing acknowledgements: who took, was assigned or
	// accepted which failed units, and when. A requeue clears a unit's.
	// +optional
	Acks []WorkAck `json:"acks,omitempty"`
	// Decided is the token of the last decision carried out
	// (AnnotationDecide).
	// +optional
	Decided string `json:"decided,omitempty"`
	// Waits are the job's waits, open and decided: its wait units, and
	// units a review escalated to a person.
	// +optional
	Waits []WorkWait `json:"waits,omitempty"`
	// Steered is the token of the last message carried out
	// (AnnotationSteer).
	// +optional
	Steered string `json:"steered,omitempty"`
	// Steers are the most recent messages sent to the job's units while
	// they run: delivered, and read.
	// +optional
	Steers []WorkSteer `json:"steers,omitempty"`
	// Rounds lists the job's loops: generators that run as bounded rounds
	// of siblings.
	// +optional
	Rounds []WorkRounds `json:"rounds,omitempty"`
	// Preemptions counts units stopped or asked to yield to make room.
	// +optional
	Preemptions int32 `json:"preemptions,omitempty"`
	// Holds lists each unit holding claims that others wait on, with its
	// waiting line in dispatch order.
	// +optional
	Holds []WorkHold `json:"holds,omitempty"`
	// Gates lists the job's units at a gate: holding it, or in its line.
	// +optional
	Gates []WorkGateEntry `json:"gates,omitempty"`
	// Measure is the latest reading of the job's measure of done.
	// +optional
	Measure *WorkMeasure `json:"measure,omitempty"`
	// Reused is the set of succeeded units that ran nothing: an earlier
	// run of the same computation (process and input views), exact and
	// verified, gave their output.
	// +optional
	Reused string `json:"reused,omitempty"`
	// Spend is what the job's units cost so far, split between
	// procedural and model units (DESIGN.md 10.6).
	// +optional
	Spend *WorkSpend `json:"spend,omitempty"`
	// ModelUsage maps each model unit's index to its token use and cost
	// over all its attempts.
	// +optional
	ModelUsage map[string]WorkUnitUsage `json:"modelUsage,omitempty"`
	// Interruptions lists the most recent attempts the farm stopped (a
	// drain, an eviction, a preemption, a node going away): they spent no
	// retry and run again. Interrupted counts them all.
	// +optional
	Interruptions []WorkInterruption `json:"interruptions,omitempty"`
	// +optional
	Interrupted int32 `json:"interrupted,omitempty"`
	// Causes are the job's failures and flags grouped by cause (an exit
	// signature, a node, a review's reason, information a brief lacked),
	// each with the units it touched and what the farm did, so people and
	// trackers see one item per cause rather than one per failure
	// (DESIGN.md 10.14). At most 16, the newest kept; CausesDropped
	// counts the rest.
	// +optional
	Causes []WorkCause `json:"causes,omitempty"`
	// +optional
	CausesDropped int32 `json:"causesDropped,omitempty"`
	// Started and Finished bound the job's run.
	// +optional
	Started *metav1.Time `json:"started,omitempty"`
	// +optional
	Finished *metav1.Time `json:"finished,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// WorkSuggestion is a suggested deadline: the earliest time the job meets
// at a generous quantile (0.95 by default) without making another job
// late. The farm never moves it earlier; tightening it is a person's
// choice, made with the deadline-options readout. It moves later only
// when it is itself at risk, at most MaxRevisions times, each recorded
// with reason at-risk, and then it is Settled and does not move again; a
// person can always set a deadline.
type WorkSuggestion struct {
	Deadline metav1.Time `json:"deadline"`
	// State is Estimating (it may still move later if at risk) or
	// Settled.
	State string `json:"state"`
	// Policy names the deadline policy kind (achievable, sessions,
	// turnaround) and Quantile the finish percentile it meets.
	Policy   string `json:"policy"`
	Quantile string `json:"quantile"`
	// +optional
	Revisions int32 `json:"revisions,omitempty"`
	// +optional
	MaxRevisions int32 `json:"maxRevisions,omitempty"`
	// History lists every value the suggestion took, oldest first.
	// +optional
	History []WorkSuggestionRevision `json:"history,omitempty"`
	// AtRisk: the job's finish at Quantile is now after the suggestion.
	// +optional
	AtRisk bool `json:"atRisk,omitempty"`
	// NextSession, for a sessions policy whose deadline the job is not
	// expected to make at Quantile, is the following session: the
	// one-click alternative.
	// +optional
	NextSession *metav1.Time `json:"nextSession,omitempty"`
}

// WorkDerivation is what the farm's record says about a job's process
// (its images, command, parameters and, for GPU work, platform): the class
// the job declared, the class the evidence supports, and the evidence.
// Class is a verification mode, never a grade. Experimental.
type WorkDerivation struct {
	// Program is the ID of the program the job's units run: images,
	// command and parameters, without what tells one unit from another
	// (frames, shards). Evidence and class accrue to it.
	Program string `json:"program"`
	// Declared is the class the job declared (annotation
	// c4.avalanche.io/derivation-class): exact, validated or volatile.
	Declared string `json:"declared"`
	// Class is the class the record supports. An exact declaration reads
	// as validated once a re-run wrote a different output, or while its
	// pods are not sealed. Verified: an exact class has been confirmed by
	// re-runs that wrote the same output, which is what allows reusing an
	// output in place of moving it.
	Class    string `json:"class"`
	Verified bool   `json:"verified,omitempty"`
	// Matched and Mismatched count re-runs of the same process on the same
	// inputs that wrote the same output, or another.
	Matched    int32 `json:"matched,omitempty"`
	Mismatched int32 `json:"mismatched,omitempty"`
	// Why says why the class is not the declared one.
	// +optional
	Why string `json:"why,omitempty"`
}

// WorkSuggestionRevision is one value a suggestion took.
type WorkSuggestionRevision struct {
	At       metav1.Time `json:"at"`
	Deadline metav1.Time `json:"deadline"`
	// UnitsMeasured is how many units had finished.
	UnitsMeasured int32 `json:"unitsMeasured"`
	// Reason is first, or at-risk for a later move made because the
	// suggestion was at risk.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Record is the C4 ID of the queue's signed record of it.
	// +optional
	Record string `json:"record,omitempty"`
}

// WorkOutput is a job's output.
type WorkOutput struct {
	// View is the output after the latest fold.
	View string `json:"view"`
	// Chain is the stored c4m chain: the job's input view, then one
	// section per fold, each sealed by the output view it produced.
	Chain string `json:"chain"`
	// Changesets folded so far.
	Changesets int32 `json:"changesets"`
	// Provenance is the C4 ID of the job's provenance index: for each file
	// the job wrote, the signed task record of the unit that wrote it.
	// +optional
	Provenance string `json:"provenance,omitempty"`
	// Record is the C4 ID of the job record the queue signed when the job
	// ended: the output view, chain and index, and the decisions that
	// touched the job.
	// +optional
	Record string `json:"record,omitempty"`
	// Conflicts lists paths two units wrote (the later write is kept).
	// +optional
	Conflicts []string `json:"conflicts,omitempty"`
}

// WorkUnitBranch is a unit's git branch: the repository on the machine
// that ran it, the worktree it worked in (gone once the unit is done), the
// branch, the commit it started from and its last, and how many commits
// it made.
type WorkUnitBranch struct {
	Repo   string `json:"repo"`
	Dir    string `json:"dir,omitempty"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	// +optional
	Head    string `json:"head,omitempty"`
	Commits int32  `json:"commits"`
	// Attempt is the attempt that recorded it.
	// +optional
	Attempt int32 `json:"attempt,omitempty"`
}

// WorkYield is one yield of a checkpointing unit.
type WorkYield struct {
	Unit   int32       `json:"unit"`
	At     metav1.Time `json:"at"`
	Reason string      `json:"reason"`
	// View is what the unit had saved when it stopped.
	View string `json:"view"`
}

// WorkDecision is one scheduling decision that stopped or asked a unit to
// yield.
type WorkDecision struct {
	Unit int32       `json:"unit"`
	At   metav1.Time `json:"at"`
	// Kind is yield (stop at the next checkpoint) or stop.
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	// For is the job the slots went to, if any.
	// +optional
	For string `json:"for,omitempty"`
	// Record is the C4 ID of the queue's signed decision record.
	// +optional
	Record string `json:"record,omitempty"`
	// Policy are the policy values that decided it, and where each came
	// from.
	// +optional
	Policy []WorkPolicyUse `json:"policy,omitempty"`
}

// WorkPolicyUse is a policy value that decided something: its key, its
// value, and where it came from (the farm's default, the job's Project,
// the job).
type WorkPolicyUse struct {
	Key   string         `json:"key"`
	Value string         `json:"value"`
	From  WorkPolicyFrom `json:"from"`
}

// WorkPolicyFrom is where a policy value came from.
type WorkPolicyFrom struct {
	// Scope is farm, project, job or unit.
	Scope string `json:"scope"`
	// +optional
	Ref string `json:"ref,omitempty"`
	// +optional
	SetBy string `json:"setBy,omitempty"`
	// +optional
	Record string `json:"record,omitempty"`
}

// WorkExpansion is one generator's evaluation: its expansion document
// added Units children, indexes First to First+Units-1.
type WorkExpansion struct {
	Unit int32 `json:"unit"`
	// Document is the C4 ID of the expansion document.
	Document string      `json:"document"`
	First    int32       `json:"first"`
	Units    int32       `json:"units"`
	At       metav1.Time `json:"at"`
	// Record is the C4 ID of the queue's signed expansion record.
	// +optional
	Record string `json:"record,omitempty"`
	// Error says why the document was refused; it added no units and the
	// generator failed.
	// +optional
	Error string `json:"error,omitempty"`
}

// WorkSpend is a job's spend: slot time priced at its pools' rates, and
// model tokens at the prices its model units reported. Dollar amounts are
// decimal strings.
type WorkSpend struct {
	// ModelUnits and ProceduralUnits count the units of each kind that
	// have run.
	// +optional
	ModelUnits int32 `json:"modelUnits,omitempty"`
	// +optional
	ProceduralUnits int32 `json:"proceduralUnits,omitempty"`
	// ModelTokens is every token model units used, raw (input, output,
	// cache reads and writes), and UnpricedTokens those no price came
	// with. The classes are counted apart below: they are priced apart.
	// +optional
	ModelTokens int64 `json:"modelTokens,omitempty"`
	// +optional
	UnpricedTokens int64 `json:"unpricedTokens,omitempty"`
	// +optional
	InputTokens int64 `json:"inputTokens,omitempty"`
	// +optional
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// +optional
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
	// +optional
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	// WeightedTokens are the model tokens in input-equivalents (work/tokens:
	// output 5, cache writes 1.25, cache reads 0.1 or the model's rate),
	// and UnpricedWeightedTokens the unpriced ones.
	// +optional
	WeightedTokens int64 `json:"weightedTokens,omitempty"`
	// +optional
	UnpricedWeightedTokens int64 `json:"unpricedWeightedTokens,omitempty"`
	// TokenCost is what the priced tokens cost, as reported;
	// EstimatedTokenCost what the unpriced ones would at their models'
	// published rates. ModelSlotCost and ProceduralSlotCost are the slot
	// time of each kind.
	// +optional
	TokenCost string `json:"tokenCost,omitempty"`
	// +optional
	EstimatedTokenCost string `json:"estimatedTokenCost,omitempty"`
	// +optional
	ModelSlotCost string `json:"modelSlotCost,omitempty"`
	// +optional
	ProceduralSlotCost string `json:"proceduralSlotCost,omitempty"`
	// ModelShare is model units' part of the job's spend, 0 to 1, with
	// unpriced tokens at their estimate. ModelShareBasis says how it was
	// reached: priced (every token came with a price), estimated (some at
	// published rates), or partial (some tokens are of a model with no
	// known price, and are left out).
	// +optional
	ModelShare string `json:"modelShare,omitempty"`
	// +optional
	ModelShareBasis string `json:"modelShareBasis,omitempty"`
	// ModelSlotHours and ProceduralSlotHours are the slot time each kind
	// of unit ran (hours times slots, every attempt), decimal strings, and
	// ModelWorkShare the model units' part of it, 0 to 1: the share of the
	// work models did, whatever it cost (ModelShare is of the spend).
	// +optional
	ModelSlotHours string `json:"modelSlotHours,omitempty"`
	// +optional
	ProceduralSlotHours string `json:"proceduralSlotHours,omitempty"`
	// +optional
	ModelWorkShare string `json:"modelWorkShare,omitempty"`
}

// WorkInputSubstitute is an input the job's units read, at Site, as the
// output of the recompute Recompute: View is the input as the job names
// it, Substitute the view they read instead.
type WorkInputSubstitute struct {
	Mount      string `json:"mount"`
	View       string `json:"view"`
	Substitute string `json:"substitute"`
	Site       string `json:"site"`
	Recompute  string `json:"recompute"`
}

// WorkUnitUsage is one model unit's use of models over its attempts:
// totals, and each model's.
type WorkUnitUsage struct {
	// +optional
	InputTokens int64 `json:"inputTokens,omitempty"`
	// +optional
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// +optional
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
	// +optional
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	// +optional
	UnpricedTokens int64 `json:"unpricedTokens,omitempty"`
	// +optional
	WeightedTokens int64 `json:"weightedTokens,omitempty"`
	// +optional
	UnpricedWeightedTokens int64 `json:"unpricedWeightedTokens,omitempty"`
	// EstimatedCost is the unpriced tokens at published rates.
	// +optional
	EstimatedCost string `json:"estimatedCost,omitempty"`
	// Cost of the priced tokens, and SlotCost of the unit's slot time,
	// decimal strings in US dollars.
	// +optional
	Cost string `json:"cost,omitempty"`
	// +optional
	SlotCost string `json:"slotCost,omitempty"`
	// SlotHours is the unit's slot time over its attempts, a decimal
	// string.
	// +optional
	SlotHours string `json:"slotHours,omitempty"`
	// CostSource says where the price came from (a metering proxy, a
	// price table).
	// +optional
	CostSource string `json:"costSource,omitempty"`
	// +optional
	Models []WorkModelTokens `json:"models,omitempty"`
	// Reasoning is what the unit's latest attempt reasoned with, as its
	// runner observed it (c4-unit usage, c4-agent's Stop hook).
	// +optional
	Reasoning *WorkReasoning `json:"reasoning,omitempty"`
	// UsageError says why an attempt's usage document was refused (an
	// unknown field, say): its tokens are not counted.
	// +optional
	UsageError string `json:"usageError,omitempty"`
}

// WorkReasoning is what an attempt reasoned with: the model (and every
// model it used), the effort level in force at its end (and every level
// it ran at), its thinking mode, its tool's name, the tools it was
// offered and the MCP servers they came from.
type WorkReasoning struct {
	// +optional
	Does string `json:"does,omitempty"`
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	Models []string `json:"models,omitempty"`
	// +optional
	Effort string `json:"effort,omitempty"`
	// +optional
	Efforts []string `json:"efforts,omitempty"`
	// +optional
	Thinking string `json:"thinking,omitempty"`
	// +optional
	Tools string `json:"tools,omitempty"`
	// +optional
	ToolNames []string `json:"toolNames,omitempty"`
	// +optional
	MCPServers []string `json:"mcpServers,omitempty"`
}

// WorkModelTokens is one model's tokens in a unit.
type WorkModelTokens struct {
	Model string `json:"model"`
	// +optional
	InputTokens int64 `json:"inputTokens,omitempty"`
	// +optional
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// +optional
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
	// +optional
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	// +optional
	UnpricedTokens int64 `json:"unpricedTokens,omitempty"`
	// +optional
	WeightedTokens int64 `json:"weightedTokens,omitempty"`
	// +optional
	UnpricedWeightedTokens int64 `json:"unpricedWeightedTokens,omitempty"`
	// EstimatedCost is its unpriced tokens at its published rates;
	// Unpriceable says the model's rates are not known.
	// +optional
	EstimatedCost string `json:"estimatedCost,omitempty"`
	// +optional
	Unpriceable bool `json:"unpriceable,omitempty"`
	// Cost of its priced tokens; absent when none were priced.
	// +optional
	Cost string `json:"cost,omitempty"`
}

// WorkProceduralCandidate is a model unit that behaved like a function of
// its inputs.
type WorkProceduralCandidate struct {
	Unit int32  `json:"unit"`
	Name string `json:"name,omitempty"`
	// Runs of the same computation (process and input views), all of
	// which wrote one output.
	Runs int32  `json:"runs"`
	Why  string `json:"why"`
}

// WorkMeasureSpec declares a job's measure of done.
type WorkMeasureSpec struct {
	// Name is what is counted ("rules sited").
	Name string `json:"name"`
	// Of is the target; a reading may carry its own.
	// +optional
	Of *int64 `json:"of,omitempty"`
	// Required: the job is done only when the measure reaches its target.
	// With its units finished short of it, the job needs a person.
	// +optional
	Required bool `json:"required,omitempty"`
}

// WorkMeasure is the latest reading of a job's measure.
type WorkMeasure struct {
	Name  string `json:"name,omitempty"`
	Value int64  `json:"value"`
	// +optional
	Of int64 `json:"of,omitempty"`
	// Done: the value reached the target.
	Done bool `json:"done"`
	// At is when the reading was taken (absent before the first).
	// +optional
	At *metav1.Time `json:"at,omitempty"`
	// Unit is the unit that reported it, and Document the reading's C4 ID.
	Unit     int32  `json:"unit"`
	Document string `json:"document,omitempty"`
	// +optional
	Detail string `json:"detail,omitempty"`
}

// WorkGateEntry is a unit at a gate: the holder (position 0) or a unit
// waiting in line (1 is next).
type WorkGateEntry struct {
	Gate     string `json:"gate"`
	Unit     int32  `json:"unit"`
	Name     string `json:"name,omitempty"`
	Position int32  `json:"position"`
	// Since is when the unit joined the line.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
	// Holder is who holds the gate now, as job#unit.
	// +optional
	Holder string `json:"holder,omitempty"`
	// ExpectStart is when the unit is expected to pass.
	// +optional
	ExpectStart *metav1.Time `json:"expectStart,omitempty"`
}

// WorkHold is a unit holding claims, and the units locked behind it.
type WorkHold struct {
	Holder int32  `json:"holder"`
	Name   string `json:"name,omitempty"`
	// On are the holder's claims.
	On      []string         `json:"on,omitempty"`
	Waiting []WorkHoldWaiter `json:"waiting,omitempty"`
}

// WorkHoldWaiter is a unit locked behind a holder, and its claim.
type WorkHoldWaiter struct {
	Unit int32  `json:"unit"`
	Name string `json:"name,omitempty"`
	On   string `json:"on,omitempty"`
}

// WorkCause is one cause of a job's failures or flags.
type WorkCause struct {
	// Kind is exit (a unit's process failed), node (a node failed units
	// up to the job's limit), verdict (a review reworked, escalated or
	// failed units) or needs (units need information their brief lacked).
	Kind string `json:"kind"`
	// Key names the cause within the job; it stays the same as the cause
	// grows.
	Key string `json:"key"`
	// Title says it in production's terms ("exit 137 OOMKilled").
	Title string `json:"title"`
	// Units is the set of units it touched ("0-9,14"); Count its attempts;
	// Sample one of them.
	Units  string      `json:"units"`
	Count  int32       `json:"count"`
	Sample int32       `json:"sample"`
	First  metav1.Time `json:"first"`
	Last   metav1.Time `json:"last"`
	// Nodes it happened on, at most four.
	// +optional
	Nodes []string `json:"nodes,omitempty"`
	// Did is what the farm did about it: retried, failed, blacklisted,
	// reworked, escalated, accepted (each once, in order).
	// +optional
	Did []string `json:"did,omitempty"`
	// To is whom a review escalated it to.
	// +optional
	To string `json:"to,omitempty"`
}

// WorkInterruption is one attempt the farm stopped.
type WorkInterruption struct {
	Unit int32       `json:"unit"`
	At   metav1.Time `json:"at"`
	// Reason is the pod's DisruptionTarget reason (PreemptionByScheduler,
	// EvictionByEvictionAPI, DeletionByTaintManager, TerminationByKubelet,
	// DeletionByPodGC), or Deleted.
	Reason string `json:"reason"`
	// +optional
	Node string `json:"node,omitempty"`
}

// WorkVerdict is one verdict a review unit wrote, carried out once for
// that unit and attempt.
type WorkVerdict struct {
	Unit    int32 `json:"unit"`
	Attempt int32 `json:"attempt"`
	// Pod is the review pod's UID: a pod's verdict is carried out once.
	// +optional
	Pod string `json:"pod,omitempty"`
	// Document is the C4 ID of the verdict document.
	Document string `json:"document"`
	// Verdict is the document's summary word: pass, rework, mixed,
	// escalate, flaky...
	// +optional
	Verdict string      `json:"verdict,omitempty"`
	At      metav1.Time `json:"at"`
	// Counts of the judged units by what was done.
	// +optional
	Passed int32 `json:"passed,omitempty"`
	// +optional
	Reworked int32 `json:"reworked,omitempty"`
	// +optional
	Escalated int32 `json:"escalated,omitempty"`
	// +optional
	Accepted int32 `json:"accepted,omitempty"`
	// +optional
	Expanded int32 `json:"expanded,omitempty"`
	// +optional
	Superseded int32 `json:"superseded,omitempty"`
	// Select is the verdict's selection rule over the scores it judged:
	// "top 2", "threshold 0.8", "ucb 1.4 top 1".
	// +optional
	Select string `json:"select,omitempty"`
	// Record is the C4 ID of the queue's signed verdict record.
	// +optional
	Record string `json:"record,omitempty"`
	// Error says why the verdict was refused: nothing was done, and the
	// review unit failed.
	// +optional
	Error string `json:"error,omitempty"`
}

// WorkReview is what reviews last decided for one unit.
type WorkReview struct {
	// Do is pass, rework, escalate, accept, fail or supersede.
	Do string `json:"do"`
	// +optional
	Why string `json:"why,omitempty"`
	// Note is an accepted unit's note.
	// +optional
	Note string `json:"note,omitempty"`
	// To is who an escalated unit waits for.
	// +optional
	To string `json:"to,omitempty"`
	// By is the review unit's index.
	By int32 `json:"by"`
	// Passes counts the unit's reworks.
	// +optional
	Passes int32 `json:"passes,omitempty"`
	// With is the rework's change to the unit's next attempts.
	// +optional
	With *WorkRework `json:"with,omitempty"`
	// Score is the unit's score (a decimal) from the last verdict that
	// scored it, and Scored how many verdicts have.
	// +optional
	Score string `json:"score,omitempty"`
	// +optional
	Scored int32 `json:"scored,omitempty"`
}

// WorkRework changes a reworked unit's next attempts: what makes a rework
// more than a retry.
type WorkRework struct {
	// +optional
	AvoidNodes []string `json:"avoidNodes,omitempty"`
	// Pool is the only pool the unit may run in.
	// +optional
	Pool string `json:"pool,omitempty"`
	// Memory is the first container's memory request and limit.
	// +optional
	Memory string `json:"memory,omitempty"`
	// Image replaces the first container's image.
	// +optional
	Image string `json:"image,omitempty"`
	// +optional
	Env map[string]string `json:"env,omitempty"`
	// Inputs are views mounted read-only (a counterexample, a note).
	// +optional
	Inputs []WorkReworkInput `json:"inputs,omitempty"`
	// Fresh starts from the job's inputs, not from the unit's last output.
	// +optional
	Fresh bool `json:"fresh,omitempty"`
}

// WorkReworkInput is a view a reworked unit mounts.
type WorkReworkInput struct {
	// +kubebuilder:validation:Pattern=`^c4[1-9A-HJ-NP-Za-km-z]{88}$`
	View      string `json:"view"`
	MountPath string `json:"mountPath"`
}

// UnitCounts counts a job's units by state.
type UnitCounts struct {
	Total int32 `json:"total,omitempty"`
	// Planned counts generators not yet evaluated (DESIGN 10.6).
	// +optional
	Planned int32 `json:"planned,omitempty"`
	// Flagged counts units a review escalated to a person.
	// +optional
	Flagged int32 `json:"flagged,omitempty"`
	// Locked counts units waiting for a unit of the job that holds what
	// they claim.
	// +optional
	Locked int32 `json:"locked,omitempty"`
	// Reused counts succeeded units whose output an earlier run gave
	// (also counted in Succeeded).
	// +optional
	Reused int32 `json:"reused,omitempty"`
	// Waits counts wait units waiting for their decision.
	Waits     int32 `json:"waits,omitempty"`
	Queued    int32 `json:"queued,omitempty"`
	Blocked   int32 `json:"blocked,omitempty"`
	Preflight int32 `json:"preflight,omitempty"`
	Running   int32 `json:"running,omitempty"`
	Retrying  int32 `json:"retrying,omitempty"`
	Backoff   int32 `json:"backoff,omitempty"`
	Egress    int32 `json:"egress,omitempty"`
	Succeeded int32 `json:"succeeded,omitempty"`
	Failed    int32 `json:"failed,omitempty"`
	Accepted  int32 `json:"accepted,omitempty"`
	Held      int32 `json:"held,omitempty"`
	// Superseded units ended because a review chose others: done, not
	// accepted, and out of the job's output.
	Superseded int32 `json:"superseded,omitempty"`
	// Needs counts units stopped on a question nobody has answered or
	// withdrawn yet: not failed, the job held until it heals.
	// +optional
	Needs int32 `json:"needs,omitempty"`
}

// Ack kinds.
const (
	AckAcknowledge = "acknowledge"
	AckAssign      = "assign"
	AckAccept      = "accept"
)

// Deadline risk (WorkEstimate.Risk).
const (
	RiskNone     = "none"
	RiskAt       = "atRisk"
	RiskWillMiss = "willMiss"
	RiskMissed   = "missed"
)

// ConditionNeedsPerson is True while the job waits on a person. Its
// LastTransitionTime is since when; its reason is one of the
// NeedsPerson reasons.
const ConditionNeedsPerson = "NeedsPerson"

// NeedsPerson reasons.
const (
	// NeedsFailed: units failed after their retries and nobody has
	// acknowledged or accepted them.
	NeedsFailed = "Failed"
	// NeedsStuck: the job can never run as it stands (no plan, no pool
	// that fits a unit, a dependency that failed).
	NeedsStuck = "Stuck"
	// NeedsApproval: capacity that would restore the deadline waits for
	// a person's approval.
	NeedsApproval = "Approval"
	// NeedsReview: a review unit escalated units to a person (they are
	// flagged): requeue or accept them.
	NeedsReview = "Review"
	// NeedsMeasure: the job's units finished short of its required
	// measure.
	NeedsMeasure = "Measure"
	// NeedsDecision: a wait for a person (a ruling, an approval) holds up
	// work that could otherwise run: decide it.
	NeedsDecision = "Decision"
	// NeedsQuota and NeedsPermission are reserved for a project's quota
	// and an access block.
	NeedsQuota      = "Quota"
	NeedsPermission = "Permission"
)

// WorkAckRequest is one request in AnnotationAcks.
type WorkAckRequest struct {
	// Token makes each request distinct; requests are carried out in
	// token order (base-36 nanoseconds).
	Token string `json:"token"`
	// Kind is acknowledge (I have it), assign (Assignee has it) or accept
	// (the failure stands: accept the units as they are).
	Kind  string `json:"kind"`
	Units string `json:"units"`
	Actor string `json:"actor"`
	// +optional
	Assignee string `json:"assignee,omitempty"`
	// +optional
	Note string      `json:"note,omitempty"`
	At   metav1.Time `json:"at"`
	// Record is the C4 ID of the signed control record.
	// +optional
	Record string `json:"record,omitempty"`
}

// WorkDecideRequest is one decision in AnnotationDecide.
type WorkDecideRequest struct {
	// Token makes each request distinct; requests are carried out in
	// token order (base-36 nanoseconds).
	Token string `json:"token"`
	// Unit is the unit decided, by name or index.
	Unit string `json:"unit"`
	// Do is the decision. On a wait unit: anything but fail completes it
	// (approve, or one of its choices); fail fails it. On a unit a review
	// escalated: accept (it stands), rework (it runs again) or fail.
	Do string `json:"do"`
	// +optional
	Note  string      `json:"note,omitempty"`
	Actor string      `json:"actor"`
	At    metav1.Time `json:"at"`
	// Record is the C4 ID of the signed decision record.
	// +optional
	Record string `json:"record,omitempty"`
	// View is the decision's reply, a view's C4 ID, for a wait that asks
	// for one: the wait unit's output, which the units it holds up mount.
	// +optional
	View string `json:"view,omitempty"`
}

// NeedsInformation is the reason of a unit attempt that stopped because
// its brief lacked information only a person could give: a work-stopping
// error, not a question (DESIGN.md 10.6). Its container exits
// NeedsExitCode with the missing information as its termination message
// (c4-unit needs), or a host runner sets the pod's status reason. The
// unit fails without a retry, and runs again once the information is
// supplied (AnnotationSupply).
const (
	NeedsInformation = "NeedsInformation"
	NeedsExitCode    = 12
)

// Withdrawn is the reason of a re-check attempt (AnnotationRecheck) whose
// block withdrew its question, with the why as the pod's message (a host
// runner). A container says it by exiting 0 with WithdrawnPrefix and the
// why as its termination message (c4-unit withdraw --why). Nothing else
// withdraws a question: a re-check that ends otherwise keeps it.
const (
	Withdrawn       = "Withdrawn"
	WithdrawnPrefix = "withdrawn: "
)

// WorkAsk is a question one unit asked another (c4-unit ask), or a wait
// unit asked the unit that serves it (Wait).
type WorkAsk struct {
	// ID is <asker unit>#<n>, or w<wait unit>.
	ID   string `json:"id"`
	From int32  `json:"from"`
	// To is the answering unit; -1 when none could.
	To   int32       `json:"to"`
	Text string      `json:"text"`
	At   metav1.Time `json:"at"`
	// Answer, or Cannot and why, once answered.
	// +optional
	Answer string `json:"answer,omitempty"`
	// +optional
	Cannot string `json:"cannot,omitempty"`
	// +optional
	Answered *metav1.Time `json:"answered,omitempty"`
	// +optional
	Wait bool `json:"wait,omitempty"`
	// Needs: the question a unit stopped for (NeedsInformation), put to
	// the job's answerer; its answer supplies it.
	// +optional
	Needs bool `json:"needs,omitempty"`
}

// WorkNeeds is what a unit stopped for.
type WorkNeeds struct {
	// Text is the information it needs, and Why why it needs it.
	Text string `json:"text"`
	// +optional
	Why string      `json:"why,omitempty"`
	At  metav1.Time `json:"at"`
	// Record is the C4 ID of the signed record.
	// +optional
	Record string `json:"record,omitempty"`
	// Supplied is the information a person (or the job's answerer)
	// supplied, By who and SuppliedAt when; the unit then runs again.
	// +optional
	Supplied string `json:"supplied,omitempty"`
	// +optional
	By string `json:"by,omitempty"`
	// +optional
	SuppliedAt *metav1.Time `json:"suppliedAt,omitempty"`
	// +optional
	SuppliedRecord string `json:"suppliedRecord,omitempty"`
	// State is raised (the unit stopped; the rest of the job runs on),
	// pressing (the same, now urgent), supplied or withdrawn.
	// +kubebuilder:validation:Enum=raised;pressing;supplied;withdrawn
	// +optional
	State string `json:"state,omitempty"`
	// AnswerBy is the latest time an answer can come with the job still on
	// time: its effective deadline less the work that waits on the unit.
	// +optional
	AnswerBy *metav1.Time `json:"answerBy,omitempty"`
	// PressingAt is when it turned pressing, and PressingWhy why, in
	// words.
	// +optional
	PressingAt *metav1.Time `json:"pressingAt,omitempty"`
	// +optional
	PressingWhy string `json:"pressingWhy,omitempty"`
	// PressingRecord is the signed decision that made it pressing.
	// +optional
	PressingRecord string `json:"pressingRecord,omitempty"`
	// About is what the question is about, so work landing there re-checks
	// it: path:<prefix> in the job's tree, unit:<name>, job:<ns>/<name>.
	// +optional
	About []string `json:"about,omitempty"`
	// Rechecks are the block's re-checks of its question.
	// +optional
	Rechecks *WorkRechecks `json:"rechecks,omitempty"`
	// Withdrawn says who withdrew the question (the block on a re-check,
	// the lead) and why; the unit runs again without an answer.
	// +optional
	Withdrawn *WorkWithdrawal `json:"withdrawn,omitempty"`
	// Seen is what its re-check triggers last saw, so a restart misses no
	// event: the unit table's size, the outputs of the units it waits on,
	// reads or is about (by index; "ok" a unit succeeded whose output is
	// not known), and the phases of the jobs it is about or reads from.
	// +optional
	Seen *WorkNeedsSeen `json:"seen,omitempty"`
}

// WorkNeedsSeen is what a question's re-check triggers last saw.
type WorkNeedsSeen struct {
	Units int32 `json:"units"`
	// +optional
	Outputs map[string]string `json:"outputs,omitempty"`
	// +optional
	Phases map[string]string `json:"phases,omitempty"`
}

// WorkRechecks are a block's re-checks of its question: how many ran, the
// most allowed, the last, and whether one is running.
type WorkRechecks struct {
	Done int32 `json:"done"`
	Max  int32 `json:"max"`
	// +optional
	LastAt *metav1.Time `json:"lastAt,omitempty"`
	// LastWhy is what started the last one.
	// +optional
	LastWhy string `json:"lastWhy,omitempty"`
	// +optional
	InFlight bool `json:"inFlight,omitempty"`
}

// WorkWithdrawal is a question withdrawn.
type WorkWithdrawal struct {
	By  string      `json:"by"`
	Why string      `json:"why"`
	At  metav1.Time `json:"at"`
	// +optional
	Record string `json:"record,omitempty"`
}

// WorkWithdrawRequest is the lead's withdrawal of a unit's question.
type WorkWithdrawRequest struct {
	Token string      `json:"token"`
	Unit  string      `json:"unit"`
	Why   string      `json:"why"`
	Actor string      `json:"actor"`
	At    metav1.Time `json:"at"`
	// +optional
	Record string `json:"record,omitempty"`
}

// WorkSupplyRequest supplies the information a unit stopped for.
type WorkSupplyRequest struct {
	// Token makes each request distinct; requests are carried out in
	// token order (base-36 nanoseconds).
	Token string      `json:"token"`
	Unit  string      `json:"unit"`
	Text  string      `json:"text"`
	Actor string      `json:"actor"`
	At    metav1.Time `json:"at"`
	// +optional
	Record string `json:"record,omitempty"`
}

// MaxSteerText bounds a message to a running unit, in bytes.
const MaxSteerText = 8192

// WorkSteerRequest is a message to a unit while it runs.
type WorkSteerRequest struct {
	// Token makes each request distinct; requests are carried out in
	// token order (base-36 nanoseconds).
	Token string `json:"token"`
	// Unit is the unit, by name or index.
	Unit  string      `json:"unit"`
	Text  string      `json:"text"`
	Actor string      `json:"actor"`
	At    metav1.Time `json:"at"`
	// Record is the C4 ID of the signed control record.
	// +optional
	Record string `json:"record,omitempty"`
}

// WorkRequeueRequest asks the queue to run units again
// (AnnotationRequeue, a JSON list of them).
type WorkRequeueRequest struct {
	// Token makes each request distinct; requests are carried out in
	// token order (base-36 nanoseconds).
	Token string `json:"token"`
	// Units is a run-length index set.
	Units string      `json:"units"`
	Actor string      `json:"actor"`
	At    metav1.Time `json:"at"`
	// Record is the C4 ID of the signed control record.
	// +optional
	Record string `json:"record,omitempty"`
}

// WorkAllowNodeRequest takes a node off the job's automatic blacklist
// (AnnotationAllowNodes, a JSON list of them).
type WorkAllowNodeRequest struct {
	Token string      `json:"token"`
	Node  string      `json:"node"`
	Actor string      `json:"actor"`
	At    metav1.Time `json:"at"`
	// +optional
	Record string `json:"record,omitempty"`
}

// WorkAcceptValidatedRequest accepts (or no longer accepts) a validated
// recompute of the job's inputs (AnnotationAcceptValidated, a JSON list
// of them; the newest the queue trusts holds).
type WorkAcceptValidatedRequest struct {
	Token  string      `json:"token"`
	Accept bool        `json:"accept"`
	Actor  string      `json:"actor"`
	At     metav1.Time `json:"at"`
	// +optional
	Record string `json:"record,omitempty"`
}

// WorkSteer is a message sent to a unit while it runs.
type WorkSteer struct {
	Unit int32 `json:"unit"`
	// +optional
	Name string `json:"name,omitempty"`
	// Seq numbers the unit's messages from 1.
	Seq   int32       `json:"seq"`
	Actor string      `json:"actor"`
	At    metav1.Time `json:"at"`
	Text  string      `json:"text"`
	// +optional
	Record string `json:"record,omitempty"`
	// Ask is the id of the question this message puts to the unit, and
	// Answer that of the question it answers (WorkAsk).
	// +optional
	Ask string `json:"ask,omitempty"`
	// +optional
	Answer string `json:"answer,omitempty"`
	// Delivered is when it reached a pod of the unit; Seen says the unit
	// read it (c4-unit steer).
	// +optional
	Delivered *metav1.Time `json:"delivered,omitempty"`
	// +optional
	Seen bool `json:"seen,omitempty"`
}

// WorkRounds is one of a job's loops: a generator run as bounded rounds of
// siblings.
type WorkRounds struct {
	// Unit is the loop's first generator, and Name its name: round k's
	// units are named r<k>/<unit name>.
	Unit int32  `json:"unit"`
	Name string `json:"name,omitempty"`
	// Round is the round running or last run, Max the bound, and Until
	// the review unit whose pass ends the loop.
	Round int32 `json:"round"`
	Max   int32 `json:"max"`
	// +optional
	Until string `json:"until,omitempty"`
	// StoppedBy says why the loop ended: until (its review passed),
	// generator (it wrote no next round), max, or failed; empty while it
	// goes on.
	// +optional
	StoppedBy string `json:"stoppedBy,omitempty"`
	// Generators are each round's generator, in round order.
	// +optional
	Generators []int32 `json:"generators,omitempty"`
	// ExpectMore is how many more rounds the queue expects, the pending
	// one included (a decimal), and ExpectSeconds their duration on the
	// critical path.
	// +optional
	ExpectMore string `json:"expectMore,omitempty"`
	// +optional
	ExpectSeconds int64 `json:"expectSeconds,omitempty"`
}

// WorkForecast is how much work a forecast job is expected to be.
type WorkForecast struct {
	// SlotHours is its expected work; empty, the class plans it from the
	// job's params as it would the real job (frames, and the class's
	// prior).
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +optional
	SlotHours string `json:"slotHours,omitempty"`
	// SD is the spread of SlotHours (default half of it).
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +optional
	SD string `json:"sd,omitempty"`
	// UnitHours is how long one unit runs. With SlotHours it is the
	// longest unit, the critical path (default the class's recent unit
	// time in the project, else an hour; at most SlotHours); with the
	// class's plan, every unit's prior.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +optional
	UnitHours string `json:"unitHours,omitempty"`
	// Start is the earliest it can begin (a turnover date): it is planned
	// to finish no earlier than Start and its own duration.
	// +optional
	Start *metav1.Time `json:"start,omitempty"`
	// Exists is the chance it happens at all, 0 to 1 (default 1): it
	// weights the expected work.
	// +kubebuilder:validation:Pattern=`^(0(\.[0-9]+)?|1(\.0+)?)$`
	// +optional
	Exists string `json:"exists,omitempty"`
}

// WorkReviewSpec is the final review of a job's work.
type WorkReviewSpec struct {
	// Server is who reviews: person:<id> or party:<name>.
	// +kubebuilder:validation:Pattern=`^(person|party):.+$`
	Server string `json:"server"`
	// Ask is what the reviewer is asked.
	// +optional
	Ask string `json:"ask,omitempty"`
	// Choices are the decisions it takes besides fail; empty, approve.
	// +optional
	Choices []string `json:"choices,omitempty"`
	// ExpectSeconds is how long the review is expected to take, until the
	// farm has measured the reviewer's decisions.
	// +optional
	ExpectSeconds int64 `json:"expectSeconds,omitempty"`
}

// WorkWait is a unit that waits for someone or something: a person, an
// outside party, or a clock. It holds no slot.
type WorkWait struct {
	Unit int32 `json:"unit"`
	// +optional
	Name string `json:"name,omitempty"`
	// Server is who or what it waits for: person:<id>, party:<name> or
	// clock:<time>.
	Server string `json:"server"`
	// +optional
	Ask string `json:"ask,omitempty"`
	// Choices are the decisions that complete it, besides fail (none: any).
	// +optional
	Choices []string `json:"choices,omitempty"`
	// Escalated: the unit is one a review escalated, waiting for a
	// person's decision on it.
	// +optional
	Escalated bool `json:"escalated,omitempty"`
	// Since is when it opened: every unit it runs after was done.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
	// ExpectSeconds is how long it is expected to wait in all, from the
	// server's measured decisions in this job, else the unit's own figure.
	// +optional
	ExpectSeconds int64 `json:"expectSeconds,omitempty"`
	// Blocking: unfinished work waits on it.
	// +optional
	Blocking bool `json:"blocking,omitempty"`
	// The decision, once made.
	// +optional
	Do string `json:"do,omitempty"`
	// +optional
	Note string `json:"note,omitempty"`
	// +optional
	Actor string `json:"actor,omitempty"`
	// +optional
	At *metav1.Time `json:"at,omitempty"`
	// +optional
	Record string `json:"record,omitempty"`
	// Reply says the decision carries content; View is the reply, once
	// decided.
	// +optional
	Reply bool `json:"reply,omitempty"`
	// Block says the unit opened the wait for itself at a safe point
	// (c4-unit wait): it stopped there, and runs again, resuming, once
	// the wait is decided. Safepoint is the safe point's number.
	// +optional
	Block bool `json:"block,omitempty"`
	// +optional
	Safepoint int32 `json:"safepoint,omitempty"`
	// +optional
	View string `json:"view,omitempty"`
}

// WorkAck is a standing acknowledgement of failed units.
type WorkAck struct {
	Kind  string `json:"kind"`
	Units string `json:"units"`
	Actor string `json:"actor"`
	// +optional
	Assignee string `json:"assignee,omitempty"`
	// +optional
	Note string      `json:"note,omitempty"`
	At   metav1.Time `json:"at"`
	// +optional
	Record string `json:"record,omitempty"`
}

// WorkEstimate is the estimate and the objective state. Durations are
// seconds; slots are fractional.
type WorkEstimate struct {
	// RemainingWork is the estimated work left (W) and its 90th percentile
	// (W90), in slot-seconds: the estimator's answer, before allocation.
	RemainingWork    int64 `json:"remainingWork,omitempty"`
	RemainingWorkP90 int64 `json:"remainingWorkP90,omitempty"`
	// RemainingSeconds is wall-clock time to the ETA (p50): the plan's
	// answer, after allocation.
	RemainingSeconds    int64  `json:"remainingSeconds,omitempty"`
	P90RemainingSeconds int64  `json:"p90RemainingSeconds,omitempty"`
	RequiredSlots       string `json:"requiredSlots,omitempty"`
	AttainableSlots     string `json:"attainableSlots,omitempty"`
	RunningSlots        int32  `json:"runningSlots,omitempty"`
	SlackSeconds        int64  `json:"slackSeconds,omitempty"`
	Feasible            bool   `json:"feasible,omitempty"`
	AtRisk              bool   `json:"atRisk,omitempty"`
	// Risk is the deadline risk a person is shown: none, atRisk (the
	// band at the job's quantile, 0.95 for a suggested deadline and 0.9
	// otherwise, ends after the deadline, or the scheduler's conservative
	// finish does), willMiss (late even with every slot it can get, or
	// the median finish is after the deadline) or missed (the deadline
	// has passed and the job is not finished).
	// +optional
	Risk string `json:"risk,omitempty"`
	// ETA50 and ETA90 are the finish a person is shown, at 50 and 90%:
	// the plan's finish with the calibrated percentiles of the remaining
	// work, and half the critical path as the last wave's tail (the
	// scheduler's own, more conservative finish is what it decides on).
	// +optional
	ETA50 *metav1.Time `json:"eta50,omitempty"`
	// +optional
	ETA90 *metav1.Time `json:"eta90,omitempty"`
	// ETA80 and ETA95 complete the band.
	// +optional
	ETA80 *metav1.Time `json:"eta80,omitempty"`
	// +optional
	ETA95 *metav1.Time `json:"eta95,omitempty"`
	// Calibrated says the percentiles come from finished jobs' outcomes
	// (live calibration), not from the estimator's own spread.
	// +optional
	Calibrated bool `json:"calibrated,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// HeavyRegions are axis ranges well above the job's median cost.
	// +optional
	HeavyRegions []HeavyRegion `json:"heavyRegions,omitempty"`
}

// HeavyRegion is a range of the unit axis that costs more than the median.
type HeavyRegion struct {
	From   int32  `json:"from"`
	To     int32  `json:"to"`
	Factor string `json:"factor"`
}

// WorkJob is a job in the farm's queue.
// +kubebuilder:object:root=true
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 50",message="a WorkJob name is at most 50 characters (it labels and prefixes its unit pods)"
// +kubebuilder:validation:XValidation:rule="has(self.spec.review) == has(oldSelf.spec.review) && (!has(self.spec.review) || self.spec.review == oldSelf.spec.review)",message="a job's review is set when it is created"
// +kubebuilder:validation:XValidation:rule="has(self.spec.forecast) == has(oldSelf.spec.forecast)",message="a job is a forecast, or not, from when it is created: a real job replaces a forecast"
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wj
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.class`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Done",type=integer,JSONPath=`.status.units.succeeded`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.units.total`
// +kubebuilder:printcolumn:name="Slack",type=integer,JSONPath=`.status.estimate.slackSeconds`
// +kubebuilder:printcolumn:name="Rank",type=integer,JSONPath=`.status.rank`
type WorkJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkJobSpec   `json:"spec,omitempty"`
	Status WorkJobStatus `json:"status,omitempty"`
}

// WorkJobList is a list of WorkJobs.
// +kubebuilder:object:root=true
type WorkJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkJob `json:"items"`
}

// PoolSpec maps a pool to nodes.
type PoolSpec struct {
	// NodeSelector picks the pool's nodes; it is also added to every unit
	// pod released into the pool.
	NodeSelector map[string]string `json:"nodeSelector"`
	// Tolerations are added to unit pods released into the pool.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// SlotsPerNode is each node's capacity in slots.
	// +kubebuilder:validation:Minimum=1
	SlotsPerNode int32 `json:"slotsPerNode"`
	// Attributes describe the pool to jobs and to the HUD: class (cpu,
	// gpu), gpuModel, vram, capacityType (on-demand, spot).
	// +optional
	Attributes map[string]string `json:"attributes,omitempty"`
	// CostPerSlotHour in US dollars, as a decimal string.
	// +optional
	CostPerSlotHour string `json:"costPerSlotHour,omitempty"`
	// Burst makes the pool grow on request.
	// +optional
	Burst *PoolBurst `json:"burst,omitempty"`
	// Owners are the users whose jobs the pool runs, by each job's owner
	// (c4.avalanche.io/owner, fixed when it is created): a host runner on a
	// person's own machine runs only their work. Empty, any job may use it.
	// +optional
	Owners []string `json:"owners,omitempty"`
}

// PoolBurst describes on-request capacity.
type PoolBurst struct {
	// MaxNodes the pool may grow to.
	MaxNodes int32 `json:"maxNodes"`
	// ProvisioningSeconds is how long a new node takes to be ready.
	ProvisioningSeconds int32 `json:"provisioningSeconds"`
	// CostPerSlotHour of burst capacity, in US dollars.
	CostPerSlotHour string `json:"costPerSlotHour"`
	// Provider fulfils CapacityRequests: kind-sim (taint toggling on a
	// kind cluster), provisioning-request (Cluster Autoscaler), or
	// karpenter.
	// +kubebuilder:validation:Enum=kind-sim;provisioning-request;karpenter
	Provider string `json:"provider"`
}

// PoolStatus reports the pool's size.
type PoolStatus struct {
	// Nodes ready in the pool, and their slots.
	Nodes int32 `json:"nodes,omitempty"`
	Slots int32 `json:"slots,omitempty"`
	// Used slots by running units.
	Used int32 `json:"used,omitempty"`
	// Provisioned burst nodes, and how many are requested.
	// +optional
	Provisioned int32 `json:"provisioned,omitempty"`
	// +optional
	Requested int32 `json:"requested,omitempty"`
}

// Pool is a named set of nodes the queue releases units into.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.nodes`
// +kubebuilder:printcolumn:name="Slots",type=integer,JSONPath=`.status.slots`
// +kubebuilder:printcolumn:name="Used",type=integer,JSONPath=`.status.used`
type Pool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PoolSpec   `json:"spec,omitempty"`
	Status PoolStatus `json:"status,omitempty"`
}

// PoolList is a list of Pools.
// +kubebuilder:object:root=true
type PoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Pool `json:"items"`
}

// CapacityRequestSpec is the scheduler's request for burst capacity in one
// pool: the explicit interface between the queue and whatever provisions
// nodes.
type CapacityRequestSpec struct {
	Pool string `json:"pool"`
	// Slots wanted from the pool's burst capacity, in total.
	Slots int32 `json:"slots"`
	// Until is when the capacity is expected to be needed to.
	// +optional
	Until *metav1.Time `json:"until,omitempty"`
	// Reason in the farm's terms, and the jobs it is for.
	Reason string `json:"reason"`
	// +optional
	Jobs []string `json:"jobs,omitempty"`
	// EstimatedCost in US dollars.
	// +optional
	EstimatedCost string `json:"estimatedCost,omitempty"`
	// Approved is set by a person for requests over budget, or by the
	// scheduler for automatic ones.
	// +optional
	Approved bool `json:"approved,omitempty"`
	// PrewarmViews are views new nodes fetch before units land on them.
	// +optional
	PrewarmViews []string `json:"prewarmViews,omitempty"`
}

// CapacityRequestStatus reports fulfilment.
type CapacityRequestStatus struct {
	// State is PendingApproval, Provisioning or Provisioned.
	// +optional
	State string `json:"state,omitempty"`
	// ProvisionedSlots are ready.
	// +optional
	ProvisionedSlots int32 `json:"provisionedSlots,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// CapacityRequest asks for burst capacity.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.pool`
// +kubebuilder:printcolumn:name="Slots",type=integer,JSONPath=`.spec.slots`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
type CapacityRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CapacityRequestSpec   `json:"spec,omitempty"`
	Status CapacityRequestStatus `json:"status,omitempty"`
}

// CapacityRequestList is a list of CapacityRequests.
// +kubebuilder:object:root=true
type CapacityRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CapacityRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkJob{}, &WorkJobList{}, &Pool{}, &PoolList{}, &CapacityRequest{}, &CapacityRequestList{})
}
