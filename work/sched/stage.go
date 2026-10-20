package sched

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Staging (experimental): a job at risk where its inputs are may finish
// in time at another site's pool once its inputs are there (Job.Sources).
// A remote pool is capacity like a burst pool: it becomes usable after a
// delay (the transfer, behind what its link already carries) and has a
// price (the transfer's, plus the work moved there at the pool's price).
// Unlike a burst pool its slots are not new: only what its own pool
// group's plan leaves spare (capacity less required slots) is counted.
//
// burst (burst.go) sizes both for each job at risk and takes the cheaper
// one that closes the deadline, under the same budget and lateness rules.
// A stage is one pool and one source: the cheapest that closes. A job
// whose inputs are already being staged (a "staging" source) that would
// close it gets nothing new.

// notePlan records each planned pool group's capacity and required slots
// on its pools, for spare.
func (a *alloc) notePlan(g []groupMember, C float64) {
	if a.poolGroupC == nil {
		a.poolGroupC, a.poolGroupR = map[string]float64{}, map[string]float64{}
	}
	r := 0.0
	for _, m := range g {
		r += m.R
	}
	for _, m := range g {
		for _, name := range m.j.Pools {
			a.poolGroupC[name] = C
			a.poolGroupR[name] = r
		}
	}
}

// spare is how many of a pool's slots its own plan does not need, less
// what stages this decision already counted on.
func (a *alloc) spare(ps *poolState) int {
	s := float64(ps.p.Slots)
	if c, ok := a.poolGroupC[ps.p.Name]; ok {
		s = math.Min(s, math.Max(0, c-a.poolGroupR[ps.p.Name]))
	}
	return max(0, int(math.Floor(s+1e-9))-a.stageSlots[ps.p.Name])
}

// delay is how long until a source has the job's inputs at the pool: its
// latency, its bytes behind what its link carries, and a recompute's
// longest unit. Not ok when it names a link the snapshot lacks.
func (a *alloc) delay(src Source) (time.Duration, bool) {
	d := src.Latency.Seconds()
	if src.Bytes > 0 {
		l := a.links[src.Link]
		if l == nil || l.BytesPerSecond <= 0 {
			return 0, false
		}
		d += (l.Committed + a.linkBytes[src.Link] + src.Bytes) / l.BytesPerSecond
	}
	if src.Via == "recompute" {
		d += src.Longest.Seconds()
	}
	return secs(d), true
}

// sizeStage is the cheapest stage that covers deficit slot-seconds by the
// job's horizon, or nil. A "staging" source is free and is preferred.
func (a *alloc) sizeStage(jc *jobCtx, deficit float64) *Stage {
	T := jc.horizon
	room := math.Inf(1)
	if jc.j.MaxParallel > 0 {
		room = float64(jc.j.MaxParallel) - jc.share
	}
	var best *Stage
	for _, name := range jc.remote {
		ps := a.pools[name]
		if ps == nil || ps.p.Burst != nil {
			continue
		}
		spare := min(float64(a.spare(ps)), room)
		for _, src := range jc.j.Sources[name] {
			d, ok := a.delay(src)
			if !ok || d.Seconds() >= T || spare < 1 {
				continue
			}
			per := T - d.Seconds()
			need := deficit
			if src.Via == "recompute" {
				need += src.Work
			}
			if math.Ceil(need/per) > spare {
				continue
			}
			x := int(math.Ceil(need / per))
			price := ps.p.CostPerSlotHour / 3600
			move, run := src.Cost, price*need
			if src.Via == "staging" {
				move, run = 0, 0
			}
			cost := move + run
			if best == nil || cost < best.Cost || cost == best.Cost && d < best.Delay {
				best = &Stage{Job: jc.j.ID, Project: jc.j.Project, Pool: name, Source: src, Delay: d, Slots: x,
					Cost: cost, MoveCost: move, RunCost: run, Closes: true}
			}
		}
	}
	return best
}

// cheapestSource is the job's cheapest source to pool that arrives
// before horizon (a "staging" one first: it is free), with its delay.
func (a *alloc) cheapestSource(jc *jobCtx, pool string, horizon float64) (Source, time.Duration, bool) {
	var best Source
	var bd time.Duration
	found := false
	for _, src := range jc.j.Sources[pool] {
		d, ok := a.delay(src)
		if !ok || d.Seconds() >= horizon {
			continue
		}
		c := src.Cost
		if src.Via == "staging" {
			c = -1
		}
		bc := best.Cost
		if best.Via == "staging" {
			bc = -1
		}
		if !found || c < bc || c == bc && d < bd {
			best, bd, found = src, d, true
		}
	}
	return best, bd, found
}

// commitStage counts an approved stage's bytes on its link and its slots
// on its pool, so later stages in the same decision see them.
func (a *alloc) commitStage(sg *Stage) {
	if sg.Source.Bytes > 0 {
		a.linkBytes[sg.Source.Link] += sg.Source.Bytes
	}
	a.stageSlots[sg.Pool] += sg.Slots
}

// stageWords is a stage in a sentence: "staging its inputs to lon-cpu via
// interconnect, arriving in 40m".
func stageWords(sg *Stage) string {
	how := "staging its inputs to " + sg.Pool
	switch sg.Source.Via {
	case "recompute":
		how = "recomputing its inputs at " + sg.Pool
	case "reuse":
		how = "reusing an identical output at " + sg.Pool
	case "staging":
		return "its inputs arriving at " + sg.Pool + " in " + human(sg.Delay)
	}
	if sg.Source.Channel != "" {
		how += " via " + sg.Source.Channel
	}
	return how + ", arriving in " + human(sg.Delay)
}

// chooseStage stages the job's inputs instead of bursting when a stage
// closes its deadline and is the better choice: one already under way,
// or one that is approved automatically (within budget, below the
// lateness cost) and cheaper than the burst or the burst would not be.
// A stage needing approval is reported when no burst closes the
// deadline. It reports whether the job is handled.
func (a *alloc) chooseStage(jc *jobCtx, deficit, lateCost float64, sg *BurstSuggestion, budget map[string]float64) bool {
	if a.cfg.NoStage || len(jc.remote) == 0 || !jc.view().atRisk {
		return false
	}
	st := a.sizeStage(jc, deficit)
	if st == nil {
		return false
	}
	st.LateCost = lateCost
	if st.Source.Via == "staging" {
		st.Auto = true
		a.commitStage(st)
		jc.stage = st
		a.showStaged(jc, st)
		return true
	}
	left := budget[jc.proj.p.Name]
	switch {
	case st.Cost > left:
		st.Why = fmt.Sprintf("over the project's remaining burst budget (%s left)", dollars(left))
	case st.Cost >= lateCost:
		st.Why = fmt.Sprintf("costs more than the lateness it prevents (%s at %s/h)", dollars(lateCost), dollars(jc.proj.p.LateCostPerHour))
	default:
		st.Auto = true
	}
	burstAuto := sg != nil && sg.Closes && a.cfg.Burst && sg.Cost <= left && sg.Cost < lateCost
	switch {
	case st.Auto && (!burstAuto || st.Cost < sg.Cost):
		budget[jc.proj.p.Name] -= st.Cost
		a.commitStage(st)
		a.showStaged(jc, st)
	case !st.Auto && (sg == nil || !sg.Closes):
	default:
		return false
	}
	jc.stage = st
	a.out.Stage = append(a.out.Stage, *st)
	return true
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// showStaged makes what a job reports the plan with its stage: it holds
// its share where its inputs are until they arrive, then that plus the
// stage's slots, so a job the stage saves reads on track with the ETA it
// will have, not the lateness of the plan without it.
func (a *alloc) showStaged(jc *jobCtx, st *Stage) {
	d := st.Delay.Seconds()
	rate := jc.share + float64(st.Slots)
	finish := func(w float64) float64 {
		f := d + math.Max(0, w-jc.share*d)/math.Max(rate, 1e-9)
		if a.cfg.TailAware {
			return f + jc.tpath
		}
		return math.Max(f, jc.tpath)
	}
	f50, f90 := finish(jc.W), finish(a.preTail(jc, jc.W90))
	if f90 > jc.T {
		return // the stage closes the planned deficit, not the reported one
	}
	jc.shown = &shownPlan{finish50: f50, finish90: f90}
}

// stageCost is a stage's price in words, the move and the run apart:
// "about $1 to move its inputs, $90 to run there".
func stageCost(st *Stage) string {
	if st.RunCost <= 0 {
		return "about " + dollars(st.MoveCost) + " to move"
	}
	return "about " + dollars(st.MoveCost) + " to move, " + dollars(st.RunCost) + " to run there"
}
