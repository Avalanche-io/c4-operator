package estimate

import (
	"math"
	"sort"
	"sync"
)

// Calibration makes the stated percentiles of remaining work true.
//
// The region model's sigma is not a calibrated spread: W + 1.28 sigma
// covered the true remaining work 88% of the time on plain shots in
// hack/sim, but 68 to 83% on shots with a heavy region, because work that
// has not been sampled yet has no term in sigma. Calibration replaces the
// fixed multiplier with split-conformal quantiles of residuals measured
// on finished jobs:
//
//	score = (actual remaining work - W) / scale,    scale = max(Sigma, W/100, 1)
//	Wq    = W + Q_q(scores of the stratum) * scale
//
// Q_q is the finite-sample conformal quantile (the ceil((n+1)q)-th
// smallest score), so Wq covers the actual remaining work at least a
// share q of the time whenever new jobs resemble the finished ones.
//
// Coverage holds per stratum: within the moments that look alike to the
// estimator. A Stratum is built only from what the estimator sees (the
// share of units finished, the heavy multiple measured on finished units,
// and running units that have outlasted that multiple), so a job that
// looks heavy is calibrated against jobs that looked heavy. Coverage for
// a true heavy multiple the estimator cannot see yet (a heavy unit still
// running) is a property of the population, not of the stratum: until
// one heavy unit finishes, a 3x and an 8x region look the same.
type Calibration struct {
	// Min is the fewest scores a stratum needs before its own quantiles
	// are used. A stratum with fewer falls back to the stratum with the
	// running units ignored, then to the share-finished band alone, then
	// to the as-built normal quantiles.
	Min int

	// Fallback, when set, is consulted when no stratum of this
	// calibration has Min scores: a farm-wide calibration that projects
	// opt in to, behind each project's own.
	Fallback *Calibration

	mu    sync.RWMutex
	cap   int
	rings map[Stratum]*ring
}

// NewCalibration returns an empty calibration that keeps the most recent
// capacity scores per stratum (0 keeps every score).
func NewCalibration(capacity int) *Calibration {
	return &Calibration{Min: 200, cap: capacity, rings: map[Stratum]*ring{}}
}

// Stratum is what the estimator can see of a job's remaining work at one
// moment. It is the key under which calibration scores are kept.
type Stratum struct {
	// Sampled is the band of the share of units finished: 0 below 1/16,
	// 1 below 1/4, 2 below 1/2, 3 below 3/4, 4 from 3/4 on.
	Sampled int8
	// Measured is the band of the typical heavy multiple measured on
	// finished units (see baseAndTypical): 0 none, 1 a mild region
	// (below HeavyFactor), then 2 below 4.2x, 3 below 6x, 4 below 8.5x,
	// 5 from 8.5x on.
	Measured int8
	// Open is the band of the longest-running unit that has outlasted
	// what is measured (elapsed time over the key's base cost above both
	// 1.5 and 1.2 times the measured multiple): 0 none, 1 below 3x,
	// 2 below 6x, 3 from 6x on. A running unit is censored: its multiple
	// is at least this, and how much more is unknown until it finishes.
	Open int8
}

// Any marks a coarsened stratum field.
const anyBand = -1

var (
	sampledEdges  = []float64{1.0 / 16, 1.0 / 4, 1.0 / 2, 3.0 / 4}
	measuredEdges = []float64{3, 3.75, 4.7, 5.9, 7.3, 9.2}
	openEdges     = []float64{3, 6}
)

func bandOf(x float64, edges []float64) int8 {
	return int8(sort.SearchFloat64s(edges, math.Nextafter(x, math.Inf(1))))
}

// coarser returns the strata consulted for s, finest first.
func (s Stratum) coarser() [3]Stratum {
	return [3]Stratum{s, {s.Sampled, s.Measured, anyBand}, {s.Sampled, anyBand, anyBand}}
}

// Scale is the normalizer of a summary's calibration scores.
func Scale(s Summary) float64 { return math.Max(s.Sigma, math.Max(s.W/100, 1)) }

// Score is the calibration score of a summary whose job, from the moment
// of the summary on, turned out to need actual slot-seconds.
func Score(s Summary, actual float64) float64 { return (actual - s.W) / Scale(s) }

// Add records a finished job's score at one of its summaries.
func (c *Calibration) Add(s Stratum, score float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range s.coarser() {
		r := c.rings[k]
		if r == nil {
			r = &ring{}
			c.rings[k] = r
		}
		r.add(score, c.cap)
	}
}

// Quantile returns the conformal q-quantile of the scores of s or of the
// finest coarser stratum with at least Min scores, then the same from
// Fallback, and whether one was found.
func (c *Calibration) Quantile(s Stratum, q float64) (float64, bool) {
	if x, ok := c.quantile(s, q); ok {
		return x, true
	}
	if c.Fallback != nil && c.Fallback != c {
		return c.Fallback.quantile(s, q)
	}
	return 0, false
}

func (c *Calibration) quantile(s Stratum, q float64) (float64, bool) {
	c.mu.Lock() // quantile caches a sorted copy
	defer c.mu.Unlock()
	for _, k := range s.coarser() {
		if r := c.rings[k]; r != nil && len(r.v) >= max(1, c.Min) {
			return r.quantile(q), true
		}
	}
	return 0, false
}

// Count returns how many scores stratum s holds itself.
func (c *Calibration) Count(s Stratum) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if r := c.rings[s]; r != nil {
		return len(r.v)
	}
	return 0
}

type ring struct {
	v      []float64
	next   int
	sorted []float64
}

func (r *ring) add(x float64, capacity int) {
	r.sorted = nil
	if capacity <= 0 || len(r.v) < capacity {
		r.v = append(r.v, x)
		return
	}
	r.v[r.next] = x
	r.next = (r.next + 1) % capacity
}

// quantile is the ceil((n+1)q)-th smallest score, the largest when that
// rank exceeds n.
func (r *ring) quantile(q float64) float64 {
	if r.sorted == nil {
		r.sorted = append([]float64(nil), r.v...)
		sort.Float64s(r.sorted)
	}
	s := r.sorted
	k := int(math.Ceil(float64(len(s)+1)*q)) - 1
	return s[min(max(k, 0), len(s)-1)]
}

// stratum computes what the estimator can see, given the units still to
// do. finished is the number of completed units.
func (e *Estimator) stratum(queued []Unit, run []RunningUnit) Stratum {
	finished := len(e.all)
	total := finished + len(queued) + len(run)
	var st Stratum
	if total > 0 {
		st.Sampled = bandOf(float64(finished)/float64(total), sampledEdges)
	}
	hf := e.cfg.HeavyFactor
	if hf <= 0 {
		hf = 3
	}
	base := map[string]float64{}
	typical := map[string]float64{}
	for k, t := range e.tracks {
		if len(t.obs) == 0 || isRatioKey(k) {
			continue
		}
		b, f := baseAndTypical(t, hf)
		base[k] = b
		if f > 0 {
			typical[k] = f
			st.Measured = max(st.Measured, 1+bandOf(f, measuredEdges))
		}
	}
	for _, r := range run {
		b := base[r.Key]
		if b <= 0 {
			continue
		}
		x := (r.Done + r.Elapsed) / b
		if x < openFrom || x < 0.9*typical[r.Key] {
			continue
		}
		st.Open = max(st.Open, 1+bandOf(x, openEdges))
	}
	return st
}

// mildFactor is the multiple of the base cost above which finished units
// may form a mild heavy region (below HeavyFactor), and openFrom the
// multiple a running unit must reach to count as outlasting the base.
const (
	mildFactor = 1.8
	openFrom   = 1.5
)

// baseAndTypical returns a key's base cost, the median of its finished
// units that are not heavy (at most mildFactor times the median of all of
// them, so a heavy region does not raise it), and the typical multiple of
// its heavy units over that base: the median over the units above
// HeavyFactor times the base, or, when there are none, over the units
// above mildFactor times the base if they are at least 3 and at least 2%
// of the finished units (fewer is the tail of plain units). It is 0 when
// no heavy units are measured.
func baseAndTypical(t *track, hf float64) (float64, float64) {
	m := t.median()
	var plain, mild, strong []float64
	for _, o := range t.obs {
		if o.dur <= mildFactor*m {
			plain = append(plain, o.dur)
		}
	}
	sort.Float64s(plain)
	b := median(plain)
	if b <= 0 {
		return m, 0
	}
	for _, o := range t.obs {
		switch x := o.dur / b; {
		case x > hf:
			strong = append(strong, x)
		case x > mildFactor:
			mild = append(mild, x)
		}
	}
	heavy := strong
	if len(heavy) == 0 {
		if len(mild) < max(3, len(t.obs)/50) {
			return b, 0
		}
		heavy = mild
	}
	sort.Float64s(heavy)
	return b, median(heavy)
}
