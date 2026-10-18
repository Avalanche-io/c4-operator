package estimate

import (
	"cmp"
	"math"
	"slices"
)

// CalibrationSnapshot is a Calibration's scores in a form that can be
// stored and restored: every stratum it keeps, the coarsened ones
// included, with its scores oldest first. It holds scores only, no names,
// inputs or IDs. Strata are in a fixed order, so the same scores make the
// same JSON.
type CalibrationSnapshot struct {
	Strata []StratumScores `json:"strata"`
}

// StratumScores is one stratum's scores, oldest first. A band of -1 is a
// coarsened (any) field.
type StratumScores struct {
	Sampled  int8      `json:"sampled"`
	Measured int8      `json:"measured"`
	Open     int8      `json:"open"`
	Scores   []float64 `json:"scores"`
}

// Stratum is the stratum the scores are kept under.
func (s StratumScores) Stratum() Stratum { return Stratum{s.Sampled, s.Measured, s.Open} }

// Snapshot returns the calibration's scores. Min and Fallback are not
// part of it.
func (c *Calibration) Snapshot() CalibrationSnapshot {
	var out CalibrationSnapshot
	if c == nil {
		return out
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for k, r := range c.rings {
		if v := r.ordered(); len(v) > 0 {
			out.Strata = append(out.Strata, StratumScores{k.Sampled, k.Measured, k.Open, v})
		}
	}
	slices.SortFunc(out.Strata, func(a, b StratumScores) int {
		return cmp.Or(cmp.Compare(a.Sampled, b.Sampled), cmp.Compare(a.Measured, b.Measured), cmp.Compare(a.Open, b.Open))
	})
	return out
}

// RestoreCalibration returns a calibration holding a snapshot's scores,
// keeping the most recent capacity per stratum (0 keeps every score). A
// stratum listed more than once keeps its lists in order, as if each had
// been added after the one before, so concatenating an older snapshot's
// strata before a newer one's merges the two. Scores that are not finite
// are dropped.
func RestoreCalibration(s CalibrationSnapshot, capacity int) *Calibration {
	c := NewCalibration(capacity)
	for _, st := range s.Strata {
		k := st.Stratum()
		r := c.rings[k]
		if r == nil {
			r = &ring{}
			c.rings[k] = r
		}
		for _, x := range st.Scores {
			if !math.IsNaN(x) && !math.IsInf(x, 0) {
				r.add(x, capacity)
			}
		}
	}
	for k, r := range c.rings {
		if len(r.v) == 0 {
			delete(c.rings, k)
		}
	}
	return c
}

// ordered returns the ring's scores oldest first, without those that
// could not be written as JSON.
func (r *ring) ordered() []float64 {
	out := make([]float64, 0, len(r.v))
	for _, x := range append(r.v[r.next:len(r.v):len(r.v)], r.v[:r.next]...) {
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			out = append(out, x)
		}
	}
	return out
}
