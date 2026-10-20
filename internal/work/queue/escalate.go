package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
	"github.com/Avalanche-io/c4-operator/work/forecast"
)

// Escalations (DESIGN.md, missing information). A unit's question to a
// person is an error that can heal. Raised when the unit stops; pressing
// once the plan says an answer now costs the deadline (answerBy, less
// escalation.pressMargin) or it has been open escalation.pressAfter;
// supplied by a person or the job's answerer; withdrawn by the lead, or by
// the block itself on a re-check.
//
// A re-check is an extra attempt of the unit, like a wake (asks.go):
// AnnotationRecheck, $C4_RECHECK, $C4_NEEDED and $C4_RECHECK_WHY, mounting
// the job's inputs and the unit's resume view as they are now. Events
// start it, never a clock: a unit it waits on or reads succeeds, a unit
// or path it is about changes, the unit table grows, or a job it is about
// or reads from finishes. Only units whose reasoning is a model re-check.
// Exit 0 withdraws the question, with the termination message as the why;
// exit NeedsExitCode keeps it (with its new text); anything else keeps it.
// No retry is spent and nothing it writes is kept. At most one runs at a
// time (policy.RecheckInFlight), one per escalation.recheck.minInterval,
// escalation.recheck.max in all.

// recheckSeconds is how long a re-check may take ($C4_RECHECK_SECONDS):
// the runner's limit, not a policy value.
const recheckSeconds = 300

// needsBase is what a question's triggers compare with.
type needsBase struct {
	units  int
	outs   map[int]string
	phases map[string]string
}

// freshPath is a path a unit's changeset wrote, folded this tick.
type freshPath struct {
	unit int
	path string
}

// projectOf is the job's Project, for its policy values.
func (q *Queue) projectOf(v *view) *c4v1.Project {
	return q.projectObjs[v.project]
}

// modelUnit reports whether unit i's reasoning is a model's: only those
// re-check their questions.
func modelUnit(v *view, i int) bool {
	if v.driver == nil || v.mem.plan == nil || i >= len(v.mem.plan.Units) {
		return false
	}
	return v.driver.Name() == "agent" || class.DoesOf(v.driver, v.mem.plan.Units[i]) == class.DoesModel
}

// escalate grades the job's open questions and starts the re-checks their
// triggers call for. It runs after the job is estimated, so answerBy is
// the plan as it is now.
func (q *Queue) escalate(ctx context.Context, v *view, phases map[string]string, now time.Time) {
	m := v.mem
	defer func() { m.out.fresh = nil }()
	if m.plan == nil || len(m.needs) == 0 {
		return
	}
	proj := q.projectOf(v)
	for _, i := range m.openNeedsList() {
		n := m.needs[i]
		n.model = modelUnit(v, i)
		n.recheckMax, _ = policy.Int(v.job, proj, i, policy.RecheckMax)
		_, launched := m.launched[i]
		n.inFlight = v.checking[i] || launched
		q.press(ctx, v, i, n, now)
		if !n.model {
			continue
		}
		if why := q.triggered(v, i, n, phases); why != "" {
			n.pending = why
		}
		q.serveRecheck(ctx, v, i, n, now)
	}
}

// press works out answerBy and turns the question pressing when an answer
// now costs the deadline, or it has been open too long.
func (q *Queue) press(ctx context.Context, v *view, i int, n *needsRec, now time.Time) {
	proj := q.projectOf(v)
	by, dl, label := q.answerBy(v, i)
	n.answerBy = by
	if n.state != needsRaised {
		return
	}
	margin, mu := policy.Duration(v.job, proj, i, policy.PressMargin)
	after, au := policy.Duration(v.job, proj, i, policy.PressAfter)
	var why string
	var use policy.Use
	switch {
	case !by.IsZero() && !now.Before(by.Add(-margin)):
		why = fmt.Sprintf("blocks %s; %s misses %s (%s) unless answered by %s", v.blocksWhat(i), v.job.Name, label,
			forecast.Clock(dl, q.Loc), forecast.Clock(by, q.Loc))
		use = mu
	case now.Sub(n.at) >= after:
		why = "unanswered for " + span(now.Sub(n.at))
		use = au
	default:
		return
	}
	n.state, n.pressingAt, n.pressingWhy = needsPressing, now, why
	extra := map[string]any{"needs": n.text}
	if !by.IsZero() {
		extra["answerBy"] = by.UTC().Format(time.RFC3339)
	}
	q.recordPolicyDecision(ctx, v, i, "needs-pressing", why, now, []policy.Use{use}, extra)
	q.Log.Info("question pressing", "job", v.key, "unit", i, "why", why)
}

// answerBy is the latest time an answer to unit i's question can come
// with the job still on time: its effective deadline (a dependent job's,
// when that is earlier), less the work from the unit to the job's end.
// Zero when the job has no deadline. label names the deadline.
func (q *Queue) answerBy(v *view, i int) (by, dl time.Time, label string) {
	dl, label = v.obj.Deadline, "its deadline"
	if v.milestone != "" {
		label = v.milestone
	}
	if js, ok := q.lastDec.Jobs[v.key]; ok && !js.Deadline.IsZero() {
		dl = js.Deadline
		if js.DeadlineFor != "" {
			label = js.DeadlineFor + "'s deadline"
		}
	}
	if dl.IsZero() || v.est == nil {
		return time.Time{}, dl, label
	}
	secs := v.downstream(i)
	return dl.Add(-time.Duration(secs * float64(time.Second))).Truncate(time.Second), dl, label
}

// downstream is the work from unit i to the job's end, in seconds: the
// unit's own, then the longest chain of unfinished units that wait on it,
// as the estimator prices them now.
func (v *view) downstream(i int) float64 {
	plan, m, e := v.mem.plan, v.mem, v.est
	succ := map[int][]int{}
	for s, u := range plan.Units {
		for _, a := range u.After {
			if a >= 0 && a < len(plan.Units) {
				succ[a] = append(succ[a], s)
			}
		}
	}
	memo, seen := map[int]float64{}, map[int]bool{}
	var path func(x int) float64
	path = func(x int) float64 {
		if d, ok := memo[x]; ok {
			return d
		}
		if seen[x] {
			return 0 // a bad table's cycle
		}
		seen[x] = true
		u := plan.Units[x]
		d := 0.0
		if !isWait(plan, x) {
			d = e.Predict(u.Key, u.Pos).Mean
		}
		if u.Generator && !m.expanded(x) {
			d += e.Generator(u.Key).Longest
		}
		longest := 0.0
		for _, s := range succ[x] {
			if !m.done(s) && m.waitsOn(s, x) {
				longest = math.Max(longest, path(s))
			}
		}
		memo[x] = d + longest
		return memo[x]
	}
	return path(i)
}

// blocksWhat names what waits on unit i: the unit, then the units that
// wait on it directly.
func (v *view) blocksWhat(i int) string {
	m := v.mem
	var after []string
	for s, u := range m.plan.Units {
		for _, a := range u.After {
			if a == i && !m.done(s) {
				after = append(after, nameOf(m, s))
				break
			}
		}
	}
	what := nameOf(m, i)
	switch {
	case len(after) == 0:
		return what
	case len(after) > 3:
		return fmt.Sprintf("%s, then %s and %d more", what, strings.Join(after[:3], ", "), len(after)-3)
	}
	return what + ", then " + strings.Join(after, ", ")
}

// triggered is why unit i should re-check its question now, if an event
// since the last look calls for it: a unit it waits on or reads succeeded,
// a unit it is about changed its output, the unit table grew, a path it is
// about was written, or a job it is about or reads from finished.
func (q *Queue) triggered(v *view, i int, n *needsRec, phases map[string]string) string {
	m := v.mem
	plan := m.plan
	units := map[int]bool{}
	for _, a := range plan.Units[i].After {
		units[a] = true
	}
	for _, in := range plan.Units[i].Inputs {
		units[in.From] = true
	}
	jobs := map[string]bool{}
	for _, in := range v.job.Spec.Inputs {
		if in.FromJob != "" {
			jobs[v.job.Namespace+"/"+in.FromJob] = true
		}
	}
	var paths []string
	for _, r := range n.about {
		kind, ref, _ := strings.Cut(r, ":")
		switch kind {
		case "unit":
			if u, ok := unitByRef(plan, ref); ok {
				units[u] = true
			}
		case "job":
			if !strings.Contains(ref, "/") {
				ref = v.job.Namespace + "/" + ref
			}
			jobs[ref] = true
		case "path":
			paths = append(paths, strings.Trim(ref, "/"))
		}
	}
	cur := &needsBase{units: len(plan.Units), outs: map[int]string{}, phases: map[string]string{}}
	for u := range units {
		if u < 0 || u >= len(plan.Units) || u == i {
			continue
		}
		if m.succeeded[u] {
			cur.outs[u] = "ok:" + m.unitOut[u] + "@" + m.ended[u].UTC().Format(time.RFC3339Nano)
		} else {
			cur.outs[u] = ""
		}
	}
	for k := range jobs {
		cur.phases[k] = phases[k]
	}
	old := n.base
	n.base = cur
	if old == nil {
		return "" // the first look: what is there now is what it asked against
	}
	if cur.units > old.units {
		return fmt.Sprintf("the job's unit table grew from %d to %d units", old.units, cur.units)
	}
	for _, u := range slices.Sorted(maps.Keys(cur.outs)) {
		o, had := old.outs[u]
		if c := cur.outs[u]; had && c != o && strings.HasPrefix(c, "ok:") {
			if o == "" {
				return nameOf(m, u) + " finished"
			}
			return nameOf(m, u) + "'s output changed"
		}
	}
	for k, ph := range cur.phases {
		if o, had := old.phases[k]; had && ph != o && (ph == "Succeeded" || ph == "Failed") {
			return fmt.Sprintf("job %s finished (%s)", k, ph)
		}
	}
	for _, f := range m.out.fresh {
		if f.unit == i {
			continue
		}
		p := strings.Trim(f.path, "/")
		for _, pre := range paths {
			if pre == "" || p == pre || strings.HasPrefix(p, pre+"/") {
				return nameOf(m, f.unit) + " wrote " + p
			}
		}
	}
	return ""
}

// serveRecheck releases unit i for a re-check of its question when a
// trigger is pending and the caps allow it.
func (q *Queue) serveRecheck(ctx context.Context, v *view, i int, n *needsRec, now time.Time) {
	if n.pending == "" || n.inFlight {
		return // one at a time (policy.RecheckInFlight)
	}
	proj := q.projectOf(v)
	limit, lu := policy.Int(v.job, proj, i, policy.RecheckMax)
	if n.rechecks >= limit {
		if !n.capped {
			why := fmt.Sprintf("%s, but its %d re-checks are used up: it waits for a person", n.pending, limit)
			q.recordPolicyDecision(ctx, v, i, "needs-recheck-refused", why, now, []policy.Use{lu}, map[string]any{"needs": n.text})
			n.capped = true
		}
		n.pending = ""
		return
	}
	every, _ := policy.Duration(v.job, proj, i, policy.RecheckMinInterval)
	if !n.lastCheck.IsZero() && now.Sub(n.lastCheck) < every {
		return // kept: served once the interval has passed
	}
	if _, running := v.running[i]; running || v.finishing[i] {
		return
	}
	if v.rechecking == nil {
		v.rechecking = map[int]string{}
	}
	v.rechecking[i] = n.pending
	v.runnable = append(v.runnable, i)
}

// recheckPod makes pod a re-check of unit idx's question.
func recheckPod(pod *corev1.Pod, n *needsRec, why string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[c4v1.AnnotationRecheck] = why
	for c := range pod.Spec.Containers {
		pod.Spec.Containers[c].Env = append(pod.Spec.Containers[c].Env,
			corev1.EnvVar{Name: "C4_RECHECK", Value: "1"},
			corev1.EnvVar{Name: "C4_NEEDED", Value: n.text},
			corev1.EnvVar{Name: "C4_RECHECK_WHY", Value: why},
			corev1.EnvVar{Name: "C4_RECHECK_SECONDS", Value: strconv.Itoa(recheckSeconds)})
	}
}

// startedRecheck notes a re-check of unit idx's question released.
func (q *Queue) startedRecheck(ctx context.Context, v *view, idx int, why string, now time.Time) {
	n := v.mem.needs[idx]
	n.rechecks++
	n.lastCheck, n.lastWhy, n.pending, n.inFlight = now, why, "", true
	q.recordDecision(ctx, v, idx, "needs-recheck", why, "", now, map[string]any{"needs": n.text, "recheck": n.rechecks})
}

// endRecheck closes a re-check of unit idx's question that ended. Exit 0
// withdraws the question, with the unit's termination message as the why;
// stopping for information again keeps it, with its new text; anything
// else keeps it. Its end changes nothing else.
func (q *Queue) endRecheck(ctx context.Context, v *view, idx int, p *corev1.Pod, now time.Time) {
	m := v.mem
	n := m.needs[idx]
	if n == nil || !m.openNeeds(idx) {
		return // answered meanwhile
	}
	n.inFlight = false
	switch {
	case p.Status.Phase == corev1.PodSucceeded:
		q.withdrawNeeds(ctx, v, idx, "unit:"+v.key+"/"+nameOf(m, idx), withdrawWhy(p), "", now)
	case isNeeds(p):
		text, why, about, _ := needsOf(p)
		if text != n.text || why != n.why {
			q.Log.Info("question restated on a re-check", "job", v.key, "unit", idx, "was", n.text, "now", text)
			n.text, n.why = text, why
		}
		if len(about) > 0 {
			n.about = about
		}
	default:
		_, title := exitCause(p)
		q.Log.Info("a re-check ended without deciding; the question stands", "job", v.key, "unit", idx, "exit", title)
	}
}

// withdrawWhy is why a block withdrew its question: its termination
// message (a host runner's pod message), plain or {"why": ...}.
func withdrawWhy(p *corev1.Pod) string {
	msg := p.Status.Message
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && strings.TrimSpace(t.Message) != "" {
			msg = t.Message
			break
		}
	}
	msg = strings.TrimSpace(msg)
	var w struct {
		Why string `json:"why"`
	}
	if json.Unmarshal([]byte(msg), &w) == nil && strings.TrimSpace(w.Why) != "" {
		msg = strings.TrimSpace(w.Why)
	}
	if msg == "" {
		msg = "the unit found on a re-check that it no longer needs it"
	}
	return msg
}

// askAnswerer puts unit idx's question to the job's answerer, if it has
// one: its answer supplies it.
func (q *Queue) askAnswerer(ctx context.Context, v *view, idx int, n *needsRec, now time.Time) {
	if names, _ := policy.List(v.job, q.projectOf(v), idx, policy.Answers); len(names) == 0 {
		return
	}
	text := n.text
	if n.why != "" {
		text += " (why: " + n.why + ")"
	}
	a := &askRec{id: fmt.Sprintf("n%d.%d", idx, now.Unix()), from: idx, to: -1, text: text, at: now, needs: true}
	q.ask(ctx, v, a, "", now)
	if a.open() {
		n.askID = a.id
	}
}

// closeNeedsAsk closes the question put to the answerer once someone
// else has settled it.
func (m *jobMem) closeNeedsAsk(n *needsRec, why string, now time.Time) {
	for _, a := range m.asks {
		if a.id == n.askID && a.open() {
			a.cannot, a.answeredAt = why, now
		}
	}
	n.askID = ""
}

// recordPolicyDecision is recordDecision for a decision policy values
// made: the signed record and the book entry name them (policy: [{key,
// value, from}]), and the scheduler's name moves to scheduler.
func (q *Queue) recordPolicyDecision(ctx context.Context, v *view, idx int, kind, reason string, now time.Time, uses []policy.Use, extra map[string]any) {
	x := map[string]any{}
	for k, val := range extra {
		x[k] = val
	}
	if len(uses) > 0 {
		x["policy"], x["scheduler"] = policy.Record(uses...), q.PolicyName
	}
	q.recordDecision(ctx, v, idx, kind, reason, "", now, x)
	if k := len(v.mem.decisions); k > 0 && len(uses) > 0 {
		v.mem.decisions[k-1].Policy = policy.Work(uses...)
	}
}
