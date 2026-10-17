package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/work/sched"
)

// The invariants. Each is checked on every decision of the policy under
// test (or at the end of a run), against the snapshot it saw and the
// engine's true state.
var invariants = []struct{ name, what string }{
	{"starvation", "a job with ready work and nothing running for 30 minutes gets the next free slot it may use"},
	{"starvation-over-deadline", "the starvation guard does not give the last free slot of a pool to a job with more than PreemptSlack of slack while a job on time in the plan needs it"},
	{"work-conservation", "no slot is left idle while ready work that may use it waits, unless a gang holds it, a borrowing limit forbids it, or it is held for another project's guaranteed deadline need"},
	{"blocked-rank", "a job whose units all wait on upstream work is never ranked ahead of a job with ready work"},
	{"blocked-dispatch", "only runnable units are dispatched"},
	{"overfill", "no pool is given more units than it has free slots"},
	{"hold", "a held job gets no units; a hold-stopped job's running units are all stopped"},
	{"borrow-limit", "no project goes past its nominal quota plus its borrowing limit"},
	{"boost-quota", "a boost takes slots only within its own project's quota or from capacity no other project is guaranteed"},
	{"lateness-unpriced", "a job planned late has LateBy > 0, so its lateness has a cost"},
	{"eta-inconsistent", "a job at risk has an ETA after its deadline; a job on track has one before it"},
	{"past-due-on-track", "an unfinished job past its deadline is reported at risk"},
	{"burst-uneconomic", "an automatic burst closes the deadline and costs less than the lateness it prevents"},
	{"burst-budget", "automatic bursts stay within each project's remaining daily budget"},
	{"burst-unjustified", "a burst pool's request grows only by automatically approved slots, and never past its limit"},
	{"churn-unit", "a unit is preempted at most MaxPreemptionsPerUnit times"},
	{"churn-pool", "a pool sees at most PreemptPerWindow preemptions per PreemptWindow"},
	{"churn-pingpong", "two jobs never take slots back and forth within an hour"},
	{"determinism", "the same point and seed give the same decisions"},
	{"metamorphic-capacity", "more owned capacity does not make more jobs late, beyond noise"},
	{"metamorphic-deadline", "looser deadlines do not make more jobs late, beyond noise"},
	{"metamorphic-budget", "a bigger burst budget does not make more jobs late, beyond noise"},
	{"decide-latency", "one decision takes under a second at every farm size in the grid"},
	{"dispatch-invalid", "dispatches name a known unit and a pool the job may use"},
}

// knownBugs are invariants that fail on main for a cause already found:
// the bugs hud-scenarios found (fixed on the scenarios branch) and the
// ones this grid found. The grid reports them, the Go test logs them
// instead of failing, and each has a skipped reproducer in stress_test.go.
// Remove an entry when its fix lands.
var knownBugs = map[string]string{
	"blocked-rank": "rank() in decide.go sorts jobs with R>0 by (running+incoming)/R; a job whose units all wait upstream has ratio 0 and sorts first " +
		"(found by hud-scenarios; the fix is on the scenarios branch)",
	"lateness-unpriced": "planFluid in plan.go: a job Moore-Hodgson evicts from the on-time set takes its lateness from its own EDF position, which can end " +
		"before its deadline, so lateBy is 0, the lateness it would cost is $0, and auto-burst never fires for it (found by hud-scenarios; the fix is on the scenarios branch)",
	"eta-inconsistent": "the same cause as lateness-unpriced: an evicted job's ETA90 is its own EDF position, before its deadline, while AtRisk is true",
	"churn-pingpong": "take() in preempt.go checks aboveShare (running - outgoing > share) and then removes a whole unit, so a victim holding ceil(share) slots " +
		"drops below its fractional share; the deadline tier rounds R up (needSlots), so a job released to ceil(share) is at once a victim, and two jobs whose " +
		"shares are each under a slot take one slot back and forth on every decision, losing the work each time. Suggested fix: take a unit only if " +
		"running - outgoing - its slots >= ceil(share - 1e-9) (found by this grid)",
	"starvation-over-deadline": "starvation() in decide.go runs before the deadline tier and releases a unit to any job idle for StarvationAfter, whatever its " +
		"slack; on a small farm that unit takes a slot a job due within the hour needs, and once it has run MaxLostWork no preemption may take it back, " +
		"so a long unit holds the slot for hours. Suggested fix: let the guard take only slots the deadline tier does not need (run it after need() for " +
		"jobs with slack above PreemptSlack), or make guard-released units preemptible regardless of MaxLostWork (found by this grid)",
	"past-due-on-track": "groups() in plan.go plans only jobs with at least one pool in the snapshot; a job none of whose pools exists gets no plan, so AtRisk " +
		"stays false even past its deadline, LateBy is 0 and no burst is sized for it. Suggested fix: mark such deadline jobs at risk (planOK false, lateBy " +
		"the time to the deadline's end of remaining work at zero capacity, or past it) (found by this grid)",
}

type violation struct {
	Inv    string
	At     float64 // seconds from the start
	Job    string
	Detail string
}

func (v violation) String() string {
	return fmt.Sprintf("%s at %s: %s %s", v.Inv, clock(v.At), v.Job, v.Detail)
}

func clock(t float64) string {
	d := int(t / day)
	h := math.Mod(t, day) / hour
	return fmt.Sprintf("day %d %02d:%02d", d, int(h), int(math.Mod(t, hour)/minute))
}

type predRec struct {
	early, mid        int8 // 0 unset, 1 on track, 2 at risk
	everAtRisk        bool
	submit, due, mid0 float64
}

type checker struct {
	e      *engine
	cfg    sched.Config
	byUnit map[string]int
	poolTS map[string][]float64
	pairs  map[[2]string]float64
	pred   map[string]*predRec
	// incoming: units being stopped for a job (sched counts their slots
	// toward it), by unit.
	incoming map[string]string
	asked    map[string]bool // units sched asked to stop, while they run
	cur      *view
	dumped   bool
}

func newChecker(e *engine) *checker {
	return &checker{e: e, cfg: e.cfg, byUnit: map[string]int{}, poolTS: map[string][]float64{},
		pairs: map[[2]string]float64{}, pred: map[string]*predRec{}, incoming: map[string]string{}, asked: map[string]bool{}}
}

// view is the bookkeeping of one decision: what was free and used before
// and after it.
type view struct {
	s          sched.Snapshot
	d          sched.Decisions
	pools      map[string]*sched.Pool
	projects   map[string]*sched.Project
	hasQuota   map[string]bool
	freeBefore map[string]int
	freeAfter  map[string]int
	useBefore  map[string]map[string]int // project, pool
	useAfter   map[string]map[string]int
	useStarve  map[string]map[string]int // before plus the starvation tier's dispatches
	sent       map[string]int            // units dispatched per job
	sentTier   map[string]sched.Tier
	byPoolTier map[string]map[sched.Tier]int
	demand     map[string]map[string]int // after the decision
	demand0    map[string]map[string]int // before it
	gangHeld   map[string]bool
}

func (c *checker) newView(s sched.Snapshot, d sched.Decisions) *view {
	v := &view{s: s, d: d, pools: map[string]*sched.Pool{}, projects: map[string]*sched.Project{}, hasQuota: map[string]bool{},
		freeBefore: map[string]int{}, freeAfter: map[string]int{}, useBefore: map[string]map[string]int{},
		useAfter: map[string]map[string]int{}, useStarve: map[string]map[string]int{}, sent: map[string]int{}, sentTier: map[string]sched.Tier{},
		byPoolTier: map[string]map[sched.Tier]int{}, demand: map[string]map[string]int{}, gangHeld: map[string]bool{}}
	for i := range s.Pools {
		p := &s.Pools[i]
		v.pools[p.Name] = p
		v.freeBefore[p.Name] = max(0, p.Slots-p.Used)
		v.freeAfter[p.Name] = v.freeBefore[p.Name]
		v.byPoolTier[p.Name] = map[sched.Tier]int{}
	}
	for i := range s.Projects {
		pr := &s.Projects[i]
		v.projects[pr.Name] = pr
		v.useBefore[pr.Name] = map[string]int{}
		v.useAfter[pr.Name] = map[string]int{}
		v.useStarve[pr.Name] = map[string]int{}
		v.demand[pr.Name] = map[string]int{}
		for pool, q := range pr.Quotas {
			if v.pools[pool] != nil && (q.Nominal > 0 || q.BorrowingLimit >= 0) {
				v.hasQuota[pool] = true
			}
		}
	}
	jobs := map[string]*sched.Job{}
	for i := range s.Jobs {
		j := &s.Jobs[i]
		jobs[j.ID] = j
		for pool, n := range j.Running {
			v.useBefore[j.Project][pool] += n
			v.useAfter[j.Project][pool] += n
			v.useStarve[j.Project][pool] += n
		}
	}
	// Deadline demand as sched computes it before planning: R = W90 over
	// the time left (less the critical path), not the planned R.
	for i := range s.Jobs {
		j := &s.Jobs[i]
		r := c.perJobR(s, j)
		if j.Hold || j.HoldStop || r <= 0 {
			continue
		}
		running := 0
		for _, n := range j.Running {
			running += n
		}
		need := min(int(math.Ceil(r-1e-9))-running, j.RunnableTotal*max(1, j.SlotsPerUnit))
		if need <= 0 {
			continue
		}
		for _, pool := range j.Pools {
			if p := v.pools[pool]; p != nil {
				v.demand[j.Project][pool] += min(need, p.Slots)
			}
		}
	}
	v.demand0 = map[string]map[string]int{}
	for name, m := range v.demand {
		v.demand0[name] = map[string]int{}
		for k, n := range m {
			v.demand0[name][k] = n
		}
	}
	for _, x := range d.Dispatch {
		j := jobs[x.Job]
		if j == nil || v.pools[x.Pool] == nil {
			continue
		}
		v.freeAfter[x.Pool] -= x.Slots
		v.useAfter[j.Project][x.Pool] += x.Slots
		if x.Tier == sched.TierStarvation {
			v.useStarve[j.Project][x.Pool] += x.Slots
		}
		v.sent[x.Job]++
		if _, ok := v.sentTier[x.Job]; !ok {
			v.sentTier[x.Job] = x.Tier
		}
		v.byPoolTier[x.Pool][x.Tier] += x.Slots
		for _, pool := range j.Pools {
			if v.demand[j.Project][pool] > 0 {
				v.demand[j.Project][pool] = max(0, v.demand[j.Project][pool]-x.Slots)
			}
		}
	}
	return v
}

func (c *checker) perJobR(s sched.Snapshot, j *sched.Job) float64 {
	switch {
	case !j.Deadline.IsZero():
		w90 := math.Max(j.Estimate.W90, j.Estimate.W)
		t := j.Deadline.Sub(s.Now).Seconds()
		if c.cfg.TailAware {
			t -= j.Estimate.TPath
		}
		return w90 / math.Max(t, c.cfg.MinRemaining.Seconds())
	case j.RequiredRate > 0:
		return j.RequiredRate
	}
	return 0
}

func quotaOf(pr *sched.Project, pool string) sched.Quota {
	if pr != nil {
		if q, ok := pr.Quotas[pool]; ok {
			return q
		}
	}
	return sched.Quota{BorrowingLimit: -1}
}

// admits says whether k more slots for project in pool pass Kueue's
// quota rules and sched's reserve after the decision, and if not, why.
func (v *view) admits(project, pool string, k int, use map[string]map[string]int, free int) (bool, string) {
	return v.admitsWith(project, pool, k, use, v.demand, free)
}

func (v *view) admitsWith(project, pool string, k int, use, demand map[string]map[string]int, free int) (bool, string) {
	if !v.hasQuota[pool] {
		return true, ""
	}
	q := quotaOf(v.projects[project], pool)
	u := use[project][pool]
	if u+k <= q.Nominal {
		return true, ""
	}
	if q.BorrowingLimit >= 0 && u+k-q.Nominal > q.BorrowingLimit {
		return false, "borrowing limit"
	}
	reserve := 0
	for name, pr := range v.projects {
		if name == project {
			continue
		}
		oq := quotaOf(pr, pool)
		if unmet := min(oq.Nominal-use[name][pool], demand[name][pool]); unmet > 0 {
			reserve += unmet
		}
	}
	if free-k < reserve {
		return false, "held for another project's guaranteed need"
	}
	return true, ""
}

func since(j *sched.Job) time.Time {
	switch {
	case !j.LastRunning.IsZero():
		return j.LastRunning
	case !j.Ready.IsZero():
		return j.Ready
	}
	return j.Submitted
}

func unitSlots(j *sched.Job, u sched.UnitRef) int {
	if u.Slots > 0 {
		return u.Slots
	}
	return max(1, j.SlotsPerUnit)
}

func running(j *sched.Job) int {
	n := 0
	for _, x := range j.Running {
		n += x
	}
	return n
}

func (c *checker) decision(s sched.Snapshot, d sched.Decisions) {
	e := c.e
	v := c.newView(s, d)
	c.cur = v
	for id := range c.asked {
		if u := e.byID[id]; u == nil || u.state != stRunning {
			delete(c.asked, id)
		}
	}
	inc := map[string]bool{}
	for id, forJob := range c.incoming {
		if u := e.byID[id]; u == nil || u.state != stRunning {
			delete(c.incoming, id)
			continue
		}
		inc[forJob] = true
	}
	starving := map[string]bool{}
	for i := range s.Jobs {
		j := &s.Jobs[i]
		if j.Hold || j.HoldStop || len(j.Runnable) == 0 || running(j) > 0 || len(j.RunningUnits) > 0 || inc[j.ID] {
			continue
		}
		if w := s.Now.Sub(since(j)); w >= c.cfg.StarvationAfter {
			starving[j.ID] = true
			if w > e.res.MaxStarve {
				e.res.MaxStarve = w
				e.res.MaxStarveJob = j.ID
			}
		}
	}
	// A starving gang that fits nowhere holds the largest pool it fits in.
	for i := range s.Jobs {
		j := &s.Jobs[i]
		if !starving[j.ID] || v.sent[j.ID] > 0 {
			continue
		}
		k := unitSlots(j, j.Runnable[0])
		if k <= 1 {
			continue
		}
		var target *sched.Pool
		for _, name := range j.Pools {
			p := v.pools[name]
			if p != nil && p.Slots >= k && (target == nil || p.Slots > target.Slots) {
				target = p
			}
		}
		if target != nil {
			v.gangHeld[target.Name] = true
		}
	}
	c.checkStarvation(v, starving)
	c.checkGuard(v)
	c.checkConservation(v)
	c.checkRank(v)
	c.checkHolds(v)
	c.checkQuotas(v)
	c.checkPlan(v)
	c.checkBursts(v)
	c.recordPredictions(v)
	_ = e
}

func (c *checker) violate(inv, job, detail string) {
	c.e.violate(inv, job, detail)
	if os.Getenv("STRESSDUMP") == inv && c.cur != nil && !c.dumped {
		c.dumped = true
		c.dump(c.cur, job, detail)
	}
}

// dump prints the decision a violation was found in (STRESSDUMP=inv).
func (c *checker) dump(v *view, job, detail string) {
	w := os.Stderr
	fmt.Fprintf(w, "== %s %s: %s\n", clock(c.e.now), job, detail)
	for _, p := range v.s.Pools {
		fmt.Fprintf(w, "pool %-10s slots %3d used %3d free after %3d quota=%v burst=%+v\n", p.Name, p.Slots, p.Used, v.freeAfter[p.Name], v.hasQuota[p.Name], p.Burst)
	}
	for _, pr := range v.s.Projects {
		fmt.Fprintf(w, "project %-4s quotas %v use %v after %v demand %v budget %.0f\n", pr.Name, pr.Quotas, v.useBefore[pr.Name], v.useAfter[pr.Name], v.demand[pr.Name], pr.BurstBudgetLeft)
	}
	for _, j := range v.s.Jobs {
		st := v.d.Jobs[j.ID]
		fmt.Fprintf(w, "job %s %-4s pools %v run %v runnable %d/%d blocked %d hold %v/%v due %s since %s sent %d | R %.2f share %.2f risk %v late %s rank %d W90 %.0f tpath %.0f | %s\n",
			j.ID, j.Project, j.Pools, j.Running, len(j.Runnable), j.RunnableTotal, j.Blocked, j.Hold, j.HoldStop, j.Deadline.Format("Mon 15:04"),
			v.s.Now.Sub(since(&j)).Round(time.Minute), v.sent[j.ID], st.Required, st.Share, st.AtRisk, st.LateBy.Round(time.Minute), st.Rank, j.Estimate.W90, j.Estimate.TPath, st.Message)
	}
	for _, x := range v.d.Dispatch {
		fmt.Fprintf(w, "dispatch %s %s %s %d\n", x.Job, x.Unit, x.Pool, x.Slots)
	}
	for _, x := range v.d.Preempt {
		fmt.Fprintf(w, "preempt %s %s %s for %s %s yield %v\n", x.Job, x.Unit, x.Pool, x.For, x.Reason, x.Yield)
	}
	for _, x := range v.d.Bursts {
		fmt.Fprintf(w, "burst %+v\n", x)
	}
}

func (c *checker) checkStarvation(v *view, starving map[string]bool) {
	for i := range v.s.Jobs {
		j := &v.s.Jobs[i]
		if !starving[j.ID] || v.sent[j.ID] > 0 {
			continue
		}
		k := unitSlots(j, j.Runnable[0])
		for _, pool := range j.Pools {
			p := v.pools[pool]
			if p == nil || v.gangHeld[pool] || v.freeBefore[pool] < k {
				continue
			}
			// Units the starvation tier released this decision count
			// against the job's quota and the pool's room.
			room := v.freeBefore[pool] - v.byPoolTier[pool][sched.TierStarvation]
			if room < k {
				continue
			}
			// The starvation tier runs first, against the deadline
			// demand held for projects within their nominal quota.
			ok, _ := v.admitsWith(j.Project, pool, k, v.useStarve, v.demand0, room)
			if !ok {
				continue
			}
			taken := v.freeBefore[pool] - v.freeAfter[pool] - v.byPoolTier[pool][sched.TierStarvation]
			if v.freeAfter[pool] >= k || taken > 0 {
				c.violate("starvation", j.ID, fmt.Sprintf("waited %s with ready work; %s had %d free slots, %d went to non-starving jobs",
					v.s.Now.Sub(since(j)).Round(time.Minute), pool, v.freeBefore[pool], taken))
				return
			}
		}
	}
}

// checkGuard flags a starvation-tier unit given to a job with plenty of
// slack when that left a pool full while a job on time in the plan was
// short of its required slots there.
func (c *checker) checkGuard(v *view) {
	for _, x := range v.d.Dispatch {
		if x.Tier != sched.TierStarvation || v.freeAfter[x.Pool] > 0 {
			continue
		}
		if st := v.d.Jobs[x.Job]; st.Slack <= c.cfg.PreemptSlack {
			continue
		}
		for i := range v.s.Jobs {
			j := &v.s.Jobs[i]
			st := v.d.Jobs[j.ID]
			if j.Deadline.IsZero() || st.AtRisk || j.Hold || j.HoldStop || len(j.Runnable) <= v.sent[j.ID] || !allowed(j.Pools, x.Pool) {
				continue
			}
			if have := running(j) + v.sent[j.ID]; float64(have) < st.Required-1e-9 {
				c.violate("starvation-over-deadline", x.Job, fmt.Sprintf("guard gave the last slot of %s to %s (slack %s) while %s, due %s, holds %d of the %.1f slots it needs",
					x.Pool, x.Job, v.d.Jobs[x.Job].Slack.Round(time.Hour), j.ID, j.Deadline.Format("Mon 15:04"), have, st.Required))
				return
			}
		}
	}
}

func (c *checker) checkConservation(v *view) {
	for i := range v.s.Jobs {
		j := &v.s.Jobs[i]
		if j.Hold || j.HoldStop {
			continue
		}
		next := v.sent[j.ID]
		if next >= len(j.Runnable) {
			continue
		}
		k := unitSlots(j, j.Runnable[next])
		if j.MaxParallel > 0 && running(j)+k > j.MaxParallel {
			continue
		}
		for _, pool := range j.Pools {
			if v.pools[pool] == nil || v.gangHeld[pool] || v.freeAfter[pool] < k {
				continue
			}
			ok, _ := v.admits(j.Project, pool, k, v.useAfter, v.freeAfter[pool])
			if !ok {
				continue
			}
			c.violate("work-conservation", j.ID, fmt.Sprintf("%d slots of %s idle while %s waits with %d runnable units (next needs %d)",
				v.freeAfter[pool], pool, j.ID, j.RunnableTotal-next, k))
			return
		}
	}
}

func (c *checker) checkRank(v *view) {
	bestBlocked, worstReady := math.MaxInt, 0
	var bj, rj string
	for i := range v.s.Jobs {
		j := &v.s.Jobs[i]
		st, ok := v.d.Jobs[j.ID]
		if !ok || st.Rank == 0 {
			continue
		}
		switch {
		case j.RunnableTotal == 0 && j.Blocked > 0 && running(j) == 0 && len(j.RunningUnits) == 0:
			if st.Rank < bestBlocked {
				bestBlocked, bj = st.Rank, j.ID
			}
		case j.RunnableTotal > 0 && !j.Hold && !j.HoldStop:
			if st.Rank > worstReady {
				worstReady, rj = st.Rank, j.ID
			}
		}
	}
	if bj != "" && rj != "" && bestBlocked < worstReady {
		c.violate("blocked-rank", bj, fmt.Sprintf("blocked, ranked %d, ahead of %s with ready work at rank %d", bestBlocked, rj, worstReady))
	}
}

func (c *checker) checkHolds(v *view) {
	pre := map[string]bool{}
	for _, p := range v.d.Preempt {
		pre[p.Unit] = true
	}
	for i := range v.s.Jobs {
		j := &v.s.Jobs[i]
		if !j.HoldStop {
			continue
		}
		for _, ru := range j.RunningUnits {
			u := c.e.byID[ru.ID]
			if u != nil && u.yieldAt == 0 && !pre[ru.ID] && !c.pendingStop(ru.ID) {
				c.violate("hold", j.ID, fmt.Sprintf("hold-stopped but %s keeps running", ru.ID))
				return
			}
		}
	}
}

// pendingStop says whether sched already asked a unit to stop (it is then
// in State and not asked again).
func (c *checker) pendingStop(id string) bool { return c.asked[id] }

func (c *checker) checkQuotas(v *view) {
	for _, x := range v.d.Dispatch {
		j := c.e.byID[x.Unit]
		if j == nil {
			continue
		}
		proj := j.j.spec.project
		pr := v.projects[proj]
		if !v.hasQuota[x.Pool] {
			continue
		}
		q := quotaOf(pr, x.Pool)
		use := v.useAfter[proj][x.Pool]
		if q.BorrowingLimit >= 0 && use-q.Nominal > q.BorrowingLimit {
			c.violate("borrow-limit", x.Job, fmt.Sprintf("%s uses %d of %s (nominal %d, borrowing limit %d)", proj, use, x.Pool, q.Nominal, q.BorrowingLimit))
			return
		}
		if x.Tier != sched.TierBoost || use <= q.Nominal {
			continue
		}
		// A boost above nominal: did another project with a deadline
		// need inside its guarantee go without in this pool?
		for name, other := range v.projects {
			if name == proj {
				continue
			}
			oq := quotaOf(other, x.Pool)
			if v.useAfter[name][x.Pool] < oq.Nominal && v.demand[name][x.Pool] > 0 && v.freeAfter[x.Pool] == 0 {
				c.violate("boost-quota", x.Job, fmt.Sprintf("boost put %s at %d of %s (nominal %d) while %s, at %d of nominal %d, needs %d",
					proj, use, x.Pool, q.Nominal, name, v.useAfter[name][x.Pool], oq.Nominal, v.demand[name][x.Pool]))
				return
			}
		}
	}
}

func (c *checker) checkPlan(v *view) {
	bad := map[string]bool{}
	for i := range v.s.Jobs {
		j := &v.s.Jobs[i]
		st, ok := v.d.Jobs[j.ID]
		if !ok || j.Deadline.IsZero() || j.Hold || j.HoldStop || math.Max(j.Estimate.W90, j.Estimate.W) <= 0 {
			continue
		}
		pr := v.projects[j.Project]
		if st.AtRisk && st.LateBy <= 0 && !bad["lateness-unpriced"] {
			bad["lateness-unpriced"] = true
			c.violate("lateness-unpriced", j.ID, fmt.Sprintf("at risk with LateBy 0 (lateness priced at $0 against %.0f/h); %s", lateCostOf(pr), st.Message))
		}
		if !st.ETA90.IsZero() && !bad["eta-inconsistent"] {
			switch {
			case st.AtRisk && !st.ETA90.After(j.Deadline):
				bad["eta-inconsistent"] = true
				c.violate("eta-inconsistent", j.ID, fmt.Sprintf("at risk but ETA90 %s is before its deadline %s", st.ETA90.Format("Mon 15:04"), j.Deadline.Format("Mon 15:04")))
			case !st.AtRisk && st.ETA90.Sub(j.Deadline) > time.Second:
				bad["eta-inconsistent"] = true
				c.violate("eta-inconsistent", j.ID, fmt.Sprintf("on track but ETA90 %s is after its deadline %s", st.ETA90.Format("Mon 15:04"), j.Deadline.Format("Mon 15:04")))
			}
		}
		if !j.Deadline.After(v.s.Now) && !st.AtRisk && !bad["past-due-on-track"] {
			bad["past-due-on-track"] = true
			c.violate("past-due-on-track", j.ID, fmt.Sprintf("past its deadline %s and reported on track", j.Deadline.Format("Mon 15:04")))
		}
	}
	for _, b := range v.d.Bursts {
		pr := v.projects[b.Project]
		if !b.Auto && strings.Contains(b.Why, "($0 at") && lateCostOf(pr) > 0 && !bad["lateness-unpriced"] {
			bad["lateness-unpriced"] = true
			c.violate("lateness-unpriced", b.Job, "approval asked: "+b.Why)
		}
	}
}

func lateCostOf(pr *sched.Project) float64 {
	if pr == nil {
		return 0
	}
	return pr.LateCostPerHour
}

func (c *checker) checkBursts(v *view) {
	spent := map[string]float64{}
	auto := map[string]int{}
	for _, b := range v.d.Bursts {
		if !b.Auto {
			continue
		}
		pr := v.projects[b.Project]
		st := v.d.Jobs[b.Job]
		late := lateCostOf(pr) * st.LateBy.Hours()
		switch {
		case !b.Closes:
			c.violate("burst-uneconomic", b.Job, "auto burst that does not close the deadline")
		case b.Cost >= late:
			c.violate("burst-uneconomic", b.Job, fmt.Sprintf("auto burst costs $%.2f, lateness prevented $%.2f", b.Cost, late))
		}
		spent[b.Project] += b.Cost
		for pool, n := range b.Slots {
			auto[pool] += n
		}
	}
	for name, x := range spent {
		if left := v.projects[name].BurstBudgetLeft; x > left+1e-6 {
			c.violate("burst-budget", name, fmt.Sprintf("auto bursts of $%.2f with $%.2f of budget left", x, left))
		}
	}
	if !c.e.pol.burst {
		return
	}
	for _, cr := range v.d.Capacity {
		p := v.pools[cr.Pool]
		if p == nil || p.Burst == nil {
			continue
		}
		if cr.Slots > p.Burst.MaxSlots {
			c.violate("burst-unjustified", cr.Pool, fmt.Sprintf("request %d over the pool's limit %d", cr.Slots, p.Burst.MaxSlots))
		}
		if cr.Slots > p.Burst.Requested+auto[cr.Pool] {
			c.violate("burst-unjustified", cr.Pool, fmt.Sprintf("request grew %d to %d with %d slots approved (%s)", p.Burst.Requested, cr.Slots, auto[cr.Pool], cr.Reason))
		}
	}
}

// requested is called for every preemption sched emits; sched keeps it
// pending and does not ask again while the unit runs. Its rate limit per
// pool counts what it emits, so the window and ping-pong checks do too.
func (c *checker) requested(x sched.Preempt) {
	c.asked[x.Unit] = true
	if x.Reason == sched.ReasonHoldStop || x.Reason == sched.ReasonWrangler {
		return
	}
	now := c.e.now
	if x.For != "" {
		c.incoming[x.Unit] = x.For
	}
	ts := append(c.poolTS[x.Pool], now)
	keep := ts[:0]
	for _, t := range ts {
		if now-t < c.cfg.PreemptWindow.Seconds() {
			keep = append(keep, t)
		}
	}
	c.poolTS[x.Pool] = keep
	if len(keep) > c.cfg.PreemptPerWindow {
		var at []string
		for _, t := range keep {
			at = append(at, clock(t)[6:])
		}
		c.violate("churn-pool", x.Pool, fmt.Sprintf("%d preemptions in %s (limit %d) at %s", len(keep), c.cfg.PreemptWindow, c.cfg.PreemptPerWindow, strings.Join(at, " ")))
	}
	if x.For == "" {
		return
	}
	if t, ok := c.pairs[[2]string{x.For, x.Job}]; ok && now-t < hour {
		c.violate("churn-pingpong", x.Job, fmt.Sprintf("lost a unit to %s, which lost one to it %s earlier", x.For, time.Duration((now-t)*float64(time.Second)).Round(time.Minute)))
	}
	c.pairs[[2]string{x.Job, x.For}] = now
}

// stopped is called when a preemption takes effect (a kill, or a yield
// at a checkpoint): a unit faulted before its yield was not preempted.
func (c *checker) stopped(x sched.Preempt) {
	if x.Reason == sched.ReasonHoldStop || x.Reason == sched.ReasonWrangler {
		return
	}
	c.byUnit[x.Unit]++
	if n := c.byUnit[x.Unit]; n > c.cfg.MaxPreemptionsPerUnit {
		c.violate("churn-unit", x.Job, fmt.Sprintf("%s preempted %d times (limit %d)", x.Unit, n, c.cfg.MaxPreemptionsPerUnit))
	}
}

// recordPredictions keeps each deadline job's first prediction after it
// is ready and its prediction halfway to its deadline.
func (c *checker) recordPredictions(v *view) {
	now := c.e.now
	for i := range v.s.Jobs {
		j := &v.s.Jobs[i]
		if j.Deadline.IsZero() || j.Hold || j.HoldStop {
			continue
		}
		st, ok := v.d.Jobs[j.ID]
		if !ok || (j.RunnableTotal == 0 && running(j) == 0) {
			continue
		}
		u := c.e.byID[j.ID+"/0"]
		if u == nil {
			continue
		}
		p := c.pred[j.ID]
		if p == nil {
			p = &predRec{submit: u.j.spec.submit}
			c.pred[j.ID] = p
		}
		val := int8(1)
		if st.AtRisk {
			val = 2
			p.everAtRisk = true
		}
		if p.early == 0 {
			p.early = val
			p.mid0 = now + (u.j.due-now)/2
		}
		if p.mid == 0 && now >= p.mid0 {
			p.mid = val
		}
	}
}

// finish scores predictions against outcomes.
func (c *checker) finish(end float64) {
	r := c.e.res
	for _, j := range c.e.jobs {
		p := c.pred[j.spec.id]
		if p == nil || !j.spec.hasDue {
			continue
		}
		late := j.finish < 0 || j.finish > j.due+1
		r.Pred.add(p.early, late, 0)
		r.Pred.add(p.mid, late, 1)
	}
}

// predStats is a confusion matrix of AtRisk against lateness, for the
// first prediction and the halfway one.
type predStats struct {
	TP, FP, FN, TN [2]int
}

func (p *predStats) add(v int8, late bool, k int) {
	switch {
	case v == 0:
	case v == 2 && late:
		p.TP[k]++
	case v == 2:
		p.FP[k]++
	case late:
		p.FN[k]++
	default:
		p.TN[k]++
	}
}

func (p *predStats) merge(o predStats) {
	for k := range 2 {
		p.TP[k] += o.TP[k]
		p.FP[k] += o.FP[k]
		p.FN[k] += o.FN[k]
		p.TN[k] += o.TN[k]
	}
}

func (p predStats) precision(k int) float64 { return ratio(p.TP[k], p.TP[k]+p.FP[k]) }
func (p predStats) recall(k int) float64    { return ratio(p.TP[k], p.TP[k]+p.FN[k]) }

func ratio(a, b int) float64 {
	if b == 0 {
		return math.NaN()
	}
	return float64(a) / float64(b)
}

// runResult is what one run of one policy on one point measured.
type runResult struct {
	Policy, Point string
	Seed          uint64

	Jobs, DueJobs, OnTime, Late, Unfinished int
	LateHours, LateDollars                  float64
	BurstCost                               float64
	Util                                    float64
	Dispatches, Preemptions, FaultKills     int
	Reclaims, Nudges                        int
	AutoBursts, Approvals                   int
	UsefulSlotHours, LostSlotHours          float64
	Decides                                 int
	DecideMean, DecideMax                   time.Duration
	MaxStarve                               time.Duration // longest wait of a ready job with nothing running
	MaxStarveJob                            string
	Hash                                    uint64
	End                                     float64
	Pred                                    predStats

	Why      map[string]int // reasons bursts that would close a deadline needed approval
	Counts   map[string]int
	Examples []violation
}

func (r *runResult) add(v violation) {
	if r.Counts == nil {
		r.Counts = map[string]int{}
	}
	r.Counts[v.Inv]++
	if r.Counts[v.Inv] <= 2 {
		r.Examples = append(r.Examples, v)
	}
}

func (r *runResult) onTimePct() float64 {
	if r.DueJobs == 0 {
		return math.NaN()
	}
	return 100 * float64(r.OnTime) / float64(r.DueJobs)
}

func (r *runResult) violations() int {
	n := 0
	for _, c := range r.Counts {
		n += c
	}
	return n
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
