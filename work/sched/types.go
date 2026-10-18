// Package sched is the work system's scheduling policy (DESIGN.md sections
// 9.9 to 9.13): a pure function from a snapshot of pools, projects and
// jobs to decisions (units to release, units to preempt, capacity to
// request, and each job's objective state).
//
// Decide has no hidden state. Hysteresis and rate limits live in a State
// value the caller keeps and passes back on every call. One State must not
// be used by two calls at the same time; distinct States may be.
//
// The package also holds baseline policies with the same signature (FIFO,
// EDF, Manual), used by the simulator in hack/sim for comparison.
//
// Units of measure: slots for capacity, slot-seconds for work, US dollars
// for cost.
package sched

import (
	"time"

	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// Snapshot is everything a decision looks at.
type Snapshot struct {
	Now      time.Time
	Pools    []Pool
	Projects []Project
	Jobs     []Job
}

// Pool is a named set of slots.
type Pool struct {
	Name string
	// Slots is the capacity ready now; for a burst pool, its provisioned
	// slots. Used is how many are occupied by running units.
	Slots, Used     int
	Attrs           map[string]string
	CostPerSlotHour float64
	// Burst is set for pools that grow on request.
	Burst *Burst
}

// Burst describes a pool that can be grown on request.
type Burst struct {
	MaxSlots        int
	Provisioning    time.Duration
	CostPerSlotHour float64
	// Requested is the current request (slots); Provisioned how many of
	// them are ready. Requested-Provisioned are on their way.
	Requested, Provisioned int
}

// Project carries quotas and burst economics.
type Project struct {
	Name   string
	Quotas map[string]Quota // by pool name
	// Weight scales the project's entitlement in fair share (default 1).
	Weight float64
	// BurstBudgetLeft is what the project may still spend on burst today,
	// in dollars, after the caller has deducted earlier approved bursts.
	BurstBudgetLeft float64
	// LateCostPerHour prices lateness of the project's jobs.
	LateCostPerHour float64
}

// Quota follows Kueue's semantics: Nominal slots are guaranteed when the
// pool is contended; unused nominal slots are lent; a project may borrow
// at most BorrowingLimit slots above its nominal (negative: unlimited).
type Quota struct {
	Nominal, BorrowingLimit int
}

// Job is one job as the policy sees it.
type Job struct {
	ID, Project, Class string
	Submitted, Ready   time.Time
	// Deadline is the effective deadline (from the job, a milestone, the
	// class turnaround, or ready time plus a latency target). Zero: none.
	Deadline time.Time
	// RequiredRate is a throughput objective in slots, used when Deadline
	// is zero.
	RequiredRate float64
	// Pools the job may use.
	Pools []string
	// MaxParallel caps the job's running slots (0: no cap).
	MaxParallel int
	// SlotsPerUnit is the size of each unit; above 1 the unit is a gang,
	// released only when one pool has room for all of it.
	SlotsPerUnit int
	// Running is slots in use per pool.
	Running map[string]int
	// Runnable lists units ready to run, in the job's dispatch order. The
	// caller may pass only a prefix (long enough to fill free capacity);
	// RunnableTotal is the full count.
	Runnable      []UnitRef
	RunnableTotal int
	// Blocked counts units that exist but wait on dependencies.
	Blocked int
	// Unevaluated counts generators not yet expanded.
	Unevaluated int
	Estimate    estimate.Summary
	// Nudges.
	Hold, HoldStop bool
	Boost          float64 // 1 to 2; 0 means 1
	// LastRunning is when the job last had a running unit (zero: never).
	LastRunning  time.Time
	RunningUnits []RunningUnit
	// Preemptible says whether the job's units may be stopped to make room.
	Preemptible bool
	// Checkpoints says whether the job's units checkpoint and can yield at
	// a checkpoint boundary when asked, instead of being killed.
	Checkpoints bool

	// Inputs used only by the Manual baseline: the static priority set at
	// submission, and what a person reads in a render manager (units done
	// and their total run time).
	Priority    int
	UnitsDone   int
	DoneSeconds float64
}

// UnitRef is a runnable unit.
type UnitRef struct {
	ID string
	// Info marks units that reveal what the rest of the job costs:
	// unevaluated generators and the coarse levels of binary order.
	Info bool
	// Slots overrides the job's SlotsPerUnit when above 0.
	Slots int
}

// RunningUnit is a running unit, for preemption.
type RunningUnit struct {
	ID, Pool string
	Slots    int
	Started  time.Time
	// LastCheckpoint is when the unit last saved its state (zero: never).
	LastCheckpoint time.Time
	// RestoreCost is the time a resumed attempt spends getting back to
	// where it was.
	RestoreCost time.Duration
	Preemptions int
}

// Decisions is what a call to Decide returns.
type Decisions struct {
	Dispatch []Dispatch
	Preempt  []Preempt
	// Capacity holds one request per burst pool: the total slots wanted
	// from it from now on.
	Capacity []CapacityRequest
	// Bursts lists every burst suggestion, approved automatically or not.
	Bursts []BurstSuggestion
	Jobs   map[string]JobState
}

// Tier says why a unit was released.
type Tier string

const (
	TierStarvation Tier = "starvation"
	TierNeed       Tier = "deadline-need"
	TierGuarantee  Tier = "nominal-guarantee"
	TierBoost      Tier = "boost"
	TierInfo       Tier = "information"
	TierFair       Tier = "surplus"
	TierBaseline   Tier = "baseline"
)

// Dispatch releases one unit into a pool.
type Dispatch struct {
	Job, Unit, Pool string
	Slots           int
	Tier            Tier
}

// PreemptReason says why a unit is stopped.
type PreemptReason string

const (
	ReasonReclaim     PreemptReason = "reclaim-nominal"
	ReasonFeasibility PreemptReason = "restore-feasibility"
	ReasonHoldStop    PreemptReason = "hold-stop"
	ReasonWrangler    PreemptReason = "wrangler-suspend"
	// ReasonGuard: a unit the starvation guard started, stopped for a
	// job that will miss its deadline without the slot. A guard start is
	// a floor on progress, not a reservation: such a unit may lose more
	// than MaxLostWork.
	ReasonGuard PreemptReason = "guard-unit"
)

// Preempt stops one running unit. With Yield set the unit is asked to stop
// at its next checkpoint boundary; otherwise it is stopped now and loses
// work since its last checkpoint.
type Preempt struct {
	Job, Unit, Pool string
	Slots           int
	Reason          PreemptReason
	For             string // the job the slots are for, if any
	Yield           bool
	LostWork        time.Duration
}

// CapacityRequest is the desired size of a burst pool.
type CapacityRequest struct {
	Pool   string
	Slots  int
	Reason string
}

// BurstSuggestion is the smallest burst that makes an at-risk job finish
// on time, or the most that would help if nothing does.
type BurstSuggestion struct {
	Job, Project string
	Slots        map[string]int // added slots per burst pool
	Order        []string       // pools in Slots, cheapest first
	Cost         float64
	// Closes says whether the suggestion restores the job's deadline.
	Closes bool
	// Auto says the suggestion was within the project's budget and below
	// the lateness cost it prevents, and is included in Capacity.
	Auto bool
	// Why explains a suggestion that needs approval.
	Why string
	// Margin says the job is on time under Graham's bound and late only
	// with the plan's margin (the critical path counted twice): the burst
	// is margin against estimate uncertainty.
	Margin bool
	// LateCost is the lateness cost the burst prevents, LateCostPerHour
	// times the job's lateness in the plan with its margin; for a Margin
	// burst that lateness is above the job's reported LateBy of 0.
	LateCost float64
}

// JobState is a job's objective state.
type JobState struct {
	// Required slots R (boosted), Attainable slots A, and slots running.
	Required, Attainable, RunningSlots float64
	// Share is the job's planned slots at current capacity (including
	// burst already requested), from water-filling every job's need.
	Share float64
	// Slack is T - max(W90/A, t_path).
	Slack time.Duration
	// Feasible: the job finishes by its deadline with all attainable
	// capacity. AtRisk: it misses at its planned share.
	Feasible, AtRisk bool
	ETA50, ETA90     time.Time
	LateBy           time.Duration
	Rank             int
	Message          string
	Burst            *BurstSuggestion
}

// Config holds policy settings. DefaultConfig returns DESIGN.md's values.
type Config struct {
	StarvationAfter time.Duration // 30m
	// StarvationDeferMax: a starving job with more than PreemptSlack of
	// slack lets the deadline tier go first until it has starved this
	// long (0: always, while it has the slack).
	StarvationDeferMax time.Duration // 2h
	// TieBand: deadline ratios in the same band (0.1 wide) tie.
	TieBand float64
	// InfoUnitsPerJob caps how many information units one job may release
	// per decision in the information tier (0: no cap).
	InfoUnitsPerJob int
	// PreemptSlack: only jobs with more slack than this lose units.
	PreemptSlack time.Duration
	// At most PreemptPerWindow units are preempted per pool per
	// PreemptWindow, and a unit at most MaxPreemptionsPerUnit times.
	PreemptWindow         time.Duration
	PreemptPerWindow      int
	MaxPreemptionsPerUnit int
	// MaxLostWork: a unit whose preemption would throw away more than
	// this (time since its last checkpoint plus restore cost) is not a
	// victim. Zero means no cap. A unit asked to yield at its next
	// checkpoint loses only its restore cost.
	MaxLostWork time.Duration
	// ScaleDownAfter: a burst request shrinks only after idle slots have
	// persisted this long.
	ScaleDownAfter time.Duration
	// Burst enables capacity requests.
	Burst bool
	// MinRemaining floors the time left when computing R for a job at or
	// near its deadline.
	MinRemaining time.Duration
	// TailAware computes required slots as R = W90/(T - t_path) instead of
	// DESIGN.md's R = W90/T. List scheduling on R slots finishes within
	// W/R + (longest unit), so W90/T slots leave no room for the last wave
	// of units: a job given exactly W90/T starts its longest units too
	// late. Default true; false is the literal formula.
	TailAware bool
	// Planner chooses how R and shares are computed (plan.go).
	Planner Planner
	// NoInfoTier skips the information tier (for ablation).
	NoInfoTier bool
	// Surplus chooses how the fourth tier hands out what is left, and
	// SlackBand is the width of a slack tie band under SurplusSlack.
	Surplus   Surplus
	SlackBand time.Duration
	// RateSlack is the slack reported for a throughput job at or above
	// its rate.
	RateSlack time.Duration
	// Manual baseline: wrangler period, the margin before a deadline at
	// which a job counts as late-looking, and the margin of comfort at
	// which a bump is removed.
	WranglerEvery      time.Duration
	WranglerMargin     time.Duration
	WranglerUnbump     time.Duration
	WranglerStartAlarm time.Duration
}

// DefaultConfig returns the settings named in DESIGN.md.
func DefaultConfig() Config {
	return Config{
		StarvationAfter:       30 * time.Minute,
		StarvationDeferMax:    2 * time.Hour,
		TieBand:               0.10,
		InfoUnitsPerJob:       0,
		PreemptSlack:          2 * time.Hour,
		PreemptWindow:         10 * time.Minute,
		PreemptPerWindow:      8,
		MaxPreemptionsPerUnit: 2,
		MaxLostWork:           15 * time.Minute,
		ScaleDownAfter:        10 * time.Minute,
		Burst:                 true,
		MinRemaining:          5 * time.Minute,
		TailAware:             true,
		Planner:               PlanFluid,
		Surplus:               SurplusSlack,
		SlackBand:             time.Hour,
		RateSlack:             24 * time.Hour,
		WranglerEvery:         2 * time.Hour,
		WranglerMargin:        time.Hour,
		WranglerUnbump:        3 * time.Hour,
		WranglerStartAlarm:    4 * time.Hour,
	}
}

// State is what the caller keeps between calls. Its zero value is ready.
type State struct {
	pending    map[string]pendingPreempt // by unit ID
	preemptLog map[string][]time.Time    // by pool
	surplus    map[string]time.Time      // by burst pool: idle since
	manual     manualState
	guard      map[string]time.Time    // units the starvation guard started, by unit ID
	guardHit   map[string]time.Time    // jobs whose guard unit was preempted, and when
	took       map[[2]string]time.Time // [victim job, job it was for]: last preemption between them
}

type pendingPreempt struct {
	job, forJob, pool string
	slots             int
	at                time.Time
}

type manualState struct {
	lastCheck time.Time
	bumped    map[string]bool
}

func (st *State) init() {
	if st.pending == nil {
		st.pending = map[string]pendingPreempt{}
	}
	if st.preemptLog == nil {
		st.preemptLog = map[string][]time.Time{}
	}
	if st.surplus == nil {
		st.surplus = map[string]time.Time{}
	}
	if st.manual.bumped == nil {
		st.manual.bumped = map[string]bool{}
	}
	if st.guard == nil {
		st.guard = map[string]time.Time{}
	}
	if st.guardHit == nil {
		st.guardHit = map[string]time.Time{}
	}
	if st.took == nil {
		st.took = map[[2]string]time.Time{}
	}
}

// Bumped reports whether the Manual baseline's wrangler has bumped a job.
func (st *State) Bumped(job string) bool { return st.manual.bumped[job] }

// Surplus names a rule for the fourth (fair-share) tier.
type Surplus string

const (
	SurplusSlack     Surplus = "least-slack"
	SurplusFairShare Surplus = "fair-share"
)

// Policy is the signature shared by Decide and the baselines.
type Policy func(s Snapshot, st *State, cfg Config) Decisions
