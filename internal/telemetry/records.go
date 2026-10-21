package telemetry

// Snapshot is one redacted policy input with the decisions the farm made
// from it (DESIGN.md section 6.9): pools, projects, jobs and
// links as pseudonyms with ranks, times relative to the snapshot's now,
// money scaled, and the policy's state and configuration, so a replay
// starts warm. Nothing of the HUD's view is in it: it is built from the
// policy's own input.
type Snapshot struct {
	Build     Build
	KeyEpoch  uint32
	Pools     []Pool
	Projects  []Project
	Jobs      []Job
	Links     []Link
	State     State
	Config    Config
	Decisions Decisions
	// Faithful is the faithfulness check's result: true when replaying
	// this redacted snapshot gives the farm's decisions. Divergences
	// counts the decisions that differed, by kind.
	Faithful    bool
	Divergences []DivergenceCount
}

func (Snapshot) telemetryRecord() {}

// DivergenceCount is how many decisions of a kind a replay changed.
type DivergenceCount struct {
	Kind  Divergence
	Count Int
}

// Pool is a pool as the policy sees it, without its attributes, node
// selectors or provider.
type Pool struct {
	Name        Pseudonym
	Slots, Used Int
	Cost        Money
	Burst       *Burst
}

// Burst is a pool's growth on request.
type Burst struct {
	MaxSlots               Int
	Provisioning           Nanos
	Cost                   Money
	Requested, Provisioned Int
}

// Project is a project's quotas and burst economics.
type Project struct {
	Name            Pseudonym
	Quotas          []Quota
	Weight          Real
	BurstBudgetLeft Money
	LateCostPerHour Money
}

// Quota is a project's quota in one pool.
type Quota struct {
	Pool                    Pseudonym
	Nominal, BorrowingLimit Int
}

// Job is a job as the policy sees it. Its messages, labels, templates,
// images, parameters, owner and the class's own name are not here.
type Job struct {
	ID, Project, Class                                 Pseudonym
	Submitted, Ready, Deadline, NotBefore, LastRunning RelTime
	After                                              []Pseudonym
	RequiredRate                                       Real
	Pools                                              []Pseudonym
	MaxParallel, SlotsPerUnit                          Int
	Running                                            []PoolSlots
	Runnable                                           []RunnableUnit
	RunnableTotal, Blocked, Unevaluated                Int
	Estimate                                           Estimate
	Hold, HoldStop                                     bool
	Boost                                              Real
	RunningUnits                                       []RunningUnit
	Preemptible, Checkpoints                           bool
	Sources                                            []PoolSources
	Priority, UnitsDone                                Int
	DoneSeconds                                        Real
}

// PoolSlots is a number of slots in a pool.
type PoolSlots struct {
	Pool  Pseudonym
	Slots Int
}

// UnitID names a unit: its job's pseudonym and its index, so positions and
// dependencies survive and unit names do not. A unit whose ID has no index
// is named by a pseudonym of its own.
type UnitID struct {
	Job   Pseudonym
	Index Int
	Whole Pseudonym
}

// RunnableUnit is a unit ready to run.
type RunnableUnit struct {
	Unit  UnitID
	Info  bool
	Slots Int
}

// RunningUnit is a running unit.
type RunningUnit struct {
	Unit                    UnitID
	Pool                    Pseudonym
	Slots                   Int
	Started, LastCheckpoint RelTime
	RestoreCost             Nanos
	Preemptions             Int
	Remaining               Nanos
}

// Estimate is the estimator's summary the policy reads.
type Estimate struct {
	W, Sigma, W90, W50, W80, W95, TPath Real
	Queued, Running, Unevaluated        Int
	Sampled, Measured, Open             Int
	Calibrated                          bool
}

// PoolSources is the ways to get a job's inputs to one pool.
type PoolSources struct {
	Pool    Pseudonym
	Sources []Source
}

// Source is one way to get a job's inputs to a pool's site. Its view and
// the derivation record's class are not here.
type Source struct {
	Via     Via
	Channel Channel
	Link    Pseudonym
	// Bytes keeps two significant figures.
	Bytes   Real
	Latency Nanos
	Cost    Money
	Work    Real
	Longest Nanos
}

// Link is a path staging moves inputs over.
type Link struct {
	Name           Pseudonym
	BytesPerSecond Real
	Committed      Real
}

// State is the policy's hysteresis.
type State struct {
	Pending      []Pending
	PreemptLog   []PoolTimes
	Surplus      []PoolTime
	ManualCheck  RelTime
	ManualBumped []Pseudonym
	Guard        []UnitTime
	GuardHit     []JobTime
	Took         []Took
}

// Pending is a preemption asked for and not yet carried out.
type Pending struct {
	Unit     UnitID
	Job, For Pseudonym
	Pool     Pseudonym
	Slots    Int
	At       RelTime
	Reason   Reason
}

// PoolTimes is a pool's recent preemptions.
type PoolTimes struct {
	Pool  Pseudonym
	Times []RelTime
}

// PoolTime is a time kept for a pool.
type PoolTime struct {
	Pool Pseudonym
	At   RelTime
}

// UnitTime is a time kept for a unit.
type UnitTime struct {
	Unit UnitID
	At   RelTime
}

// JobTime is a time kept for a job.
type JobTime struct {
	Job Pseudonym
	At  RelTime
}

// Took is the last preemption of Victim's units for For.
type Took struct {
	Victim, For Pseudonym
	At          RelTime
}

// Config is the policy's settings. Its time zone, which only changes how
// times read inside messages, is not here.
type Config struct {
	StarvationAfter, StarvationDeferMax     Nanos
	TieBand                                 Real
	InfoUnitsPerJob                         Int
	PreemptSlack, PreemptWindow             Nanos
	PreemptPerWindow, MaxPreemptionsPerUnit Int
	MaxLostWork, ScaleDownAfter             Nanos
	Burst, NoStage                          bool
	MinRemaining                            Nanos
	TailAware                               bool
	Planner                                 Planner
	NoInfoTier                              bool
	Surplus                                 Surplus
	SlackBand, RateSlack                    Nanos
	WranglerEvery, WranglerMargin           Nanos
	WranglerUnbump, WranglerStartAlarm      Nanos
}

// Decisions is what the policy decided, without its messages and reasons
// in words.
type Decisions struct {
	Dispatch []Dispatch
	Preempt  []Preempt
	Capacity []PoolSlots
	Bursts   []BurstDecision
	Jobs     []JobState
}

// Dispatch releases one unit into a pool.
type Dispatch struct {
	Job   Pseudonym
	Unit  UnitID
	Pool  Pseudonym
	Slots Int
	Tier  Tier
}

// Preempt stops one running unit.
type Preempt struct {
	Job      Pseudonym
	Unit     UnitID
	Pool     Pseudonym
	Slots    Int
	Reason   Reason
	For      Pseudonym
	Yield    bool
	LostWork Nanos
}

// BurstDecision is a burst the policy suggested.
type BurstDecision struct {
	Job, Project                Pseudonym
	Slots                       []PoolSlots
	Order                       []Pseudonym
	Cost, LateCost              Money
	Closes, Auto, Margin, Floor bool
}

// JobState is a job's objective state.
type JobState struct {
	Job                                                   Pseudonym
	Required, Attainable, RunningSlots, Share, GroupSlots Real
	Slack, LateBy                                         Nanos
	Feasible, AtRisk                                      bool
	ETA50, ETA90, Deadline, StartAfter                    RelTime
	Rank                                                  Int
	Burst                                                 bool
}
