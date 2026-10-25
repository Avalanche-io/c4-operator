package queue

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
	"github.com/Avalanche-io/c4-operator/work/forecast"
)

// Usage limits (DESIGN.md, usage limits). A pod block whose turn hits a
// model's usage limit ends with c4v1.UsageLimitExitCode and the limit as
// its termination message, "usage limit: <the CLI's words> (until <RFC
// 3339>)". That is the model's capacity, not the unit's failure: it spends
// no retry and marks no node, like a yield. The unit keeps the view it
// captured and waits until the limit resets (or, when the limit did not
// say, usage.limitProbe), then runs again from that view. The job says so
// (condition UsageLimit), and the wait is a yield and a decision in the
// book. Host runners handle their own limits (no slots until the reset).

type usageRec struct {
	why       string
	at, until time.Time
	probe     bool
	// stated is the reset the unit gave, when it was further off than
	// maxUsageWait and so not taken as given.
	stated time.Time
}

// maxUsageWait bounds how long a stated reset holds a unit: the longest
// window a provider's usage limit runs (a week today). The reset comes
// from the unit's own termination message, so a wrong or far-off one
// would otherwise hold the unit, and show in the forecast, for as long as
// it said.
const maxUsageWait = 7 * 24 * time.Hour

// usageLimitPrefix starts a usage limit's termination message. Exit 75
// alone is EX_TEMPFAIL, which ordinary tools use for "try again later",
// so it is a usage limit only with these words, and only from a model's
// unit (isUsageLimit).
const usageLimitPrefix = "usage limit:"

// usageLimitOf reads a failed pod's usage limit: why, in the limit's
// words, and until when, if it said.
func usageLimitOf(p *corev1.Pod) (why string, until time.Time, ok bool) {
	for _, cs := range p.Status.ContainerStatuses {
		t := cs.State.Terminated
		if t == nil || t.ExitCode != c4v1.UsageLimitExitCode {
			continue
		}
		why = strings.TrimSpace(t.Message)
		if !strings.HasPrefix(why, usageLimitPrefix) {
			continue
		}
		if k := strings.LastIndex(why, " (until "); k >= 0 && strings.HasSuffix(why, ")") {
			if u, err := time.Parse(time.RFC3339, why[k+len(" (until "):len(why)-1]); err == nil {
				until = u
			}
		}
		return why, until, true
	}
	return "", time.Time{}, false
}

// isUsageLimit reports whether unit idx's failed pod hit a model's usage
// limit: only a model's unit can, and only one that says so in the
// limit's words. Anything else that exits 75 is an ordinary failure.
func isUsageLimit(v *view, idx int, p *corev1.Pod) bool {
	if !modelUnit(v, idx) {
		return false
	}
	_, _, ok := usageLimitOf(p)
	return ok
}

// noteUsageLimit gives back unit idx's attempt, whose pod p hit a usage
// limit, keeps the view it captured, and holds the unit until the limit
// resets.
func (q *Queue) noteUsageLimit(ctx context.Context, v *view, idx int, p *corev1.Pod, d class.Driver, now time.Time) {
	m := v.mem
	why, until, _ := usageLimitOf(p)
	if attemptOf(p) == m.attempts[idx] && m.attempts[idx] > 0 {
		m.attempts[idx]--
	}
	tree := p.Annotations[c4v1.AnnotationOutputTree]
	if _, ok := d.(class.Resumer); ok {
		tree = p.Annotations[c4v1.AnnotationOutputCheckpoint]
	}
	if tree != "" {
		m.resume[idx] = tree
	}
	r := &usageRec{why: why, at: finishOf(p, now), until: until}
	if limit := now.Add(maxUsageWait); until.After(limit) {
		r.until, r.stated = limit, until
	}
	var uses []policy.Use
	if until.IsZero() || !until.After(now) {
		probe, use := policy.Duration(v.job, q.projectOf(v), idx, policy.UsageLimitProbe)
		r.until, r.probe, uses = now.Add(probe), true, []policy.Use{use}
	}
	if m.usage == nil {
		m.usage = map[int]*usageRec{}
	}
	m.usage[idx] = r
	m.yields = append(m.yields, yieldRecord{Unit: idx, At: r.at, Reason: why, View: tree})
	q.recordPolicyDecision(ctx, v, idx, "usage-limit", why, now, uses, map[string]any{"until": r.until.UTC().Format(time.RFC3339)})
	q.Log.Info("usage limit", "job", v.key, "unit", idx, "why", why, "until", r.until)
}

// holdUsageLimits keeps the units waiting for a usage limit out of what
// may run, until it resets; then they run again, from their view.
func (v *view) holdUsageLimits(now time.Time) {
	m := v.mem
	for i, r := range m.usage {
		if !r.until.After(now) {
			delete(m.usage, i)
		}
	}
	if len(m.usage) == 0 {
		return
	}
	keep := v.runnable[:0]
	for _, i := range v.runnable {
		if m.usage[i] == nil {
			keep = append(keep, i)
		}
	}
	v.runnable = keep
}

// usageCondition sets or clears the job's UsageLimit condition.
func (q *Queue) usageCondition(st *c4v1.WorkJobStatus, v *view) {
	m := v.mem
	if len(m.usage) == 0 {
		meta.RemoveStatusCondition(&st.Conditions, c4v1.ConditionUsageLimit)
		return
	}
	idx := make([]int, 0, len(m.usage))
	for i := range m.usage {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool { return m.usage[idx[a]].until.Before(m.usage[idx[b]].until) })
	first := m.usage[idx[0]]
	msg := "waiting for the model's usage limit to reset at " + forecast.Clock(first.until, q.Loc)
	if first.probe {
		msg = "waiting for the model's usage limit: trying again at " + forecast.Clock(first.until, q.Loc)
	}
	if !first.stated.IsZero() {
		msg += fmt.Sprintf(" (it said %s, more than %s off, so the wait is held to that)", forecast.Clock(first.stated, q.Loc), "a week")
	}
	if n := len(idx); n > 1 {
		msg = fmt.Sprintf("%s (%d units, the first)", msg, n)
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: c4v1.ConditionUsageLimit, Status: metav1.ConditionTrue,
		Reason: "UsageLimit", Message: msg, ObservedGeneration: v.job.Generation})
}

// usageStatus is status.usageLimits.
func usageStatus(m *jobMem) map[string]c4v1.WorkUsageLimit {
	if len(m.usage) == 0 {
		return nil
	}
	out := map[string]c4v1.WorkUsageLimit{}
	for i, r := range m.usage {
		out[strconv.Itoa(i)] = c4v1.WorkUsageLimit{Why: r.why, At: metav1.Time{Time: r.at.Truncate(time.Second)},
			Until: metav1.Time{Time: r.until.Truncate(time.Second)}, Probe: r.probe}
	}
	return out
}

// loadUsage rebuilds the units held for usage limits from status.
func (m *jobMem) loadUsage(st *c4v1.WorkJobStatus) {
	for k, w := range st.UsageLimits {
		i, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		if m.usage == nil {
			m.usage = map[int]*usageRec{}
		}
		m.usage[i] = &usageRec{why: w.Why, at: w.At.Time, until: w.Until.Time, probe: w.Probe}
	}
}

// backgroundWaitEnv is how long a member's yield waits for its background
// work ($C4_YIELD_BACKGROUND_WAIT, yield.backgroundWait), for units whose
// reasoning is a model's.
func (q *Queue) backgroundWaitEnv(v *view, idx int) []corev1.EnvVar {
	if !modelUnit(v, idx) {
		return nil
	}
	d, _ := policy.Duration(v.job, q.projectOf(v), idx, policy.YieldBackgroundWait)
	return []corev1.EnvVar{{Name: "C4_YIELD_BACKGROUND_WAIT", Value: d.String()}}
}
