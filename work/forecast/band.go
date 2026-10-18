package forecast

import (
	"time"

	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// Band is a job's finish forecast: the times by which it finishes with
// probability 50, 80, 90 and 95%. The zero Band means no forecast.
type Band struct {
	P50, P80, P90, P95 time.Time
}

// IsZero reports whether the band holds no forecast.
func (b Band) IsZero() bool { return b.P50.IsZero() }

// At returns the band's time for quantile q, rounded to the nearest
// percentile the band holds.
func (b Band) At(q float64) time.Time {
	switch {
	case q < 0.65:
		return b.P50
	case q < 0.85:
		return b.P80
	case q < 0.925:
		return b.P90
	}
	return b.P95
}

// Width is the spread of the band a person reads, P80 less P50, as a share
// of the forecast run time from start to P50. It is what a revision must
// narrow (Suggestion).
func (b Band) Width(start time.Time) float64 {
	if b.IsZero() {
		return 0
	}
	total := b.P50.Sub(start)
	if total < time.Minute {
		total = time.Minute
	}
	return float64(b.P80.Sub(b.P50)) / float64(total)
}

// Display turns the scheduler's finish times into the band a person is
// shown.
//
// The scheduler's ETA50 and ETA90 are its planned finishes for the job's
// W and W90 (sched reads Summary.W and Summary.W90), with the critical
// path tPath added once as a tail (TailAware). That tail is the right
// margin for deciding who is late and what burst to buy; as the finish
// shown to a person it runs late (eta-deadlines evidence F9: W/P + t_path
// was 26% late as a median, W/P + t_path/2 unbiased). Display therefore:
//
//  1. places each calibrated percentile Wq on the line through (W, ETA50)
//     and (W90, ETA90), so the band follows the calibrated percentiles
//     and the plan's rate for this job alike;
//  2. takes back half the tail, s.TPath/2, but never ends before now
//     plus the whole critical path: every remaining unit must finish, so
//     a job whose units all run in one wave (a CI pipeline, a short
//     preview) finishes no sooner than its longest unit, however small
//     W/C is. (hack/sim's ETA experiment found the half-tail floor
//     halved such jobs' remaining time.)
//
// It returns the zero Band when the scheduler gave no ETA.
func Display(now time.Time, s estimate.Summary, eta50, eta90 time.Time) Band {
	if eta50.IsZero() {
		return Band{}
	}
	if eta90.Before(eta50) {
		eta90 = eta50
	}
	at := func(w float64) time.Time {
		t := eta50
		if s.W90 > s.W && eta90.After(eta50) {
			f := (w - s.W) / (s.W90 - s.W)
			t = eta50.Add(time.Duration(f * float64(eta90.Sub(eta50))))
		}
		half := time.Duration(s.TPath / 2 * float64(time.Second))
		t = t.Add(-half)
		if floor := now.Add(2 * half); t.Before(floor) {
			t = floor
		}
		return t
	}
	w50, w80, w90, w95 := s.W50, s.W80, s.W90, s.W95
	if w50 == 0 && w80 == 0 && w95 == 0 {
		// A Summary built without percentiles (older callers): read the
		// normal quantiles from Sigma.
		w50, w80, w95 = s.W, s.W+0.8416*s.Sigma, s.W+1.6449*s.Sigma
	}
	b := Band{P50: at(w50), P80: at(w80), P90: at(w90), P95: at(w95)}
	// Percentiles never cross.
	if b.P80.Before(b.P50) {
		b.P80 = b.P50
	}
	if b.P90.Before(b.P80) {
		b.P90 = b.P80
	}
	if b.P95.Before(b.P90) {
		b.P95 = b.P90
	}
	return b
}
