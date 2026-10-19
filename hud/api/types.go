// Package api is the HUD's wire contract (internal/hud/API.md) as Go
// types, shared by the real backend (internal/hud/server) and the mock
// (cmd/hudmock in the c4-hud repository), so the two cannot drift apart.
package api

import "time"

// Viewer is who the response is filtered for.
type Viewer struct {
	User     string   `json:"user"`
	Projects []string `json:"projects"`
	All      bool     `json:"allProjects"`
	// Role picks the viewer's starting detail level: artist, supervisor,
	// producer, wrangler, farm-admin, facilities or pipeline-td. Empty:
	// the HUD guesses from All (wrangler) or not (artist).
	Role string `json:"role,omitempty"`
}

// Roles a viewer can have (Viewer.Role).
var Roles = []string{"artist", "supervisor", "producer", "wrangler", "farm-admin", "facilities", "pipeline-td"}

// Sees reports whether the viewer may see a project's jobs and objects.
func (v *Viewer) Sees(project string) bool {
	if v == nil {
		return false
	}
	for _, p := range v.Projects {
		if p == project {
			return true
		}
	}
	return false
}

// BurstOffer is the smallest burst that restores a job's deadline.
type BurstOffer struct {
	Pool          string  `json:"pool"`
	Slots         int     `json:"slots"`
	Cost          float64 `json:"cost"`
	NeedsApproval bool    `json:"needsApproval"`
}

// MoveOffer (experimental) is a move of a job's inputs to another site's
// pool that restores its deadline (sched.Stage): how (via move,
// recompute or reuse; staging when under way), over which channel, what
// it costs and how long until the job can start there.
type MoveOffer struct {
	Pool          string  `json:"pool"`
	Via           string  `json:"via"`
	Channel       string  `json:"channel,omitempty"`
	Cost          float64 `json:"cost"`
	DelaySeconds  float64 `json:"delaySeconds"`
	Slots         int     `json:"slots"`
	NeedsApproval bool    `json:"needsApproval"`
	Why           string  `json:"why,omitempty"`
}

type Snapshot struct {
	Now      time.Time  `json:"now"`
	Viewer   *Viewer    `json:"viewer"`
	Farm     Farm       `json:"farm"`
	Pools    []Pool     `json:"pools"`
	Classes  []Class    `json:"classes"`
	Projects []Project  `json:"projects"`
	Jobs     []Job      `json:"jobs"`
	Nodes    []Node     `json:"nodes"`
	Tiers    Tiers      `json:"tiers"`
	Capacity []Capacity `json:"capacity"`
	Usage    Usage      `json:"usage"`
	// Sites place nodes on a map; omitted when the farm declares none.
	Sites []Site `json:"sites,omitempty"`
	// Gates are the one-slot resources a project's jobs pass first come,
	// first served (a merge gate), each with its holder and line.
	Gates []Gate `json:"gates,omitempty"`
}

// Gate is a gate and the units at it.
type Gate struct {
	Namespace string     `json:"namespace"`
	Name      string     `json:"name"`
	Holder    *GateUnit  `json:"holder,omitempty"`
	Waiting   []GateUnit `json:"waiting"`
}

// GateUnit is a unit at a gate: its job, index and name, when it joined
// the line, and when it is expected to pass.
type GateUnit struct {
	Job         string     `json:"job"`
	Unit        int        `json:"unit"`
	Name        string     `json:"name,omitempty"`
	Since       *time.Time `json:"since,omitempty"`
	ExpectStart *time.Time `json:"expectStart,omitempty"`
}

// Site is a place the farm's nodes run: a studio machine room or a cloud
// region. Nodes join a site by region, and the site's localities are the
// node localities (racks, zones) it holds.
type Site struct {
	Name       string   `json:"name"`
	Label      string   `json:"label"`
	Region     string   `json:"region"`
	Lat        float64  `json:"lat"`
	Lon        float64  `json:"lon"`
	Kind       string   `json:"kind"` // onprem or cloud
	Localities []string `json:"localities"`
	// TZ is the site's time zone, an IANA name (America/New_York), so a
	// site's clock can show its local time; empty shows the viewer's.
	TZ string `json:"tz,omitempty"`
}

// Usage is metered use. Billing is by cluster-hours, container-hours or
// a production window, never by bytes; bytes avoided is the savings proof.
type Usage struct {
	ClusterHours      float64          `json:"clusterHours"`
	ContainerHours    []ContainerHours `json:"containerHours"`
	GPUHours          []GPUHours       `json:"gpuHours"`
	ProductionWindows []Window         `json:"productionWindows"`
}

type ContainerHours struct {
	Project string  `json:"project"`
	Pool    string  `json:"pool"`
	Hours   float64 `json:"hours"`
}

type GPUHours struct {
	Project string  `json:"project"`
	Hours   float64 `json:"hours"`
}

type Window struct {
	Project string    `json:"project"`
	Name    string    `json:"name"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Active  bool      `json:"active"`
}

type Farm struct {
	SlotsUsed          int     `json:"slotsUsed"`
	SlotsTotal         int     `json:"slotsTotal"`
	QueuedUnits        int     `json:"queuedUnits"`
	UnitsPerHour       int     `json:"unitsPerHour"`
	FailuresLastHour   int     `json:"failuresLastHour"`
	BurstSlots         int     `json:"burstSlots"`
	BurstCostPerHour   float64 `json:"burstCostPerHour"`
	BytesMaterialized  float64 `json:"bytesMaterialized"`
	BytesFetchedStore  float64 `json:"bytesFetchedFromStore"`
	BytesAvoided       float64 `json:"bytesAvoided"`
	OtherProjectsSlots int     `json:"otherProjectsSlots"`
}

type Pool struct {
	Name            string         `json:"name"`
	Attrs           map[string]any `json:"attrs"`
	Slots           int            `json:"slots"`
	Used            int            `json:"used"`
	CostPerSlotHour float64        `json:"costPerSlotHour"`
	Burst           *PoolBurst     `json:"burst,omitempty"`
}

type PoolBurst struct {
	MaxSlots            int     `json:"maxSlots"`
	ProvisioningSeconds float64 `json:"provisioningSeconds"`
	CostPerSlotHour     float64 `json:"costPerSlotHour"`
	Requested           int     `json:"requested"`
	Provisioned         int     `json:"provisioned"`
}

type Class struct {
	Name   string         `json:"name"`
	Driver string         `json:"driver"`
	SLO    map[string]any `json:"slo"`
	Lens   string         `json:"lens"`
}

type Project struct {
	Name              string           `json:"name"`
	Quotas            []map[string]any `json:"quotas"`
	BurstSpendToday   float64          `json:"burstSpendToday"`
	BurstBudgetPerDay float64          `json:"burstBudgetPerDay"`
	BytesAvoided      float64          `json:"bytesAvoided"`
	WorkerHours       []map[string]any `json:"workerHours"`
	GPUHours          float64          `json:"gpuHours"`
}

// Spend is a job's spend so far: slot time at its pools' rates and model
// tokens at the prices its model units reported, in US dollars, and the
// model units' share of it (LLM-OR-PROCEDURE 6.2).
type Spend struct {
	ModelUnits      int   `json:"modelUnits"`
	ProceduralUnits int   `json:"proceduralUnits"`
	ModelTokens     int64 `json:"modelTokens"`
	UnpricedTokens  int64 `json:"unpricedTokens,omitempty"`
	// The token classes, raw, and model tokens weighted by price
	// (input-equivalents); a dollar of output is not a dollar of cache
	// reads.
	InputTokens            int64   `json:"inputTokens,omitempty"`
	OutputTokens           int64   `json:"outputTokens,omitempty"`
	CacheReadTokens        int64   `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens       int64   `json:"cacheWriteTokens,omitempty"`
	WeightedTokens         int64   `json:"weightedTokens,omitempty"`
	UnpricedWeightedTokens int64   `json:"unpricedWeightedTokens,omitempty"`
	EstimatedTokenCost     float64 `json:"estimatedTokenCost,omitempty"`
	ModelShareBasis        string  `json:"modelShareBasis,omitempty"`
	TokenCost              float64 `json:"tokenCost"`
	ModelSlotCost          float64 `json:"modelSlotCost"`
	ProceduralSlotCost     float64 `json:"proceduralSlotCost"`
	ModelShare             float64 `json:"modelShare"`
}

// Measure is a job's own measure of done: value of the target, from the
// latest reading a unit reported.
type Measure struct {
	Name   string    `json:"name,omitempty"`
	Value  int64     `json:"value"`
	Of     int64     `json:"of,omitempty"`
	Done   bool      `json:"done"`
	At     time.Time `json:"at,omitempty"`
	Detail string    `json:"detail,omitempty"`
	// Required: the job is done only when the measure is.
	Required bool `json:"required,omitempty"`
}

type Counts struct {
	Total       int `json:"total"`
	Unevaluated int `json:"unevaluated"`
	Queued      int `json:"queued"`
	Blocked     int `json:"blocked"`
	Running     int `json:"running"`
	Retrying    int `json:"retrying"`
	Succeeded   int `json:"succeeded"`
	Failed      int `json:"failed"`
	Held        int `json:"held"`
	// The phases a person is shown (each unit in exactly one count):
	// Preflight preparing to run; Running the first attempt working,
	// Retrying a later one; Backoff a failed attempt waiting for its
	// retry; Egress done, output still moving; Accepted failed and
	// accepted by a person.
	Preflight int `json:"preflight,omitempty"`
	Backoff   int `json:"backoff,omitempty"`
	Egress    int `json:"egress,omitempty"`
	Accepted  int `json:"accepted,omitempty"`
	// Waits counts wait units waiting for their decision ("waits for").
	Waits int `json:"waits,omitempty"`
	// Planned counts generators not evaluated yet (also Unevaluated, the
	// older name), Flagged units a review escalated to a person.
	Planned int `json:"planned,omitempty"`
	Flagged int `json:"flagged,omitempty"`
	// Locked counts units waiting for a unit that holds what they claim.
	Locked int `json:"locked,omitempty"`
	// Reused counts succeeded units that ran nothing: an earlier run of
	// the same computation gave their output (also in Succeeded).
	Reused int `json:"reused,omitempty"`
}

type States struct {
	Unevaluated string `json:"unevaluated"`
	Queued      string `json:"queued"`
	Blocked     string `json:"blocked"`
	Running     string `json:"running"`
	Retrying    string `json:"retrying"`
	Succeeded   string `json:"succeeded"`
	Failed      string `json:"failed"`
	Preflight   string `json:"preflight,omitempty"`
	Backoff     string `json:"backoff,omitempty"`
	Egress      string `json:"egress,omitempty"`
	Accepted    string `json:"accepted,omitempty"`
	Waits       string `json:"waits,omitempty"`
	Planned     string `json:"planned,omitempty"`
	Flagged     string `json:"flagged,omitempty"`
	Locked      string `json:"locked,omitempty"`
}

type Objective struct {
	Type                 string     `json:"type"`
	Deadline             *time.Time `json:"deadline,omitempty"`
	LatencyTargetSeconds float64    `json:"latencyTargetSeconds,omitempty"`
	From                 string     `json:"from,omitempty"`
	// Source is where the deadline comes from: spec (a person, a tool or
	// tracking), suggested (the farm), or class (the class's objective).
	Source string `json:"source,omitempty"`
	// Mode is who owns the deadline: Pinned, Suggested or Tracking.
	Mode string `json:"mode,omitempty"`
	// Suggestion is the farm's suggested deadline, when it made one.
	Suggestion *Suggestion `json:"suggestion,omitempty"`
}

// Suggestion is a suggested deadline and how it got there.
type Suggestion struct {
	Deadline time.Time `json:"deadline"`
	// State is Estimating (it may still move later if at risk) or
	// Settled. The farm never moves a suggestion earlier.
	State string `json:"state"`
	// Policy is achievable, sessions or turnaround; Quantile the finish
	// percentile it meets (0.95 by default: suggestions are generous, and
	// tightening is the person's choice).
	Policy       string  `json:"policy"`
	Quantile     float64 `json:"quantile"`
	Revisions    int     `json:"revisions"`
	MaxRevisions int     `json:"maxRevisions"`
	// NextSession is the following session, offered when the job is not
	// expected to make the suggested one.
	NextSession *time.Time           `json:"nextSession,omitempty"`
	History     []SuggestionRevision `json:"history"`
	// AtRisk: the job's finish at Quantile is now after the suggestion
	// (the HUD shows it in orchid).
	AtRisk bool `json:"atRisk,omitempty"`
}

// SuggestionRevision is one value a suggestion took, with its signed
// record.
type SuggestionRevision struct {
	At            time.Time `json:"at"`
	Deadline      time.Time `json:"deadline"`
	UnitsMeasured int       `json:"unitsMeasured"`
	Record        string    `json:"record,omitempty"`
	// Reason is first, or at-risk for a later move the farm made because
	// the suggestion was at risk.
	Reason string `json:"reason,omitempty"`
}

// Band is a finish forecast: the times by which a job finishes with
// probability 50, 80, 90 and 95%.
type Band struct {
	P50 time.Time `json:"p50"`
	P80 time.Time `json:"p80"`
	P90 time.Time `json:"p90"`
	P95 time.Time `json:"p95"`
}

// DeadlineOptions is GET /jobs/{ns}/{name}/deadline-options: what each
// candidate deadline would take.
type DeadlineOptions struct {
	Job        string           `json:"job"`
	Now        time.Time        `json:"now"`
	Policy     string           `json:"policy"`
	Quantile   float64          `json:"quantile"`
	Deadline   *time.Time       `json:"deadline,omitempty"`
	Band       *Band            `json:"band,omitempty"`
	Achievable *time.Time       `json:"achievable,omitempty"`
	Floor      time.Time        `json:"floor"`
	FloorWhy   string           `json:"floorWhy"`
	Options    []DeadlineOption `json:"options"`
}

// DeadlineOption is the readout for one candidate time.
type DeadlineOption struct {
	At time.Time `json:"at"`
	// Label names a choice the queue offered ("dailies session Tue
	// 08:00"); empty for a time the caller gave.
	Label  string  `json:"label,omitempty"`
	Band   *Band   `json:"band,omitempty"`
	Meets  int     `json:"meets"`
	OnTime bool    `json:"onTime"`
	Boost  float64 `json:"boost,omitempty"`
	Burst  *Burst  `json:"burst,omitempty"`
	// Displaces counts the jobs this deadline would make late; Displaced
	// names those in the viewer's projects, and Unnamed counts the rest.
	Displaces int        `json:"displaces"`
	Displaced []string   `json:"displaced"`
	Unnamed   int        `json:"unnamed"`
	Floor     *time.Time `json:"floor,omitempty"`
	FloorWhy  string     `json:"floorWhy,omitempty"`
}

// Burst is the burst capacity that would serve a deadline.
type Burst struct {
	Slots     map[string]int `json:"slots"`
	Pools     []string       `json:"pools"`
	Cost      float64        `json:"cost"`
	Closes    bool           `json:"closes"`
	Automatic bool           `json:"automatic"`
	Why       string         `json:"why,omitempty"`
}

type Estimate struct {
	RemainingSeconds    float64   `json:"remainingSeconds"`
	P90RemainingSeconds float64   `json:"p90RemainingSeconds"`
	EtaP50              time.Time `json:"etaP50"`
	EtaP90              time.Time `json:"etaP90"`
	// NoETA, when set, says why the job has no ETA (held, suspended,
	// waiting for capacity, no deadline). EtaP50 and EtaP90 then carry the
	// snapshot's time, never a past start or the zero time; show the reason,
	// not a time.
	NoETA           string      `json:"noEta,omitempty"`
	RequiredSlots   float64     `json:"requiredSlots"`
	AttainableSlots float64     `json:"attainableSlots"`
	RunningSlots    int         `json:"runningSlots"`
	SlackSeconds    float64     `json:"slackSeconds"`
	Feasible        bool        `json:"feasible"`
	Message         string      `json:"message,omitempty"`
	Burst           *BurstOffer `json:"burst,omitempty"`
	Move            *MoveOffer  `json:"move,omitempty"`
	// EtaP80 and EtaP95 complete the band etaP50 and etaP90 begin.
	EtaP80 *time.Time `json:"etaP80,omitempty"`
	EtaP95 *time.Time `json:"etaP95,omitempty"`
	// Calibrated says the band comes from finished jobs' outcomes.
	Calibrated bool `json:"calibrated"`
	// Risk is the deadline risk a person is shown: none, atRisk,
	// willMiss or missed (WorkEstimate.Risk).
	Risk string `json:"risk,omitempty"`
}

type Input struct {
	Materialization string `json:"materialization,omitempty"`
	View            string `json:"view,omitempty"`
	FromJob         string `json:"fromJob,omitempty"`
}

type Job struct {
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	Project      string    `json:"project"`
	Class        string    `json:"class"`
	Driver       string    `json:"driver"`
	Owner        string    `json:"owner,omitempty"`
	Phase        string    `json:"phase"`
	Order        string    `json:"order"`
	Units        Counts    `json:"units"`
	UnitStates   States    `json:"unitStates"`
	Objective    Objective `json:"objective"`
	Estimate     Estimate  `json:"estimate"`
	Rank         int       `json:"rank"`
	Pools        []string  `json:"pools"`
	Inputs       []Input   `json:"inputs"`
	Output       *Output   `json:"output,omitempty"`
	DependsOn    []string  `json:"dependsOn"`
	CreatedAt    time.Time `json:"createdAt"`
	Nudge        Nudge     `json:"nudge"`
	HeavyRegions []Heavy   `json:"heavyRegions,omitempty"`
	Blacklist    []string  `json:"blacklist,omitempty"`
	// Shot is the job's c4.avalanche.io/shot label: the jobs of one shot
	// (sim, render, comp, transcode) are drawn as one sheet.
	Shot string `json:"shot,omitempty"`
	// Work is the job's progress weighted by work, in slot-seconds.
	Work Work `json:"work"`
	// NeedsPerson is set while the job waits on a person.
	NeedsPerson *NeedsPerson `json:"needsPerson,omitempty"`
	// Measure is the job's own measure of done, when it declares one.
	Measure *Measure `json:"measure,omitempty"`
	// Spend splits what the job cost between procedural and model units.
	Spend *Spend `json:"spend,omitempty"`
	// Acks are the standing acknowledgements of failed units.
	Acks []Ack `json:"acks,omitempty"`
	// Waits are what the job waits for: its wait units (a ruling, an
	// approval, an outside party, a clock) and units a review escalated,
	// open and decided.
	Waits []Wait `json:"waits,omitempty"`
}

// NeedsPerson says why a job waits on a person, and since when.
type NeedsPerson struct {
	// Reason is failed, stuck, approval, quota or permission.
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

// Ack is a person taking failed units (acknowledge, or assign to
// Assignee) or accepting them as they are (accept).
type Ack struct {
	Kind     string    `json:"kind"`
	Units    string    `json:"units"`
	Actor    string    `json:"actor"`
	Assignee string    `json:"assignee,omitempty"`
	Note     string    `json:"note,omitempty"`
	At       time.Time `json:"at"`
	Record   string    `json:"record,omitempty"`
}

// Work is a job's work done, in flight and left, in slot-seconds. With
// Basis "units" no estimate exists and the fields count units instead.
type Work struct {
	DoneSlotSeconds             float64 `json:"doneSlotSeconds"`
	RunningSlotSeconds          float64 `json:"runningSlotSeconds"`
	RunningRemainingSlotSeconds float64 `json:"runningRemainingSlotSeconds"`
	RemainingSlotSeconds        float64 `json:"remainingSlotSeconds"`
	P90RemainingSlotSeconds     float64 `json:"p90RemainingSlotSeconds"`
	Basis                       string  `json:"basis"` // estimate or units
	// DoneFraction is the share of the job's work done, counting units
	// whose output is still moving; DeliveredFraction counts only units
	// whose output is added and recorded. Both are monotonic: the highest
	// seen for the job, lowered only when finished units are requeued.
	DoneFraction      *float64 `json:"doneFraction,omitempty"`
	DeliveredFraction *float64 `json:"deliveredFraction,omitempty"`
}

// Work bases.
const (
	BasisEstimate = "estimate"
	BasisUnits    = "units"
)

// UnitWork is one unit's work in slot-seconds: spent so far and expected
// in total.
type UnitWork struct {
	Spent    float64 `json:"spent"`
	Expected float64 `json:"expected"`
}

// UnitRead names units of another job that a unit reads, when that is
// not simply the same frame (a temporal comp reads frames n-1 to n+1).
type UnitRead struct {
	Job   string `json:"job"`
	Units string `json:"units"`
}

// Book is GET /book: the farm's history, newest last.
type Book struct {
	Entries []BookEntry `json:"entries"`
}

// BookEntry is one line of the book: who did what to which object, and
// the signed record that proves it when there is one.
type BookEntry struct {
	Seq     int64      `json:"seq"`
	At      time.Time  `json:"at"`
	Actor   string     `json:"actor"`
	Signer  string     `json:"signer"` // node, queue, controller, hud, importer, ingest
	Kind    string     `json:"kind"`
	Object  BookObject `json:"object"`
	Message string     `json:"message"`
	Record  string     `json:"record,omitempty"`
}

// BookObject is what a book entry is about.
type BookObject struct {
	Type string `json:"type"` // job, unit, node, capacity, view
	Job  string `json:"job,omitempty"`
	Unit *int   `json:"unit,omitempty"`
	Node string `json:"node,omitempty"`
	Pool string `json:"pool,omitempty"`
	View string `json:"view,omitempty"`
}

// Signers of book entries.
const (
	SignerNode       = "node"
	SignerQueue      = "queue"
	SignerController = "controller"
	SignerHUD        = "hud"
	SignerImporter   = "importer"
	SignerIngest     = "ingest"
)

type Output struct {
	View       string `json:"view"`
	Changesets int    `json:"changesets"`
}

type Nudge struct {
	Hold  bool    `json:"hold"`
	Boost float64 `json:"boost"`
}

type Heavy struct {
	From   int     `json:"from"`
	To     int     `json:"to"`
	Factor float64 `json:"factor"`
	Label  string  `json:"label,omitempty"`
}

type Node struct {
	Name             string    `json:"name"`
	Pool             string    `json:"pool"`
	Locality         string    `json:"locality"`
	State            string    `json:"state"`
	Slots            int       `json:"slots"`
	Used             int       `json:"used"`
	FailuresLastHour int       `json:"failuresLastHour"`
	Suspect          bool      `json:"suspect"`
	CacheBytes       float64   `json:"cacheBytes"`
	CacheHitRate     float64   `json:"cacheHitRate"`
	Running          []Running `json:"running,omitempty"`
	// Zone and Region are the node's topology.kubernetes.io/zone and
	// /region labels; Region joins the node to a Site.
	Zone   string `json:"zone,omitempty"`
	Region string `json:"region,omitempty"`
	// Capped says a limit (a host runner's power cap, the node's
	// c4.avalanche.io/slot-limit label) lowered the node below its pool's
	// shape: Slots is what it offers now, ShapeSlots what its pool gives
	// it, and CapacityReason says why in plain words.
	Capped         bool   `json:"capped,omitempty"`
	ShapeSlots     int    `json:"shapeSlots,omitempty"`
	CapacityReason string `json:"capacityReason,omitempty"`
}

type Running struct {
	Job   string `json:"job"`
	Unit  int    `json:"unit"`
	Slots int    `json:"slots"`
}

type Tiers struct {
	Tiers      []Tier         `json:"tiers"`
	FanIn      map[string]int `json:"fanIn"`
	PublishP50 float64        `json:"publishP50"`
	PublishP95 float64        `json:"publishP95"`
	CaptureP50 float64        `json:"captureP50"`
}

type Tier struct {
	Name             string  `json:"name"`
	HitRate          float64 `json:"hitRate"`
	RequestsIn       int     `json:"requestsIn"`
	UpstreamRequests int     `json:"upstreamRequests"`
	BytesServed      float64 `json:"bytesServed"`
	ShedRate         float64 `json:"shedRate"`
	InflightFills    int     `json:"inflightFills"`
}

type Capacity struct {
	Pool         string    `json:"pool"`
	Slots        int       `json:"slots"`
	Until        time.Time `json:"until"`
	Reason       string    `json:"reason"`
	Jobs         []string  `json:"jobs"`
	EstCost      float64   `json:"estCost"`
	State        string    `json:"state"`
	PrewarmViews []string  `json:"prewarmViews"`
}

// Wait is one thing a job waits for (PRIMITIVES Q6; the HUD word is
// "waits for"). A person's open wait that holds up work needs them (red);
// one that holds up nothing yet is pale.
type Wait struct {
	Unit int    `json:"unit"`
	Name string `json:"name,omitempty"`
	// WaitsFor is person:<id>, party:<name> or clock:<time>.
	WaitsFor  string     `json:"waitsFor"`
	Ask       string     `json:"ask,omitempty"`
	Choices   []string   `json:"choices,omitempty"`
	Escalated bool       `json:"escalated,omitempty"`
	Since     *time.Time `json:"since,omitempty"`
	// ExpectSeconds is the expected wait in all, from the server's measured
	// decisions, else the unit's own figure.
	ExpectSeconds int64 `json:"expectSeconds,omitempty"`
	Blocking      bool  `json:"blocking,omitempty"`
	// NeedsYou: a person's open wait that holds up work, or a unit a
	// review escalated to them.
	NeedsYou bool `json:"needsYou,omitempty"`
	// Decision, once made.
	Do     string     `json:"do,omitempty"`
	Note   string     `json:"note,omitempty"`
	Actor  string     `json:"actor,omitempty"`
	At     *time.Time `json:"at,omitempty"`
	Record string     `json:"record,omitempty"`
}
