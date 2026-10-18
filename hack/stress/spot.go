package main

import (
	"fmt"
	"math"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/work/sched"
)

// Spot capacity as each provider sells it today. A spot point (Provider
// above 0) gets, per pool class, an on-demand burst pool and a spot burst
// pool, and the spot pool's price and interruptions follow the provider:
//
// AWS (EC2 Spot). Since the November 2017 pricing change there is no
// bidding: the price moves gradually with long-term supply and demand,
// and interruptions come from EC2 reclaiming capacity, not from price.
// The Spot Instance Advisor publishes each instance type's frequency of
// interruption in bands of <5%, 5-10%, 10-15%, 15-20% and >20% per month.
// An interruption is preceded by the two-minute interruption notice, and
// often by an EC2 instance rebalance recommendation, which can arrive
// earlier. A maximum price never makes an instance safer: if set, the
// instance is also interrupted when the price passes it, and a running
// request's maximum cannot be raised. Diversifying over instance types
// and Availability Zones with the capacity-optimized allocation strategy
// lowers the chance of interruption.
//
// GCP (Spot VMs). A fixed discount of 60-91% off the on-demand price that
// changes at most once every 30 days; preemption can come at any time
// with a 30-second notice, and nothing the user sets changes its rate.
//
// Azure (Spot Virtual Machines). The user sets a maximum price, or -1 to
// be evicted for capacity only; the price varies with supply and demand;
// evictions give 30 seconds' notice (Scheduled Events) and either
// deallocate the VM (it can be started again when capacity returns) or
// delete it.
//
// Assumptions, chosen here and not quoted from any provider:
//   - AWS hazard per instance-hour from the band's midpoint (2.5, 7.5,
//     12.5, 17.5 and 25% a month): -ln(1-p)/730.
//   - Diversifying over N capacity pools scales the AWS hazard by
//     0.5 + 0.5/N (up to half the hazard removed).
//   - A rebalance recommendation precedes an AWS interruption with
//     probability Rebalance, by an exponential lead with a mean of 15
//     minutes before the two-minute notice.
//   - GCP Spot preempts at 0.5% per VM-hour (about 11% of VMs a day),
//     Azure Spot evicts for capacity at 0.3% per VM-hour.
//   - HazardScale multiplies every provider's hazard: 1 is a quiet region,
//     larger values a capacity crunch.
//   - AWS and Azure spot prices walk 3% and 5% a day around the discounted
//     price, within 10-100% of on-demand; GCP's is fixed for the run.
//   - A deallocated Azure VM comes back after half the provisioning delay.
//   - Writing a training checkpoint takes CkptWrite seconds; a unit can
//     checkpoint on a notice only if that fits in the notice.

type spotDef struct {
	provider  string
	hazard    float64 // interruptions per slot-hour, before any crunch scaling
	notice    float64 // seconds
	rebalance float64 // AWS: chance a rebalance recommendation comes first
	lead      float64 // AWS: mean seconds from the recommendation to the notice
	walk      float64 // daily sigma of the price's log
	maxPrice  float64 // dollars per slot-hour; 0: none (Azure -1)
	od        float64 // the on-demand price, for bounds and the policy
	restart   float64 // seconds to bring a deallocated VM back (Azure); 0: reprovision
}

var providerNames = []string{"", "aws", "gcp", "azure"}

var awsBands = []float64{0.025, 0.075, 0.125, 0.175, 0.25} // monthly, band midpoints

// spotFor builds the spot model of a pool class at a point.
func spotFor(pt Point, od float64) *spotDef {
	s := &spotDef{provider: providerNames[int(pt.Provider)%len(providerNames)], od: od}
	scale := math.Max(1, pt.HazardScale)
	switch s.provider {
	case "aws":
		p := awsBands[min(len(awsBands)-1, max(0, int(pt.SpotBand)))]
		n := math.Max(1, pt.Diversity)
		s.hazard = -math.Log(1-p) / 730 * (0.5 + 0.5/n) * scale
		s.notice = 120
		s.rebalance = pt.Rebalance
		s.lead = 15 * minute
		s.walk = 0.03
	case "gcp":
		s.hazard = 0.005 * scale
		s.notice = 30
	case "azure":
		s.hazard = 0.003 * scale
		s.notice = 30
		s.walk = 0.05
		if pt.Evict == 0 {
			s.restart = 0.5
		}
	}
	if pt.MaxPrice > 0 && s.provider != "gcp" {
		s.maxPrice = pt.MaxPrice * od
	}
	return s
}

// The spot grid's own ranges: farms that burst, every provider.
var spotRanges = map[string][2]float64{
	"nodes": {2, 200}, "load": {0.8, 2}, "tightness": {0.7, 6}, "unitMin": {2, 120}, "days": {0.5, 1.5},
	"burstFrac": {0.5, 1.5}, "burstBudget": {0.3, 1}, "lateCost": {50, 2000}, "provisioning": {2, 15},
	"mlShare": {0, 0.8}, "ckptMin": {5, 60}, "gpuFrac": {0, 0.3}, "burstCost": {0.5, 4},
	"provider": {1, 4}, "spotBand": {0, 5}, "diversity": {1, 7}, "hazardScale": {1, 30},
	"spotDiscount": {0.6, 0.91}, "maxPrice": {0, 1}, "evict": {0, 2}, "rebalance": {0, 1}, "ckptWrite": {5, 120},
}

// sampleSpot draws the spot grid: the main grid's sampler with the
// ranges above, and the remaining dimensions at a middle farm's values.
func sampleSpot(o gridOpts) []Point {
	saved := make([]dim, len(dims))
	copy(saved, dims)
	defer func() { copy(dims, saved) }()
	for i := range dims {
		r, ok := spotRanges[dims[i].name]
		if !ok {
			dims[i].lo, dims[i].hi = 0, 0
			continue
		}
		dims[i].lo, dims[i].hi = r[0], r[1]
		if o.MaxNodes > 0 && dims[i].name == "nodes" {
			dims[i].hi = math.Min(dims[i].hi, o.MaxNodes)
		}
		if dims[i].name == "maxPrice" {
			dims[i].zero = 0.5 // half the points set no maximum
		}
	}
	pts := sample(gridOpts{N: o.N, Seed: o.Seed ^ 0x5b07, MaxDays: o.MaxDays})
	for i := range pts {
		p := &pts[i]
		b := base(fmt.Sprintf("s%03d", i))
		for _, d := range saved {
			if _, ok := spotRanges[d.name]; ok || d.lo == d.hi {
				continue
			}
			*d.f(p) = *d.f(&b)
		}
		p.Name = b.Name
		p.Drain = o.Drain
	}
	return pts
}

// Spot policies: the same work under four ways of using spot capacity.
var spotPolicies = []policy{
	{name: "all-spot", fn: sched.Decide, burst: true, usesEs: true},
	{name: "all-on-demand", fn: sched.Decide, burst: true, usesEs: true},
	{name: "cheapest-first", fn: sched.Decide, burst: true, usesEs: true, checked: true},
	{name: "completion-aware", fn: sched.Decide, burst: true, usesEs: true, spotAware: true},
}

// spotWorkload removes the pools a spot policy does not use.
func spotWorkload(w *workload, pol string) *workload {
	out := *w
	out.pools = nil
	for _, p := range w.pools {
		switch {
		case pol == "all-spot" && strings.HasSuffix(p.name, "-od"):
			continue
		case pol == "all-on-demand" && strings.HasSuffix(p.name, "-spot"):
			continue
		}
		out.pools = append(out.pools, p)
	}
	return &out
}

// Engine side: the interruption pipeline (rebalance, notice, kill) and the
// price walk of a spot pool.

func (e *engine) startSpot(p *pool) {
	s := p.def.burst.spot
	if s == nil {
		return
	}
	p.price = p.def.burst.price
	e.nextSpot(p, 0, 0)
	if s.walk > 0 {
		e.push(&event{t: hour, kind: evPrice, p: p, attempt: 1})
	}
}

func (e *engine) nextSpot(p *pool, k int, from float64) {
	b := p.def.burst
	if b.max <= 0 || b.spot.hazard <= 0 {
		return
	}
	t := from + e.expo(b.spot.hazard*float64(b.max)/hour, e.hrand(9, uint64(p.idx), 1, uint64(k)))
	if t < e.w.horizon {
		e.push(&event{t: t, kind: evSpot, p: p, attempt: k})
	}
}

// spotEvent picks the slot an interruption hits (thinned to the pool's
// provisioned slots) and starts its warnings.
func (e *engine) spotEvent(p *pool, k int) {
	e.nextSpot(p, k+1, e.now)
	s := p.def.burst.spot
	if p.slots == 0 || e.hrand(10, uint64(p.idx), 1, uint64(k)) > float64(p.slots)/float64(p.def.burst.max) {
		return
	}
	var target *unit
	slot := int(e.hrand(11, uint64(p.idx), 1, uint64(k)) * float64(p.slots))
	at := 0
	for _, u := range p.running {
		if slot >= at && slot < at+u.spec.slots {
			target = u
			break
		}
		at += u.spec.slots
	}
	e.res.SpotInterruptions++
	e.warn(p, target, e.hrand(12, uint64(p.idx), 1, uint64(k)) < s.rebalance, e.hrand(13, uint64(p.idx), 1, uint64(k)))
}

// warn schedules the warnings and the interruption of one slot (target:
// the unit on it, or nil for an idle slot).
func (e *engine) warn(p *pool, target *unit, rebalance bool, u float64) {
	s := p.def.burst.spot
	t := e.now
	att := 0
	if target != nil {
		att = target.attempt
	}
	if rebalance {
		e.push(&event{t: t, kind: evSignal, p: p, u: target, attempt: att, n: 0})
		t += -math.Log(math.Max(u, 1e-9)) * s.lead
	}
	e.push(&event{t: t, kind: evSignal, p: p, u: target, attempt: att, n: 1})
	e.push(&event{t: t + s.notice, kind: evKill, p: p, u: target, attempt: att})
}

// signal is a rebalance recommendation (n 0) or an interruption notice
// (n 1) for a unit. A spot-aware policy acts on it; others do not.
func (e *engine) signal(p *pool, u *unit, att, n int) {
	if u == nil || u.state != stRunning || u.attempt != att || u.pool != p || !e.pol.spotAware {
		return
	}
	ml := u.j.spec.ml
	prog := math.Max(0, e.now-u.start-u.restore)
	switch {
	case n == 0 && ml != nil:
		// Rebalance: checkpoint now and move.
		e.res.SpotSaves++
		e.stopAs(u, evYield, false)
	case n == 0 && prog < 0.15*u.spec.dur:
		// Rebalance, little done: restart it elsewhere now.
		e.res.SpotSaves++
		e.stopAs(u, evFail, false)
	case n == 1 && ml != nil && e.w.pt.CkptWrite <= p.def.burst.spot.notice:
		// Notice: a checkpoint fits in it.
		u.noticeKept = math.Max(0, prog-e.w.pt.CkptWrite)
		e.res.SpotSaves++
	}
}

// kill ends an interrupted slot: its unit if still there, else an idle
// slot, else the newest unit. The slot comes back after the provisioning
// delay (a deallocated Azure VM sooner).
func (e *engine) kill(p *pool, target *unit, att int) {
	if p.slots == 0 {
		return
	}
	p.meter(e.now)
	switch {
	case target != nil && target.state == stRunning && target.attempt == att && target.pool == p:
		e.spotLoss(target)
		e.stopAs(target, evFail, false)
	case p.slots > p.used:
	case len(p.running) > 0:
		v := p.running[len(p.running)-1]
		e.spotLoss(v)
		e.stopAs(v, evFail, false)
	}
	p.slots--
	p.reclaims++
	b := p.def.burst
	delay := b.delay
	if b.spot.restart > 0 {
		delay *= b.spot.restart
	}
	bt := &batch{n: 1}
	p.batches = append(p.batches, bt)
	e.push(&event{t: e.now + delay, kind: evProvision, p: p, b: bt})
	e.requestDecide()
}

func (e *engine) spotLoss(u *unit) {
	e.res.SpotKills++
	el := math.Max(0, e.now-u.start-u.restore)
	if ml := u.j.spec.ml; ml != nil {
		el -= math.Max(math.Floor(el/ml.ckpt)*ml.ckpt, u.noticeKept)
	}
	e.res.SpotLostSlotHours += math.Max(0, el) * float64(u.spec.slots) / hour
	e.res.SpotLostByLen[lenBin(u.spec.dur)] += math.Max(0, el) * float64(u.spec.slots) / hour
}

// lenBin buckets a unit's length for the exposure table.
func lenBin(sec float64) int {
	switch {
	case sec < 10*minute:
		return 0
	case sec < hour:
		return 1
	case sec < 4*hour:
		return 2
	}
	return 3
}

var lenBins = []string{"under 10 min", "10 to 60 min", "1 to 4 h", "4 h or more"}

// priceStep moves a spot price hourly; past a maximum price every
// instance in the pool is interrupted (AWS and Azure).
func (e *engine) priceStep(p *pool, k int) {
	s := p.def.burst.spot
	p.meter(e.now)
	z := math.Sqrt(-2*math.Log(e.hrand(14, uint64(p.idx), 2, uint64(k)))) * math.Cos(2*math.Pi*e.hrand(15, uint64(p.idx), 2, uint64(k)))
	p.price *= math.Exp(s.walk / math.Sqrt(24) * z)
	p.price = math.Min(s.od, math.Max(0.1*s.od, p.price))
	if e.now+hour < e.w.horizon {
		e.push(&event{t: e.now + hour, kind: evPrice, p: p, attempt: k + 1})
	}
	if s.maxPrice <= 0 || p.price <= s.maxPrice {
		return
	}
	// Price-based interruption of every instance: the maximum only adds
	// interruptions.
	e.res.PriceInterruptions += p.slots
	for _, u := range append([]*unit(nil), p.running...) {
		e.warn(p, u, false, 0.5)
	}
	for range p.slots - p.used {
		e.warn(p, nil, false, 0.5)
	}
}

// The completion-aware policy: sched.Decide, then a pass over its
// dispatches into spot pools. A unit's expected loss on spot is the
// chance it is interrupted before it ends (hazard times its remaining
// time) times what an interruption costs: the work since its last
// checkpoint, redone at the on-demand price, plus the lateness cost of the
// delay if its slack does not cover it. When that exceeds the on-demand
// premium over the unit's run, the unit goes to the on-demand pool of its
// class (if it has room), and on-demand capacity is requested for those
// that found none, within the project's burst budget. Long units without
// checkpoints and units of jobs with little slack go on demand; short and
// checkpointing units stay on spot. The engine side (signal) checkpoints
// on a rebalance recommendation or a notice.
func (e *engine) completionAware(s sched.Snapshot, st *sched.State, cfg sched.Config) sched.Decisions {
	d := sched.Decide(s, st, cfg)
	pools := map[string]*sched.Pool{}
	free := map[string]int{}
	for i := range s.Pools {
		pools[s.Pools[i].Name] = &s.Pools[i]
		free[s.Pools[i].Name] = s.Pools[i].Slots - s.Pools[i].Used
	}
	for _, x := range d.Dispatch {
		free[x.Pool] -= x.Slots
	}
	jobs := map[string]*sched.Job{}
	for i := range s.Jobs {
		jobs[s.Jobs[i].ID] = &s.Jobs[i]
	}
	late := map[string]float64{}
	budget := map[string]float64{}
	for _, pr := range s.Projects {
		late[pr.Name], budget[pr.Name] = pr.LateCostPerHour, pr.BurstBudgetLeft
	}
	want := map[string]int{}
	for i, x := range d.Dispatch {
		if !strings.HasSuffix(x.Pool, "-spot") {
			continue
		}
		od := strings.TrimSuffix(x.Pool, "-spot") + "-od"
		sp, op := e.byName[x.Pool], e.byName[od]
		j := jobs[x.Job]
		if sp == nil || op == nil || j == nil {
			continue
		}
		u := e.byID[x.Unit]
		if u == nil {
			continue
		}
		if !e.riskyOnSpot(j, u, sp, op, d.Jobs[x.Job], late[j.Project]) {
			continue
		}
		e.res.SpotRiskyUnits++
		if free[od] >= x.Slots {
			free[od] -= x.Slots
			free[x.Pool] += x.Slots
			d.Dispatch[i].Pool = od
			e.res.SpotMoved++
			continue
		}
		premium := (op.def.burst.price - sp.price) * e.unitHours(j, u) * float64(x.Slots)
		if premium <= budget[j.Project] {
			budget[j.Project] -= premium
			want[od] += x.Slots
			d.Bursts = append(d.Bursts, sched.BurstSuggestion{Job: j.ID, Project: j.Project, Slots: map[string]int{od: x.Slots},
				Order: []string{od}, Cost: premium, Closes: true, Auto: true, Why: "on demand: expected interruption loss above the premium"})
		}
	}
	for i, c := range d.Capacity {
		if n := want[c.Pool]; n > 0 {
			p := pools[c.Pool]
			d.Capacity[i].Slots = min(p.Burst.MaxSlots, max(c.Slots, p.Burst.Requested+n))
			d.Capacity[i].Reason += fmt.Sprintf("; +%d on demand for units at risk of interruption", n)
		}
	}
	return d
}

// unitHours is a unit's expected remaining run, from the job's estimate.
func (e *engine) unitHours(j *sched.Job, u *unit) float64 {
	n := max(1, j.Estimate.Queued+j.Estimate.Running)
	per := j.Estimate.W / float64(n) / float64(max(1, u.spec.slots))
	if u.retained > 0 {
		per = math.Max(0, per-u.retained)
	}
	return math.Max(per, minute) / hour
}

func (e *engine) riskyOnSpot(j *sched.Job, u *unit, sp, op *pool, st sched.JobState, lateCost float64) bool {
	sd := sp.def.burst.spot
	if sd == nil {
		return false
	}
	h := e.unitHours(j, u)
	pInt := 1 - math.Exp(-sd.hazard*h)
	lost := h / 2 // a non-checkpointing unit loses on average half its run
	if ml := u.j.spec.ml; ml != nil {
		lost = math.Min(lost, ml.ckpt/hour/2)
		if e.w.pt.CkptWrite <= sd.notice {
			lost = ml.restore / hour
		}
	}
	slots := float64(u.spec.slots)
	delay := lost + sp.def.burst.delay/hour
	lateness := 0.0
	if !j.Deadline.IsZero() {
		lateness = lateCost * math.Max(0, delay-st.Slack.Hours())
	}
	loss := pInt * (lost*slots*op.def.burst.price + lateness)
	premium := (op.def.burst.price - sp.price) * h * slots
	return loss > premium
}
