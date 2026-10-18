package main

import (
	"container/heap"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// The engine is a discrete-event simulation of the farm. Time is seconds
// from Monday 00:00 of the simulated week. The policy under test is called
// through the same function signature the controller uses, on a snapshot
// the engine builds, and its decisions are applied as the controller
// would: units start, preempted units stop (at once, or at their next
// checkpoint when asked to yield), and burst pools grow after their
// provisioning delay.

var weekStart = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC) // a Monday

const (
	coalesce  = 10.0  // seconds between decisions, at most one per window
	heartbeat = 60.0  // a decision at least this often while work exists
	stale     = 300.0 // a job's estimate is refreshed at least this often while it runs
	horizon   = 11 * day
)

type unitState uint8

const (
	stHidden unitState = iota
	stBlocked
	stRunnable
	stRunning
	stDone
)

type unit struct {
	spec        *unitSpec
	j           *job
	idx         int
	id          string
	state       unitState
	waiting     int
	dependents  []int
	attempt     int
	preemptions int
	pool        *pool
	start       float64
	restore     float64
	endAt       float64
	retained    float64 // ML progress secured in checkpoints, seconds
	yieldAt     float64
	// finStart and finEnd bound the attempt that finished (etaeval.go).
	finStart, finEnd float64
	kidsLeft         int
	kidsWork         float64
	kidsLongest      float64
}

type job struct {
	spec        *jobSpec
	idx         int
	units       []*unit
	runnable    []*unit // dispatch order
	runningU    []*unit
	runSlots    map[string]int
	done        int
	blocked     int
	uneval      int
	doneSec     float64
	est         *estimate.Estimator
	sum         estimate.Summary
	sumAt       float64
	dirty       bool
	finish      float64
	lastRunning float64
	// deadline is the job's deadline in this run: the workload's, unless
	// an ETA experiment arm sets another (etaeval.go).
	deadline float64
	eta      *etaJob
}

type batch struct {
	n     int
	ready float64
}

type pool struct {
	def       poolDef
	slots     int // owned capacity, or provisioned burst slots
	used      int
	requested int
	batches   []*batch
	busy      float64 // slot-seconds in use inside the week
	bill      float64 // dollars
	last      float64
}

func (p *pool) meter(now float64) {
	if now <= p.last {
		return
	}
	lo, hi := p.last, math.Min(now, 7*day)
	if hi > lo {
		p.busy += float64(p.used) * (hi - lo)
	}
	if p.def.burst != nil {
		p.bill += float64(p.requested) * (now - p.last) * p.def.burst.price / hour
	}
	p.last = now
}

type evKind uint8

const (
	evArrive evKind = iota
	evFinish
	evFail
	evYield
	evProvision
	evDecide
	evTick
)

type event struct {
	t       float64
	seq     int
	kind    evKind
	j       *job
	u       *unit
	attempt int
	p       *pool
	b       *batch
}

type evHeap []*event

func (h evHeap) Len() int { return len(h) }
func (h evHeap) Less(i, j int) bool {
	if h[i].t != h[j].t {
		return h[i].t < h[j].t
	}
	return h[i].seq < h[j].seq
}
func (h evHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *evHeap) Push(x any)   { *h = append(*h, x.(*event)) }
func (h *evHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type policySpec struct {
	name     string
	fn       sched.Policy
	burst    bool
	usesEs   bool
	ablation bool                // reported separately from the five policies
	tweak    func(*sched.Config) // config changes for this policy
}

var policies = []policySpec{
	{name: "manual", fn: sched.Manual},
	{name: "fifo", fn: sched.FIFO},
	{name: "edf", fn: sched.EDF},
	{name: "objective", fn: sched.Decide, usesEs: true},
	{name: "objective+burst", fn: sched.Decide, burst: true, usesEs: true},
	{name: "objective, no information tier", fn: sched.Decide, usesEs: true, ablation: true,
		tweak: func(c *sched.Config) { c.NoInfoTier = true }},
	{name: "objective, fair-share surplus", fn: sched.Decide, usesEs: true, ablation: true,
		tweak: func(c *sched.Config) { c.Surplus = sched.SurplusFairShare }},
	{name: "objective, DESIGN 9.9 as written", fn: sched.Decide, usesEs: true, ablation: true,
		tweak: func(c *sched.Config) {
			c.Planner, c.TailAware, c.Surplus = sched.PlanPerJob, false, sched.SurplusFairShare
		}},
}

type engine struct {
	farm      farm
	lastTrace float64
	w         *workload
	hist      *history
	seed      uint64
	pol       policySpec
	pcfg      sched.Config
	pst       sched.State
	eta       *etaArm // nil outside the ETA experiment
	now       float64
	q         evHeap
	seq       int
	jobs      []*job
	active    []*job
	pools     []*pool
	byName    map[string]*pool
	byID      map[string]*unit
	spent     map[string]float64
	spentDay  int
	lastDec   float64
	decAt     float64 // time of the scheduled decision, or -1
	left      int     // jobs not finished
	res       *runResult
}

func newEngine(w *workload, h *history, seed uint64, pol policySpec, f farm) *engine {
	e := &engine{w: w, hist: h, seed: seed, pol: pol, pcfg: sched.DefaultConfig(), farm: f,
		byName: map[string]*pool{}, byID: map[string]*unit{}, spent: map[string]float64{},
		lastDec: -coalesce, decAt: -1}
	e.pcfg.Burst = pol.burst
	if pol.tweak != nil {
		pol.tweak(&e.pcfg)
	}
	e.res = newResult(pol.name, seed)
	for _, pd := range f.pools {
		p := &pool{def: pd, slots: pd.slots}
		e.pools = append(e.pools, p)
		e.byName[pd.name] = p
	}
	for i, js := range w.jobs {
		j := &job{spec: js, idx: i, runSlots: map[string]int{}, finish: -1, lastRunning: -1, dirty: true, deadline: js.deadline}
		for k := range js.units {
			us := &js.units[k]
			u := &unit{spec: us, j: j, idx: k, id: fmt.Sprintf("%s/%d", js.id, k), state: stHidden}
			j.units = append(j.units, u)
			e.byID[u.id] = u
		}
		for k, us := range js.units {
			for _, d := range us.deps {
				j.units[d].dependents = append(j.units[d].dependents, k)
			}
		}
		e.jobs = append(e.jobs, j)
		e.push(&event{t: js.submit, kind: evArrive, j: j})
	}
	e.left = len(e.jobs)
	e.push(&event{t: 0, kind: evTick})
	return e
}

func (e *engine) push(ev *event) {
	e.seq++
	ev.seq = e.seq
	heap.Push(&e.q, ev)
}

func (e *engine) requestDecide() {
	if e.decAt >= 0 {
		return
	}
	e.decAt = math.Max(e.now, e.lastDec+coalesce)
	e.push(&event{t: e.decAt, kind: evDecide})
}

func (e *engine) run() *runResult {
	for e.q.Len() > 0 {
		ev := heap.Pop(&e.q).(*event)
		if ev.t > horizon {
			break
		}
		e.now = ev.t
		switch ev.kind {
		case evArrive:
			e.arrive(ev.j)
		case evFinish, evFail, evYield:
			u := ev.u
			if u.state != stRunning || u.attempt != ev.attempt {
				continue
			}
			e.stop(u, ev.kind, false)
		case evProvision:
			if ev.b.n > 0 {
				ev.p.meter(e.now)
				ev.p.slots += ev.b.n
				ev.b.n = 0
				e.requestDecide()
			}
		case evDecide:
			e.decAt = -1
			e.decide()
		case evTick:
			if e.left > 0 {
				e.requestDecide()
				e.push(&event{t: e.now + heartbeat, kind: evTick})
			}
		}
		if e.left == 0 {
			break
		}
	}
	e.finishRun()
	return e.res
}

func (e *engine) arrive(j *job) {
	j.est = e.hist.estimatorFor(j.spec)
	if e.eta != nil {
		e.eta.arrive(e, j)
	}
	for _, u := range j.units {
		if u.spec.hidden {
			continue
		}
		e.reveal(u)
	}
	e.active = append(e.active, j)
	e.requestDecide()
}

func (e *engine) reveal(u *unit) {
	u.waiting = len(u.spec.deps)
	if u.spec.genKey != "" {
		u.j.uneval++
	}
	if u.waiting > 0 {
		u.state = stBlocked
		u.j.blocked++
		return
	}
	e.makeRunnable(u)
}

func (j *job) less(a, b *unit) bool {
	ga, gb := a.spec.genKey != "", b.spec.genKey != ""
	if ga != gb {
		return ga
	}
	ra, rb := j.spec.rank[a.spec.pos], j.spec.rank[b.spec.pos]
	if ra != rb {
		return ra < rb
	}
	if a.spec.sub != b.spec.sub {
		return a.spec.sub < b.spec.sub
	}
	return a.idx < b.idx
}

func (e *engine) makeRunnable(u *unit) {
	j := u.j
	u.state = stRunnable
	i := sort.Search(len(j.runnable), func(i int) bool { return j.less(u, j.runnable[i]) })
	j.runnable = append(j.runnable, nil)
	copy(j.runnable[i+1:], j.runnable[i:])
	j.runnable[i] = u
	j.dirty = true
}

func (e *engine) removeRunnable(u *unit) {
	j := u.j
	for i, x := range j.runnable {
		if x == u {
			j.runnable = append(j.runnable[:i], j.runnable[i+1:]...)
			return
		}
	}
	panic("unit not runnable: " + u.id)
}

// urand is a deterministic uniform draw for (job, unit, attempt, k), so a
// unit fails the same way under every policy.
func (e *engine) urand(u *unit, k int) float64 {
	x := e.seed*0x9E3779B97F4A7C15 ^ uint64(u.j.idx)<<32 ^ uint64(u.idx)<<12 ^ uint64(u.attempt)<<4 ^ uint64(k)
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return float64(x>>11) / (1 << 53)
}

func (e *engine) start(u *unit, p *pool) {
	j := u.j
	e.removeRunnable(u)
	slots := u.spec.slots
	if p.slots-p.used < slots {
		panic(fmt.Sprintf("policy %s overfilled pool %s with %s", e.pol.name, p.def.name, u.id))
	}
	p.meter(e.now)
	p.used += slots
	u.state = stRunning
	u.pool = p
	u.start = e.now
	u.attempt++
	u.yieldAt = 0
	j.runSlots[p.def.name] += slots
	j.runningU = append(j.runningU, u)
	j.dirty = true
	if ml := j.spec.ml; ml != nil {
		u.restore = 0
		if u.retained > 0 {
			u.restore = ml.restore
		}
		rem := u.spec.dur - u.retained
		tf := -math.Log(1-e.urand(u, 1)) / ml.hazard
		if tf < rem {
			u.endAt = e.now + u.restore + tf
			e.push(&event{t: u.endAt, kind: evFail, u: u, attempt: u.attempt})
			return
		}
		u.endAt = e.now + u.restore + rem
		e.push(&event{t: u.endAt, kind: evFinish, u: u, attempt: u.attempt})
		return
	}
	if u.attempt < 4 && e.urand(u, 1) < j.spec.failProb {
		u.endAt = e.now + e.urand(u, 2)*u.spec.dur
		e.push(&event{t: u.endAt, kind: evFail, u: u, attempt: u.attempt})
		return
	}
	u.endAt = e.now + u.spec.dur
	e.push(&event{t: u.endAt, kind: evFinish, u: u, attempt: u.attempt})
}

func isGPU(p *pool) bool {
	return p.def.name == "gpu" || p.def.name == "gpu-spot" || p.def.name == "gpu-od"
}

// stop ends a running unit: it finished, failed, yielded at a checkpoint,
// or (killed) was stopped by a preemption without a checkpoint.
func (e *engine) stop(u *unit, how evKind, killed bool) {
	j := u.j
	p := u.pool
	slots := float64(u.spec.slots)
	p.meter(e.now)
	p.used -= u.spec.slots
	j.runSlots[p.def.name] -= u.spec.slots
	for i, x := range j.runningU {
		if x == u {
			j.runningU = append(j.runningU[:i], j.runningU[i+1:]...)
			break
		}
	}
	j.lastRunning = e.now
	j.dirty = true
	elapsed := e.now - u.start
	gpu := isGPU(p)
	ml := j.spec.ml
	useful, lost := 0.0, 0.0
	switch {
	case how == evFinish && ml != nil:
		useful = (u.spec.dur - u.retained) * slots
		lost = u.restore * slots
	case how == evFinish:
		useful = elapsed * slots
	case how == evYield:
		prog := math.Max(0, elapsed-u.restore)
		u.retained += prog
		useful = prog * slots
		lost = u.restore * slots
	case ml != nil: // failure or kill: keep the last checkpoint
		prog := math.Max(0, elapsed-u.restore)
		kept := math.Floor(prog/ml.ckpt) * ml.ckpt
		u.retained += kept
		useful = kept * slots
		lost = (elapsed - kept) * slots
	default:
		lost = elapsed * slots
	}
	e.res.work(j.spec.kind, gpu, useful, lost)
	if how == evYield || killed {
		u.preemptions++
		e.res.preemptions++
	}
	u.pool = nil
	if how != evFinish {
		e.makeRunnable(u)
		e.requestDecide()
		return
	}
	u.state = stDone
	u.finStart, u.finEnd = u.start, e.now
	e.complete(u, elapsed)
	e.requestDecide()
}

func (e *engine) complete(u *unit, elapsed float64) {
	j := u.j
	j.done++
	j.doneSec += elapsed
	if j.spec.ml == nil {
		j.est.Observe(u.spec.key, u.spec.pos, elapsed)
	}
	if u.spec.genKey != "" {
		j.uneval--
		u.kidsLeft = len(u.spec.children)
		j.est.Expanded(u.spec.genKey, len(u.spec.children))
		for _, c := range u.spec.children {
			e.reveal(j.units[c])
		}
	}
	if pi := u.spec.parent; pi >= 0 {
		pu := j.units[pi]
		pu.kidsLeft--
		pu.kidsWork += elapsed * float64(u.spec.slots)
		pu.kidsLongest = math.Max(pu.kidsLongest, elapsed)
		if pu.kidsLeft == 0 {
			j.est.ExpansionDone(pu.spec.genKey, pu.kidsWork, pu.kidsLongest)
		}
	}
	for _, d := range u.dependents {
		du := j.units[d]
		du.waiting--
		if du.waiting == 0 && du.state == stBlocked {
			j.blocked--
			e.makeRunnable(du)
		}
	}
	if j.done == len(j.units) {
		j.finish = e.now
		e.left--
		e.res.jobDone(j)
		if e.eta != nil {
			e.eta.done(e, j)
		}
		for i, x := range e.active {
			if x == j {
				e.active = append(e.active[:i], e.active[i+1:]...)
				break
			}
		}
	}
}

func (e *engine) at(t float64) time.Time {
	return weekStart.Add(time.Duration(t * float64(time.Second)))
}

func (e *engine) snapshot() sched.Snapshot {
	now := e.at(e.now)
	s := sched.Snapshot{Now: now}
	free := map[string]int{}
	for _, p := range e.pools {
		sp := sched.Pool{Name: p.def.name, Slots: p.slots, Used: p.used}
		if b := p.def.burst; b != nil {
			sp.CostPerSlotHour = b.price
			sp.Burst = &sched.Burst{MaxSlots: b.max, Provisioning: b.delay, CostPerSlotHour: b.price,
				Requested: p.requested, Provisioned: p.slots}
		}
		free[p.def.name] = p.slots - p.used
		s.Pools = append(s.Pools, sp)
	}
	dayIdx := int(e.now / day)
	if dayIdx != e.spentDay {
		e.spent = map[string]float64{}
		e.spentDay = dayIdx
	}
	for _, pd := range e.farm.projects {
		pr := sched.Project{Name: pd.name, Weight: 1, Quotas: map[string]sched.Quota{},
			BurstBudgetLeft: math.Max(0, pd.budget-e.spent[pd.name]), LateCostPerHour: pd.lateCost}
		for pool, n := range pd.nominal {
			pr.Quotas[pool] = sched.Quota{Nominal: n, BorrowingLimit: -1}
		}
		s.Projects = append(s.Projects, pr)
	}
	for _, j := range e.active {
		js := j.spec
		room := 0
		for _, name := range poolsFor(js.kind, js.gpu) {
			room += max(0, free[name])
		}
		slots := js.units[0].slots
		k := min(len(j.runnable), room/slots+1)
		sj := sched.Job{
			ID: js.id, Project: js.project, Class: js.kind,
			Submitted: e.at(js.submit), Ready: e.at(js.submit), Deadline: e.at(j.deadline),
			Pools: poolsFor(js.kind, js.gpu), SlotsPerUnit: slots,
			Running: j.runSlots, RunnableTotal: len(j.runnable), Blocked: j.blocked, Unevaluated: j.uneval,
			Preemptible: js.kind != "ci", Checkpoints: js.ml != nil,
			Priority: js.priority, UnitsDone: j.done, DoneSeconds: j.doneSec,
		}
		sj.Runnable = make([]sched.UnitRef, k)
		for i := 0; i < k; i++ {
			u := j.runnable[i]
			info := u.spec.genKey != "" || (js.coarse > 0 && js.rank[u.spec.pos] < js.coarse)
			sj.Runnable[i] = sched.UnitRef{ID: u.id, Info: info, Slots: u.spec.slots}
		}
		for _, u := range j.runningU {
			ru := sched.RunningUnit{ID: u.id, Pool: u.pool.def.name, Slots: u.spec.slots,
				Started: e.at(u.start), Preemptions: u.preemptions}
			if ml := js.ml; ml != nil {
				prog := e.now - u.start - u.restore
				if prog >= ml.ckpt {
					ru.LastCheckpoint = e.at(u.start + u.restore + math.Floor(prog/ml.ckpt)*ml.ckpt)
				}
				ru.RestoreCost = time.Duration(ml.restore * float64(time.Second))
			}
			sj.RunningUnits = append(sj.RunningUnits, ru)
		}
		switch {
		case len(j.runningU) > 0:
			sj.LastRunning = now
		case j.lastRunning >= 0:
			sj.LastRunning = e.at(j.lastRunning)
		}
		if e.pol.usesEs {
			sj.Estimate = e.estimate(j)
		}
		s.Jobs = append(s.Jobs, sj)
	}
	return s
}

// estimate returns the job's remaining-work summary, recomputed when the
// job changed or the cached one is older than stale.
func (e *engine) estimate(j *job) estimate.Summary {
	if !j.dirty && e.now-j.sumAt < stale {
		return j.sum
	}
	var q []estimate.Unit
	var r []estimate.RunningUnit
	var members []*unit
	for _, u := range j.units {
		if u.state == stHidden || u.state == stDone {
			continue
		}
		eu := estimate.Unit{Key: u.spec.key, Pos: u.spec.pos, Slots: u.spec.slots, Done: u.retained, Gen: u.spec.genKey}
		members = append(members, u)
		if u.state == stRunning {
			r = append(r, estimate.RunningUnit{Unit: eu, Elapsed: math.Max(0, e.now-u.start-u.restore)})
			continue
		}
		q = append(q, eu)
	}
	// Summarize first: it enters overrunning units as provisional
	// observations, which the critical path below should see too.
	s := j.est.Summarize(q, r)
	idx := map[int]int{}
	dur := make([]float64, len(members))
	deps := make([][]int, len(members))
	for k, u := range members {
		idx[u.idx] = k
		p := j.est.Predict(u.spec.key, u.spec.pos)
		d := p.Mean - u.retained
		if u.state == stRunning {
			d, _ = j.est.Remainder(p, u.retained, math.Max(0, e.now-u.start-u.restore))
		}
		if u.spec.genKey != "" {
			d += j.est.Generator(u.spec.genKey).Longest
		}
		dur[k] = math.Max(0, d)
	}
	for k, u := range members {
		for _, d := range u.spec.deps {
			if x, ok := idx[d]; ok {
				deps[k] = append(deps[k], x)
			}
		}
	}
	s.TPath, _ = estimate.CriticalPath(dur, deps)
	j.sum, j.sumAt, j.dirty = s, e.now, false
	return s
}

func (e *engine) decide() {
	for _, p := range e.pools {
		p.meter(e.now)
	}
	s := e.snapshot()
	if e.eta != nil {
		e.eta.beforeDecide(e, &s)
	}
	d := e.pol.fn(s, &e.pst, e.pcfg)
	if e.eta != nil {
		e.eta.afterDecide(e, s, d)
	}
	if traceJob != "" && e.pol.name == tracePolicy && e.seed == 1 && e.farm.name == "main" {
		e.trace(s, d)
	}
	e.lastDec = e.now
	e.res.decides++
	for _, x := range d.Dispatch {
		u := e.byID[x.Unit]
		if u == nil || u.state != stRunnable {
			panic(fmt.Sprintf("policy %s dispatched %s in state %d", e.pol.name, x.Unit, u.state))
		}
		e.start(u, e.byName[x.Pool])
	}
	for _, x := range d.Preempt {
		u := e.byID[x.Unit]
		if u == nil || u.state != stRunning || u.yieldAt > 0 {
			continue
		}
		e.res.PreemptBy[string(x.Reason)]++
		if x.Yield && u.j.spec.ml != nil {
			ml := u.j.spec.ml
			prog := e.now - u.start - u.restore
			nb := u.start + u.restore + (math.Floor(math.Max(0, prog)/ml.ckpt)+1)*ml.ckpt
			if nb >= u.endAt {
				continue // it ends first
			}
			u.yieldAt = nb
			e.push(&event{t: nb, kind: evYield, u: u, attempt: u.attempt})
			continue
		}
		e.stop(u, evFail, true)
	}
	for _, b := range d.Bursts {
		e.res.burstSuggestion(b)
		if b.Auto {
			e.spent[b.Project] += b.Cost
		}
	}
	if !e.pol.burst {
		return
	}
	for _, c := range d.Capacity {
		e.setRequest(e.byName[c.Pool], c.Slots)
	}
}

func (e *engine) setRequest(p *pool, n int) {
	p.meter(e.now)
	n = min(max(n, 0), p.def.burst.max)
	if n > p.requested {
		b := &batch{n: n - p.requested, ready: e.now + p.def.burst.delay.Seconds()}
		p.batches = append(p.batches, b)
		p.requested = n
		e.push(&event{t: b.ready, kind: evProvision, p: p, b: b})
		e.res.burstRequests++
		return
	}
	cut := p.requested - n
	for i := len(p.batches) - 1; i >= 0 && cut > 0; i-- {
		b := p.batches[i]
		c := min(cut, b.n)
		b.n -= c
		cut -= c
		p.requested -= c
	}
	idle := p.slots - p.used
	c := min(cut, idle)
	p.slots -= c
	p.requested -= c
}

func (e *engine) finishRun() {
	end := math.Min(e.now, horizon)
	for _, p := range e.pools {
		p.meter(end)
		e.res.pool(p)
	}
	for _, j := range e.jobs {
		if j.finish < 0 {
			e.res.unfinished(j, end)
		}
	}
	e.res.end = end
}

var traceJob = os.Getenv("SIMTRACE")
var tracePolicy = envOr("SIMTRACEPOL", "objective")

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (e *engine) trace(s sched.Snapshot, d sched.Decisions) {
	if e.now-e.lastTrace < 600 {
		return
	}
	for _, j := range s.Jobs {
		if j.ID != traceJob {
			continue
		}
		e.lastTrace = e.now
		st := d.Jobs[j.ID]
		run := 0
		for _, n := range j.Running {
			run += n
		}
		used := 0
		for _, p := range s.Pools {
			if p.Name == "cpu" {
				used = p.Used
			}
		}
		fmt.Fprintf(os.Stderr, "t=%6.2fh run=%3d R=%6.1f share=%6.1f W=%7.1f W90=%7.1f tpath=%5.2fh slack=%v runnable=%d done=%d risk=%v cpuUsed=%d nJobs=%d | %s\n",
			e.now/hour, run, st.Required, st.Share, j.Estimate.W/hour, j.Estimate.W90/hour, j.Estimate.TPath/hour, st.Slack.Round(time.Minute), j.RunnableTotal, j.UnitsDone, st.AtRisk, used, len(s.Jobs), st.Message)
	}
}
