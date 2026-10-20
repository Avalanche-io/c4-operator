package server

import (
	"math"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// LabelShot is the WorkJob label naming the shot a job belongs to; the
// HUD draws a shot's jobs (sim, render, comp, transcode) as one sheet.
const LabelShot = "c4.avalanche.io/shot"

// jobPods are the job's unit pods that have not finished.
func (m *model) jobPods(key string) []*corev1.Pod {
	if m.podsByJob != nil {
		return m.podsByJob[key]
	}
	var out []*corev1.Pod
	for i := range m.pods {
		p := &m.pods[i]
		if p.Namespace+"/"+p.Labels[c4v1.LabelJob] != key || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		out = append(out, p)
	}
	return out
}

// indexPods groups the unfinished unit pods by job, once per collection.
func (m *model) indexPods() {
	m.podsByJob = map[string][]*corev1.Pod{}
	for i := range m.pods {
		p := &m.pods[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		key := p.Namespace + "/" + p.Labels[c4v1.LabelJob]
		m.podsByJob[key] = append(m.podsByJob[key], p)
	}
}

// unitSlots is the slots unit i of the job holds while it runs.
func unitSlots(r *jobRec, i int) float64 {
	if r.plan != nil && i >= 0 && i < len(r.plan.Units) && r.plan.Units[i].Slots > 1 {
		return float64(r.plan.Units[i].Slots)
	}
	return 1
}

func overrun(r *jobRec) float64 {
	if r.plan != nil && r.plan.Estimate.Overrun > 0 {
		return r.plan.Estimate.Overrun
	}
	return estimate.DefaultConfig().Overrun
}

// jobWork is JobSummary.work, built without an estimator so the snapshot
// stays cheap: done sums the measured unit times in status, running comes
// from the unit pods' start times, remaining and its 90th percentile are
// the queue's W and W90. The running units' remainder is approximated from
// the job's mean measured unit time (the job detail uses the estimator).
func jobWork(m *model, r *jobRec) api.Work {
	st := &r.job.Status
	if st.Estimate == nil {
		u := st.Units
		done := u.Succeeded + u.Egress
		active := u.Running + u.Retrying + u.Preflight
		w := api.Work{Basis: api.BasisUnits, DoneSlotSeconds: float64(done), RunningSlotSeconds: float64(active)}
		delivered := float64(u.Succeeded)
		rest := float64(u.Total - done - active - u.Failed - u.Accepted)
		if u.Total == 0 && r.plan != nil {
			rest = float64(len(r.plan.Units))
		}
		w.RemainingSlotSeconds, w.P90RemainingSlotSeconds = math.Max(0, rest), math.Max(0, rest)
		return fractions(w, delivered, st.Phase)
	}
	w := api.Work{Basis: api.BasisEstimate}
	succeeded, _ := order.DecodeSet(st.UnitStates["succeeded"])
	sum, n := 0.0, 0
	for _, i := range succeeded {
		if i < 0 || i >= len(st.UnitMillis) || st.UnitMillis[i] <= 0 {
			continue
		}
		sec := float64(st.UnitMillis[i]) / 1000
		w.DoneSlotSeconds += sec * unitSlots(r, i)
		sum += sec
		n++
	}
	// Units whose work is done and whose output still moves count as done,
	// not yet delivered.
	delivered := w.DoneSlotSeconds
	if egress, _ := order.DecodeSet(st.UnitStates["egress"]); len(egress) > 0 {
		in := map[int]bool{}
		for _, i := range egress {
			in[i] = true
		}
		for _, p := range m.finishedPods(r.key) {
			i, err := strconv.Atoi(p.Labels[c4v1.LabelUnit])
			if err != nil || !in[i] {
				continue
			}
			w.DoneSlotSeconds += podSeconds(p, m.at) * unitSlots(r, i)
		}
	}
	mean := 0.0
	switch {
	case n > 0:
		mean = sum / float64(n)
	case r.plan != nil && r.plan.Prior != nil:
		mean = r.plan.Prior.Mean
	}
	over := overrun(r)
	for _, p := range m.jobPods(r.key) {
		if p.Status.StartTime == nil {
			continue
		}
		el := math.Max(0, m.at.Sub(p.Status.StartTime.Time).Seconds())
		sl := float64(podSlots(p))
		w.RunningSlotSeconds += el * sl
		rem := mean - el
		if rem <= 0 {
			rem = over * el
		}
		w.RunningRemainingSlotSeconds += rem * sl
	}
	w.RemainingSlotSeconds = float64(st.Estimate.RemainingWork)
	w.P90RemainingSlotSeconds = math.Max(float64(st.Estimate.RemainingWorkP90), w.RemainingSlotSeconds)
	if st.Finished != nil {
		w.RunningSlotSeconds, w.RunningRemainingSlotSeconds, w.RemainingSlotSeconds, w.P90RemainingSlotSeconds = 0, 0, 0, 0
	}
	w.RunningRemainingSlotSeconds = math.Min(w.RunningRemainingSlotSeconds, w.RemainingSlotSeconds)
	return fractions(roundWork(w), delivered, st.Phase)
}

// fractions sets the done and delivered shares of w, by work: done over
// done, in flight and remaining. A succeeded job is whole.
func fractions(w api.Work, delivered float64, phase string) api.Work {
	total := w.DoneSlotSeconds + w.RunningSlotSeconds + w.RemainingSlotSeconds
	d, v := 0.0, 0.0
	switch {
	case phase == "Succeeded":
		d, v = 1, 1
	case total > 0:
		d, v = math.Min(1, w.DoneSlotSeconds/total), math.Min(1, delivered/total)
	}
	d, v = math.Round(d*1e4)/1e4, math.Round(v*1e4)/1e4
	w.DoneFraction, w.DeliveredFraction = &d, &v
	return w
}

// progressMark is a job's highest done and delivered shares so far.
type progressMark struct {
	finished        int32 // units succeeded or with output moving
	done, delivered float64
}

// steady keeps each job's shares from going backwards between
// collections: an estimate that grows shows in the time left, never as
// done work lost. Only a requeue of finished units (fewer of them than
// before) starts the job's marks again.
func (s *Server) steady(m *model) {
	seen := make(map[string]progressMark, len(m.jobs))
	for _, r := range m.jobs {
		w := &r.work
		if w.DoneFraction == nil || w.DeliveredFraction == nil {
			continue
		}
		key := string(r.job.UID)
		u := r.job.Status.Units
		mk := progressMark{finished: u.Succeeded + u.Egress, done: *w.DoneFraction, delivered: *w.DeliveredFraction}
		if old, ok := s.marks[key]; ok && mk.finished >= old.finished {
			mk.done, mk.delivered = math.Max(mk.done, old.done), math.Max(mk.delivered, old.delivered)
		}
		mk.done = math.Max(mk.done, mk.delivered)
		d, v := mk.done, mk.delivered
		w.DoneFraction, w.DeliveredFraction = &d, &v
		seen[key] = mk
	}
	s.marks = seen
}

// podSeconds is how long a finished pod's containers ran.
func podSeconds(p *corev1.Pod, now time.Time) float64 {
	var start, end time.Time
	for _, c := range p.Status.ContainerStatuses {
		t := c.State.Terminated
		if t == nil {
			continue
		}
		if start.IsZero() || t.StartedAt.Time.Before(start) {
			start = t.StartedAt.Time
		}
		if t.FinishedAt.After(end) {
			end = t.FinishedAt.Time
		}
	}
	if start.IsZero() && p.Status.StartTime != nil {
		start, end = p.Status.StartTime.Time, now
	}
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Seconds()
}

// finishedPods are the job's succeeded unit pods the queue still keeps
// (their output is still moving).
func (m *model) finishedPods(key string) []*corev1.Pod {
	var out []*corev1.Pod
	for i := range m.pods {
		p := &m.pods[i]
		if p.Status.Phase == corev1.PodSucceeded && p.Namespace+"/"+p.Labels[c4v1.LabelJob] == key {
			out = append(out, p)
		}
	}
	return out
}

func roundWork(w api.Work) api.Work {
	w.DoneSlotSeconds = math.Round(w.DoneSlotSeconds)
	w.RunningSlotSeconds = math.Round(w.RunningSlotSeconds)
	w.RunningRemainingSlotSeconds = math.Round(w.RunningRemainingSlotSeconds)
	w.RemainingSlotSeconds = math.Round(w.RemainingSlotSeconds)
	w.P90RemainingSlotSeconds = math.Round(w.P90RemainingSlotSeconds)
	return w
}

// unitWorks is unitList[].work for every unit of a planned job, and the
// running units' expected remainder by the estimator, in slot-seconds.
func unitWorks(m *model, r *jobRec, states []string, e *estimate.Estimator) ([]api.UnitWork, float64) {
	st := &r.job.Status
	out := make([]api.UnitWork, len(r.plan.Units))
	started := map[int]float64{}
	for _, p := range m.jobPods(r.key) {
		if p.Status.StartTime == nil {
			continue
		}
		i, err := strconv.Atoi(p.Labels[c4v1.LabelUnit])
		if err == nil {
			started[i] = math.Max(0, m.at.Sub(p.Status.StartTime.Time).Seconds())
		}
	}
	moving := map[int]float64{}
	if st.UnitStates["egress"] != "" {
		for _, p := range m.finishedPods(r.key) {
			if i, err := strconv.Atoi(p.Labels[c4v1.LabelUnit]); err == nil {
				moving[i] = podSeconds(p, m.at)
			}
		}
	}
	runRem := 0.0
	for i, u := range r.plan.Units {
		sl := unitSlots(r, i)
		measured := 0.0
		if i < len(st.UnitMillis) && st.UnitMillis[i] > 0 {
			measured = float64(st.UnitMillis[i]) / 1000
		}
		pred := e.Predict(u.Key, u.Pos)
		var w api.UnitWork
		el, running := started[i]
		switch {
		case states[i] == "superseded":
			// a review chose others: what it spent, and no more
			w.Spent, w.Expected = measured*sl, measured*sl
		case states[i] == "succeeded" && measured > 0:
			w.Spent, w.Expected = measured*sl, measured*sl
		case states[i] == "egress" && moving[i] > 0:
			w.Spent, w.Expected = moving[i]*sl, moving[i]*sl
		case running:
			rem, _ := e.Remainder(pred, 0, el)
			w.Spent, w.Expected = el*sl, (el+rem)*sl
			runRem += rem * sl
		case states[i] == "failed" || states[i] == "accepted":
			w.Spent, w.Expected = measured*sl, pred.Mean*sl
		default:
			w.Expected = pred.Mean * sl
		}
		w.Spent, w.Expected = math.Round(w.Spent), math.Round(w.Expected)
		out[i] = w
	}
	return out, runRem
}

// runningRemainder is the estimator's expected remainder of the job's
// running units, in slot-seconds.
func runningRemainder(m *model, r *jobRec, e *estimate.Estimator) float64 {
	total := 0.0
	for _, p := range m.jobPods(r.key) {
		i, err := strconv.Atoi(p.Labels[c4v1.LabelUnit])
		if err != nil || p.Status.StartTime == nil || i < 0 || i >= len(r.plan.Units) {
			continue
		}
		u := r.plan.Units[i]
		rem, _ := e.Remainder(e.Predict(u.Key, u.Pos), 0, math.Max(0, m.at.Sub(p.Status.StartTime.Time).Seconds()))
		total += rem * float64(podSlots(p))
	}
	return total
}

// displayBatch is axis.batch: frames per display batch, the render
// class's chunk when it is above 1, else 24 frames (one second of screen
// time, DESIGN.md section 10.15).
func displayBatch(r *jobRec) int {
	if r.job.Spec.Class == "render" {
		if c, err := strconv.Atoi(r.job.Spec.Params["chunk"]); err == nil && c > 1 {
			return c
		}
	}
	return 24
}
