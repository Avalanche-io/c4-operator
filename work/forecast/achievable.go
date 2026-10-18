package forecast

import "time"

// FirstAchievable returns the earliest deadline in [lo, hi] for which ok
// reports true, to within tol, assuming ok is monotone (false before some
// time, true from it on). It returns hi when ok(hi) is false, and lo when
// ok(lo) is true.
//
// The caller's ok typically runs the scheduling policy on a copy of the
// farm with the job's deadline set to the candidate, and reports whether
// the job finishes by it at the policy quantile while every job that is on
// time now stays on time: the earliest deadline the job can have without
// bumping anyone. With tol of a minute over a day the policy runs about
// thirteen times.
func FirstAchievable(lo, hi time.Time, tol time.Duration, ok func(time.Time) bool) time.Time {
	if tol <= 0 {
		tol = time.Minute
	}
	if !hi.After(lo) || ok(lo) {
		return lo
	}
	if !ok(hi) {
		return hi
	}
	for hi.Sub(lo) > tol {
		mid := lo.Add(hi.Sub(lo) / 2)
		if ok(mid) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}
