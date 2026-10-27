package queue

import (
	"math"
	"sort"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
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

// fixture reports whether a job is the farm's test of a published tool
// (c4v1.LabelToolTest). Its units run the tool's own small fixture (two
// frames of a test stage at 64 pixels wide, say), so the queue learns
// nothing from them: kept, the tests c4 farm up runs (every frames tool
// lowers to the render class) became the render class's history, and a
// production render of 48 frames was planned at their few seconds a
// frame, ready at once.
func fixture(j *c4v1.WorkJob) bool { return j.Labels[c4v1.LabelToolTest] != "" }

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

// Agent work has no shot: its history is its agent environment's. A
// block's run time is kept under the version of the agent environment it
// runs in (C4_AGENT_ENV, its own, else its job's) and the tool that runs
// it (C4_BLOCK_PROFILE), in the same window as a class's, and saved with
// it; the farm's tool tests leave none, as they leave no class history.
// A unit of agent work with no history of its own in its job is
// estimated from that window first, then from its lead's estimate (its
// work), then from its class's.

// agentKeyPrefix marks a window of agent work among the classes'.
const agentKeyPrefix = "agent-env/"

// agentKey is the history key of unit u of job: its agent environment's
// version and its tool, or "" for a unit that runs no block. A block
// whose job names no environment runs the release's base, keyed "base".
func agentKey(job *c4v1.WorkJob, u class.Unit) string {
	if unitAgent(job.Spec.Template.Spec, u) != "block" {
		return ""
	}
	version, tool := "", ""
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			switch e.Name {
			case "C4_AGENT_ENV":
				version = e.Value
			case "C4_BLOCK_PROFILE":
				tool = e.Value
			}
		}
	}
	for _, e := range u.Env {
		if e.Name == "C4_AGENT_ENV" && e.Value != "" {
			version = e.Value
		}
	}
	if version == "" {
		version = "base"
	}
	if tool == "" {
		tool = job.Spec.Params["key"]
	}
	return agentKeyPrefix + version + "/" + tool
}

// agentPriors gives each key of the job's agent work that has no prior
// yet the history of its agent environment and tool in the project (else
// the farm's shared one), when enough of it has run.
func (q *Queue) agentPriors(e *estimate.Estimator, v *view, hasPrior map[string]bool, from map[string]string) {
	for _, u := range v.mem.plan.Units {
		if hasPrior[u.Key] {
			continue
		}
		k := agentKey(v.job, u)
		if k == "" {
			continue
		}
		if p, ok := q.classPrior(v.project, k); ok {
			e.SetPrior(u.Key, p)
			hasPrior[u.Key], from[u.Key] = true, FromAgentEnv
		}
	}
}
