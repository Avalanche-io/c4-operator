package telemetry

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Redactor turns the policy's input into a Snapshot record: every name a
// pseudonym under the farm key, every time relative to the snapshot's now,
// money multiplied by a secret factor that is not written anywhere, and
// source bytes cut to two significant figures. What the policy does not
// read (pool attributes, source views, messages) is left out.
type Redactor struct {
	Key Key
	// MoneyFactor scales every amount of money. NewRedactor draws it at
	// random for each bundle; it is never in the bundle.
	MoneyFactor float64
	// Coarsen also cuts unit durations and work to two significant
	// figures. Off by default: exact counts and durations are what replay
	// needs at the policy's thresholds (DESIGN.md section 6.9).
	Coarsen bool
}

// NewRedactor is a redactor with a fresh secret money factor in [0.5, 2).
func NewRedactor(key Key) *Redactor {
	var b [8]byte
	_, _ = rand.Read(b[:])
	f := 0.5 + 1.5*float64(binary.BigEndian.Uint64(b[:])>>11)/float64(1<<53)
	return &Redactor{Key: key, MoneyFactor: f}
}

// Names is the namer a snapshot was redacted with, for the faithfulness
// check to map the farm's own decisions.
type Names struct{ n *Namer }

// Redact writes the snapshot, the state before the decision, the
// configuration and the farm's decisions as one record.
func (r *Redactor) Redact(s sched.Snapshot, st *sched.State, cfg sched.Config, d sched.Decisions) (Snapshot, Names) {
	n := NewNamer(r.Key)
	doc := st.Doc()
	noteSnapshot(n, s, doc, d)
	n.Freeze()
	epoch := s.Now
	rel := func(t time.Time) RelTime { return RelTimeOf(t, epoch) }
	money := func(x float64) Money { return Money(x * r.MoneyFactor) }
	out := Snapshot{Build: CurrentBuild(), KeyEpoch: r.Key.Epoch}

	for _, p := range s.Pools {
		rp := Pool{Name: n.Of(KindPool, p.Name), Slots: Int(p.Slots), Used: Int(p.Used), Cost: money(p.CostPerSlotHour)}
		if b := p.Burst; b != nil {
			rp.Burst = &Burst{MaxSlots: Int(b.MaxSlots), Provisioning: DurationOf(b.Provisioning), Cost: money(b.CostPerSlotHour),
				Requested: Int(b.Requested), Provisioned: Int(b.Provisioned)}
		}
		out.Pools = append(out.Pools, rp)
	}
	for _, p := range s.Projects {
		rp := Project{Name: n.Of(KindProject, p.Name), Weight: Real(p.Weight), BurstBudgetLeft: money(p.BurstBudgetLeft), LateCostPerHour: money(p.LateCostPerHour)}
		for _, pool := range sortedKeys(p.Quotas) {
			q := p.Quotas[pool]
			rp.Quotas = append(rp.Quotas, Quota{Pool: n.Of(KindPool, pool), Nominal: Int(q.Nominal), BorrowingLimit: Int(q.BorrowingLimit)})
		}
		out.Projects = append(out.Projects, rp)
	}
	for _, j := range s.Jobs {
		rj := Job{ID: n.Of(KindJob, j.ID), Project: n.Of(KindProject, j.Project), Class: n.Of(KindClass, j.Class),
			Submitted: rel(j.Submitted), Ready: rel(j.Ready), Deadline: rel(j.Deadline), NotBefore: rel(j.NotBefore), LastRunning: rel(j.LastRunning),
			RequiredRate: Real(j.RequiredRate), MaxParallel: Int(j.MaxParallel), SlotsPerUnit: Int(j.SlotsPerUnit),
			RunnableTotal: Int(j.RunnableTotal), Blocked: Int(j.Blocked), Unevaluated: Int(j.Unevaluated),
			Estimate: r.estimate(j.Estimate), Hold: j.Hold, HoldStop: j.HoldStop, Boost: Real(j.Boost),
			Preemptible: j.Preemptible, Checkpoints: j.Checkpoints, Priority: Int(j.Priority), UnitsDone: Int(j.UnitsDone), DoneSeconds: Real(j.DoneSeconds)}
		for _, a := range j.After {
			rj.After = append(rj.After, n.Of(KindJob, a))
		}
		for _, p := range j.Pools {
			rj.Pools = append(rj.Pools, n.Of(KindPool, p))
		}
		for _, pool := range sortedKeys(j.Running) {
			rj.Running = append(rj.Running, PoolSlots{Pool: n.Of(KindPool, pool), Slots: Int(j.Running[pool])})
		}
		for _, u := range j.Runnable {
			rj.Runnable = append(rj.Runnable, RunnableUnit{Unit: unitOf(n, u.ID), Info: u.Info, Slots: Int(u.Slots)})
		}
		for _, u := range j.RunningUnits {
			rj.RunningUnits = append(rj.RunningUnits, RunningUnit{Unit: unitOf(n, u.ID), Pool: n.Of(KindPool, u.Pool), Slots: Int(u.Slots),
				Started: rel(u.Started), LastCheckpoint: rel(u.LastCheckpoint), RestoreCost: r.dur(u.RestoreCost),
				Preemptions: Int(u.Preemptions), Remaining: r.dur(u.Remaining)})
		}
		for _, pool := range sortedKeys(j.Sources) {
			ps := PoolSources{Pool: n.Of(KindPool, pool)}
			for _, src := range j.Sources[pool] {
				ps.Sources = append(ps.Sources, Source{Via: ViaOf(src.Via), Channel: ChannelOf(src.Channel), Link: n.Of(KindLink, src.Link),
					Bytes: Real(twoFigures(src.Bytes)), Latency: DurationOf(src.Latency), Cost: money(src.Cost), Work: r.real(src.Work), Longest: r.dur(src.Longest)})
			}
			rj.Sources = append(rj.Sources, ps)
		}
		out.Jobs = append(out.Jobs, rj)
	}
	for _, l := range s.Links {
		out.Links = append(out.Links, Link{Name: n.Of(KindLink, l.Name), BytesPerSecond: Real(l.BytesPerSecond), Committed: Real(l.Committed)})
	}

	out.State = redactState(n, doc, rel)
	out.Config = configOf(cfg)
	out.Decisions = redactDecisions(n, d, rel, money)
	return out, Names{n}
}

func (r *Redactor) estimate(e estimate.Summary) Estimate {
	return Estimate{W: r.real(e.W), Sigma: r.real(e.Sigma), W90: r.real(e.W90), W50: r.real(e.W50), W80: r.real(e.W80), W95: r.real(e.W95),
		TPath: r.real(e.TPath), Queued: Int(e.Queued), Running: Int(e.Running), Unevaluated: Int(e.Unevaluated),
		Sampled: Int(e.Stratum.Sampled), Measured: Int(e.Stratum.Measured), Open: Int(e.Stratum.Open), Calibrated: e.Calibrated}
}

func (r *Redactor) real(x float64) Real {
	if r.Coarsen {
		return Real(twoFigures(x))
	}
	return Real(x)
}

func (r *Redactor) dur(d time.Duration) Nanos {
	if r.Coarsen {
		return Nanos(twoFigures(float64(d)))
	}
	return DurationOf(d)
}

// twoFigures keeps x's two most significant decimal figures.
func twoFigures(x float64) float64 {
	if x == 0 || math.IsInf(x, 0) || math.IsNaN(x) {
		return x
	}
	e := math.Floor(math.Log10(math.Abs(x))) - 1
	p := math.Pow(10, e)
	return math.Round(x/p) * p
}

// noteSnapshot adds every name the snapshot, state and decisions use.
func noteSnapshot(n *Namer, s sched.Snapshot, doc sched.StateDoc, d sched.Decisions) {
	unit := func(id string) {
		if job, _, ok := splitUnit(id); ok {
			n.Add(KindJob, job)
			return
		}
		n.Add(KindUnit, id)
	}
	for _, p := range s.Pools {
		n.Add(KindPool, p.Name)
	}
	for _, p := range s.Projects {
		n.Add(KindProject, p.Name)
		for pool := range p.Quotas {
			n.Add(KindPool, pool)
		}
	}
	for _, j := range s.Jobs {
		n.Add(KindJob, j.ID)
		n.Add(KindProject, j.Project)
		n.Add(KindClass, j.Class)
		for _, a := range j.After {
			n.Add(KindJob, a)
		}
		for _, p := range j.Pools {
			n.Add(KindPool, p)
		}
		for p := range j.Running {
			n.Add(KindPool, p)
		}
		for _, u := range j.Runnable {
			unit(u.ID)
		}
		for _, u := range j.RunningUnits {
			unit(u.ID)
			n.Add(KindPool, u.Pool)
		}
		for p, srcs := range j.Sources {
			n.Add(KindPool, p)
			for _, src := range srcs {
				n.Add(KindLink, src.Link)
			}
		}
	}
	for _, l := range s.Links {
		n.Add(KindLink, l.Name)
	}
	for _, p := range doc.Pending {
		unit(p.Unit)
		n.Add(KindJob, p.Job)
		n.Add(KindJob, p.For)
		n.Add(KindPool, p.Pool)
	}
	for _, l := range doc.PreemptLog {
		n.Add(KindPool, l.Pool)
	}
	for _, x := range doc.Surplus {
		n.Add(KindPool, x.Name)
	}
	for _, x := range doc.Guard {
		unit(x.Name)
	}
	for _, x := range doc.GuardHit {
		n.Add(KindJob, x.Name)
	}
	for _, j := range doc.Manual.Bumped {
		n.Add(KindJob, j)
	}
	for _, t := range doc.Took {
		n.Add(KindJob, t.Victim)
		n.Add(KindJob, t.For)
	}
	for _, x := range d.Dispatch {
		n.Add(KindJob, x.Job)
		unit(x.Unit)
		n.Add(KindPool, x.Pool)
	}
	for _, x := range d.Preempt {
		n.Add(KindJob, x.Job)
		unit(x.Unit)
		n.Add(KindPool, x.Pool)
		n.Add(KindJob, x.For)
	}
	for _, c := range d.Capacity {
		n.Add(KindPool, c.Pool)
	}
	for _, b := range d.Bursts {
		n.Add(KindJob, b.Job)
		n.Add(KindProject, b.Project)
		for p := range b.Slots {
			n.Add(KindPool, p)
		}
	}
	for j := range d.Jobs {
		n.Add(KindJob, j)
	}
}

// splitUnit reads a unit ID the queue writes, <job>#<index>.
func splitUnit(id string) (string, int, bool) {
	i := strings.LastIndexByte(id, '#')
	if i <= 0 {
		return "", 0, false
	}
	idx, err := strconv.Atoi(id[i+1:])
	if err != nil || idx < 0 || strconv.Itoa(idx) != id[i+1:] {
		return "", 0, false
	}
	return id[:i], idx, true
}

func unitOf(n *Namer, id string) UnitID {
	if job, idx, ok := splitUnit(id); ok {
		return UnitID{Job: n.Of(KindJob, job), Index: Int(idx)}
	}
	return UnitID{Whole: n.Of(KindUnit, id), Index: -1}
}

func redactState(n *Namer, doc sched.StateDoc, rel func(time.Time) RelTime) State {
	var st State
	for _, p := range doc.Pending {
		st.Pending = append(st.Pending, Pending{Unit: unitOf(n, p.Unit), Job: n.Of(KindJob, p.Job), For: n.Of(KindJob, p.For),
			Pool: n.Of(KindPool, p.Pool), Slots: Int(p.Slots), At: rel(p.At), Reason: ReasonOf(string(p.Reason))})
	}
	for _, l := range doc.PreemptLog {
		pt := PoolTimes{Pool: n.Of(KindPool, l.Pool)}
		for _, t := range l.Times {
			pt.Times = append(pt.Times, rel(t))
		}
		st.PreemptLog = append(st.PreemptLog, pt)
	}
	for _, x := range doc.Surplus {
		st.Surplus = append(st.Surplus, PoolTime{Pool: n.Of(KindPool, x.Name), At: rel(x.At)})
	}
	st.ManualCheck = rel(doc.Manual.LastCheck)
	for _, j := range doc.Manual.Bumped {
		st.ManualBumped = append(st.ManualBumped, n.Of(KindJob, j))
	}
	for _, x := range doc.Guard {
		st.Guard = append(st.Guard, UnitTime{Unit: unitOf(n, x.Name), At: rel(x.At)})
	}
	for _, x := range doc.GuardHit {
		st.GuardHit = append(st.GuardHit, JobTime{Job: n.Of(KindJob, x.Name), At: rel(x.At)})
	}
	for _, t := range doc.Took {
		st.Took = append(st.Took, Took{Victim: n.Of(KindJob, t.Victim), For: n.Of(KindJob, t.For), At: rel(t.At)})
	}
	return st
}

func configOf(c sched.Config) Config {
	return Config{StarvationAfter: DurationOf(c.StarvationAfter), StarvationDeferMax: DurationOf(c.StarvationDeferMax), TieBand: Real(c.TieBand),
		InfoUnitsPerJob: Int(c.InfoUnitsPerJob), PreemptSlack: DurationOf(c.PreemptSlack), PreemptWindow: DurationOf(c.PreemptWindow),
		PreemptPerWindow: Int(c.PreemptPerWindow), MaxPreemptionsPerUnit: Int(c.MaxPreemptionsPerUnit), MaxLostWork: DurationOf(c.MaxLostWork),
		ScaleDownAfter: DurationOf(c.ScaleDownAfter), Burst: c.Burst, NoStage: c.NoStage, MinRemaining: DurationOf(c.MinRemaining),
		TailAware: c.TailAware, Planner: PlannerOf(string(c.Planner)), NoInfoTier: c.NoInfoTier, Surplus: SurplusOf(string(c.Surplus)),
		SlackBand: DurationOf(c.SlackBand), RateSlack: DurationOf(c.RateSlack), WranglerEvery: DurationOf(c.WranglerEvery),
		WranglerMargin: DurationOf(c.WranglerMargin), WranglerUnbump: DurationOf(c.WranglerUnbump), WranglerStartAlarm: DurationOf(c.WranglerStartAlarm)}
}

func redactDecisions(n *Namer, d sched.Decisions, rel func(time.Time) RelTime, money func(float64) Money) Decisions {
	var out Decisions
	for _, x := range d.Dispatch {
		out.Dispatch = append(out.Dispatch, Dispatch{Job: n.Of(KindJob, x.Job), Unit: unitOf(n, x.Unit), Pool: n.Of(KindPool, x.Pool), Slots: Int(x.Slots), Tier: TierOf(string(x.Tier))})
	}
	for _, x := range d.Preempt {
		out.Preempt = append(out.Preempt, Preempt{Job: n.Of(KindJob, x.Job), Unit: unitOf(n, x.Unit), Pool: n.Of(KindPool, x.Pool), Slots: Int(x.Slots),
			Reason: ReasonOf(string(x.Reason)), For: n.Of(KindJob, x.For), Yield: x.Yield, LostWork: DurationOf(x.LostWork)})
	}
	for _, c := range d.Capacity {
		out.Capacity = append(out.Capacity, PoolSlots{Pool: n.Of(KindPool, c.Pool), Slots: Int(c.Slots)})
	}
	for _, b := range d.Bursts {
		rb := BurstDecision{Job: n.Of(KindJob, b.Job), Project: n.Of(KindProject, b.Project), Cost: money(b.Cost), LateCost: money(b.LateCost),
			Closes: b.Closes, Auto: b.Auto, Margin: b.Margin, Floor: b.Floor}
		for _, p := range sortedKeys(b.Slots) {
			rb.Slots = append(rb.Slots, PoolSlots{Pool: n.Of(KindPool, p), Slots: Int(b.Slots[p])})
		}
		for _, p := range b.Order {
			rb.Order = append(rb.Order, n.Of(KindPool, p))
		}
		out.Bursts = append(out.Bursts, rb)
	}
	for _, id := range sortedKeys(d.Jobs) {
		js := d.Jobs[id]
		out.Jobs = append(out.Jobs, JobState{Job: n.Of(KindJob, id), Required: Real(js.Required), Attainable: Real(js.Attainable),
			RunningSlots: Real(js.RunningSlots), Share: Real(js.Share), GroupSlots: Real(js.GroupSlots), Slack: DurationOf(js.Slack),
			LateBy: DurationOf(js.LateBy), Feasible: js.Feasible, AtRisk: js.AtRisk, ETA50: rel(js.ETA50), ETA90: rel(js.ETA90),
			Deadline: rel(js.Deadline), StartAfter: rel(js.StartAfter), Rank: Int(js.Rank), Burst: js.Burst != nil})
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
