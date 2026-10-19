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
	// AnnotationRequeue on a WorkJob asks the queue to run units again:
	// "TOKEN:SET", where SET is a run-length index set and TOKEN makes
	// each request distinct.
	AnnotationRequeue = "c4.avalanche.io/requeue"
	// AnnotationAllowNodes on a WorkJob lists nodes (comma-separated) to
	// take off the job's automatic blacklist.
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
	// AnnotationOwner on a WorkJob is the user who submitted it, set at
	// admission from the request's user (or, for jobs the HUD creates for
	// a signed-in viewer, by the HUD) and never changed afterwards.
	AnnotationOwner = "c4.avalanche.io/owner"
	// AnnotationAcceptValidated, "true" on a WorkJob, accepts a validated
	// recompute of its inputs in place of moving them (staging,
	// experimental): the recomputed output differs from the recorded one,
	// so only the consuming job may choose it. Exact recomputes and
	// identical outputs need no consent; volatile ones are never offered.
	AnnotationAcceptValidated = "c4.avalanche.io/accept-validated"
	// AnnotationSubmittedVia says how the job was submitted: hud, mcp,
	// or api (anything else that wrote the WorkJob directly).
	AnnotationSubmittedVia = "c4.avalanche.io/submitted-via"
	// LabelShot groups a shot's jobs (the HUD's shot view).
	LabelShot = "c4.avalanche.io/shot"
	// LabelJobType names the prebuilt job type a job was made from
	// (render, usd, sim, comp, transcode, ci, lora, infer, ...).
	LabelJobType = "c4.avalanche.io/job-type"
	// AnnotationAcks on a WorkJob carries people's acknowledgements of
	// units that need a person: a JSON list of WorkAckRequest, newest last.
	// The queue carries out each request once (status.acked) and records
	// it in status.acks.
	AnnotationAcks = "c4.avalanche.io/acks"
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

	// DependsOn names jobs in this namespace that must succeed first.
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

	// Inputs are C4 views mounted into every unit.
	// +optional
	Inputs []WorkInput `json:"inputs,omitempty"`

	// Nudge holds or boosts the job. There are no priorities.
	// +optional
	Nudge WorkNudge `json:"nudge,omitempty"`

	// Retries per unit after the first attempt.
	// +kubebuilder:default=2
	// +optional
	Retries int32 `json:"retries,omitempty"`

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
	// Phase is Queued, Running, Blocked, Held, Succeeded or Failed.
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
	// Tracking), suggested, or class (the class's objective).
	// +optional
	DeadlineSource string `json:"deadlineSource,omitempty"`
	// Suggestion is the farm's suggested deadline and how it got there.
	// +optional
	Suggestion *WorkSuggestion `json:"suggestion,omitempty"`
	// Derivation is the record of the job's process. Experimental: it
	// will change with C4's derivation-record design.
	// +optional
	Derivation *WorkDerivation `json:"derivation,omitempty"`
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
	// Preemptions counts units stopped or asked to yield to make room.
	// +optional
	Preemptions int32 `json:"preemptions,omitempty"`
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

// UnitCounts counts a job's units by state.
type UnitCounts struct {
	Total int32 `json:"total,omitempty"`
	// Planned counts generators not yet evaluated (DESIGN 10.6).
	// +optional
	Planned   int32 `json:"planned,omitempty"`
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
