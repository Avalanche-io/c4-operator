package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/bundle"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// replayFile reads the diagnostic bundle at path and replays it (DESIGN.md
// section 6.9), writing what it found to w. It reports whether every
// snapshot replayed to the farm's own decisions.
//
// decide replays each snapshot's decision through the policy it names and
// compares it with the farm's, by decision hash; a snapshot that differs
// is listed with how many decisions of each kind differ.
//
// forward runs the farm on from the last snapshot for horizon, deciding
// every step, with each unit taking its job's mean estimate: when each job
// would finish, and what the policy dispatches, preempts and bursts on the
// way. Names stay pseudonyms; the farm maps them back.
func replayFile(path, mode string, horizon, step time.Duration, w io.Writer) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	c, err := bundle.Read(f)
	if err != nil {
		return false, err
	}
	if len(c.Snapshots) == 0 {
		return false, fmt.Errorf("%s holds no snapshots", path)
	}
	fmt.Fprintf(w, "bundle %s: %d snapshots, key epoch %d\n", c.ID, len(c.Snapshots), c.Redaction.KeyEpoch)
	switch mode {
	case "decide":
		return replayDecide(c.Snapshots, w), nil
	case "forward":
		return replayForward(c.Snapshots[len(c.Snapshots)-1], horizon, step, w)
	}
	return false, fmt.Errorf("-replay %q: decide or forward", mode)
}

func replayDecide(snaps []telemetry.Snapshot, w io.Writer) bool {
	all := true
	for i, s := range snaps {
		policy := s.Policy.Func()
		pol := telemetry.PolicyName(s.Policy)
		d := s.Decisions
		counts := fmt.Sprintf("dispatch %d, preempt %d, capacity %d, burst %d, jobs %d", len(d.Dispatch), len(d.Preempt), len(d.Capacity), len(d.Bursts), len(d.Jobs))
		if policy == nil {
			all = false
			fmt.Fprintf(w, "%03d  cannot replay: decided by a policy this build does not know (%s)\n", i, pol)
			continue
		}
		if telemetry.ReplayHash(s, policy) == s.DecisionHash {
			fmt.Fprintf(w, "%03d  replays (%s): %s\n", i, pol, counts)
			continue
		}
		all = false
		check := s
		telemetry.CheckFaithful(&check, policy)
		var diff []string
		for _, dc := range check.Divergences {
			diff = append(diff, fmt.Sprintf("%d %s", dc.Count, telemetry.DivergenceName(dc.Kind)))
		}
		fmt.Fprintf(w, "%03d  DIFFERS (%s): %s; the farm decided %s\n", i, pol, strings.Join(diff, ", "), counts)
	}
	if all {
		fmt.Fprintln(w, "every snapshot replays to the farm's own decisions")
	}
	return all
}

// fjob is a job as the forward run plays it.
type fjob struct {
	rec      telemetry.Job
	tmpl     sched.Job
	queue    []sched.UnitRef
	blocked  int
	next     int
	running  []frun
	unitSec  float64
	w0, tp0  float64
	finished time.Time
}

type frun struct {
	id, pool string
	slots    int
	started  time.Time
	end      time.Time
}

type provision struct {
	pool  string
	slots int
	at    time.Time
}

func replayForward(rec telemetry.Snapshot, horizon, step time.Duration, w io.Writer) (bool, error) {
	policy := rec.Policy.Func()
	if policy == nil {
		return false, fmt.Errorf("the last snapshot was decided by a policy this build does not know (%s)", telemetry.PolicyName(rec.Policy))
	}
	if step <= 0 || horizon <= 0 {
		return false, fmt.Errorf("-step and -horizon must be positive")
	}
	s, st, cfg := telemetry.Restore(rec)
	t0 := s.Now
	pools := map[string]*sched.Pool{}
	for i := range s.Pools {
		pools[s.Pools[i].Name] = &s.Pools[i]
	}
	var jobs []*fjob
	byName := map[string]*fjob{}
	for i, j := range s.Jobs {
		fj := &fjob{rec: rec.Jobs[i], tmpl: j, queue: append([]sched.UnitRef(nil), j.Runnable...), blocked: j.Blocked,
			w0: j.Estimate.W, tp0: j.Estimate.TPath}
		k := max(1, j.SlotsPerUnit)
		if n := j.Estimate.Queued + j.Estimate.Running; n > 0 && j.Estimate.W > 0 {
			fj.unitSec = j.Estimate.W / float64(n) / float64(k)
		} else {
			fj.unitSec = math.Max(j.Estimate.TPath, 60)
		}
		for _, u := range append(append([]sched.UnitRef(nil), j.Runnable...), refs(j.RunningUnits)...) {
			if _, idx, ok := strings.Cut(u.ID, "#"); ok {
				if n, err := strconv.Atoi(idx); err == nil && n >= fj.next {
					fj.next = n + 1
				}
			}
		}
		for len(fj.queue) < j.RunnableTotal {
			fj.queue = append(fj.queue, fj.unit())
		}
		for _, ru := range j.RunningUnits {
			end := t0.Add(ru.Remaining)
			if ru.Remaining <= 0 {
				left := fj.unitSec - t0.Sub(ru.Started).Seconds()
				end = t0.Add(time.Duration(math.Max(left, step.Seconds()) * float64(time.Second)))
			}
			fj.running = append(fj.running, frun{id: ru.ID, pool: ru.Pool, slots: max(1, ru.Slots), started: ru.Started, end: end})
		}
		jobs = append(jobs, fj)
		byName[j.ID] = fj
	}
	var pending []provision
	var dispatched, preempted, burstUp int
	end := t0.Add(horizon)
	for now := t0; now.Before(end); now = now.Add(step) {
		for _, fj := range jobs {
			kept := fj.running[:0]
			for _, r := range fj.running {
				if !r.end.After(now) {
					fj.tmpl.UnitsDone++
					fj.tmpl.DoneSeconds += r.end.Sub(r.started).Seconds()
					continue
				}
				kept = append(kept, r)
			}
			fj.running = kept
		}
		kept := pending[:0]
		for _, p := range pending {
			if p.at.After(now) {
				kept = append(kept, p)
				continue
			}
			pools[p.pool].Slots = p.slots
			if b := pools[p.pool].Burst; b != nil {
				b.Provisioned = p.slots
			}
		}
		pending = kept
		left := 0
		for _, fj := range jobs {
			if !fj.finished.IsZero() {
				continue
			}
			if len(fj.queue) == 0 && len(fj.running) == 0 && fj.blocked > 0 && fj.upstreamDone(byName) {
				for ; fj.blocked > 0; fj.blocked-- {
					fj.queue = append(fj.queue, fj.unit())
				}
			}
			if len(fj.queue) == 0 && len(fj.running) == 0 && fj.blocked == 0 {
				fj.finished = now
				continue
			}
			left++
		}
		if left == 0 {
			break
		}
		snap := forwardSnapshot(now, s, jobs, pools)
		d := policy(snap, st, cfg)
		for _, x := range d.Dispatch {
			fj := byName[x.Job]
			if fj == nil {
				continue
			}
			for i, u := range fj.queue {
				if u.ID != x.Unit {
					continue
				}
				fj.queue = append(fj.queue[:i], fj.queue[i+1:]...)
				slots := max(1, x.Slots)
				fj.running = append(fj.running, frun{id: u.ID, pool: x.Pool, slots: slots, started: now,
					end: now.Add(time.Duration(fj.unitSec * float64(time.Second)))})
				dispatched++
				break
			}
		}
		for _, x := range d.Preempt {
			fj := byName[x.Job]
			if fj == nil {
				continue
			}
			for i, r := range fj.running {
				if r.id != x.Unit {
					continue
				}
				fj.running = append(fj.running[:i], fj.running[i+1:]...)
				fj.queue = append([]sched.UnitRef{{ID: r.id}}, fj.queue...)
				preempted++
				break
			}
		}
		for _, c := range d.Capacity {
			p := pools[c.Pool]
			if p == nil || p.Burst == nil {
				continue
			}
			p.Burst.Requested = c.Slots
			switch {
			case c.Slots > p.Slots:
				burstUp += c.Slots - p.Slots
				pending = append(pending, provision{pool: c.Pool, slots: c.Slots, at: now.Add(p.Burst.Provisioning)})
			case c.Slots < p.Slots:
				p.Slots = max(c.Slots, used(jobs, c.Pool))
				p.Burst.Provisioned = p.Slots
			}
		}
	}
	fmt.Fprintf(w, "forward from the last snapshot, %s on, deciding every %s, each unit taking its job's mean estimate:\n", horizon, step)
	fmt.Fprintf(w, "%d dispatches, %d preemptions, %d burst slots asked for\n", dispatched, preempted, burstUp)
	onTime := true
	for _, fj := range jobs {
		due := "no deadline"
		if !fj.tmpl.Deadline.IsZero() {
			due = "due " + rel(fj.tmpl.Deadline.Sub(t0))
		}
		switch {
		case fj.finished.IsZero():
			fmt.Fprintf(w, "%s  %s, not finished within %s (%d units left)\n", fj.rec.ID.Text(), due, horizon, len(fj.queue)+len(fj.running)+fj.blocked)
			if !fj.tmpl.Deadline.IsZero() && fj.tmpl.Deadline.Before(end) {
				onTime = false
			}
		case !fj.tmpl.Deadline.IsZero() && fj.finished.After(fj.tmpl.Deadline):
			onTime = false
			fmt.Fprintf(w, "%s  %s, finishes %s, late by %s\n", fj.rec.ID.Text(), due, rel(fj.finished.Sub(t0)), strings.TrimPrefix(rel(fj.finished.Sub(fj.tmpl.Deadline)), "+"))
		default:
			fmt.Fprintf(w, "%s  %s, finishes %s\n", fj.rec.ID.Text(), due, rel(fj.finished.Sub(t0)))
		}
	}
	return onTime, nil
}

// unit is the job's next unit, named after its last.
func (fj *fjob) unit() sched.UnitRef {
	u := sched.UnitRef{ID: fj.tmpl.ID + "#" + strconv.Itoa(fj.next)}
	fj.next++
	return u
}

func (fj *fjob) upstreamDone(byName map[string]*fjob) bool {
	for _, a := range fj.tmpl.After {
		if up := byName[a]; up != nil && up.finished.IsZero() {
			return false
		}
	}
	return true
}

func refs(rs []sched.RunningUnit) []sched.UnitRef {
	out := make([]sched.UnitRef, len(rs))
	for i, r := range rs {
		out[i] = sched.UnitRef{ID: r.ID}
	}
	return out
}

func used(jobs []*fjob, pool string) int {
	n := 0
	for _, fj := range jobs {
		for _, r := range fj.running {
			if r.pool == pool {
				n += r.slots
			}
		}
	}
	return n
}

// forwardSnapshot is the farm at now as the forward run has it, shaped as
// the queue shapes one: a job lists the runnable units that could start,
// one more than fit.
func forwardSnapshot(now time.Time, base sched.Snapshot, jobs []*fjob, pools map[string]*sched.Pool) sched.Snapshot {
	s := sched.Snapshot{Now: now, Projects: base.Projects, Links: base.Links}
	free := map[string]int{}
	for _, p := range base.Pools {
		cur := *pools[p.Name]
		if cur.Burst != nil {
			b := *cur.Burst
			cur.Burst = &b
		}
		cur.Used = used(jobs, p.Name)
		free[p.Name] = cur.Slots - cur.Used
		s.Pools = append(s.Pools, cur)
	}
	for _, fj := range jobs {
		if !fj.finished.IsZero() {
			continue
		}
		j := fj.tmpl
		k := max(1, j.SlotsPerUnit)
		room := 0
		for _, p := range j.Pools {
			room += max(0, free[p])
		}
		n := min(len(fj.queue), room/k+1)
		j.Runnable = append([]sched.UnitRef(nil), fj.queue[:n]...)
		j.RunnableTotal = len(fj.queue)
		j.Blocked = fj.blocked
		j.Running = map[string]int{}
		j.RunningUnits = nil
		for _, r := range fj.running {
			j.Running[r.pool] += r.slots
			j.RunningUnits = append(j.RunningUnits, sched.RunningUnit{ID: r.id, Pool: r.pool, Slots: r.slots, Started: r.started, Remaining: r.end.Sub(now)})
		}
		if len(fj.running) > 0 {
			j.LastRunning = now
		}
		units := len(fj.queue) + len(fj.running) + fj.blocked
		e := j.Estimate
		if fj.w0 > 0 {
			left := float64(len(fj.queue)+fj.blocked) * fj.unitSec * float64(k)
			for _, r := range fj.running {
				left += r.end.Sub(now).Seconds() * float64(r.slots)
			}
			f := left / fj.w0
			e.W, e.Sigma, e.W50, e.W80, e.W90, e.W95 = e.W*f, e.Sigma*f, e.W50*f, e.W80*f, e.W90*f, e.W95*f
		}
		e.Queued, e.Running = len(fj.queue)+fj.blocked, len(fj.running)
		if units > 0 {
			e.TPath = math.Min(fj.tp0, fj.unitSec)
		}
		j.Estimate = e
		s.Jobs = append(s.Jobs, j)
	}
	return s
}

// rel is a duration from the snapshot, to the minute: "+2h10m".
func rel(d time.Duration) string {
	d = d.Round(time.Minute)
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	return sign + strings.TrimSuffix(d.String(), "0s")
}
