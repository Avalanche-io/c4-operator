package sched

import "time"

// Link is a path between sites that staging moves bytes over: a zone's
// network, an inter-region route, the internet, a private interconnect,
// or a shipment lane. Jobs staging over one link share it: a new transfer
// waits behind the bytes already committed to it (see Source).
type Link struct {
	Name string
	// BytesPerSecond is the link's usable throughput.
	BytesPerSecond float64
	// Committed is bytes already queued or moving on it: staging the
	// caller is doing. Decide adds the bytes of the stages it approves.
	Committed float64
}

// Source (experimental) is one way to have a job's inputs at a pool's
// site, as the caller (which knows the sites, the tiers and the
// derivation records) offers it. Via says how:
//
//   - "move": copy the bytes the site lacks over Link.
//   - "recompute": rebuild missing outputs at the pool from inputs the site
//     has or can move more cheaply (Bytes over Link), then run Work
//     slot-seconds there. Offered for an exact process, or a validated one
//     where the consumer accepts a different output; never for volatile.
//   - "reuse": an identical output already exists (exact and verified,
//     from the derivation record); only its bytes move.
//   - "staging": a stage already approved and under way; Latency is what
//     is left of it and Cost is 0. It is never staged again.
//
// The delay before the job can start at the pool is Latency, plus the
// time for Bytes to clear Link behind what is committed to it, plus a
// recompute's Longest unit. Cost is the caller's whole price: egress at
// the sending end, ingress or processing fees at the receiving end where
// the provider charges them, outputs back to where they are needed, and a
// shipment's fee; Decide adds a recompute's Work at the pool's price.
type Source struct {
	Via     string
	Channel string // "same-zone", "inter-region", "internet", "interconnect", "shipment"
	Link    string
	Bytes   float64
	Latency time.Duration
	Cost    float64
	Work    float64 // slot-seconds, at p90
	Longest time.Duration
	// Class is the process's class as the derivation record sees it
	// ("exact", "validated"); experimental, reported only.
	Class string
}

// Stage moves a job's inputs to a pool at another site so the job can
// run there: the cheapest way, among the job's Sources for that pool and
// the burst pools it may use, to restore its deadline. A stage is
// requested automatically under the same rules as a burst (within the
// project's remaining budget and below the lateness cost it prevents);
// otherwise it is reported for approval. Stage and burst spend one
// budget.
type Stage struct {
	Job, Project, Pool string
	Source             Source
	// Delay is how long until the job can start at the pool; Slots is the
	// pool's spare slots the plan counts on.
	Delay time.Duration
	Slots int
	// Cost is MoveCost plus RunCost: MoveCost is Source.Cost (the bytes
	// and fees), RunCost a recompute's work and the job's work moved there,
	// at the pool's price. For a stage that goes with a burst (to a burst
	// pool at another site) the burst carries the run cost, and Cost is the
	// move alone.
	Cost, MoveCost, RunCost float64
	// Closes says the stage restores the job's deadline.
	Closes, Auto bool
	Why          string
	// LateCost is the lateness cost it prevents.
	LateCost float64
}
