package main

import (
	"math"

	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// calibratedW90 (-calibrated) calibrates every job's W90 with conformal
// scores (estimate.Calibration) per job kind and stratum, as the queue
// does live, but fed by the simulator's truth (see calibrate). Until a
// stratum holds enough scores (Calibration.Min) W90 stays uncalibrated,
// as on a new farm.
var calibratedW90 bool

// calEvery is how often a job's estimate is sampled for scoring.
const calEvery = 600.0

func (e *engine) initCalibration() {
	if calibratedW90 {
		e.cal = map[string]*estimate.Calibration{}
	}
}

// trueDone is the job's work finished so far in true slot-seconds: done
// units, and running units' progress.
func (e *engine) trueDone(j *job) float64 {
	d := 0.0
	for _, u := range j.units {
		sl := float64(u.spec.slots)
		switch u.state {
		case stDone:
			d += u.spec.dur * sl
		case stRunning:
			el := math.Max(0, e.now-u.start-u.restore)
			d += math.Min(u.spec.dur, u.retained+el) * sl
		}
	}
	return d
}

func calStratum(j *job) estimate.Stratum {
	share := float64(j.done) / math.Max(1, float64(len(j.units)))
	var b int8
	switch {
	case share >= 0.75:
		b = 4
	case share >= 0.5:
		b = 3
	case share >= 0.25:
		b = 2
	case share >= 1.0/16:
		b = 1
	}
	return estimate.Stratum{Sampled: b}
}

// calibrate replaces s's W90 with the calibrated one when the job's
// stratum has enough scores, and every calEvery scores the moment: the
// simulator knows the job's true remaining work now (its units' true
// durations less the work done), so the score enters the calibration at
// once. That is calibration that works: no censoring of long jobs still
// running, an upper bound on what the queue's live calibration (scores
// only from finished jobs) achieves.
func (e *engine) calibrate(j *job, s *estimate.Summary) {
	if e.cal == nil {
		return
	}
	st := calStratum(j)
	c := e.cal[j.spec.kind]
	if c == nil {
		c = estimate.NewCalibration(calKeep)
		e.cal[j.spec.kind] = c
	}
	if q, ok := e.calQuantile(j.spec.kind, c, st); ok {
		s.W90 = math.Max(s.W, s.W+q*estimate.Scale(*s))
		s.Calibrated = true
	}
	if j.calAt > 0 && e.now-j.calAt < calEvery {
		return
	}
	j.calAt = e.now
	total := 0.0
	for _, u := range j.units {
		total += u.spec.dur * float64(u.spec.slots)
	}
	actual := total - e.trueDone(j)
	c.Add(st, estimate.Score(*s, actual))
	e.res.W90Moments++
	if actual <= s.W90 {
		e.res.W90Covered++
	}
	if s.Calibrated {
		e.res.W90CalMoments++
		if actual <= s.W90 {
			e.res.W90CalCovered++
		}
	}
}

// calKeep is how many recent scores a calibration keeps per stratum.
const calKeep = 2000

type calQ struct {
	q, at float64
	ok    bool
}

// calQuantile is c's 90th-percentile score for st, recomputed at most
// every calEvery: every Add invalidates the calibration's sorted copy, and
// sorting it at every estimate made calibrated runs several times slower.
func (e *engine) calQuantile(kind string, c *estimate.Calibration, st estimate.Stratum) (float64, bool) {
	if e.calQs == nil {
		e.calQs = map[string]map[estimate.Stratum]calQ{}
	}
	m := e.calQs[kind]
	if m == nil {
		m = map[estimate.Stratum]calQ{}
		e.calQs[kind] = m
	}
	if x, hit := m[st]; hit && e.now-x.at < calEvery {
		return x.q, x.ok
	}
	q, ok := c.Quantile(st, 0.9)
	m[st] = calQ{q: q, at: e.now, ok: ok}
	return q, ok
}
