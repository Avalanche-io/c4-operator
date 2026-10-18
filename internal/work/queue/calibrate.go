package queue

import (
	"math"
	"time"

	"github.com/Avalanche-io/c4-operator/work/estimate"
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
// IDs. Calibrations are in memory: after a restart they refill from the
// jobs that finish, and until a stratum has estimate.Calibration.Min
// scores the as-built percentiles apply.

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
// count.
func (m *jobMem) notePoint(now time.Time, finished, total int, s estimate.Summary) {
	if finished < 0 || total <= 0 || finished >= total || len(m.calPts) >= calPointsMax {
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
	}
}
