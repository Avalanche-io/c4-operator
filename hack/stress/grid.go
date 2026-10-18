package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
)

// A Point is one farm and one workload of the stress grid. Every field is
// a number so that a point can be sampled, encoded on a command line and
// read back. Names follow the scenario spec in c4-hud's cmd/hudmock (spec.go)
// where the two overlap, so a point can be written out as a scenario
// (see spec.go here).
type Point struct {
	Name string // grid label: g017, or a corner's name

	// Farm.
	Nodes    float64 // owned CPU nodes, one render slot each
	GPUFrac  float64 // GPU nodes as a fraction of Nodes; four GPU slots per node
	Projects float64
	// QuotaFill is the share of each owned pool given out as nominal
	// quota (0: no quotas). QuotaSkew concentrates it: project i's share
	// is proportional to exp(-QuotaSkew*i). Borrow is each project's
	// borrowing limit as a fraction of the pool (negative: unlimited).
	QuotaFill, QuotaSkew, Borrow float64

	// Burst. BurstFrac is each burst pool's MaxNodes as a fraction of the
	// owned pool (0: no burst). Provisioning in minutes, BurstCost in
	// dollars per slot-hour (GPU slots cost three times as much).
	// BurstBudget is each project's daily budget as a fraction of a full
	// day of the burst pools at their limit, split over projects.
	// Interruptions is the spot reclaim rate per burst node-hour.
	BurstFrac, Provisioning, BurstCost, BurstBudget, Interruptions float64
	LateCost                                                       float64 // lateCostPerHour, dollars

	// Workload. Load is offered work per day over owned capacity per day,
	// by pool class. Days is the length of the arrival window.
	Load, Days float64
	// Arrivals: 0 steady, 1 dailies (17:00 to 23:30), 2 crunch (most of
	// the work in one four-hour window), 3 follow-the-sun (projects in
	// three time zones, each submitting in its own evening).
	Arrivals float64
	// Tightness is the median of (deadline - submit) over the job's
	// makespan at an eighth of its pool; SlackSpread the lognormal sigma
	// of that ratio. NoDue is the share of jobs without a deadline.
	Tightness, SlackSpread, NoDue float64
	// UnitMin is the median unit (frame) duration in minutes; Spread its
	// lognormal sigma within a job (the tail); Heavy the chance a job has
	// a heavy region, HeavyMult how much heavier it is.
	UnitMin, Spread, Heavy, HeavyMult float64
	// Depth and Width shape each shot's pipeline: Depth levels of Width
	// jobs, every job waiting for the whole level before it, the last
	// level one job. Expand is the share of render jobs whose frames are
	// generators that reveal their passes when they run.
	Depth, Width, Expand float64
	// EstErr is the lognormal sigma of each job's estimate bias.
	EstErr float64
	// MLShare is the share of GPU work in checkpointing training gangs of
	// up to Ranks GPUs, checkpointing every CkptMin minutes.
	MLShare, Ranks, CkptMin float64

	// Faults and nudges. FailRate is per attempt; NodeFaults per node-day.
	// BoostRate, HoldRate and MoveRate are the chance a job is boosted,
	// held (or hold-stopped) for a while, or has its deadline moved.
	FailRate, NodeFaults          float64
	BoostRate, HoldRate, MoveRate float64

	// Corners.
	PastDue float64 // every deadline already past at submission
	Gate    float64 // every shot waits on a gate job that is held throughout
	Huge    float64 // one job carrying the whole load
	// Spot points (Provider above 0: 1 AWS, 2 GCP, 3 Azure; see spot.go).
	// Each burst pool becomes an on-demand pool at BurstCost and a spot
	// pool at SpotDiscount off it. SpotBand is the AWS Spot Instance
	// Advisor band (0: <5% a month .. 4: >20%), Diversity the number of
	// capacity pools AWS allocates over, HazardScale a capacity-crunch
	// multiplier on the hazard, MaxPrice a maximum price as a fraction of
	// on-demand (0: none, -1 on Azure), Evict 1 for Azure's delete (0:
	// deallocate), Rebalance the chance AWS sends a rebalance
	// recommendation first, CkptWrite the seconds a training checkpoint
	// takes to write.
	Provider, SpotBand, Diversity, HazardScale, SpotDiscount float64
	MaxPrice, Evict, Rebalance, CkptWrite                    float64

	// Drain is how long the run goes on after the arrival window, in days
	// (0: one day). Jobs unfinished at the end and not yet due are left
	// out of on-time counts.
	Drain float64
}

type dim struct {
	name   string
	lo, hi float64
	log    bool
	integ  bool
	zero   float64 // values below this snap to 0 (a dimension that is off)
	f      func(*Point) *float64
}

// dims is the grid. quick narrows some ranges (scaleQuick).
var dims = []dim{
	{name: "nodes", lo: 1, hi: 3000, log: true, integ: true, f: func(p *Point) *float64 { return &p.Nodes }},
	{name: "gpuFrac", lo: 0, hi: 0.3, zero: 0.03, f: func(p *Point) *float64 { return &p.GPUFrac }},
	{name: "projects", lo: 1, hi: 12, integ: true, f: func(p *Point) *float64 { return &p.Projects }},
	{name: "quotaFill", lo: 0, hi: 1, zero: 0.15, f: func(p *Point) *float64 { return &p.QuotaFill }},
	{name: "quotaSkew", lo: 0, hi: 2, f: func(p *Point) *float64 { return &p.QuotaSkew }},
	{name: "borrow", lo: -0.5, hi: 1, f: func(p *Point) *float64 { return &p.Borrow }},
	{name: "burstFrac", lo: 0, hi: 1, zero: 0.15, f: func(p *Point) *float64 { return &p.BurstFrac }},
	{name: "provisioning", lo: 1, hi: 30, f: func(p *Point) *float64 { return &p.Provisioning }},
	{name: "burstCost", lo: 0.3, hi: 4, log: true, f: func(p *Point) *float64 { return &p.BurstCost }},
	{name: "burstBudget", lo: 0, hi: 1, zero: 0.08, f: func(p *Point) *float64 { return &p.BurstBudget }},
	{name: "interruptions", lo: 0, hi: 0.3, zero: 0.04, f: func(p *Point) *float64 { return &p.Interruptions }},
	{name: "lateCost", lo: 10, hi: 2000, log: true, f: func(p *Point) *float64 { return &p.LateCost }},
	{name: "load", lo: 0.3, hi: 2, f: func(p *Point) *float64 { return &p.Load }},
	{name: "days", lo: 0.5, hi: 3, f: func(p *Point) *float64 { return &p.Days }},
	{name: "arrivals", lo: 0, hi: 4, integ: true, f: func(p *Point) *float64 { return &p.Arrivals }},
	{name: "tightness", lo: 0.5, hi: 12, log: true, f: func(p *Point) *float64 { return &p.Tightness }},
	{name: "slackSpread", lo: 0, hi: 1.2, f: func(p *Point) *float64 { return &p.SlackSpread }},
	{name: "noDue", lo: 0, hi: 0.3, zero: 0.03, f: func(p *Point) *float64 { return &p.NoDue }},
	{name: "unitMin", lo: 2, hi: 90, log: true, f: func(p *Point) *float64 { return &p.UnitMin }},
	{name: "spread", lo: 0.1, hi: 1.5, f: func(p *Point) *float64 { return &p.Spread }},
	{name: "heavy", lo: 0, hi: 0.6, zero: 0.05, f: func(p *Point) *float64 { return &p.Heavy }},
	{name: "heavyMult", lo: 2, hi: 10, f: func(p *Point) *float64 { return &p.HeavyMult }},
	{name: "depth", lo: 1, hi: 5, integ: true, f: func(p *Point) *float64 { return &p.Depth }},
	{name: "width", lo: 1, hi: 4, integ: true, f: func(p *Point) *float64 { return &p.Width }},
	{name: "expand", lo: 0, hi: 0.6, zero: 0.05, f: func(p *Point) *float64 { return &p.Expand }},
	{name: "estErr", lo: 0, hi: 0.6, f: func(p *Point) *float64 { return &p.EstErr }},
	{name: "mlShare", lo: 0, hi: 0.8, zero: 0.05, f: func(p *Point) *float64 { return &p.MLShare }},
	{name: "ranks", lo: 1, hi: 9, integ: true, f: func(p *Point) *float64 { return &p.Ranks }},
	{name: "ckptMin", lo: 5, hi: 60, f: func(p *Point) *float64 { return &p.CkptMin }},
	{name: "failRate", lo: 0, hi: 0.1, zero: 0.005, f: func(p *Point) *float64 { return &p.FailRate }},
	{name: "nodeFaults", lo: 0, hi: 0.5, zero: 0.05, f: func(p *Point) *float64 { return &p.NodeFaults }},
	{name: "boostRate", lo: 0, hi: 0.3, zero: 0.02, f: func(p *Point) *float64 { return &p.BoostRate }},
	{name: "holdRate", lo: 0, hi: 0.3, zero: 0.02, f: func(p *Point) *float64 { return &p.HoldRate }},
	{name: "moveRate", lo: 0, hi: 0.3, zero: 0.02, f: func(p *Point) *float64 { return &p.MoveRate }},
	{name: "pastDue", lo: 0, hi: 0, f: func(p *Point) *float64 { return &p.PastDue }},
	{name: "gate", lo: 0, hi: 0, f: func(p *Point) *float64 { return &p.Gate }},
	{name: "huge", lo: 0, hi: 0, f: func(p *Point) *float64 { return &p.Huge }},
	{name: "drain", lo: 0, hi: 0, f: func(p *Point) *float64 { return &p.Drain }},
	{name: "provider", integ: true, f: func(p *Point) *float64 { return &p.Provider }},
	{name: "spotBand", integ: true, f: func(p *Point) *float64 { return &p.SpotBand }},
	{name: "diversity", integ: true, f: func(p *Point) *float64 { return &p.Diversity }},
	{name: "hazardScale", log: true, f: func(p *Point) *float64 { return &p.HazardScale }},
	{name: "spotDiscount", f: func(p *Point) *float64 { return &p.SpotDiscount }},
	{name: "maxPrice", f: func(p *Point) *float64 { return &p.MaxPrice }},
	{name: "evict", integ: true, f: func(p *Point) *float64 { return &p.Evict }},
	{name: "rebalance", f: func(p *Point) *float64 { return &p.Rebalance }},
	{name: "ckptWrite", f: func(p *Point) *float64 { return &p.CkptWrite }},
}

var arrivalNames = []string{"steady", "dailies", "crunch", "sun"}

// gridOpts bounds a grid: the quick grid keeps farms and windows small
// enough to run in CI.
type gridOpts struct {
	N        int
	Seed     uint64
	MaxNodes float64
	MaxDays  float64
	Drain    float64
}

// sample draws n points by Latin hypercube: each dimension's range is cut
// into n strata and every stratum is used once, in an independent random
// order per dimension.
func sample(o gridOpts) []Point {
	r := rand.New(rand.NewPCG(o.Seed, 0x6721d))
	pts := make([]Point, o.N)
	for i := range pts {
		pts[i].Name = fmt.Sprintf("g%03d", i)
	}
	for _, d := range dims {
		if d.lo == d.hi {
			continue
		}
		lo, hi := d.lo, d.hi
		if d.name == "nodes" && o.MaxNodes > 0 {
			hi = math.Min(hi, o.MaxNodes)
		}
		if d.name == "days" && o.MaxDays > 0 {
			hi = math.Min(hi, o.MaxDays)
			lo = math.Min(lo, hi/2)
		}
		perm := r.Perm(o.N)
		for i := range pts {
			u := (float64(perm[i]) + r.Float64()) / float64(o.N)
			*d.f(&pts[i]) = d.value(u, lo, hi)
		}
	}
	for i := range pts {
		pts[i].Drain = o.Drain
		// Big farms get shorter windows: a run's cost grows with the
		// work in it, and a few hours of a thousand-node farm already
		// hold thousands of jobs.
		if n := pts[i].Nodes; n > 200 {
			pts[i].Days = roundSig(math.Max(0.25, pts[i].Days*200/n))
			pts[i].Drain = 0.5
		}
	}
	return pts
}

func (d dim) value(u, lo, hi float64) float64 {
	var x float64
	switch {
	case d.log:
		x = math.Exp(math.Log(lo) + u*(math.Log(hi)-math.Log(lo)))
	default:
		x = lo + u*(hi-lo)
	}
	if d.integ {
		x = math.Min(math.Floor(x), hi-1)
		if d.name == "nodes" {
			x = math.Max(1, math.Round(math.Exp(math.Log(lo)+u*(math.Log(hi)-math.Log(lo)))))
		}
	}
	if d.zero > 0 && x < d.zero {
		return 0
	}
	return roundSig(x)
}

// roundSig keeps three significant digits, so encoded points stay short
// and decode to the same value.
func roundSig(x float64) float64 {
	if x == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'g', 3, 64), 64)
	return v
}

// base is a middle-of-the-road farm the corners start from.
func base(name string) Point {
	return Point{
		Name: name, Nodes: 24, GPUFrac: 0.15, Projects: 3, QuotaFill: 0.8, QuotaSkew: 0.5, Borrow: -1,
		BurstFrac: 0.5, Provisioning: 6, BurstCost: 1, BurstBudget: 0.3, Interruptions: 0, LateCost: 400,
		Load: 1, Days: 0.5, Arrivals: 1, Tightness: 2, SlackSpread: 0.4, NoDue: 0.1,
		UnitMin: 15, Spread: 0.5, Heavy: 0.3, HeavyMult: 4, Depth: 2, Width: 2, Expand: 0.3, EstErr: 0.2,
		MLShare: 0.4, Ranks: 4, CkptMin: 15, FailRate: 0.02,
	}
}

// corners are hand-picked edges of the grid.
func corners() []Point {
	var out []Point
	add := func(name string, f func(*Point)) {
		p := base(name)
		f(&p)
		out = append(out, p)
	}
	add("empty-farm", func(p *Point) { p.Nodes, p.BurstFrac = 0, 0 })
	add("cloud-only", func(p *Point) { p.Nodes, p.BurstFrac, p.BurstBudget = 0, 1, 1 })
	add("one-node", func(p *Point) { p.Nodes, p.GPUFrac, p.Projects, p.BurstFrac = 1, 0, 1, 0 })
	add("one-node-gpu", func(p *Point) { p.Nodes, p.GPUFrac, p.Projects = 1, 1, 2 })
	add("all-past-due", func(p *Point) { p.PastDue = 1 })
	add("zero-budget", func(p *Point) { p.BurstBudget, p.Load, p.Tightness, p.LateCost = 0, 1.6, 1, 2000 })
	add("zero-late-cost", func(p *Point) { p.LateCost, p.Load, p.Tightness = 0, 1.6, 1 })
	add("all-blocked", func(p *Point) { p.Gate, p.Depth = 1, 3 })
	add("single-huge-job", func(p *Point) { p.Huge, p.Load = 1, 1.5 })
	add("oversize-gang", func(p *Point) { p.GPUFrac, p.MLShare, p.Ranks = 0.1, 0.9, 9 })
	add("overload-2x-tight", func(p *Point) { p.Load, p.Tightness, p.SlackSpread = 2, 0.8, 0.1 })
	add("spot-storm", func(p *Point) { p.Interruptions, p.BurstFrac, p.BurstBudget, p.Load = 0.3, 1, 1, 1.5 })
	add("deep-dag", func(p *Point) { p.Depth, p.Width, p.Expand = 5, 3, 0.6 })
	add("nudge-storm", func(p *Point) { p.BoostRate, p.HoldRate, p.MoveRate = 0.3, 0.3, 0.3 })
	add("quota-skew", func(p *Point) { p.Projects, p.QuotaSkew, p.QuotaFill, p.Borrow, p.Load = 8, 2, 1, 0.1, 1.4 })
	return out
}

// encode writes a point as name=value pairs, skipping zeros.
func (p Point) encode() string {
	var parts []string
	for _, d := range dims {
		v := *d.f(&p)
		if v == 0 {
			continue
		}
		parts = append(parts, d.name+"="+strconv.FormatFloat(v, 'g', -1, 64))
	}
	return strings.Join(parts, ",")
}

// decodePoint reads encode's format. A corner name alone is that corner.
func decodePoint(s string) (Point, error) {
	for _, c := range corners() {
		if c.Name == s {
			return c, nil
		}
	}
	p := Point{Name: "point"}
	byName := map[string]dim{}
	for _, d := range dims {
		byName[d.name] = d
	}
	for _, kv := range strings.Split(s, ",") {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		d, known := byName[k]
		if !ok || !known {
			return p, fmt.Errorf("point: %q is not name=value with a known name", kv)
		}
		x, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return p, fmt.Errorf("point: %s: %v", k, err)
		}
		*d.f(&p) = x
	}
	return p, nil
}

// label is a short human description for tables.
func (p Point) label() string {
	return fmt.Sprintf("%d nodes, load %.2g, tight %.2g, %s", int(p.Nodes), p.Load, p.Tightness, arrivalNames[int(p.Arrivals)%len(arrivalNames)])
}

func sortPoints(ps []Point) {
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
}
