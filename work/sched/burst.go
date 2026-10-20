package sched

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/work/forecast"
)

// burst implements the cost-aware capacity decisions of DESIGN.md section
// 9.12.
//
// For every job at risk in the plan with time left, the planner gives the
// shortfall D in slot-seconds and the horizon T by which it must be
// covered (plan.go). Burst pools
// the job may use are tried cheapest first by price per slot-hour. A pool
// whose provisioning delay d is not shorter than T is skipped; otherwise
// each of its new slots contributes T-d seconds, so it takes
// ceil(D_left/(T-d)) slots, bounded by what the pool has left. The cost
// estimate is price * (slots*d + work moved), since nodes bill from
// launch. Greedy by price is a heuristic: it finds the cheapest set when
// prices and delays rank the same way, and can overspend when a cheap
// slow pool needs many slots that a pricier fast pool would not.
//
// A suggestion that restores the deadline is requested automatically if
// its cost is within the project's remaining daily budget (less what this
// call already approved) and below the lateness cost it prevents,
// LateCostPerHour times LateBy. Otherwise it is reported for approval.
// Partial bursts that only reduce lateness are reported, never requested.
//
// Candidates, D, T and the lateness cost come from the plan with its
// margin (the critical path counted twice, see planFluid). A job on time
// under Graham's bound but late with the margin gets a burst marked
// Margin: it buys margin against estimate uncertainty, and its message
// and capacity request say so. A burst priced at the lateness a job
// would have at the share the plan leaves it now, above the lateness it
// reports, is marked Floor, and its message and capacity request say so.
//
// Each burst pool's request is its current request plus approved
// additions. It shrinks only after the pool has had idle provisioned
// slots for ScaleDownAfter, and then by the idle count.
func (a *alloc) burst() {
	promised, margin, floor := map[string]int{}, map[string]int{}, map[string]int{}
	budget := map[string]float64{}
	for name, pr := range a.projects {
		budget[name] = pr.p.BurstBudgetLeft
	}
	var cands []*jobCtx
	for _, jc := range a.jobs {
		if jc.hasDeadline && jc.atRisk && jc.T > 0 && jc.deficit > 0 && !jc.j.Hold && !jc.j.HoldStop {
			cands = append(cands, jc)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].horizon != cands[j].horizon {
			return cands[i].horizon < cands[j].horizon
		}
		return cands[i].j.ID < cands[j].j.ID
	})
	for _, jc := range cands {
		deficit, lateBy := jc.deficit, jc.lateBy
		lateCost := jc.proj.p.LateCostPerHour * lateBy.Hours()
		sg := a.sizeBurst(jc, promised, deficit, 0)
		if a.chooseStage(jc, deficit, lateCost, sg, budget) {
			continue
		}
		if sg == nil {
			continue
		}
		sg.Margin = !jc.view().atRisk
		sg.Floor = !sg.Margin && lateBy > jc.view().lateBy+time.Minute
		sg.LateCost = lateCost
		switch {
		case !a.cfg.Burst:
			sg.Why = "burst requests are disabled"
		case !sg.Closes:
			// Why was set by sizeBurst.
		case sg.Cost > budget[jc.proj.p.Name]:
			sg.Why = fmt.Sprintf("over the project's remaining burst budget (%s left)", dollars(budget[jc.proj.p.Name]))
		case sg.Cost >= lateCost:
			sg.Why = fmt.Sprintf("costs more than the lateness it prevents (%s at %s/h)", dollars(lateCost), dollars(jc.proj.p.LateCostPerHour))
		default:
			sg.Auto = true
			budget[jc.proj.p.Name] -= sg.Cost
			for i := range sg.Stages {
				st := &sg.Stages[i]
				st.Auto, st.LateCost = true, lateCost
				a.commitStage(st)
				if st.Source.Via != "staging" {
					a.out.Stage = append(a.out.Stage, *st)
				}
			}
			for pool, n := range sg.Slots {
				promised[pool] += n
				switch {
				case sg.Margin:
					margin[pool] += n
				case sg.Floor:
					floor[pool] += n
				}
			}
		}
		jc.burst = sg
		a.out.Bursts = append(a.out.Bursts, *sg)
	}
	a.topUpBursts(cands, promised, margin, budget)
	if !a.cfg.Burst {
		return
	}
	for _, ps := range a.poolList {
		b := ps.p.Burst
		if b == nil {
			continue
		}
		name := ps.p.Name
		req := b.Requested + promised[name]
		reason := "steady"
		switch {
		case promised[name] > 0 && margin[name] == promised[name]:
			delete(a.st.surplus, name)
			reason = fmt.Sprintf("+%d as margin against estimate uncertainty", promised[name])
		case promised[name] > 0 && floor[name] == promised[name]:
			delete(a.st.surplus, name)
			reason = fmt.Sprintf("+%d for jobs at risk, priced at their lateness at the share they have now", promised[name])
		case promised[name] > 0 && margin[name]+floor[name] > 0:
			delete(a.st.surplus, name)
			reason = fmt.Sprintf("+%d for jobs at risk", promised[name])
			if margin[name] > 0 {
				reason += fmt.Sprintf(", %d of them as margin against estimate uncertainty", margin[name])
			}
			if floor[name] > 0 {
				reason += fmt.Sprintf(", %d priced at their lateness at the share they have now", floor[name])
			}
		case promised[name] > 0:
			delete(a.st.surplus, name)
			reason = fmt.Sprintf("+%d for jobs at risk", promised[name])
		case ps.free > 0:
			since, ok := a.st.surplus[name]
			if !ok {
				a.st.surplus[name] = a.now
				reason = fmt.Sprintf("%d idle, waiting %s before shrinking", ps.free, a.cfg.ScaleDownAfter)
				break
			}
			if a.now.Sub(since) < a.cfg.ScaleDownAfter {
				reason = fmt.Sprintf("%d idle since %s", ps.free, forecast.Clock(since, a.cfg.Loc))
				break
			}
			req = max(0, b.Requested-ps.free)
			delete(a.st.surplus, name)
			reason = fmt.Sprintf("-%d idle for %s", ps.free, a.now.Sub(since).Round(time.Minute))
		default:
			delete(a.st.surplus, name)
		}
		req = min(req, b.MaxSlots)
		a.out.Capacity = append(a.out.Capacity, CapacityRequest{Pool: name, Slots: req, Reason: reason})
	}
}

func (a *alloc) sizeBurst(jc *jobCtx, promised map[string]int, deficit float64, used int) *BurstSuggestion {
	sg := &BurstSuggestion{Job: jc.j.ID, Project: jc.proj.p.Name, Slots: map[string]int{}}
	if jc.tpath > jc.T {
		sg.Why = fmt.Sprintf("its critical path alone (%s) runs past the deadline", human(secs(jc.tpath)))
		return sg
	}
	var pools []*poolState
	for _, name := range jc.j.Pools {
		if ps := a.pools[name]; ps != nil && ps.p.Burst != nil {
			pools = append(pools, ps)
		}
	}
	// A burst pool at another site provisions while the job's inputs move
	// there (remoteBurst): usable after the longer of the two, and the
	// move is part of the burst's cost.
	moves := map[string]Source{}
	moveDelay := map[string]float64{}
	if !a.cfg.NoStage {
		for _, name := range jc.remote {
			ps := a.pools[name]
			if ps == nil || ps.p.Burst == nil {
				continue
			}
			if src, d, ok := a.cheapestSource(jc, name, jc.horizon); ok {
				moves[name], moveDelay[name] = src, d.Seconds()
				pools = append(pools, ps)
			}
		}
	}
	if len(pools) == 0 {
		return nil
	}
	sort.SliceStable(pools, func(i, j int) bool {
		return pools[i].p.Burst.CostPerSlotHour < pools[j].p.Burst.CostPerSlotHour
	})
	left := deficit
	T := jc.horizon
	room := math.Inf(1)
	if jc.j.MaxParallel > 0 {
		room = float64(jc.j.MaxParallel) - jc.share
	}
	if !jc.expanding() {
		units := jc.j.RunnableTotal + jc.j.Blocked + len(jc.j.RunningUnits)
		room = math.Min(room, float64(units*jc.slots)-jc.share)
		// A gang that fits in none of its pools now holds no part of its
		// share: the burst may have to carry the whole gang.
		if k := a.waitingGang(jc); k > 0 {
			room = math.Max(room, float64(k))
		}
	}
	room -= float64(used)
	slow := 0
	added := 0.0
	for _, ps := range pools {
		if left <= 0 || room < 1 {
			break
		}
		b := ps.p.Burst
		d := b.Provisioning.Seconds()
		src, far := moves[ps.p.Name]
		if far {
			d = math.Max(d, moveDelay[ps.p.Name])
		}
		if d >= T {
			slow++
			continue
		}
		avail := b.MaxSlots - b.Requested - promised[ps.p.Name]
		if avail <= 0 {
			continue
		}
		per := T - d
		x := min(atMost(math.Ceil(left/per), avail), atMost(room, avail))
		if x <= 0 {
			continue
		}
		take := math.Min(left, float64(x)*per)
		sg.Cost += b.CostPerSlotHour / 3600 * (float64(x)*d + take)
		if far {
			move := src.Cost
			if src.Via == "staging" {
				move = 0
			}
			sg.Cost += move
			sg.Stages = append(sg.Stages, Stage{Job: jc.j.ID, Project: jc.proj.p.Name, Pool: ps.p.Name, Source: src,
				Delay: secs(moveDelay[ps.p.Name]), Slots: x, MoveCost: move, Cost: move, Closes: true})
		}
		sg.Slots[ps.p.Name] += x
		sg.Order = append(sg.Order, ps.p.Name)
		left -= take
		room -= float64(x)
		added += float64(x)
	}
	if left <= 1e-6 {
		sg.Closes = true
		return sg
	}
	switch {
	case added == 0 && slow > 0:
		sg.Why = "no burst pool can provision before the deadline"
	case added == 0:
		sg.Why = "no burst capacity is left"
	default:
		rest := secs(left / math.Max(1, jc.share+added))
		sg.Why = fmt.Sprintf("the most burst available still leaves it about %s late", human(rest))
	}
	return sg
}

// atMost is x as an int, at most hi. A float above every int (room is
// +Inf for a job still expanding) converts by architecture: MaxInt64 on
// arm64, MinInt64 on amd64, where such a job was offered no burst. So it
// is compared before it is converted.
func atMost(x float64, hi int) int {
	if !(x < float64(hi)) {
		return hi
	}
	return int(x)
}

// waitingGang returns the size of the job's next unit if it is a gang
// that fits in none of its pools' free slots now, else 0.
func (a *alloc) waitingGang(jc *jobCtx) int {
	if jc.next >= len(jc.j.Runnable) {
		return 0
	}
	k := jc.unitSlots(jc.j.Runnable[jc.next])
	if k <= 1 {
		return 0
	}
	for _, name := range jc.j.Pools {
		if ps := a.pools[name]; ps != nil && ps.free >= k {
			return 0
		}
	}
	return k
}

// report fills JobState for every job, with a message in production's
// terms.
func (a *alloc) report() {
	for _, jc := range a.jobs {
		st := a.out.Jobs[jc.j.ID]
		st.Required = jc.Rb
		st.Attainable = jc.A
		st.RunningSlots = float64(jc.running)
		st.Share = jc.share
		st.GroupSlots = jc.groupC
		st.Slack = jc.slack
		st.Feasible = jc.feasible
		v := jc.view()
		st.AtRisk = v.atRisk
		st.LateBy = v.lateBy
		st.Burst = jc.burst
		st.Stage = jc.stage
		if jc.hasDeadline {
			st.Deadline, st.DeadlineFor = jc.deadline, jc.deadlineFor
		}
		if jc.startShown > 0 && !math.IsInf(jc.startShown, 1) {
			st.StartAfter = a.now.Add(secs(jc.startShown))
		}
		if jc.hasDeadline && v.finish90 > 0 && !math.IsInf(v.finish90, 1) {
			st.ETA50 = a.now.Add(secs(v.finish50))
			st.ETA90 = a.now.Add(secs(v.finish90))
		}
		st.Message = a.message(jc, st)
		a.out.Jobs[jc.j.ID] = st
	}
}

func (a *alloc) message(jc *jobCtx, st JobState) string {
	id := jc.j.ID
	switch {
	case jc.j.Hold || jc.j.HoldStop:
		return id + " is on hold."
	case jc.hasRate && jc.atRisk:
		return fmt.Sprintf("%s needs %.0f slots for its rate and can expect %.0f.", id, jc.R, jc.share)
	case jc.hasRate:
		return fmt.Sprintf("%s is meeting its rate of %.0f slots.", id, jc.R)
	case !jc.hasDeadline:
		return ""
	}
	// A deadline set by a dependent job names it: "the deadline of
	// sh0220-comp, Fri 07:30 PDT (14:30 UTC)". dl is the phrase, dlc the
	// same with a closing comma where a clause follows it.
	own := forecast.Clock(jc.deadline, a.cfg.Loc)
	dl, dlc, past, due := own, own, "its deadline "+own, own
	if jc.deadlineFor != "" {
		dl = "the deadline of " + shortID(jc.deadlineFor, id) + ", " + own
		dlc, past = dl+",", dl+","
		due = own + ", the deadline of " + shortID(jc.deadlineFor, id)
	}
	v := jc.view()
	if jc.noPool {
		return fmt.Sprintf("%s cannot run: no pool can run it (none of %s is in the farm); due %s.", id, strings.Join(jc.j.Pools, ", "), due)
	}
	if jc.T <= 0 {
		more := "with no capacity planned for it"
		if v.finish90 > 0 && !math.IsInf(v.finish90, 1) {
			more = fmt.Sprintf("about %s more at current capacity", human(secs(v.finish90)))
		}
		return fmt.Sprintf("%s is past %s by %s; %s.", id, past, human(secs(-jc.T)), more)
	}
	sg := jc.burst
	if !v.atRisk {
		var b strings.Builder
		if st.ETA90.IsZero() {
			fmt.Fprintf(&b, "%s is on track for %s.", id, dl)
		} else {
			// The slack against the finish it shows, as the ETA beside it:
			// jc.slack is at all attainable capacity (Feasible's question),
			// and read "slack 12h" beside an ETA half an hour from the
			// deadline.
			slack := secs(jc.T - v.finish90)
			fmt.Fprintf(&b, "%s is on track for %s: ETA %s, slack %s.", id, dl, forecast.Clock(st.ETA90, a.cfg.Loc), human(slack))
		}
		if s := jc.stage; s != nil {
			via := ""
			if s.Source.Channel != "" {
				via = " via " + s.Source.Channel
			}
			if s.Source.Via == "staging" {
				fmt.Fprintf(&b, " It runs at %s once its inputs arrive, in %s.", s.Pool, human(s.Delay))
			} else {
				fmt.Fprintf(&b, " It runs at %s once its inputs arrive%s in %s (%s).", s.Pool, via, human(s.Delay), stageCost(s))
			}
		}
		// A burst for a job on track is margin against estimate
		// uncertainty (see burst); say so rather than leave it unexplained.
		if sg != nil && sg.Closes && a.cfg.Burst {
			verb := "add"
			if !sg.Auto {
				verb = "would add"
			} else if slotCount(sg.Slots) == 1 {
				verb = "adds"
			}
			fmt.Fprintf(&b, " %s %s margin against estimate uncertainty (about %s).", slotList(sg), verb, dollars(sg.Cost))
			if !sg.Auto {
				fmt.Fprintf(&b, " Needs approval: %s.", sg.Why)
			}
		}
		return b.String()
	}
	var b strings.Builder
	if math.IsInf(v.finish90, 1) {
		fmt.Fprintf(&b, "%s will miss %s: no capacity is planned for it.", id, dl)
	} else {
		fmt.Fprintf(&b, "%s will miss %s by about %s at current capacity.", id, dlc, human(v.lateBy))
	}
	switch {
	case jc.stage != nil:
		st := jc.stage
		switch {
		// Still at risk with its stage (showStaged found the plan with it
		// late): say what is under way, not that it closes the deadline.
		case st.Source.Via == "staging":
			fmt.Fprintf(&b, " Its inputs are arriving at %s in %s.", st.Pool, human(st.Delay))
		case st.Auto:
			fmt.Fprintf(&b, " %s (%s).", capitalize(stageWords(st)), stageCost(st))
		default:
			fmt.Fprintf(&b, " %s, would close it (%s). Needs approval: %s.", capitalize(stageWords(st)), stageCost(st), st.Why)
		}
	case sg == nil:
		if !jc.feasible {
			b.WriteString(" It cannot finish in time even with all the capacity its pools and quota allow.")
		}
	case sg.Closes:
		fmt.Fprintf(&b, " %s %s it (about %s).", slotList(sg), closesVerb(sg.Slots), dollars(sg.Cost))
		if sg.Floor {
			// Priced at the plan's floor, not at the lateness shown.
			fmt.Fprintf(&b, " Priced at its lateness at the share it has now, about %s.", human(jc.lateBy))
		}
		if !sg.Auto {
			fmt.Fprintf(&b, " Needs approval: %s.", sg.Why)
		}
	default:
		fmt.Fprintf(&b, " No burst closes it: %s.", sg.Why)
	}
	return b.String()
}

// closesVerb agrees with slotList's subject: one slot closes, more close.
func closesVerb(m map[string]int) string {
	if slotCount(m) == 1 {
		return "closes"
	}
	return "close"
}

// slotList is a burst in words: "1 more gpu-spot slot", "3 more gpu-spot
// slots", "2 more gpu-spot and 1 more gpu slots".
func slotList(sg *BurstSuggestion) string {
	parts := make([]string, len(sg.Order))
	for i, n := range sg.Order {
		parts[i] = fmt.Sprintf("%d more %s", sg.Slots[n], n)
	}
	if slotCount(sg.Slots) == 1 {
		return parts[0] + " slot"
	}
	return strings.Join(parts, " and ") + " slots"
}

func slotCount(m map[string]int) int {
	n := 0
	for _, x := range m {
		n += x
	}
	return n
}

// shortID is a job ID as another job's message names it: without the
// namespace when both are in the same one.
func shortID(other, self string) string {
	if i := strings.Index(other, "/"); i > 0 && strings.HasPrefix(self, other[:i+1]) {
		return other[i+1:]
	}
	return other
}

func dollars(x float64) string {
	switch {
	case x >= 100:
		return fmt.Sprintf("$%.0f", math.Round(x/10)*10)
	case x > 0 && x < 1:
		// Cents, so a small lateness cost never reads as $0.
		return fmt.Sprintf("$%.2f", math.Max(x, 0.01))
	}
	return fmt.Sprintf("$%.0f", math.Round(x))
}

// human renders a duration the way a person would say it: minutes in
// fives under 90 minutes, whole hours under two days, days beyond.
func human(d time.Duration) string {
	switch {
	case d < 90*time.Minute:
		m := int(math.Round(d.Minutes()/5) * 5)
		return fmt.Sprintf("%dm", max(m, 5))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.0fh", math.Round(d.Hours()))
	default:
		return fmt.Sprintf("%.0f days", math.Round(d.Hours()/24))
	}
}

// topUpBursts sizes requested bursts to the shortfall the job's merged
// plan implies, with what budget and burst slots are left once every job
// has its planned burst.
//
// A job confined to a subset of its group's pools takes the more
// pessimistic of the group's plan and the subset's (planSubsets): the
// riskier plan's shortfall, but the smaller share of the two. That
// shortfall was computed at the riskier plan's share, so at the merged
// share the job needs more: W90 - share x horizon. Sizing every burst on
// it buys bursts that close less often, and so are not bought at all;
// sizing a job's burst on it first takes budget and slots from the jobs
// after it. So a requested burst is topped up afterwards, from what is
// left, and only when the top-up covers the rest of that shortfall and
// the whole burst still costs less than the lateness it prevents.
func (a *alloc) topUpBursts(cands []*jobCtx, promised, margin map[string]int, budget map[string]float64) {
	if !a.cfg.Burst {
		return
	}
	for _, jc := range cands {
		sg := jc.burst
		if sg == nil || !sg.Auto {
			continue
		}
		extra := math.Max(jc.deficit, jc.W90-jc.share*jc.horizon) - jc.deficit
		if extra <= 1e-6 {
			continue
		}
		used := 0
		for _, n := range sg.Slots {
			used += n
		}
		up := a.sizeBurst(jc, promised, extra, used)
		if up == nil || !up.Closes || len(up.Stages) > 0 || up.Cost > budget[jc.proj.p.Name] || sg.Cost+up.Cost >= sg.LateCost {
			continue
		}
		budget[jc.proj.p.Name] -= up.Cost
		sg.Cost += up.Cost
		for pool, n := range up.Slots {
			if _, ok := sg.Slots[pool]; !ok {
				sg.Order = append(sg.Order, pool)
			}
			sg.Slots[pool] += n
			promised[pool] += n
			if sg.Margin {
				margin[pool] += n
			}
		}
		for i := range a.out.Bursts {
			if a.out.Bursts[i].Job == sg.Job {
				a.out.Bursts[i] = *sg
			}
		}
	}
}
