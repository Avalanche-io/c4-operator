package queue

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Deadline options (proposal eta-deadlines section 2): for a job and a
// candidate deadline D, what D would take. Each answer comes from what-if
// runs of the policy on a copy of the queue's last snapshot, the same way
// the achievable suggestion is found:
//
//   - the band with the job's deadline at D, and which of its percentiles
//     finish by D;
//   - boost: the smallest boost from 1.1 to 2.0 by which the job finishes
//     by D at the policy's quantile;
//   - burst: the burst the policy would suggest for the job at D;
//   - displacement: the jobs on time with the job's deadline far out that
//     D would make late, named only in the viewer's projects;
//   - floor: when nothing closes D, the earliest the job can finish at all.
//
// The readout never changes the farm. It prices; people decide.

// MaxDeadlineOptions bounds the candidate times of one call: each runs the
// policy several times.
const MaxDeadlineOptions = 8

var (
	// ErrNoPlan: the queue has not planned yet, or runs on another replica.
	ErrNoPlan = errors.New("the queue has not planned yet (it runs on the leader replica)")
	// ErrNotPlanned: the job is not in the queue's plan (finished, not
	// schedulable, or new since the last tick).
	ErrNotPlanned = errors.New("the job is not in the queue's plan: it has finished, cannot be scheduled, or is newer than the last tick")
)

// Options is the readout for one job.
type Options struct {
	Job string
	Now time.Time
	// Policy and Quantile are the job's deadline policy kind and the
	// finish percentile its deadlines are read at.
	Policy   forecast.Kind
	Quantile float64
	// Deadline is the job's effective deadline now (zero: none), and Band
	// its finish band as things stand.
	Deadline time.Time
	Band     forecast.Band
	// Achievable is the earliest deadline, on the policy's times, by which
	// the job finishes at Quantile without making a job that is on time
	// late. Zero when there is none within a week.
	Achievable time.Time
	// Floor is the earliest the job can finish with every slot it can
	// attain, and FloorWhy says what binds.
	Floor    time.Time
	FloorWhy string
	Options  []Option
}

// Option is the readout for one candidate deadline.
type Option struct {
	At time.Time
	// Band is the job's finish band with its deadline at At, and Meets the
	// highest of 50, 80, 90 and 95 whose finish is at or before At (0:
	// none). OnTime: the finish at the policy's quantile is by At.
	Band   forecast.Band
	Meets  int
	OnTime bool
	// Boost is the smallest boost (1.1 to 2.0) by which the job, keeping
	// its own deadline, finishes by At at the quantile without making a
	// job that is on time late. Zero: none does, or none is needed.
	Boost float64
	// Burst is the burst the policy suggests for the job at At; nil when
	// it suggests none.
	Burst *OptionBurst
	// Displaces counts the jobs, on time with this job's deadline far
	// out, that At would make late. Displaced names those in the viewer's
	// projects; the other Unnamed are counted, never named.
	Displaces int
	Displaced []string
	Unnamed   int
	// Floor is set when nothing closes At: the earliest finish possible.
	Floor    time.Time
	FloorWhy string
}

// OptionBurst is a burst that would serve a deadline.
type OptionBurst struct {
	Slots map[string]int // added slots per burst pool
	Pools []string       // pools in Slots, cheapest first
	Cost  float64        // US dollars
	// Closes: the burst restores the deadline. Auto: it is within the
	// project's budget and below the lateness cost, so the queue would
	// request it on its own; otherwise a person approves (Why).
	Closes, Auto bool
	Why          string
}

// DeadlineOptions reads out what each candidate deadline would take for
// job (namespace/name). sees reports whether the viewer may see a
// project; displaced jobs of other projects are counted, not named. A nil
// sees names every job.
func (q *Queue) DeadlineOptions(job string, at []time.Time, sees func(project string) bool) (*Options, error) {
	if len(at) == 0 {
		return nil, errors.New("give at least one candidate time")
	}
	if len(at) > MaxDeadlineOptions {
		return nil, fmt.Errorf("at most %d candidate times", MaxDeadlineOptions)
	}
	// Copy what the last tick left and plan without the lock: the
	// snapshot is rebuilt every tick, never changed in place, and each
	// what-if copies the jobs it changes.
	q.mu.Lock()
	snap, pol, policy, cfg, adj := q.lastSnap, q.lastPol[job], q.Policy, q.Cfg, q.lastAdj[job]
	q.mu.Unlock()
	if snap.Now.IsZero() || policy == nil {
		return nil, ErrNoPlan
	}
	w := newWhatIf(policy, cfg, snap, job)
	if w == nil {
		return nil, ErrNotPlanned
	}
	w.adjust = adj
	if pol.Quantile <= 0 {
		pol = forecast.DefaultPolicy()
	}
	now, j := snap.Now, snap.Jobs[w.i]
	out := &Options{Job: job, Now: now, Policy: pol.Kind, Quantile: pol.Quantile, Deadline: j.Deadline}

	cur := w.run(j.Deadline, 0)
	out.Band = w.band(cur)
	hi := now.Add(whatIfHorizon)
	base := w.run(hi, 0)
	onTime := w.onTime(base)
	if d := w.achievable(pol.Quantile, onTime, now.Add(pol.MinLead), hi); !d.Equal(hi) {
		ref := pol
		if ref.Kind != forecast.Sessions {
			ref.Kind = forecast.Achievable
		}
		out.Achievable = ref.Earliest(d, now, j.Submitted, j.Ready)
	}
	out.Floor, out.FloorWhy = w.floor(cur.Jobs[job])

	for _, d := range at {
		o := Option{At: d}
		r := w.run(d, 0)
		js := r.Jobs[job]
		o.Band = w.band(r)
		o.Meets = meets(o.Band, d)
		o.OnTime = !o.Band.IsZero() && !o.Band.At(pol.Quantile).After(d)
		if b := js.Burst; b != nil && len(b.Slots) > 0 {
			o.Burst = &OptionBurst{Slots: b.Slots, Pools: b.Order, Cost: math.Round(b.Cost*100) / 100, Closes: b.Closes, Auto: b.Auto, Why: b.Why}
		}
		for _, id := range onTime {
			if !r.Jobs[id].AtRisk {
				continue
			}
			o.Displaces++
			if sees == nil || sees(w.project[id]) {
				o.Displaced = append(o.Displaced, id)
			}
		}
		o.Unnamed = o.Displaces - len(o.Displaced)
		if !o.OnTime || o.Displaces > 0 {
			o.Boost = w.boost(j.Deadline, d, pol.Quantile, onTime, out.Band)
		}
		if !o.OnTime && o.Boost == 0 && (o.Burst == nil || !o.Burst.Closes) {
			o.Floor, o.FloorWhy = w.floor(js)
		}
		out.Options = append(out.Options, o)
	}
	return out, nil
}

// meets is the highest band percentile finishing by d, 0 when none does.
func meets(b forecast.Band, d time.Time) int {
	if b.IsZero() {
		return 0
	}
	best := 0
	for _, p := range []struct {
		t time.Time
		n int
	}{{b.P50, 50}, {b.P80, 80}, {b.P90, 90}, {b.P95, 95}} {
		if !p.t.After(d) {
			best = p.n
		}
	}
	return best
}

// whatIf runs the policy on copies of a snapshot with one job changed.
type whatIf struct {
	policy  sched.Policy
	cfg     sched.Config
	snap    sched.Snapshot
	i       int
	id      string
	project map[string]string // job ID to project
	// adjust finish-calibrates the job's band (nil: as displayed).
	adjust func(time.Time, forecast.Band) forecast.Band
}

// newWhatIf returns nil when the job is not in the snapshot.
func newWhatIf(policy sched.Policy, cfg sched.Config, snap sched.Snapshot, id string) *whatIf {
	w := &whatIf{policy: policy, cfg: cfg, snap: snap, i: -1, id: id, project: make(map[string]string, len(snap.Jobs))}
	for k := range snap.Jobs {
		w.project[snap.Jobs[k].ID] = snap.Jobs[k].Project
		if snap.Jobs[k].ID == id {
			w.i = k
		}
	}
	if w.i < 0 {
		return nil
	}
	return w
}

// run plans the farm with the job's deadline at d and, when boost is
// above zero, that boost.
func (w *whatIf) run(d time.Time, boost float64) sched.Decisions {
	s2 := w.snap
	s2.Jobs = append([]sched.Job(nil), w.snap.Jobs...)
	s2.Jobs[w.i].Deadline = d
	if boost > 0 {
		s2.Jobs[w.i].Boost = boost
	}
	var st sched.State
	return w.policy(s2, &st, w.cfg)
}

// band is the job's display band in a run.
func (w *whatIf) band(r sched.Decisions) forecast.Band {
	js := r.Jobs[w.id]
	b := forecast.Display(w.snap.Now, w.snap.Jobs[w.i].Estimate, js.ETA50, js.ETA90)
	if w.adjust != nil {
		b = w.adjust(w.snap.Now, b)
	}
	return b
}

// onTime lists the other jobs on time in a run.
func (w *whatIf) onTime(r sched.Decisions) []string {
	var out []string
	for id, js := range r.Jobs {
		if id != w.id && js.Feasible && !js.AtRisk {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// achievable is the earliest deadline in [lo, hi] by which the job
// finishes at quantile q while every job in onTime stays on time; hi when
// there is none.
func (w *whatIf) achievable(q float64, onTime []string, lo, hi time.Time) time.Time {
	ok := func(d time.Time) bool {
		r := w.run(d, 0)
		b := w.band(r)
		if b.IsZero() || b.At(q).After(d) {
			return false
		}
		return !anyAtRisk(r, onTime)
	}
	return forecast.FirstAchievable(lo, hi, whatIfTolerance, ok)
}

func anyAtRisk(r sched.Decisions, ids []string) bool {
	for _, id := range ids {
		if r.Jobs[id].AtRisk {
			return true
		}
	}
	return false
}

// boost is the smallest boost, in tenths from 1.1 to 2.0, by which the
// job keeping its deadline finishes by d at quantile q without making a
// job in onTime late; 0 when none does, or when none is needed (cur, the
// band as things stand, already finishes by d).
//
// The plan is made on deadlines, not boosts: a boost b makes the job ask
// for b times its required slots, the slots it would need for a deadline
// (T-now)/b away. So each step plans that deadline with the boost set,
// and asks that every job on time stay on time, since boost is served
// only after every job's need.
func (w *whatIf) boost(deadline, d time.Time, q float64, onTime []string, cur forecast.Band) float64 {
	now := w.snap.Now
	if deadline.IsZero() || !deadline.After(now) || (!cur.IsZero() && !cur.At(q).After(d)) {
		return 0
	}
	ok := func(k int) bool {
		b := 1 + float64(k)/10
		r := w.run(now.Add(time.Duration(float64(deadline.Sub(now))/b)), b)
		band := w.band(r)
		return !band.IsZero() && !band.At(q).After(d) && !anyAtRisk(r, onTime)
	}
	if !ok(10) {
		return 0
	}
	lo, hi := 0, 10 // ok(lo) false (no boost), ok(hi) true
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if ok(mid) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return 1 + float64(hi)/10
}

// floor is the earliest the job can finish: its critical path, or its
// remaining work over every slot it can attain, whichever is longer.
func (w *whatIf) floor(js sched.JobState) (time.Time, string) {
	s := w.snap.Jobs[w.i].Estimate
	path := time.Duration(s.TPath * float64(time.Second))
	why := "its longest remaining unit alone takes " + span(path)
	if js.Attainable > 0 {
		if all := time.Duration(s.W / js.Attainable * float64(time.Second)); all > path {
			path = all
			why = fmt.Sprintf("its remaining work takes %s on all %.0f slots it can use", span(all), js.Attainable)
		}
	}
	return w.snap.Now.Add(path).Truncate(time.Second), why
}

// span is a duration as a person reads it: 40m, 2h10m.
func span(d time.Duration) string {
	d = d.Round(time.Minute)
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
