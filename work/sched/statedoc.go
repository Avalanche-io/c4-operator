package sched

import (
	"sort"
	"time"
)

// StateDoc is a State's contents in exported form, for a caller that has
// to write a State down and read it back: the diagnostic bundle's
// snapshots (DESIGN.md section 6.9), which replay a decision with the
// hysteresis it was made under. Its slices are sorted, so the same State
// always gives the same document.
type StateDoc struct {
	Pending    []PendingDoc
	PreemptLog []PoolTimesDoc
	Surplus    []NamedTimeDoc // by burst pool: idle since
	Manual     ManualDoc
	Guard      []NamedTimeDoc // units the starvation guard started
	GuardHit   []NamedTimeDoc // jobs whose guard unit was preempted
	Took       []TookDoc
}

// PendingDoc is a preemption asked for and not yet carried out.
type PendingDoc struct {
	Unit, Job, For, Pool string
	Slots                int
	At                   time.Time
	Reason               PreemptReason
}

// PoolTimesDoc is a pool's recent preemptions.
type PoolTimesDoc struct {
	Pool  string
	Times []time.Time
}

// NamedTimeDoc is a time kept for a pool, unit or job.
type NamedTimeDoc struct {
	Name string
	At   time.Time
}

// ManualDoc is the Manual baseline's wrangler.
type ManualDoc struct {
	LastCheck time.Time
	Bumped    []string
}

// TookDoc is the last preemption of Victim's units for For.
type TookDoc struct {
	Victim, For string
	At          time.Time
}

// Doc writes the State down. A nil State gives an empty document.
func (st *State) Doc() StateDoc {
	var d StateDoc
	if st == nil {
		return d
	}
	for u, p := range st.pending {
		d.Pending = append(d.Pending, PendingDoc{Unit: u, Job: p.job, For: p.forJob, Pool: p.pool, Slots: p.slots, At: p.at, Reason: p.reason})
	}
	sort.Slice(d.Pending, func(i, j int) bool { return d.Pending[i].Unit < d.Pending[j].Unit })
	for pool, ts := range st.preemptLog {
		d.PreemptLog = append(d.PreemptLog, PoolTimesDoc{Pool: pool, Times: append([]time.Time(nil), ts...)})
	}
	sort.Slice(d.PreemptLog, func(i, j int) bool { return d.PreemptLog[i].Pool < d.PreemptLog[j].Pool })
	d.Surplus = namedTimes(st.surplus)
	d.Guard = namedTimes(st.guard)
	d.GuardHit = namedTimes(st.guardHit)
	d.Manual.LastCheck = st.manual.lastCheck
	for j, b := range st.manual.bumped {
		if b {
			d.Manual.Bumped = append(d.Manual.Bumped, j)
		}
	}
	sort.Strings(d.Manual.Bumped)
	for k, at := range st.took {
		d.Took = append(d.Took, TookDoc{Victim: k[0], For: k[1], At: at})
	}
	sort.Slice(d.Took, func(i, j int) bool {
		if d.Took[i].Victim != d.Took[j].Victim {
			return d.Took[i].Victim < d.Took[j].Victim
		}
		return d.Took[i].For < d.Took[j].For
	})
	return d
}

// StateFrom reads a State back from its document.
func StateFrom(d StateDoc) *State {
	st := &State{}
	st.init()
	for _, p := range d.Pending {
		st.pending[p.Unit] = pendingPreempt{job: p.Job, forJob: p.For, pool: p.Pool, slots: p.Slots, at: p.At, reason: p.Reason}
	}
	for _, l := range d.PreemptLog {
		st.preemptLog[l.Pool] = append([]time.Time(nil), l.Times...)
	}
	for _, s := range d.Surplus {
		st.surplus[s.Name] = s.At
	}
	for _, g := range d.Guard {
		st.guard[g.Name] = g.At
	}
	for _, g := range d.GuardHit {
		st.guardHit[g.Name] = g.At
	}
	st.manual.lastCheck = d.Manual.LastCheck
	for _, j := range d.Manual.Bumped {
		st.manual.bumped[j] = true
	}
	for _, t := range d.Took {
		st.took[[2]string{t.Victim, t.For}] = t.At
	}
	return st
}

func namedTimes(m map[string]time.Time) []NamedTimeDoc {
	var out []NamedTimeDoc
	for n, at := range m {
		out = append(out, NamedTimeDoc{Name: n, At: at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
