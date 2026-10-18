package forecast

import (
	"cmp"
	"math"
	"slices"
)

// FinishCalibrationSnapshot is a FinishCalibration's scores in a form
// that can be stored and restored: every key it keeps, the AnyStage
// fallbacks included, with its scores oldest first. Keys are in a fixed
// order, so the same scores make the same JSON.
type FinishCalibrationSnapshot struct {
	Keys []FinishScores `json:"keys"`
}

// FinishScores is one key's scores, oldest first.
type FinishScores struct {
	Class  string    `json:"class"`
	Stage  int8      `json:"stage"`
	Scores []float64 `json:"scores"`
}

// Key is the key the scores are kept under.
func (s FinishScores) Key() FinishKey { return FinishKey{s.Class, s.Stage} }

// Snapshot returns the calibration's scores. Min is not part of it.
func (c *FinishCalibration) Snapshot() FinishCalibrationSnapshot {
	var out FinishCalibrationSnapshot
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, r := range c.rings {
		if v := r.ordered(); len(v) > 0 {
			out.Keys = append(out.Keys, FinishScores{k.Class, k.Stage, v})
		}
	}
	slices.SortFunc(out.Keys, func(a, b FinishScores) int {
		return cmp.Or(cmp.Compare(a.Class, b.Class), cmp.Compare(a.Stage, b.Stage))
	})
	return out
}

// RestoreFinishCalibration returns a calibration holding a snapshot's
// scores, keeping the most recent capacity per key (0 keeps every score).
// A key listed more than once keeps its lists in order, so concatenating
// an older snapshot's keys before a newer one's merges the two. Scores
// that are not finite are dropped.
func RestoreFinishCalibration(s FinishCalibrationSnapshot, capacity int) *FinishCalibration {
	c := NewFinishCalibration(capacity)
	for _, ks := range s.Keys {
		k := ks.Key()
		r := c.rings[k]
		if r == nil {
			r = &scoreRing{}
			c.rings[k] = r
		}
		for _, x := range ks.Scores {
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
func (r *scoreRing) ordered() []float64 {
	out := make([]float64, 0, len(r.v))
	for _, x := range append(r.v[r.next:len(r.v):len(r.v)], r.v[:r.next]...) {
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			out = append(out, x)
		}
	}
	return out
}
