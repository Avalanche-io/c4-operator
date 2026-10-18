package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
)

const (
	minute = 60.0
	hour   = 3600.0
	day    = 24 * hour
)

// poolDef is a pool of the generated farm. Owned pools have nodes; burst
// pools start empty and grow by request, one slot per node.
type poolDef struct {
	name, class string
	nodes, spn  int // nodes and slots per node
	burst       *burstDef
	site        int // with Sites above 1, the CPU pool's site
}

func (p poolDef) slots() int { return p.nodes * p.spn }

type burstDef struct {
	max       int     // slots
	delay     float64 // seconds
	price     float64 // dollars per slot-hour
	interrupt float64 // reclaims per slot-hour
	spot      *spotDef
}

type projectDef struct {
	name     string
	tz       float64 // hours east of UTC
	nominal  map[string]int
	borrow   int // -1: unlimited
	budget   float64
	lateCost float64
	rank     int // static priority order, 0 first
	site     int // home site (Sites above 1)
}

type unitSpec struct {
	pos      int
	pass     int     // 0 generator, 1.. passes; the last pass is the beauty
	nominal  float64 // the unit's typical duration, without its heavy region or noise
	dur      float64 // true seconds
	slots    int
	deps     []int
	gen      bool
	children []int
	hidden   bool
}

type mlSpec struct {
	ckpt, restore, hazard float64 // seconds, seconds, per second
}

type nudgeKind uint8

const (
	nBoost nudgeKind = iota
	nHold
	nHoldStop
	nMove
)

type nudge struct {
	at   float64
	kind nudgeKind
	x    float64 // boost factor, hold length (seconds), or new window factor
}

type jobSpec struct {
	idx            int
	id, project    string
	kind, class    string
	pools          []string
	submit, due    float64
	hasDue         bool
	units          []unitSpec
	upstream       []*jobSpec
	level          int
	priority       int
	estBias        float64
	spread         float64
	failRate       float64
	ml             *mlSpec
	preemptible    bool
	nudges         []nudge
	work           float64 // true slot-seconds
	makespan       float64 // reference makespan for the deadline
	maxUnitSlots   int
	gate           bool
	final          bool // the last stage of its shot (or a job alone): its deadline is the shot's
	shot           int
	deadlineFactor float64
}

type workload struct {
	pt       Point
	seed     uint64
	pools    []poolDef
	projects []projectDef
	jobs     []*jobSpec
	horizon  float64
}

// build generates the farm and the work of a grid point. Every random draw
// comes from one stream in a fixed order, so a (point, seed) pair is
// always the same farm and the same work.
func build(pt Point, seed uint64) *workload {
	r := rand.New(rand.NewPCG(seed, 0xfa53))
	w := &workload{pt: pt, seed: seed}
	cpuSlots := int(pt.Nodes)
	gpuNodes := 0
	if pt.GPUFrac > 0 && pt.Nodes > 0 {
		gpuNodes = max(0, int(math.Round(pt.Nodes*pt.GPUFrac)))
	}
	// The reference capacity sizes the work: owned slots, or for a farm
	// with none, a small studio's worth.
	refCPU := float64(cpuSlots)
	refGPU := float64(gpuNodes * 4)
	if pt.Nodes == 0 {
		refCPU = 16
		refGPU = 4 * math.Round(16*pt.GPUFrac)
	}
	sites := max(1, int(pt.Sites))
	switch {
	case cpuSlots > 0 && sites > 1:
		for s := range sites {
			n := cpuSlots / sites
			if s < cpuSlots%sites {
				n++
			}
			if n > 0 {
				w.pools = append(w.pools, poolDef{name: fmt.Sprintf("cpu-s%d", s), class: "cpu", nodes: n, spn: 1, site: s})
			}
		}
	case cpuSlots > 0:
		w.pools = append(w.pools, poolDef{name: "cpu", class: "cpu", nodes: cpuSlots, spn: 1})
	}
	if gpuNodes > 0 {
		w.pools = append(w.pools, poolDef{name: "gpu", class: "gpu", nodes: gpuNodes, spn: 4})
	}
	if pt.BurstFrac > 0 {
		b := func(ref, price float64) *burstDef {
			return &burstDef{max: int(math.Ceil(pt.BurstFrac * ref)), delay: pt.Provisioning * minute, price: price, interrupt: pt.Interruptions}
		}
		for _, c := range []struct {
			class      string
			ref, price float64
		}{{"cpu", refCPU, pt.BurstCost}, {"gpu", refGPU, 3 * pt.BurstCost}} {
			if c.ref <= 0 {
				continue
			}
			if pt.Provider == 0 && c.class == "cpu" && sites > 1 {
				for s := range sites {
					bd := b(c.ref/float64(sites), c.price)
					w.pools = append(w.pools, poolDef{name: fmt.Sprintf("cpu-s%d-burst", s), class: c.class, spn: 1, burst: bd, site: s})
				}
				continue
			}
			if pt.Provider == 0 {
				w.pools = append(w.pools, poolDef{name: c.class + "-burst", class: c.class, spn: 1, burst: b(c.ref, c.price)})
				continue
			}
			od := b(c.ref, c.price)
			od.interrupt = 0
			sp := b(c.ref, c.price*(1-pt.SpotDiscount))
			sp.interrupt = 0
			sp.spot = spotFor(pt, c.price)
			w.pools = append(w.pools,
				poolDef{name: c.class + "-spot", class: c.class, spn: 1, burst: sp},
				poolDef{name: c.class + "-od", class: c.class, spn: 1, burst: od})
		}
	}
	fullDay := 0.0
	for _, pd := range w.pools {
		if pd.burst != nil {
			fullDay += float64(pd.burst.max) * pd.burst.price * 24
		}
	}
	np := max(1, int(pt.Projects))
	shares := make([]float64, np)
	tot := 0.0
	for i := range shares {
		shares[i] = math.Exp(-pt.QuotaSkew * float64(i))
		tot += shares[i]
	}
	for i := range np {
		pd := projectDef{name: fmt.Sprintf("p%d", i), nominal: map[string]int{}, borrow: -1, rank: i, site: i % sites}
		if pt.Arrivals == 3 {
			pd.tz = float64((i % 3) * 8)
		}
		if pt.QuotaFill > 0 {
			for _, pl := range w.pools {
				if pl.burst == nil {
					pd.nominal[pl.name] = int(math.Floor(pt.QuotaFill * float64(pl.slots()) * shares[i] / tot))
				}
			}
			if pt.Borrow >= 0 {
				pd.borrow = int(math.Round(pt.Borrow * float64(cpuSlots)))
			}
		}
		pd.budget = pt.BurstBudget * fullDay / float64(np)
		if pt.LateCost > 0 {
			pd.lateCost = pt.LateCost * (0.5 + r.Float64())
		}
		w.projects = append(w.projects, pd)
	}

	g := &gen{w: w, r: r, pt: pt, refCPU: refCPU, refGPU: refGPU}
	target := pt.Load * pt.Days * day
	switch {
	case pt.Huge > 0:
		g.hugeJob(target * refCPU)
	default:
		for done := 0.0; done < target*refCPU; {
			done += g.shot("cpu")
		}
		gpuTarget := target * refGPU
		for done := 0.0; done < gpuTarget*(1-pt.MLShare); {
			done += g.shot("gpu")
		}
		for done := 0.0; done < gpuTarget*pt.MLShare; {
			done += g.mlJob()
		}
	}
	sort.SliceStable(w.jobs, func(i, j int) bool {
		if w.jobs[i].submit != w.jobs[j].submit {
			return w.jobs[i].submit < w.jobs[j].submit
		}
		return w.jobs[i].id < w.jobs[j].id
	})
	last := 0.0
	for i, js := range w.jobs {
		js.idx = i
		last = math.Max(last, js.submit)
	}
	drain := pt.Drain
	if drain <= 0 {
		drain = 1
	}
	w.horizon = math.Max(last, pt.Days*day) + drain*day
	return w
}

type gen struct {
	w              *workload
	r              *rand.Rand
	pt             Point
	refCPU, refGPU float64
	n, shots       int
}

func (g *gen) lognorm(median, sigma float64) float64 {
	return median * math.Exp(sigma*g.r.NormFloat64())
}

func (g *gen) id(project string) string {
	g.n++
	return fmt.Sprintf("%s-%05d", project, g.n)
}

// arrival is a submission time for a project under the point's pattern.
func (g *gen) arrival(pd projectDef) float64 {
	pt := g.pt
	span := pt.Days * day
	evening := func(d int, tz float64) float64 {
		t := float64(d)*day + (17+6.5*g.r.Float64()-tz)*hour
		for t < 0 {
			t += day
		}
		return t
	}
	switch int(pt.Arrivals) {
	case 1:
		return evening(int(g.r.Float64()*math.Max(1, math.Floor(pt.Days))), 0)
	case 2:
		if g.r.Float64() < 0.7 {
			d := math.Floor(pt.Days / 2)
			return d*day + (14+4*g.r.Float64())*hour
		}
	case 3:
		return evening(int(g.r.Float64()*math.Max(1, math.Floor(pt.Days))), pd.tz)
	}
	return g.r.Float64() * span
}

func (g *gen) project() projectDef {
	return g.w.projects[g.r.IntN(len(g.w.projects))]
}

var stageCost = []float64{1.5, 1, 0.6, 0.35, 0.2}

// shot generates one shot's pipeline of jobs on a pool class and returns
// its total work in slot-seconds.
func (g *gen) shot(class string) float64 {
	pt := g.pt
	pd := g.project()
	at := g.arrival(pd)
	g.shots++
	depth, width := max(1, int(pt.Depth)), max(1, int(pt.Width))
	if class == "gpu" {
		depth, width = 1, 1
	}
	unit := g.lognorm(pt.UnitMin*minute, 0.4)
	if class == "gpu" {
		unit /= 3
	}
	ref := g.refCPU
	if class == "gpu" {
		ref = g.refGPU
	}
	// Shots are at most about a tenth of the window's work, so that a
	// small farm's load is made of several shots rather than one.
	perFrame := pt.UnitMin * minute / map[bool]float64{true: 3, false: 1}[class == "gpu"]
	stages := 0.0
	for l := range depth {
		n := float64(width)
		if l == depth-1 {
			n = 1
		}
		stages += n * stageCost[min(l, len(stageCost)-1)]
	}
	if pt.Expand > 0 {
		stages *= 1.3
	}
	median := math.Min(60, math.Max(2, pt.Load*pt.Days*day*ref/10/(perFrame*stages)))
	frames := int(math.Max(1, math.Min(1000, g.lognorm(median, 0.6))))
	par := math.Max(1, math.Ceil(ref/8))
	f := pt.Tightness * math.Exp(pt.SlackSpread*g.r.NormFloat64())
	noDue := g.r.Float64() < pt.NoDue
	var levels [][]*jobSpec
	var gate *jobSpec
	if pt.Gate > 0 {
		gate = g.newJob(pd, "gate", class, at, 0)
		gate.units = []unitSpec{{nominal: minute, dur: minute, slots: 1}}
		gate.work, gate.makespan, gate.maxUnitSlots = minute, minute, 1
		gate.gate = true
		gate.nudges = []nudge{{at: at, kind: nHold, x: 1e9}}
	}
	total := 0.0
	for l := range depth {
		n := width
		if l == depth-1 {
			n = 1
		}
		var lv []*jobSpec
		for range n {
			js := g.newJob(pd, fmt.Sprintf("stage%d", l), class, at, l)
			g.renderUnits(js, frames, unit*stageCost[min(l, len(stageCost)-1)])
			js.makespan = math.Max(js.makespan, js.work/math.Min(par, float64(len(js.units))))
			switch {
			case l > 0:
				js.upstream = append(js.upstream, levels[l-1]...)
			case gate != nil:
				js.upstream = append(js.upstream, gate)
			}
			total += js.work
			lv = append(lv, js)
		}
		if l == depth-1 {
			for _, js := range lv {
				js.final = true
			}
		}
		levels = append(levels, lv)
	}
	// The shot is due f times its reference makespan after submission;
	// each stage is due early enough for the stages after it.
	lm := make([]float64, depth)
	shotM := 0.0
	for l, lv := range levels {
		for _, js := range lv {
			lm[l] = math.Max(lm[l], js.makespan)
		}
		shotM += lm[l]
	}
	due := at + f*shotM
	after := 0.0
	for l := depth - 1; l >= 0; l-- {
		for _, js := range levels[l] {
			stageDue := due - f*after
			if pt.FlatDue > 0 {
				stageDue = due
			}
			g.setDue(js, stageDue, noDue, f)
		}
		after += lm[l]
	}
	return total
}

func (g *gen) newJob(pd projectDef, kind, class string, at float64, level int) *jobSpec {
	js := &jobSpec{
		id: g.id(pd.name), project: pd.name, kind: kind, class: class, submit: at, level: level,
		pools: g.poolsFor(pd, class), estBias: math.Exp(g.pt.EstErr * g.r.NormFloat64()),
		spread: g.pt.Spread, failRate: g.pt.FailRate, preemptible: true, shot: g.shots,
		priority: 50 + 10*(len(g.w.projects)-pd.rank) + 5*level,
	}
	g.w.jobs = append(g.w.jobs, js)
	return js
}

// poolsFor is the pools a new job of project pd may use: with several
// sites, its home site's CPU pools, and with probability Spill also the
// next site's owned CPUs.
func (g *gen) poolsFor(pd projectDef, class string) []string {
	sites := max(1, int(g.pt.Sites))
	if class != "cpu" || sites == 1 {
		return poolsOf(g.w, class)
	}
	var out []string
	for _, p := range g.w.pools {
		if p.class == "cpu" && p.site == pd.site {
			out = append(out, p.name)
		}
	}
	if g.pt.Spill > 0 && g.r.Float64() < g.pt.Spill {
		next := fmt.Sprintf("cpu-s%d", (pd.site+1)%sites)
		for _, p := range g.w.pools {
			if p.name == next {
				out = append(out, next)
			}
		}
	}
	if len(out) == 0 {
		return poolsOf(g.w, class)
	}
	return out
}

func poolsOf(w *workload, class string) []string {
	var out []string
	for _, p := range w.pools {
		if p.class == class {
			out = append(out, p.name)
		}
	}
	if len(out) == 0 {
		out = []string{class} // a pool the farm does not have
	}
	return out
}

// renderUnits fills a render job: one unit per frame, or with expansion a
// generator per frame that reveals two passes, the second waiting for the
// first.
func (g *gen) renderUnits(js *jobSpec, frames int, unit float64) {
	pt := g.pt
	lo, hi := -1, -1
	mult := 1.0
	if g.r.Float64() < pt.Heavy {
		l := max(1, int(float64(frames)*(0.1+0.2*g.r.Float64())))
		lo = g.r.IntN(frames - l + 1)
		hi = lo + l - 1
		mult = pt.HeavyMult
	}
	expand := g.r.Float64() < pt.Expand
	dur := func(nominal float64, pos int) float64 {
		d := nominal * math.Exp(pt.Spread*g.r.NormFloat64())
		if pos >= lo && pos <= hi {
			d *= mult
		}
		return math.Max(1, d)
	}
	longest := 0.0
	for pos := range frames {
		if !expand {
			u := unitSpec{pos: pos, pass: 1, nominal: unit, dur: dur(unit, pos), slots: 1}
			js.units = append(js.units, u)
			js.work += u.dur
			longest = math.Max(longest, u.dur)
			continue
		}
		gi := len(js.units)
		gd := g.lognorm(45, 0.3)
		js.units = append(js.units, unitSpec{pos: pos, pass: 0, nominal: 45, dur: gd, slots: 1, gen: true})
		a := unitSpec{pos: pos, pass: 1, nominal: 0.3 * unit, dur: dur(0.3*unit, pos), slots: 1, hidden: true, deps: []int{gi}}
		b := unitSpec{pos: pos, pass: 2, nominal: unit, dur: dur(unit, pos), slots: 1, hidden: true, deps: []int{gi, gi + 1}}
		js.units = append(js.units, a, b)
		js.units[gi].children = []int{gi + 1, gi + 2}
		js.work += gd + a.dur + b.dur
		longest = math.Max(longest, gd+a.dur+b.dur)
	}
	js.makespan = longest
	js.maxUnitSlots = 1
}

func (g *gen) setDue(js *jobSpec, due float64, noDue bool, f float64) {
	pt := g.pt
	js.deadlineFactor = f
	if noDue {
		return
	}
	js.hasDue = true
	js.due = due
	if pt.PastDue > 0 {
		js.due = js.submit - hour
	}
	g.addNudges(js)
}

func (g *gen) addNudges(js *jobSpec) {
	pt := g.pt
	window := math.Max(js.due-js.submit, 10*minute)
	if g.r.Float64() < pt.BoostRate {
		js.nudges = append(js.nudges, nudge{at: js.submit + g.r.Float64()*window/2, kind: nBoost, x: 1.3 + 0.7*g.r.Float64()})
	}
	if g.r.Float64() < pt.HoldRate {
		k := nHold
		if g.r.Float64() < 0.3 {
			k = nHoldStop
		}
		js.nudges = append(js.nudges, nudge{at: js.submit + g.r.Float64()*hour, kind: k, x: (0.5 + 2.5*g.r.Float64()) * hour})
	}
	if g.r.Float64() < pt.MoveRate {
		js.nudges = append(js.nudges, nudge{at: js.submit + g.r.Float64()*window/2, kind: nMove, x: 0.6 + g.r.Float64()})
	}
}

// mlJob is a training run: one gang unit of 1 to Ranks GPUs that
// checkpoints, and returns its work.
func (g *gen) mlJob() float64 {
	pt := g.pt
	pd := g.project()
	at := g.arrival(pd)
	g.shots++
	var sizes []int
	for _, s := range []int{1, 2, 4, 8, 16} {
		if s <= int(pt.Ranks) || (s == 16 && pt.Ranks > 8) {
			sizes = append(sizes, s)
		}
	}
	if len(sizes) == 0 {
		sizes = []int{1}
	}
	ranks := sizes[g.r.IntN(len(sizes))]
	length := g.lognorm(math.Max(hour, 8*pt.UnitMin*minute), 0.5)
	js := g.newJob(pd, "ml", "gpu", at, 0)
	js.pools = poolsOf(g.w, "gpu")
	js.priority = 30
	js.final = true
	js.ml = &mlSpec{ckpt: math.Max(5, pt.CkptMin) * minute, restore: 3 * minute, hazard: pt.FailRate * 0.5 / hour}
	js.units = []unitSpec{{nominal: length, dur: length, slots: ranks}}
	js.work = length * float64(ranks)
	js.makespan = length
	js.maxUnitSlots = ranks
	f := pt.Tightness * math.Exp(pt.SlackSpread*g.r.NormFloat64())
	g.setDue(js, at+f*length, g.r.Float64() < pt.NoDue, f)
	return js.work
}

// hugeJob is the single-huge-job corner: all of the load in one job.
func (g *gen) hugeJob(work float64) {
	pd := g.w.projects[0]
	js := g.newJob(pd, "huge", "cpu", 0, 0)
	unit := g.pt.UnitMin * minute
	frames := max(1, int(work/unit))
	js.final = true
	g.renderUnits(js, frames, unit)
	par := math.Max(1, g.refCPU)
	js.makespan = math.Max(js.makespan, js.work/par)
	g.setDue(js, g.pt.Tightness*js.makespan, false, g.pt.Tightness)
}
