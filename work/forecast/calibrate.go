package forecast

import (
	"math"
	"sort"
	"sync"
	"time"
)

// FinishCalibration makes a band's percentiles true for the finish
// itself, not only for the remaining work.
//
// A calibrated W90 (estimate.Calibration) covers the work; the finish
// also depends on the plan, which assumes the job gets its planned share
// of a fluid schedule. Under contention that assumption is optimistic:
// hack/sim's ETA experiment found the displayed P90 covering about half of
// finishes with W calibrated. FinishCalibration records, for finished jobs,
// how the actual remaining time compared with the displayed median,
//
//	score = log(actual remaining time / (P50 - now))
//
// and gives each percentile q as now + (P50 - now) * exp(Q_q), Q_q the
// split-conformal quantile of the scores of jobs of the same class at the
// same stage (how much of the job had finished). The P50 itself moves by
// Q_0.5, which removes the plan's bias.
type FinishCalibration struct {
	// Min is the fewest scores a key needs before it is used.
	Min int

	mu    sync.Mutex
	cap   int
	rings map[FinishKey]*scoreRing
}

// FinishKey is what a finish score is kept under. Every score is also
// kept under its class at AnyStage, the fallback for a stage with too few.
type FinishKey struct {
	Class string
	Stage int8
}

// AnyStage marks the fallback key of a class.
const AnyStage int8 = -1

// NewFinishCalibration keeps the most recent capacity scores per key
// (0 keeps every score).
func NewFinishCalibration(capacity int) *FinishCalibration {
	return &FinishCalibration{Min: 100, cap: capacity, rings: map[FinishKey]*scoreRing{}}
}

// StageOf is the stage of a job with finished of total units: 0 nothing
// finished, 1 under a quarter, 2 under three quarters, 3 from there on.
func StageOf(finished, total int) int8 {
	switch {
	case finished <= 0 || total <= 0:
		return 0
	case 4*finished < total:
		return 1
	case 4*finished < 3*total:
		return 2
	}
	return 3
}

// FinishScore is the score of a band whose median was p50 at now, for a
// job that finished at finish.
func FinishScore(now, p50, finish time.Time) float64 {
	return math.Log(math.Max(finish.Sub(now).Seconds(), 1) / math.Max(p50.Sub(now).Seconds(), 1))
}

// Add records a score.
func (c *FinishCalibration) Add(k FinishKey, score float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range []FinishKey{k, {k.Class, AnyStage}} {
		r := c.rings[key]
		if r == nil {
			r = &scoreRing{}
			c.rings[key] = r
		}
		r.add(score, c.cap)
		if k.Stage == AnyStage {
			break
		}
	}
}

// Apply returns the calibrated band, or b unchanged when the key has
// fewer than Min scores (or c is nil).
func (c *FinishCalibration) Apply(k FinishKey, now time.Time, b Band) Band {
	if c == nil || b.IsZero() {
		return b
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rings[k]
	if r == nil || len(r.v) < max(1, c.Min) {
		r = c.rings[FinishKey{k.Class, AnyStage}]
	}
	if r == nil || len(r.v) < max(1, c.Min) {
		return b
	}
	left := math.Max(b.P50.Sub(now).Seconds(), 1)
	at := func(q float64) time.Time {
		return now.Add(time.Duration(left * math.Exp(r.quantile(q)) * float64(time.Second)))
	}
	out := Band{P50: at(0.5), P80: at(0.8), P90: at(0.9), P95: at(0.95)}
	return out
}

// Count returns how many scores a key holds.
func (c *FinishCalibration) Count(k FinishKey) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.rings[k]; r != nil {
		return len(r.v)
	}
	return 0
}

type scoreRing struct {
	v      []float64
	next   int
	sorted []float64
}

func (r *scoreRing) add(x float64, capacity int) {
	r.sorted = nil
	if capacity <= 0 || len(r.v) < capacity {
		r.v = append(r.v, x)
		return
	}
	r.v[r.next] = x
	r.next = (r.next + 1) % capacity
}

// quantile is the finite-sample conformal quantile: the ceil((n+1)q)-th
// smallest score, the largest when that rank exceeds n.
func (r *scoreRing) quantile(q float64) float64 {
	if r.sorted == nil {
		r.sorted = append([]float64(nil), r.v...)
		sort.Float64s(r.sorted)
	}
	s := r.sorted
	k := int(math.Ceil(float64(len(s)+1)*q)) - 1
	return s[min(max(k, 0), len(s)-1)]
}
