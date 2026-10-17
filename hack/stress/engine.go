package main

import (
	"container/heap"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/work/estimate"
	"github.com/Avalanche-io/c4-operator/internal/work/sched"
)

// The engine is a discrete-event simulation of a generated farm, in the
// manner of hack/sim but with every farm parameter open: it calls a
// policy through sched.Policy on a snapshot, applies its decisions as the
// controller would (units start, preempted units stop at once or at their
// next checkpoint, burst pools grow after their provisioning delay), and
// injects what goes wrong on a farm: failed attempts, node faults, spot
// reclaims, holds, boosts and moved deadlines. Time is seconds from
// Monday 00:00 UTC.

var t0 = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

const (
	coalesce  = 10.0 // seconds between decisions, at most one per window
	heartbeat = 60.0 // a decision at least this often while work exists
	stale     = 300.0
	minFresh  = 30.0 // a changed job's estimate is recomputed at most this often
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
	retained    float64
	yieldAt     float64
}

type job struct {
	spec        *jobSpec
	units       []*unit
	runnable    []*unit
	running     []*unit
	runSlots    map[string]int
	done        int
	blocked     int
	uneval      int
	upLeft      int
	downstream  []*job
	arrived     bool
	released    bool
	ready       float64
	lastRunning float64
	finish      float64
	boost       float64
	hold, stop  bool
	due         float64
	obsSum      float64
	obsN        int
	sum         estimate.Summary
	sumAt       float64
	dirty       bool
}

func (j *job) live() bool { return j.arrived && j.finish < 0 }

type batch struct{ n int }

type pool struct {
	def       poolDef
	idx       int
	slots     int
	used      int
	requested int
	batches   []*batch
	running   []*unit
	down      []bool
	busy      float64
	bill      float64
	last      float64
	reclaims  int
}

func (p *pool) meter(now float64) {
	if now <= p.last {
		return
	}
	p.busy += float64(p.used) * (now - p.last)
	if b := p.def.burst; b != nil {
		p.bill += float64(p.requested) * (now - p.last) * b.price / hour
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
	evFault
	evRepair
	evReclaim
	evNudge
	evUnnudge
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
	n       int
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

// policy is a policy under test.
type policy struct {
	name    string
	fn      sched.Policy
	burst   bool
	usesEs  bool
	checked bool // run the invariant checks on its decisions
	tweak   func(*sched.Config)
}

type engine struct {
	w      *workload
	pol    policy
	cfg    sched.Config
	st     sched.State
	now    float64
	q      evHeap
	seq    int
	jobs   []*job
	active []*job
	pools  []*pool
	byName map[string]*pool
	byID   map[string]*unit
	spent  map[string]float64
	budget map[string]float64
	day    int
	decAt  float64
	lastD  float64
	left   int
	ck     *checker
	hash   uint64
	res    *runResult
	lat    []time.Duration
}

func newEngine(w *workload, pol policy, check bool) *engine {
	e := &engine{w: w, pol: pol, cfg: sched.DefaultConfig(), byName: map[string]*pool{}, byID: map[string]*unit{},
		spent: map[string]float64{}, budget: map[string]float64{}, decAt: -1, lastD: -coalesce}
	e.cfg.Burst = pol.burst
	if pol.tweak != nil {
		pol.tweak(&e.cfg)
	}
	e.res = &runResult{Policy: pol.name, Point: w.pt.Name, Seed: w.seed}
	for i, pd := range w.pools {
		p := &pool{def: pd, idx: i, slots: pd.slots(), down: make([]bool, pd.nodes)}
		e.pools = append(e.pools, p)
		e.byName[pd.name] = p
		e.scheduleFaults(p)
		if pd.burst != nil && pd.burst.interrupt > 0 {
			e.nextReclaim(p, 0, 0)
		}
	}
	for _, pd := range w.projects {
		e.budget[pd.name] = pd.budget
	}
	byIdx := map[*jobSpec]*job{}
	for _, js := range w.jobs {
		j := &job{spec: js, runSlots: map[string]int{}, finish: -1, lastRunning: -1, ready: -1, dirty: true, due: js.due, boost: 1}
		for k := range js.units {
			us := &js.units[k]
			u := &unit{spec: us, j: j, idx: k, id: fmt.Sprintf("%s/%d", js.id, k)}
			j.units = append(j.units, u)
			e.byID[u.id] = u
		}
		for k, us := range js.units {
			for _, d := range us.deps {
				j.units[d].dependents = append(j.units[d].dependents, k)
			}
		}
		byIdx[js] = j
		e.jobs = append(e.jobs, j)
		e.push(&event{t: js.submit, kind: evArrive, j: j})
		for n, nd := range js.nudges {
			e.push(&event{t: nd.at, kind: evNudge, j: j, n: n})
		}
	}
	for _, j := range e.jobs {
		for _, up := range j.spec.upstream {
			j.upLeft++
			byIdx[up].downstream = append(byIdx[up].downstream, j)
		}
	}
	e.left = len(e.jobs)
	if check {
		e.ck = newChecker(e)
	}
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
	e.decAt = math.Max(e.now, e.lastD+coalesce)
	e.push(&event{t: e.decAt, kind: evDecide})
}

// hrand is a deterministic uniform draw in (0,1) from a key, so that
// failures, faults and reclaims are the same under every policy.
func (e *engine) hrand(a, b, c, k uint64) float64 {
	x := e.w.seed*0x9E3779B97F4A7C15 ^ a<<40 ^ b<<20 ^ c<<4 ^ k
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return (float64(x>>11) + 0.5) / (1 << 53)
}

func (e *engine) expo(rate, u float64) float64 { return -math.Log(u) / rate }

func (e *engine) run() *runResult {
	for e.q.Len() > 0 && e.left > 0 {
		ev := heap.Pop(&e.q).(*event)
		if ev.t > e.w.horizon {
			break
		}
		e.now = ev.t
		switch ev.kind {
		case evArrive:
			e.arrive(ev.j)
		case evFinish, evFail, evYield:
			if ev.u.state != stRunning || ev.u.attempt != ev.attempt {
				continue
			}
			e.stop(ev.u, ev.kind)
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
			e.requestDecide()
			e.push(&event{t: e.now + heartbeat, kind: evTick})
		case evFault:
			e.fault(ev.p, ev.n, ev.attempt)
		case evRepair:
			ev.p.meter(e.now)
			ev.p.down[ev.n] = false
			ev.p.slots += ev.p.def.spn
			e.requestDecide()
		case evReclaim:
			e.reclaim(ev.p, ev.attempt)
		case evNudge:
			e.applyNudge(ev.j, ev.j.spec.nudges[ev.n])
		case evUnnudge:
			ev.j.hold, ev.j.stop = false, false
			ev.j.dirty = true
			e.requestDecide()
		}
	}
	e.finishRun()
	return e.res
}

func (e *engine) arrive(j *job) {
	j.arrived = true
	for _, u := range j.units {
		if !u.spec.hidden {
			e.reveal(u)
		}
	}
	if j.upLeft == 0 {
		e.release(j)
	}
	e.active = append(e.active, j)
	e.requestDecide()
}

// release makes a job's ready units runnable once its upstream jobs are
// done.
func (e *engine) release(j *job) {
	j.released = true
	j.ready = e.now
	for _, u := range j.units {
		if u.state == stBlocked && u.waiting == 0 {
			j.blocked--
			e.makeRunnable(u)
		}
	}
}

func (e *engine) reveal(u *unit) {
	u.waiting = len(u.spec.deps)
	for _, d := range u.spec.deps {
		if u.j.units[d].state == stDone {
			u.waiting--
		}
	}
	if u.spec.gen {
		u.j.uneval++
	}
	if u.waiting > 0 || !u.j.released && u.j.upLeft > 0 {
		u.state = stBlocked
		u.j.blocked++
		return
	}
	e.makeRunnable(u)
}

func (e *engine) makeRunnable(u *unit) {
	j := u.j
	u.state = stRunnable
	// Dispatch order: generators first, then by position, pass, index.
	less := func(a, b *unit) bool {
		if a.spec.gen != b.spec.gen {
			return a.spec.gen
		}
		if a.spec.pos != b.spec.pos {
			return a.spec.pos < b.spec.pos
		}
		return a.idx < b.idx
	}
	i := sort.Search(len(j.runnable), func(i int) bool { return less(u, j.runnable[i]) })
	j.runnable = append(j.runnable, nil)
	copy(j.runnable[i+1:], j.runnable[i:])
	j.runnable[i] = u
	j.dirty = true
}

func removeUnit(xs []*unit, u *unit) []*unit {
	for i, x := range xs {
		if x == u {
			return append(xs[:i], xs[i+1:]...)
		}
	}
	return xs
}

func (e *engine) start(u *unit, p *pool) {
	j := u.j
	j.runnable = removeUnit(j.runnable, u)
	slots := u.spec.slots
	p.meter(e.now)
	p.used += slots
	p.running = append(p.running, u)
	u.state = stRunning
	u.pool = p
	u.start = e.now
	u.attempt++
	u.yieldAt = 0
	j.runSlots[p.def.name] += slots
	j.running = append(j.running, u)
	j.dirty = true
	key := uint64(j.spec.idx)
	if ml := j.spec.ml; ml != nil {
		u.restore = 0
		if u.retained > 0 {
			u.restore = ml.restore
		}
		rem := u.spec.dur - u.retained
		if ml.hazard > 0 {
			if tf := e.expo(ml.hazard, e.hrand(1, key, uint64(u.idx), uint64(u.attempt))); tf < rem {
				u.endAt = e.now + u.restore + tf
				e.push(&event{t: u.endAt, kind: evFail, u: u, attempt: u.attempt})
				return
			}
		}
		u.endAt = e.now + u.restore + rem
		e.push(&event{t: u.endAt, kind: evFinish, u: u, attempt: u.attempt})
		return
	}
	if u.attempt < 4 && e.hrand(2, key, uint64(u.idx), uint64(u.attempt)) < j.spec.failRate {
		u.endAt = e.now + e.hrand(3, key, uint64(u.idx), uint64(u.attempt))*u.spec.dur
		e.push(&event{t: u.endAt, kind: evFail, u: u, attempt: u.attempt})
		return
	}
	u.endAt = e.now + u.spec.dur
	e.push(&event{t: u.endAt, kind: evFinish, u: u, attempt: u.attempt})
}

// stop ends a running unit: it finished, failed, yielded at a checkpoint,
// or was killed (evFail with a preemption, a fault or a reclaim).
func (e *engine) stop(u *unit, how evKind) {
	e.stopAs(u, how, false)
}

func (e *engine) stopAs(u *unit, how evKind, preempted bool) {
	j := u.j
	p := u.pool
	slots := float64(u.spec.slots)
	p.meter(e.now)
	p.used -= u.spec.slots
	p.running = removeUnit(p.running, u)
	j.runSlots[p.def.name] -= u.spec.slots
	if j.runSlots[p.def.name] == 0 {
		delete(j.runSlots, p.def.name)
	}
	j.running = removeUnit(j.running, u)
	j.lastRunning = e.now
	j.dirty = true
	elapsed := e.now - u.start
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
		useful, lost = prog*slots, u.restore*slots
	case ml != nil:
		prog := math.Max(0, elapsed-u.restore)
		kept := math.Floor(prog/ml.ckpt) * ml.ckpt
		u.retained += kept
		useful, lost = kept*slots, (elapsed-kept)*slots
	default:
		lost = elapsed * slots
	}
	e.res.UsefulSlotHours += useful / hour
	e.res.LostSlotHours += lost / hour
	if how == evYield || preempted {
		u.preemptions++
		e.res.Preemptions++
	}
	u.pool = nil
	e.requestDecide()
	if how != evFinish {
		u.state = stRunnable
		e.makeRunnable(u)
		return
	}
	u.state = stDone
	e.complete(u, elapsed)
}

func (e *engine) complete(u *unit, elapsed float64) {
	j := u.j
	j.done++
	if j.spec.ml == nil && u.spec.nominal > 0 {
		j.obsSum += elapsed / (u.spec.nominal * j.spec.estBias)
		j.obsN++
	}
	// Dependents already visible first; hidden children count their done
	// dependencies when they are revealed.
	for _, d := range u.dependents {
		du := j.units[d]
		if du.state != stBlocked {
			continue
		}
		du.waiting--
		if du.waiting == 0 && j.released {
			j.blocked--
			e.makeRunnable(du)
		}
	}
	if u.spec.gen {
		j.uneval--
		for _, c := range u.spec.children {
			e.reveal(j.units[c])
		}
	}
	if j.done < len(j.units) {
		return
	}
	j.finish = e.now
	e.left--
	e.active = removeJob(e.active, j)
	for _, d := range j.downstream {
		d.upLeft--
		if d.upLeft == 0 && d.arrived {
			e.release(d)
		}
	}
}

func removeJob(xs []*job, j *job) []*job {
	for i, x := range xs {
		if x == j {
			return append(xs[:i], xs[i+1:]...)
		}
	}
	return xs
}

func (e *engine) applyNudge(j *job, n nudge) {
	if j.finish >= 0 {
		return
	}
	j.dirty = true
	switch n.kind {
	case nBoost:
		j.boost = n.x
	case nHold, nHoldStop:
		j.hold = n.kind == nHold
		j.stop = n.kind == nHoldStop
		if n.x < 1e8 {
			e.push(&event{t: e.now + n.x, kind: evUnnudge, j: j})
		}
	case nMove:
		j.due = j.spec.submit + n.x*(j.due-j.spec.submit)
	}
	e.res.Nudges++
	e.requestDecide()
}

// Node faults: each owned node fails as a Poisson process of rate
// NodeFaults per day, drawn from its own key so that a farm with more
// nodes sees the same faults on the nodes it shares with a smaller one.
func (e *engine) scheduleFaults(p *pool) {
	if e.w.pt.NodeFaults <= 0 || p.def.burst != nil {
		return
	}
	for n := range p.def.nodes {
		e.nextFault(p, n, 0, 0)
	}
}

func (e *engine) nextFault(p *pool, n, k int, from float64) {
	rate := e.w.pt.NodeFaults / day
	t := from + e.expo(rate, e.hrand(4, uint64(p.idx), uint64(n), uint64(k)))
	if t < e.w.horizon {
		e.push(&event{t: t, kind: evFault, p: p, n: n, attempt: k})
	}
}

func (e *engine) fault(p *pool, n, k int) {
	e.nextFault(p, n, k+1, e.now)
	if p.down[n] {
		return
	}
	p.meter(e.now)
	spn := p.def.spn
	// Units are laid out over the pool's slots in start order; the node
	// holds slots [n*spn, (n+1)*spn).
	lo, hi := n*spn, (n+1)*spn
	at := 0
	var kill []*unit
	for _, u := range p.running {
		a, b := at, at+u.spec.slots
		at = b
		if a < hi && b > lo {
			kill = append(kill, u)
		}
	}
	for _, u := range kill {
		e.stopAs(u, evFail, false)
		e.res.FaultKills++
	}
	p.down[n] = true
	p.slots -= spn
	for p.used > p.slots && len(p.running) > 0 {
		e.stopAs(p.running[len(p.running)-1], evFail, false)
		e.res.FaultKills++
	}
	repair := (1 + 3*e.hrand(5, uint64(p.idx), uint64(n), uint64(k))) * hour
	e.push(&event{t: e.now + repair, kind: evRepair, p: p, n: n})
	e.requestDecide()
}

// Spot reclaims: provisioned burst slots are taken back at rate
// interrupt per slot-hour (thinned from the pool's limit). A reclaimed
// slot's unit is killed and a replacement provisions after the delay.
func (e *engine) nextReclaim(p *pool, k int, from float64) {
	b := p.def.burst
	if b.max <= 0 {
		return
	}
	t := from + e.expo(b.interrupt*float64(b.max)/hour, e.hrand(6, uint64(p.idx), 0, uint64(k)))
	if t < e.w.horizon {
		e.push(&event{t: t, kind: evReclaim, p: p, attempt: k})
	}
}

func (e *engine) reclaim(p *pool, k int) {
	e.nextReclaim(p, k+1, e.now)
	b := p.def.burst
	if p.slots == 0 || e.hrand(7, uint64(p.idx), 0, uint64(k)) > float64(p.slots)/float64(b.max) {
		return
	}
	p.meter(e.now)
	slot := int(e.hrand(8, uint64(p.idx), 0, uint64(k)) * float64(p.slots))
	at := 0
	for _, u := range p.running {
		if slot >= at && slot < at+u.spec.slots {
			e.stopAs(u, evFail, false)
			e.res.FaultKills++
			break
		}
		at += u.spec.slots
	}
	p.slots--
	p.reclaims++
	bt := &batch{n: 1}
	p.batches = append(p.batches, bt)
	e.push(&event{t: e.now + b.delay, kind: evProvision, p: p, b: bt})
	e.requestDecide()
}

func (e *engine) at(t float64) time.Time {
	return t0.Add(time.Duration(t * float64(time.Second)))
}

func (e *engine) snapshot() sched.Snapshot {
	now := e.at(e.now)
	s := sched.Snapshot{Now: now}
	free := map[string]int{}
	for _, p := range e.pools {
		sp := sched.Pool{Name: p.def.name, Slots: p.slots, Used: p.used}
		if b := p.def.burst; b != nil {
			sp.CostPerSlotHour = b.price
			sp.Burst = &sched.Burst{MaxSlots: b.max, Provisioning: time.Duration(b.delay * float64(time.Second)),
				CostPerSlotHour: b.price, Requested: p.requested, Provisioned: p.slots}
		}
		free[p.def.name] = p.slots - p.used
		s.Pools = append(s.Pools, sp)
	}
	if d := int(e.now / day); d != e.day {
		e.spent = map[string]float64{}
		e.day = d
	}
	for _, pd := range e.w.projects {
		pr := sched.Project{Name: pd.name, Weight: 1, Quotas: map[string]sched.Quota{},
			BurstBudgetLeft: math.Max(0, e.budget[pd.name]-e.spent[pd.name]), LateCostPerHour: pd.lateCost}
		for pool, n := range pd.nominal {
			pr.Quotas[pool] = sched.Quota{Nominal: n, BorrowingLimit: pd.borrow}
		}
		s.Projects = append(s.Projects, pr)
	}
	for _, j := range e.active {
		s.Jobs = append(s.Jobs, e.jobSnap(j, now, free))
	}
	return s
}

func (e *engine) jobSnap(j *job, now time.Time, free map[string]int) sched.Job {
	js := j.spec
	room := 0
	for _, name := range js.pools {
		room += max(0, free[name])
	}
	slots := max(1, js.maxUnitSlots)
	k := min(len(j.runnable), room/slots+1)
	run := map[string]int{}
	for p, n := range j.runSlots {
		run[p] = n
	}
	sj := sched.Job{
		ID: js.id, Project: js.project, Class: js.kind,
		Submitted: e.at(js.submit), Ready: e.at(js.submit),
		Pools: js.pools, SlotsPerUnit: slots, Running: run,
		RunnableTotal: len(j.runnable), Blocked: j.blocked, Unevaluated: j.uneval,
		Hold: j.hold, HoldStop: j.stop, Preemptible: js.preemptible, Checkpoints: js.ml != nil,
		Priority: js.priority, UnitsDone: j.done,
	}
	if j.boost > 1 {
		sj.Boost = j.boost
	}
	if j.ready >= 0 {
		sj.Ready = e.at(j.ready)
	}
	if js.hasDue {
		sj.Deadline = e.at(j.due)
	}
	sj.Runnable = make([]sched.UnitRef, k)
	for i := range k {
		u := j.runnable[i]
		sj.Runnable[i] = sched.UnitRef{ID: u.id, Info: u.spec.gen, Slots: u.spec.slots}
	}
	for _, u := range j.running {
		ru := sched.RunningUnit{ID: u.id, Pool: u.pool.def.name, Slots: u.spec.slots, Started: e.at(u.start), Preemptions: u.preemptions}
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
	case len(j.running) > 0:
		sj.LastRunning = now
	case j.lastRunning >= 0:
		sj.LastRunning = e.at(j.lastRunning)
	}
	if e.pol.usesEs {
		sj.Estimate = e.estimate(j)
	}
	return sj
}

// estimate is the job's remaining work as an estimator would see it: each
// unit's typical duration times the job's estimate bias, corrected by the
// mean ratio of observed to predicted durations once two units are done.
// Heavy regions are unknown until they run.
func (e *engine) estimate(j *job) estimate.Summary {
	age := e.now - j.sumAt
	if j.sumAt > 0 && (!j.dirty || age < minFresh) && age < stale {
		return j.sum
	}
	js := j.spec
	ratio := 1.0
	if j.obsN >= 2 {
		ratio = j.obsSum / float64(j.obsN)
	}
	var s estimate.Summary
	v := 0.0
	fin := make([]float64, len(j.units))
	for i, u := range j.units {
		if u.state == stDone {
			continue
		}
		pred := u.spec.nominal * js.estBias * ratio
		rem := math.Max(0, pred-u.retained)
		if u.state == stRunning {
			el := math.Max(0, e.now-u.start-u.restore)
			rem = pred - u.retained - el
			if rem <= 0 {
				rem = 0.5 * el
			}
			s.Running++
		} else if u.state != stHidden {
			s.Queued++
		}
		sl := float64(u.spec.slots)
		s.W += rem * sl
		v += math.Pow(js.spread*0.5*rem*sl, 2)
		dep := 0.0
		for _, d := range u.spec.deps {
			dep = math.Max(dep, fin[d])
		}
		fin[i] = dep + rem
		s.TPath = math.Max(s.TPath, fin[i])
	}
	s.Sigma = math.Sqrt(v)
	s.W90 = s.W + 1.28*s.Sigma
	s.Unevaluated = j.uneval
	j.sum, j.sumAt, j.dirty = s, e.now, false
	return s
}

func (e *engine) decide() {
	for _, p := range e.pools {
		p.meter(e.now)
	}
	if len(e.active) == 0 {
		e.lastD = e.now
		return
	}
	s := e.snapshot()
	t := time.Now()
	d := e.pol.fn(s, &e.st, e.cfg)
	e.lat = append(e.lat, time.Since(t))
	e.lastD = e.now
	e.res.Decides++
	if e.ck != nil {
		e.ck.decision(s, d)
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%x|%.3f", e.hash, e.now)
	for _, x := range d.Dispatch {
		u := e.byID[x.Unit]
		p := e.byName[x.Pool]
		switch {
		case u == nil || p == nil || u.j.spec.id != x.Job:
			e.violate("dispatch-invalid", x.Job, fmt.Sprintf("dispatched unknown unit %s or pool %s", x.Unit, x.Pool))
			continue
		case u.state != stRunnable:
			e.violate("blocked-dispatch", x.Job, fmt.Sprintf("dispatched %s in state %d (not runnable)", x.Unit, u.state))
			continue
		case p.slots-p.used < u.spec.slots:
			e.violate("overfill", x.Job, fmt.Sprintf("dispatched %s (%d slots) into %s with %d free", x.Unit, u.spec.slots, x.Pool, p.slots-p.used))
			continue
		case !allowed(u.j.spec.pools, x.Pool):
			e.violate("dispatch-invalid", x.Job, fmt.Sprintf("dispatched %s into %s, which the job may not use", x.Unit, x.Pool))
			continue
		case u.j.hold || u.j.stop:
			e.violate("hold", x.Job, fmt.Sprintf("dispatched %s while the job is on hold", x.Unit))
		}
		fmt.Fprintf(h, "|d%s@%s", x.Unit, x.Pool)
		e.start(u, p)
		e.res.Dispatches++
	}
	for _, x := range d.Preempt {
		u := e.byID[x.Unit]
		if u == nil || u.state != stRunning || u.yieldAt > 0 {
			continue
		}
		fmt.Fprintf(h, "|p%s", x.Unit)
		if e.ck != nil {
			e.ck.preempted(x)
		}
		if x.Yield && u.j.spec.ml != nil {
			ml := u.j.spec.ml
			prog := e.now - u.start - u.restore
			nb := u.start + u.restore + (math.Floor(math.Max(0, prog)/ml.ckpt)+1)*ml.ckpt
			if nb >= u.endAt {
				continue
			}
			u.yieldAt = nb
			e.push(&event{t: nb, kind: evYield, u: u, attempt: u.attempt})
			continue
		}
		e.stopAs(u, evFail, true)
	}
	for _, b := range d.Bursts {
		if b.Auto {
			e.spent[b.Project] += b.Cost
			e.res.AutoBursts++
		} else if b.Closes {
			e.res.Approvals++
			why := b.Why
			if i := strings.Index(why, " ("); i > 0 {
				if strings.Contains(why, "($0 at") {
					why = why[:i] + " ($0)"
				} else {
					why = why[:i]
				}
			}
			if e.res.Why == nil {
				e.res.Why = map[string]int{}
			}
			e.res.Why[why]++
		}
	}
	if e.pol.burst {
		for _, c := range d.Capacity {
			if p := e.byName[c.Pool]; p != nil && p.def.burst != nil {
				fmt.Fprintf(h, "|c%s=%d", c.Pool, c.Slots)
				e.setRequest(p, c.Slots)
			}
		}
	}
	e.hash = h.Sum64()
}

func allowed(pools []string, p string) bool {
	for _, x := range pools {
		if x == p {
			return true
		}
	}
	return false
}

func (e *engine) violate(inv, job, detail string) {
	e.res.add(violation{Inv: inv, At: e.now, Job: job, Detail: detail})
}

func (e *engine) setRequest(p *pool, n int) {
	p.meter(e.now)
	n = min(max(n, 0), p.def.burst.max)
	if n > p.requested {
		b := &batch{n: n - p.requested}
		p.batches = append(p.batches, b)
		p.requested = n
		e.push(&event{t: e.now + p.def.burst.delay, kind: evProvision, p: p, b: b})
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
	c := min(cut, p.slots-p.used)
	p.slots -= c
	p.requested -= c
}

func (e *engine) finishRun() {
	end := math.Min(e.now, e.w.horizon)
	if e.left > 0 {
		end = e.w.horizon
	}
	r := e.res
	owned, busy := 0.0, 0.0
	for _, p := range e.pools {
		p.meter(end)
		if p.def.burst != nil {
			r.BurstCost += p.bill
			r.Reclaims += p.reclaims
			continue
		}
		owned += float64(p.def.slots()) * end
		busy += p.busy
	}
	if owned > 0 {
		r.Util = busy / owned
	}
	late := map[string]float64{}
	for _, pd := range e.w.projects {
		late[pd.name] = pd.lateCost
	}
	for _, j := range e.jobs {
		js := j.spec
		if js.gate {
			continue
		}
		r.Jobs++
		done := j.finish
		if done < 0 {
			r.Unfinished++
			done = end
		}
		if !js.hasDue || (j.finish < 0 && j.due > end) {
			continue
		}
		r.DueJobs++
		lateBy := done - j.due
		if j.finish >= 0 && lateBy <= 1 {
			r.OnTime++
			continue
		}
		r.Late++
		lh := math.Max(0, lateBy) / hour
		r.LateHours += lh
		r.LateDollars += lh * late[js.project]
	}
	if len(e.lat) > 0 {
		var tot time.Duration
		for _, l := range e.lat {
			tot += l
			r.DecideMax = max(r.DecideMax, l)
		}
		r.DecideMean = tot / time.Duration(len(e.lat))
	}
	r.Hash = e.hash
	r.End = end
	if e.ck != nil {
		e.ck.finish(end)
	}
}
