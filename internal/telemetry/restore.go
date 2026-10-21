package telemetry

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// ReplayEpoch is the now a restored snapshot is replayed at. Any time
// works, since the policy reads only differences; a fixed one keeps
// replays repeatable.
var ReplayEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Restore is a snapshot record as the policy's input again: every name the
// synthetic one its rank gives, so names compare as they did on the farm;
// times from ReplayEpoch; money as scaled. Feeding it to sched.Decide
// replays the farm's decision.
func Restore(rec Snapshot) (sched.Snapshot, *sched.State, sched.Config) {
	epoch := ReplayEpoch
	at := func(r RelTime) time.Time { return r.At(epoch) }
	s := sched.Snapshot{Now: epoch}
	for _, p := range rec.Pools {
		sp := sched.Pool{Name: p.Name.Synthetic(), Slots: int(p.Slots), Used: int(p.Used), CostPerSlotHour: float64(p.Cost)}
		if b := p.Burst; b != nil {
			sp.Burst = &sched.Burst{MaxSlots: int(b.MaxSlots), Provisioning: b.Provisioning.Duration(), CostPerSlotHour: float64(b.Cost),
				Requested: int(b.Requested), Provisioned: int(b.Provisioned)}
		}
		s.Pools = append(s.Pools, sp)
	}
	for _, p := range rec.Projects {
		sp := sched.Project{Name: p.Name.Synthetic(), Weight: float64(p.Weight), BurstBudgetLeft: float64(p.BurstBudgetLeft), LateCostPerHour: float64(p.LateCostPerHour)}
		if len(p.Quotas) > 0 {
			sp.Quotas = map[string]sched.Quota{}
			for _, q := range p.Quotas {
				sp.Quotas[q.Pool.Synthetic()] = sched.Quota{Nominal: int(q.Nominal), BorrowingLimit: int(q.BorrowingLimit)}
			}
		}
		s.Projects = append(s.Projects, sp)
	}
	for _, j := range rec.Jobs {
		sj := sched.Job{ID: j.ID.Synthetic(), Project: j.Project.Synthetic(), Class: name(j.Class),
			Submitted: at(j.Submitted), Ready: at(j.Ready), Deadline: at(j.Deadline), NotBefore: at(j.NotBefore), LastRunning: at(j.LastRunning),
			RequiredRate: float64(j.RequiredRate), MaxParallel: int(j.MaxParallel), SlotsPerUnit: int(j.SlotsPerUnit),
			RunnableTotal: int(j.RunnableTotal), Blocked: int(j.Blocked), Unevaluated: int(j.Unevaluated), Estimate: summaryOf(j.Estimate),
			Hold: j.Hold, HoldStop: j.HoldStop, Boost: float64(j.Boost), Preemptible: j.Preemptible, Checkpoints: j.Checkpoints,
			Priority: int(j.Priority), UnitsDone: int(j.UnitsDone), DoneSeconds: float64(j.DoneSeconds)}
		for _, a := range j.After {
			sj.After = append(sj.After, a.Synthetic())
		}
		for _, p := range j.Pools {
			sj.Pools = append(sj.Pools, p.Synthetic())
		}
		if len(j.Running) > 0 {
			sj.Running = map[string]int{}
			for _, r := range j.Running {
				sj.Running[r.Pool.Synthetic()] = int(r.Slots)
			}
		}
		for _, u := range j.Runnable {
			sj.Runnable = append(sj.Runnable, sched.UnitRef{ID: unitName(u.Unit), Info: u.Info, Slots: int(u.Slots)})
		}
		for _, u := range j.RunningUnits {
			sj.RunningUnits = append(sj.RunningUnits, sched.RunningUnit{ID: unitName(u.Unit), Pool: u.Pool.Synthetic(), Slots: int(u.Slots),
				Started: at(u.Started), LastCheckpoint: at(u.LastCheckpoint), RestoreCost: u.RestoreCost.Duration(),
				Preemptions: int(u.Preemptions), Remaining: u.Remaining.Duration()})
		}
		if len(j.Sources) > 0 {
			sj.Sources = map[string][]sched.Source{}
			for _, ps := range j.Sources {
				list := []sched.Source{}
				for _, src := range ps.Sources {
					list = append(list, sched.Source{Via: enumName(src.Via), Channel: enumName(src.Channel), Link: name(src.Link),
						Bytes: float64(src.Bytes), Latency: src.Latency.Duration(), Cost: float64(src.Cost), Work: float64(src.Work), Longest: src.Longest.Duration()})
				}
				sj.Sources[ps.Pool.Synthetic()] = list
			}
		}
		s.Jobs = append(s.Jobs, sj)
	}
	for _, l := range rec.Links {
		s.Links = append(s.Links, sched.Link{Name: l.Name.Synthetic(), BytesPerSecond: float64(l.BytesPerSecond), Committed: float64(l.Committed)})
	}
	return s, sched.StateFrom(stateDocOf(rec.State, at)), configFrom(rec.Config)
}

func name(p Pseudonym) string {
	if p.None() {
		return ""
	}
	return p.Synthetic()
}

func unitName(u UnitID) string {
	if !u.Whole.None() {
		return u.Whole.Synthetic()
	}
	return u.Job.Synthetic() + "#" + strconv.FormatInt(int64(u.Index), 10)
}

func summaryOf(e Estimate) estimate.Summary {
	return estimate.Summary{W: float64(e.W), Sigma: float64(e.Sigma), W90: float64(e.W90), W50: float64(e.W50), W80: float64(e.W80), W95: float64(e.W95),
		TPath: float64(e.TPath), Queued: int(e.Queued), Running: int(e.Running), Unevaluated: int(e.Unevaluated),
		Stratum: estimate.Stratum{Sampled: int8(e.Sampled), Measured: int8(e.Measured), Open: int8(e.Open)}, Calibrated: e.Calibrated}
}

func stateDocOf(st State, at func(RelTime) time.Time) sched.StateDoc {
	var d sched.StateDoc
	for _, p := range st.Pending {
		d.Pending = append(d.Pending, sched.PendingDoc{Unit: unitName(p.Unit), Job: name(p.Job), For: name(p.For), Pool: name(p.Pool),
			Slots: int(p.Slots), At: at(p.At), Reason: sched.PreemptReason(enumName(p.Reason))})
	}
	for _, l := range st.PreemptLog {
		pt := sched.PoolTimesDoc{Pool: name(l.Pool)}
		for _, t := range l.Times {
			pt.Times = append(pt.Times, at(t))
		}
		d.PreemptLog = append(d.PreemptLog, pt)
	}
	for _, x := range st.Surplus {
		d.Surplus = append(d.Surplus, sched.NamedTimeDoc{Name: name(x.Pool), At: at(x.At)})
	}
	d.Manual.LastCheck = at(st.ManualCheck)
	for _, j := range st.ManualBumped {
		d.Manual.Bumped = append(d.Manual.Bumped, name(j))
	}
	for _, x := range st.Guard {
		d.Guard = append(d.Guard, sched.NamedTimeDoc{Name: unitName(x.Unit), At: at(x.At)})
	}
	for _, x := range st.GuardHit {
		d.GuardHit = append(d.GuardHit, sched.NamedTimeDoc{Name: name(x.Job), At: at(x.At)})
	}
	for _, t := range st.Took {
		d.Took = append(d.Took, sched.TookDoc{Victim: name(t.Victim), For: name(t.For), At: at(t.At)})
	}
	return d
}

func configFrom(c Config) sched.Config {
	return sched.Config{StarvationAfter: c.StarvationAfter.Duration(), StarvationDeferMax: c.StarvationDeferMax.Duration(), TieBand: float64(c.TieBand),
		InfoUnitsPerJob: int(c.InfoUnitsPerJob), PreemptSlack: c.PreemptSlack.Duration(), PreemptWindow: c.PreemptWindow.Duration(),
		PreemptPerWindow: int(c.PreemptPerWindow), MaxPreemptionsPerUnit: int(c.MaxPreemptionsPerUnit), MaxLostWork: c.MaxLostWork.Duration(),
		ScaleDownAfter: c.ScaleDownAfter.Duration(), Burst: c.Burst, NoStage: c.NoStage, MinRemaining: c.MinRemaining.Duration(),
		TailAware: c.TailAware, Planner: sched.Planner(enumName(c.Planner)), NoInfoTier: c.NoInfoTier, Surplus: sched.Surplus(enumName(c.Surplus)),
		SlackBand: c.SlackBand.Duration(), RateSlack: c.RateSlack.Duration(), WranglerEvery: c.WranglerEvery.Duration(),
		WranglerMargin: c.WranglerMargin.Duration(), WranglerUnbump: c.WranglerUnbump.Duration(), WranglerStartAlarm: c.WranglerStartAlarm.Duration()}
}

// CheckFaithful replays the record through policy and records whether it
// gives the farm's decisions, and how many of each kind differ. Losing
// detail is allowed; a bundle that does not reproduce says so.
func CheckFaithful(rec *Snapshot, policy sched.Policy) {
	s, st, cfg := Restore(*rec)
	replay := policy(s, st, cfg)
	farm := canonical(rec.Decisions)
	again := canonicalReplay(replay)
	rec.Divergences = nil
	rec.Faithful = true
	for _, k := range []Divergence{DivergeDispatch, DivergePreempt, DivergeCapacity, DivergeBurst, DivergeRisk} {
		if n := differ(farm[k], again[k]); n > 0 {
			rec.Faithful = false
			rec.Divergences = append(rec.Divergences, DivergenceCount{Kind: k, Count: Int(n)})
		}
	}
}

func ref(p Pseudonym) string {
	if p.None() {
		return "-"
	}
	return fmt.Sprintf("%s%d", p.Kind.prefix(), p.Rank)
}

func unitRef(u UnitID) string {
	if !u.Whole.None() {
		return ref(u.Whole)
	}
	return fmt.Sprintf("%s#%d", ref(u.Job), u.Index)
}

func canonical(d Decisions) map[Divergence][]string {
	out := map[Divergence][]string{}
	for _, x := range d.Dispatch {
		out[DivergeDispatch] = append(out[DivergeDispatch], fmt.Sprintf("%s|%s|%s|%d|%d", ref(x.Job), unitRef(x.Unit), ref(x.Pool), x.Slots, x.Tier))
	}
	for _, x := range d.Preempt {
		out[DivergePreempt] = append(out[DivergePreempt], fmt.Sprintf("%s|%s|%s|%d|%d|%s|%t", ref(x.Job), unitRef(x.Unit), ref(x.Pool), x.Slots, x.Reason, ref(x.For), x.Yield))
	}
	for _, c := range d.Capacity {
		out[DivergeCapacity] = append(out[DivergeCapacity], fmt.Sprintf("%s|%d", ref(c.Pool), c.Slots))
	}
	for _, b := range d.Bursts {
		var slots []string
		for _, s := range b.Slots {
			slots = append(slots, fmt.Sprintf("%s=%d", ref(s.Pool), s.Slots))
		}
		out[DivergeBurst] = append(out[DivergeBurst], fmt.Sprintf("%s|%t|%t|%s", ref(b.Job), b.Closes, b.Auto, strings.Join(slots, ",")))
	}
	for _, j := range d.Jobs {
		out[DivergeRisk] = append(out[DivergeRisk], fmt.Sprintf("%s|%t|%t", ref(j.Job), j.Feasible, j.AtRisk))
	}
	return out
}

// synthRef reads a synthetic name back as kind letter and rank, the form
// ref writes.
func synthRef(s string) string {
	if s == "" {
		return "-"
	}
	if i := strings.LastIndexByte(s, '#'); i > 0 {
		return synthRef(s[:i]) + s[i:]
	}
	n, err := strconv.ParseUint(s[1:], 10, 32)
	if err != nil {
		return "?" + s
	}
	return fmt.Sprintf("%s%d", s[:1], n)
}

func canonicalReplay(d sched.Decisions) map[Divergence][]string {
	out := map[Divergence][]string{}
	for _, x := range d.Dispatch {
		out[DivergeDispatch] = append(out[DivergeDispatch], fmt.Sprintf("%s|%s|%s|%d|%d", synthRef(x.Job), synthRef(x.Unit), synthRef(x.Pool), x.Slots, TierOf(string(x.Tier))))
	}
	for _, x := range d.Preempt {
		out[DivergePreempt] = append(out[DivergePreempt], fmt.Sprintf("%s|%s|%s|%d|%d|%s|%t", synthRef(x.Job), synthRef(x.Unit), synthRef(x.Pool), x.Slots, ReasonOf(string(x.Reason)), synthRef(x.For), x.Yield))
	}
	for _, c := range d.Capacity {
		out[DivergeCapacity] = append(out[DivergeCapacity], fmt.Sprintf("%s|%d", synthRef(c.Pool), c.Slots))
	}
	for _, b := range d.Bursts {
		var slots []string
		for _, p := range sortedKeys(b.Slots) {
			slots = append(slots, fmt.Sprintf("%s=%d", synthRef(p), b.Slots[p]))
		}
		out[DivergeBurst] = append(out[DivergeBurst], fmt.Sprintf("%s|%t|%t|%s", synthRef(b.Job), b.Closes, b.Auto, strings.Join(slots, ",")))
	}
	for _, id := range sortedKeys(d.Jobs) {
		j := d.Jobs[id]
		out[DivergeRisk] = append(out[DivergeRisk], fmt.Sprintf("%s|%t|%t", synthRef(id), j.Feasible, j.AtRisk))
	}
	return out
}

// differ is the size of the symmetric difference of two multisets.
func differ(a, b []string) int {
	count := map[string]int{}
	for _, s := range a {
		count[s]++
	}
	for _, s := range b {
		count[s]--
	}
	n := 0
	for _, c := range count {
		if c < 0 {
			c = -c
		}
		n += c
	}
	return n
}
