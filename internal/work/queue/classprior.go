package queue

import (
	"math"
	"sort"

	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// The farm's prior for work with no history of its own. A job with no
// estimateSeconds, no shot profile and no derivation of its units starts
// from its class's default (render ten minutes a frame, with as much
// spread), which is a guess. Once units of the class have run in the
// project, what they took is a better one: the median of the class's last
// classDurKept unit durations there, with a robust spread (the median
// absolute deviation, scaled to a standard deviation, and at least a
// quarter of the median). A project that shares its history in aggregate
// (Project spec.work.shareHistory) also lends its durations to the
// farm-wide prior, used where its own project has none yet. The window is
// saved with the project's calibrations, so it survives a restart.

const (
	classDurKept = 256
	classDurMin  = 3
)

// classDurations are a project's recent unit durations, by class.
type classDurations map[string][]float64

// noteDuration keeps one unit's run time, in seconds.
func (q *Queue) noteDuration(project, class string, seconds float64) {
	if seconds <= 0 || class == "" {
		return
	}
	if q.durations == nil {
		q.durations = map[string]classDurations{}
	}
	d := q.durations[project]
	if d == nil {
		d = classDurations{}
		q.durations[project] = d
	}
	w := append(d[class], seconds)
	if len(w) > classDurKept {
		w = w[len(w)-classDurKept:]
	}
	d[class] = w
	q.calp.touch(project)
}

// classPrior is the prior for one unit of class in project, from the
// project's durations, else the farm's shared ones; false when too few
// units have run.
func (q *Queue) classPrior(project, class string) (estimate.Prior, bool) {
	if p, ok := durationPrior(q.durations[project][class]); ok {
		return p, true
	}
	var shared []float64
	for proj, d := range q.durations {
		if proj != project && q.shareHist[proj] {
			shared = append(shared, d[class]...)
		}
	}
	return durationPrior(shared)
}

func durationPrior(xs []float64) (estimate.Prior, bool) {
	if len(xs) < classDurMin {
		return estimate.Prior{}, false
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	med := median(s)
	dev := make([]float64, len(s))
	for i, x := range s {
		dev[i] = math.Abs(x - med)
	}
	sort.Float64s(dev)
	sd := math.Max(1.4826*median(dev), 0.25*med)
	return estimate.Prior{Mean: med, SD: sd}, true
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
