package queue

import (
	"math"
	"sort"
	"strconv"

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
//
// The window is the key's, shared by every block of the environment and
// tool: it is evidence about them all. A lead's estimate is not. A lead's
// workers share their tool's unit key, so a key's first worker's work
// priced every worker of the key, the large refactor and the small docs
// fix alike, and so did the forecast of each plan call. A unit that
// carries its own work, where its environment and tool have no history
// yet, is priced under a key of its own (workKey) by that work; its run
// time is still kept under its environment's key, and once that has
// history the unit is priced by it, under the shared key again.

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
// the farm's shared one), when enough of it has run. It returns, by
// index, the keys of their own (workKey) it gives two kinds of unit: one
// that carries its own work where its environment has no such history,
// and one whose key another environment's history prices already. The
// rest keep their keys, so a job whose environment has history, or that
// is not agent work, adds none. A generator keeps its key, its fan-out being
// its key's, and so does a unit with a profile of its shot, or whose key
// the job's estimateSeconds prices.
func (q *Queue) agentPriors(e *estimate.Estimator, v *view, hasPrior map[string]bool, from map[string]string, profiled map[string]map[int]float64) map[int]string {
	var keys map[int]string
	own := func(i int, u class.Unit) string {
		if keys == nil {
			keys = map[int]string{}
		}
		keys[i] = workKey(u.Key, i)
		return keys[i]
	}
	// histOf is the environment whose history prices each unit key, and
	// hist each environment's history, looked up once.
	histOf := map[string]string{}
	type found struct {
		p  estimate.Prior
		ok bool
	}
	hist := map[string]found{}
	// A unit's environment key is its job's but for the unit's own
	// C4_AGENT_ENV, and none for a member's unit (agentKey): made once for
	// each version, not once a unit.
	envKeys := map[string]string{}
	for i, u := range v.mem.plan.Units {
		if from[u.Key] == FromEstimate {
			continue
		}
		version, member := "", false
		for _, ev := range u.Env {
			switch {
			case ev.Name == "C4_AGENT_ENV" && ev.Value != "":
				version = ev.Value
			case ev.Name == "C4_AGENT_MEMBER" && ev.Value != "":
				member = true
			}
		}
		k, seen := envKeys[version]
		if !seen && !member {
			k = agentKey(v.job, u)
			envKeys[version] = k
		}
		if member || k == "" || histOf[u.Key] == k {
			continue
		}
		h, seen := hist[k]
		if !seen {
			h.p, h.ok = q.classPrior(v.project, k)
			hist[k] = h
		}
		switch {
		case h.ok && !hasPrior[u.Key]:
			e.SetPrior(u.Key, h.p)
			hasPrior[u.Key], from[u.Key], histOf[u.Key] = true, FromAgentEnv, k
		case u.Generator || profiled[u.Key] != nil:
		case h.ok:
			// Its key is another environment's: its own, priced by its own.
			key := own(i, u)
			e.SetPrior(key, h.p)
			hasPrior[key], from[key] = true, FromAgentEnv
		case u.Work > 0:
			own(i, u)
		}
	}
	return keys
}

// workKey is unit idx's key of its own: its unit key and its index, which
// no unit key spells.
func workKey(key string, idx int) string { return key + "\x00work/" + strconv.Itoa(idx) }

// estKey is the key the estimator prices unit idx of the job under, as
// the estimate last left it: its unit key, or its own (agentPriors).
func (v *view) estKey(idx int) string {
	if k, ok := v.workKeys[idx]; ok {
		return k
	}
	return v.mem.plan.Units[idx].Key
}
