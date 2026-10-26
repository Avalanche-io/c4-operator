package queue

import (
	"math"
	"time"

	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/forecast"
)

// Live calibration (estimate.Calibration). While a job runs, the queue
// keeps the summaries it planned on at a bounded set of moments. When the
// job succeeds, the work its units actually did after each moment is
// known, and each moment becomes a calibration score: how far the true
// remaining work fell from W, in units of the summary's scale. The next
// jobs of the same project and class read their W percentiles from those
// scores.
//
// Scores stay with their project. A project with shareHistory
// "aggregate" also adds its scores to a farm-wide calibration per class,
// which every opted-in project consults behind its own once at least
// farmMinProjects projects contribute. Scores carry no names, inputs or
// IDs. Each project's own calibrations are saved in its store and
// restored after a restart (persist.go); until a stratum has
// estimate.Calibration.Min scores the as-built percentiles apply.

const (
	// calCapacity is how many recent scores each stratum keeps.
	calCapacity = 500
	// farmMinProjects is how many projects must contribute before the
	// farm-wide calibration is consulted.
	farmMinProjects = 3
	// calPointsMax bounds the moments kept per job.
	calPointsMax = 80
)

// calPoint is one moment of a running job: the summary the queue planned
// on and how many units had finished.
type calPoint struct {
	at       time.Time
	finished int
	sum      estimate.Summary
}

type calibrations struct {
	own    map[[2]string]*estimate.Calibration // by project, class
	farm   map[string]*estimate.Calibration    // by class
	farmBy map[string]map[string]bool          // class: contributing projects
}

// forJob returns the calibration a job of project and class reads.
func (c *calibrations) forJob(project, class string, share bool) *estimate.Calibration {
	if c.own == nil {
		c.own = map[[2]string]*estimate.Calibration{}
		c.farm = map[string]*estimate.Calibration{}
		c.farmBy = map[string]map[string]bool{}
	}
	k := [2]string{project, class}
	cal := c.own[k]
	if cal == nil {
		cal = estimate.NewCalibration(calCapacity)
		c.own[k] = cal
	}
	cal.Fallback = nil
	if share && len(c.farmBy[class]) >= farmMinProjects {
		cal.Fallback = c.farm[class]
	}
	return cal
}

// add records a score for project and class.
func (c *calibrations) add(project, class string, share bool, st estimate.Stratum, score float64) {
	c.forJob(project, class, share).Add(st, score)
	if !share {
		return
	}
	f := c.farm[class]
	if f == nil {
		f = estimate.NewCalibration(calCapacity)
		c.farm[class] = f
		c.farmBy[class] = map[string]bool{}
	}
	f.Add(st, score)
	c.farmBy[class][project] = true
}

// keepPoint reports whether the moment with finished units of total
// should be kept: every count up to 33 (the binary-order levels that move
// an estimate most), then about every 1/32 of the job.
func keepPoint(finished, total int) bool {
	if finished <= 33 {
		return true
	}
	step := max(1, total/32)
	return finished%step == 0
}

// notePoint keeps the job's summary at this moment, once per finished
// count. A summary that rests on the class default (NoHistory) is not
// kept: how far a guess was off says nothing of the estimator's spread.
func (m *jobMem) notePoint(now time.Time, finished, total int, s estimate.Summary) {
	if finished < 0 || total <= 0 || finished >= total || len(m.calPts) >= calPointsMax || s.NoHistory {
		return
	}
	if n := len(m.calPts); n > 0 && m.calPts[n-1].finished >= finished {
		return
	}
	if !keepPoint(finished, total) {
		return
	}
	m.calPts = append(m.calPts, calPoint{at: now, finished: finished, sum: s})
}

// remainingAfter is the work, in slot-seconds, that the job's units did
// after t: each unit ran for dur seconds ending at end, on slots slots.
// It reports false if a unit's end time is unknown (it finished before a
// restart), since the moment's true remaining work cannot be known then.
func remainingAfter(t time.Time, ends map[int]time.Time, millis map[int]int64, slots func(int) int) (float64, bool) {
	var w float64
	for i, ms := range millis {
		end, ok := ends[i]
		if !ok {
			return 0, false
		}
		dur := float64(ms) / 1000
		start := end.Add(-time.Duration(ms) * time.Millisecond)
		from := start
		if t.After(from) {
			from = t
		}
		x := math.Min(dur, math.Max(0, end.Sub(from).Seconds()))
		w += x * float64(max(1, slots(i)))
	}
	return w, true
}

// learn turns a succeeded job's kept moments into calibration scores.
func (q *Queue) learn(v *view) {
	m := v.mem
	if m.learned || m.plan == nil {
		return
	}
	m.learned = true
	slots := func(i int) int {
		if i < 0 || i >= len(m.plan.Units) {
			return 1
		}
		return m.plan.Units[i].Slots
	}
	share := q.shareHist[v.project]
	for _, p := range m.calPts {
		actual, ok := remainingAfter(p.at, m.ended, m.millis, slots)
		if !ok {
			return
		}
		q.cal.add(v.project, v.job.Spec.Class, share, p.sum.Stratum, estimate.Score(p.sum, actual))
		q.calp.touch(v.project)
	}
}

// Live finish calibration (forecast.FinishCalibration). The band a person
// is shown comes from the plan, which assumes each job gets its planned
// share; under contention that is optimistic, and calibrating W alone
// does not fix it (hack/sim out/eta.md). So the queue keeps, while a job
// runs, the uncalibrated median it would show at a bounded set of
// moments, and when the job succeeds scores each against the actual
// finish. Bands for jobs of the same project and class are then shown
// calibrated. Sharing follows the same rule as the W calibration.

// finishPointsMax bounds the moments kept per job; finishEvery spaces
// them when the finished count does not move.
const (
	finishPointsMax = 64
	finishEvery     = time.Hour
)

type finishPoint struct {
	at     time.Time
	rawP50 time.Time
	stage  int8
}

type finishCals struct {
	own    map[[2]string]*forecast.FinishCalibration
	farm   map[string]*forecast.FinishCalibration
	farmBy map[string]map[string]bool
}

func (c *finishCals) init() {
	if c.own == nil {
		c.own = map[[2]string]*forecast.FinishCalibration{}
		c.farm = map[string]*forecast.FinishCalibration{}
		c.farmBy = map[string]map[string]bool{}
	}
}

// adjust returns the function that calibrates a band for a job of project
// and class at stage: its project's calibration when it covers the key,
// else the farm's for projects that share, else none.
func (c *finishCals) adjust(project, class string, share bool, stage int8) func(time.Time, forecast.Band) forecast.Band {
	c.init()
	k := forecast.FinishKey{Class: class, Stage: stage}
	if own := c.own[[2]string{project, class}]; own.Covers(k) {
		return func(now time.Time, b forecast.Band) forecast.Band { return own.Apply(k, now, b) }
	}
	if f := c.farm[class]; share && len(c.farmBy[class]) >= farmMinProjects && f.Covers(k) {
		return func(now time.Time, b forecast.Band) forecast.Band { return f.Apply(k, now, b) }
	}
	return nil
}

func (c *finishCals) add(project, class string, share bool, k forecast.FinishKey, score float64) {
	c.init()
	own := c.own[[2]string{project, class}]
	if own == nil {
		own = forecast.NewFinishCalibration(calCapacity)
		c.own[[2]string{project, class}] = own
	}
	own.Add(k, score)
	if !share {
		return
	}
	f := c.farm[class]
	if f == nil {
		f = forecast.NewFinishCalibration(calCapacity)
		c.farm[class] = f
		c.farmBy[class] = map[string]bool{}
	}
	f.Add(k, score)
	c.farmBy[class][project] = true
}

// noteFinish keeps the uncalibrated median shown now, at each new
// finished count or after finishEvery.
func (m *jobMem) noteFinish(now time.Time, finished, total int, rawP50 time.Time) {
	if rawP50.IsZero() || len(m.finPts) >= finishPointsMax || finished >= total {
		return
	}
	if n := len(m.finPts); n > 0 && finished <= m.finLast && now.Sub(m.finPts[n-1].at) < finishEvery {
		return
	}
	m.finLast = finished
	m.finPts = append(m.finPts, finishPoint{at: now, rawP50: rawP50, stage: forecast.StageOf(finished, total)})
}

// learnFinish scores a succeeded job's kept medians against its finish.
func (q *Queue) learnFinish(v *view) {
	m := v.mem
	if m.finLearned || m.finished.IsZero() {
		return
	}
	m.finLearned = true
	share := q.shareHist[v.project]
	for _, p := range m.finPts {
		q.fcal.add(v.project, v.job.Spec.Class, share, forecast.FinishKey{Class: v.job.Spec.Class, Stage: p.stage},
			forecast.FinishScore(p.at, p.rawP50, m.finished))
		q.calp.touch(v.project)
	}
}

// shown is the band a person is shown for a job: the display band,
// finish-calibrated when its project and class have the scores.
func (q *Queue) shown(v *view, now time.Time, raw forecast.Band) forecast.Band {
	if adj := q.adjustFor(v); adj != nil {
		return adj(now, raw)
	}
	return raw
}

// adjustFor is the finish calibration of a job's band, or nil when its
// project and class have too few scores. A calibrated band ends no sooner
// than now plus the job's critical path, as Display's does: every
// remaining unit must finish. Scores far below zero (jobs that finished in
// seconds against minutes predicted) would otherwise show a job of 48
// frames ready now.
func (q *Queue) adjustFor(v *view) func(time.Time, forecast.Band) forecast.Band {
	total := 0
	if v.mem.plan != nil {
		total = len(v.mem.plan.Units)
	}
	adj := q.fcal.adjust(v.project, v.job.Spec.Class, q.shareHist[v.project], forecast.StageOf(len(v.mem.millis), total))
	if adj == nil {
		return nil
	}
	return func(now time.Time, b forecast.Band) forecast.Band {
		out := adj(now, b)
		if out.IsZero() {
			return out
		}
		floor := now.Add(time.Duration(v.sum.TPath * float64(time.Second)))
		for _, t := range []*time.Time{&out.P50, &out.P80, &out.P90, &out.P95} {
			if t.Before(floor) {
				*t = floor
			}
		}
		return out
	}
}
