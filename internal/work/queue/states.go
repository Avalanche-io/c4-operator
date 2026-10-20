package queue

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/order"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// The unit states a person is shown beyond queued, running and done
// (docs: c4-hud docs/intro/c4-hud-changes.md, sections 2 and 3): a unit
// preparing to run (preflight), a unit whose output is still moving
// (egress), a failed attempt waiting for its retry (backoff), and failed
// units a person took (acknowledged, assigned) or accepted.

// Preflight is what a unit pod reports while it prepares.
type Preflight struct {
	// Step is place (waiting for a node), stage (inputs), fetch (cache),
	// image, license or start.
	Step  string `json:"step"`
	Bytes int64  `json:"bytes,omitempty"`
	Of    int64  `json:"of,omitempty"`
}

// PreflightOf is the step a live unit pod is preparing, or nil once its
// work runs.
func PreflightOf(p *corev1.Pod) *Preflight {
	if p == nil || p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return nil
	}
	if a := p.Annotations[c4v1.AnnotationPreflight]; a != "" {
		var pf Preflight
		if json.Unmarshal([]byte(a), &pf) == nil && pf.Step != "" && pf.Step != "done" {
			return &pf
		}
		if pf.Step == "done" {
			return nil
		}
	}
	if p.Status.Phase == corev1.PodRunning {
		return nil
	}
	if p.Spec.NodeName == "" {
		return &Preflight{Step: "place"}
	}
	for _, c := range p.Status.InitContainerStatuses {
		if c.State.Terminated == nil {
			return &Preflight{Step: "stage"}
		}
	}
	for _, c := range p.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") {
			return &Preflight{Step: "image"}
		}
	}
	if len(p.Spec.InitContainers) > 0 && len(p.Status.InitContainerStatuses) == 0 {
		return &Preflight{Step: "stage"}
	}
	return &Preflight{Step: "start"}
}

// Egress is what a finished unit pod reports while its output moves.
type Egress struct {
	Bytes int64 `json:"bytes,omitempty"`
	Of    int64 `json:"of,omitempty"`
	// Sealed: the output is captured (its tree has a C4 ID); Folded:
	// added to the job's output; Verified: the addition is recorded.
	Sealed   bool `json:"sealed"`
	Folded   bool `json:"folded"`
	Verified bool `json:"verified"`
}

// EgressOf is a finished unit pod's capture progress.
func EgressOf(p *corev1.Pod) *Egress {
	if p == nil || p.Status.Phase != corev1.PodSucceeded {
		return nil
	}
	e := &Egress{}
	if a := p.Annotations[c4v1.AnnotationEgress]; a != "" {
		_ = json.Unmarshal([]byte(a), e)
	}
	e.Sealed = p.Annotations[c4v1.AnnotationOutputTree] != ""
	if e.Sealed && e.Of > 0 && e.Bytes < e.Of {
		e.Bytes = e.Of
	}
	return e
}

// ackRecord is a standing acknowledgement in the queue's memory.
type ackRecord struct {
	c4v1.WorkAck
	units map[int]bool
}

func tokenValue(t string) int64 {
	n, _ := strconv.ParseInt(t, 36, 64)
	return n
}

// applyAcks carries out the job's new acknowledgement requests, in token
// order, each once.
func applyAcks(j *c4v1.WorkJob, m *jobMem, n int) {
	a := j.Annotations[c4v1.AnnotationAcks]
	if a == "" {
		return
	}
	var reqs []c4v1.WorkAckRequest
	if err := json.Unmarshal([]byte(a), &reqs); err != nil {
		return
	}
	sort.SliceStable(reqs, func(x, y int) bool { return tokenValue(reqs[x].Token) < tokenValue(reqs[y].Token) })
	last := tokenValue(m.acked)
	for _, r := range reqs {
		if tokenValue(r.Token) <= last {
			continue
		}
		m.acked, last = r.Token, tokenValue(r.Token)
		units := map[int]bool{}
		for _, i := range decode(r.Units) {
			// Only units that need a person can be taken or accepted.
			if i >= 0 && i < n && m.failed[i] && !m.accepted[i] {
				units[i] = true
			}
		}
		if len(units) == 0 {
			continue
		}
		if r.Kind == c4v1.AckAccept {
			for i := range units {
				m.accepted[i] = true
			}
			m.causesDid(units, "accepted")
		}
		// A newer acknowledgement of a unit replaces an older one.
		for _, old := range m.acks {
			if old.Kind == c4v1.AckAccept && r.Kind != c4v1.AckAccept {
				continue
			}
			for i := range units {
				delete(old.units, i)
			}
		}
		m.acks = append(m.acks, &ackRecord{WorkAck: c4v1.WorkAck{Kind: r.Kind, Actor: r.Actor, Assignee: r.Assignee,
			Note: r.Note, At: metav1.Time{Time: r.At.Truncate(time.Second)}, Record: r.Record}, units: units})
	}
}

// forgetAcks drops unit i from every acknowledgement (it was requeued).
func (m *jobMem) forgetAcks(i int) {
	delete(m.accepted, i)
	for _, a := range m.acks {
		delete(a.units, i)
	}
}

// ackStatus is status.acks: the standing acknowledgements, oldest
// first, each naming only the units it still covers.
func (m *jobMem) ackStatus() []c4v1.WorkAck {
	var keep []*ackRecord
	var out []c4v1.WorkAck
	for _, a := range m.acks {
		var set []int
		for i := range a.units {
			if a.Kind == c4v1.AckAccept && m.accepted[i] || a.Kind != c4v1.AckAccept && m.failed[i] && !m.accepted[i] {
				set = append(set, i)
			}
		}
		if len(set) == 0 {
			continue
		}
		sort.Ints(set)
		w := a.WorkAck
		w.Units = order.EncodeSet(set)
		keep = append(keep, a)
		out = append(out, w)
	}
	m.acks = keep
	if len(out) > 32 {
		out = out[len(out)-32:]
	}
	return out
}

func (m *jobMem) loadAcks(st *c4v1.WorkJobStatus) {
	m.acked = st.Acked
	for _, a := range st.Acks {
		units := map[int]bool{}
		for _, i := range decode(a.Units) {
			units[i] = true
		}
		m.acks = append(m.acks, &ackRecord{WorkAck: a, units: units})
	}
	for _, i := range decode(st.UnitStates["accepted"]) {
		m.accepted[i] = true
		m.failed[i] = true
	}
}

// risk is the deadline risk a person is shown (WorkEstimate.Risk): from
// the shown band at the job's quantile, the scheduler's own finish, and
// the clock.
func risk(now time.Time, v *view, js sched.JobState, e *c4v1.WorkEstimate) string {
	d := plannedDeadline(v, js)
	if d.IsZero() {
		return c4v1.RiskNone
	}
	at := func(t *metav1.Time) bool { return t != nil && t.After(d) }
	q := e.ETA90
	if v.source == "suggested" {
		q = e.ETA95
	}
	switch {
	case now.After(d):
		return c4v1.RiskMissed
	case !js.Feasible || at(e.ETA50):
		return c4v1.RiskWillMiss
	case js.AtRisk || at(q):
		return c4v1.RiskAt
	}
	return c4v1.RiskNone
}

// plannedDeadline is the deadline the scheduler planned the job for: its
// own, or the earlier one a dependent job set (sched DeadlineFor names it,
// and the job's message names it too); the job's own when sched has none.
func plannedDeadline(v *view, js sched.JobState) time.Time {
	if !js.Deadline.IsZero() {
		return js.Deadline
	}
	return v.obj.Deadline
}

// shownSlack is the planned deadline less the shown finish risk() judges
// by (p90; p95 for a suggested deadline, which the farm sets at p95), so
// slack, ETA and risk agree. The scheduler's Slack is at all attainable
// capacity and only decides Feasible: shown, it read "+7h" beside a late
// ETA. ok is false without a deadline or a shown band: then it is unset.
func shownSlack(v *view, js sched.JobState, e *c4v1.WorkEstimate) (int64, bool) {
	d := plannedDeadline(v, js)
	f := e.ETA90
	if v.source == "suggested" && e.ETA95 != nil {
		f = e.ETA95
	}
	if d.IsZero() || f == nil {
		return 0, false
	}
	return int64(d.Sub(f.Time).Seconds()), true
}

// needsPerson sets or clears the NeedsPerson condition.
func needsPerson(st *c4v1.WorkJobStatus, v *view, approval bool, waiting, flagged int, gen int64) {
	reason, msg := "", ""
	switch {
	case len(v.mem.openNeedsList()) > 0:
		open := v.mem.openNeedsList()
		reason, msg = c4v1.NeedsInformation, "needs information: "+v.mem.needs[open[0]].text
		if len(open) > 1 {
			msg = fmt.Sprintf("%d units need information the brief lacked, first: %s", len(open), v.mem.needs[open[0]].text)
		}
	case waiting > 0:
		reason, msg = c4v1.NeedsFailed, fmt.Sprintf("%d units failed after their retries; requeue, take or accept them", waiting)
	case v.measureShort != "":
		reason, msg = c4v1.NeedsMeasure, v.measureShort
	case flagged > 0:
		reason, msg = c4v1.NeedsReview, fmt.Sprintf("a review escalated %d units%s; decide, requeue, take or accept them", flagged, escalatedTo(v.mem))
	case personWaitsBlockingN(v.mem) > 0:
		n, who := personWaitsBlocking(v.mem)
		reason, msg = c4v1.NeedsDecision, fmt.Sprintf("%d %s for %s's decision, and work waits on %s", n, plural(n, "unit waits", "units wait"), strings.Join(who, ", "), plural(n, "it", "them"))
	case v.mem.planErr != nil || v.mem.plan == nil:
		reason, msg = c4v1.NeedsStuck, "the job has no plan; fix its parameters"
	case v.unschedulable != "":
		reason, msg = c4v1.NeedsStuck, v.unschedulable
	case v.stuckOn != "":
		reason, msg = c4v1.NeedsStuck, "it waits on "+v.stuckOn+", which failed"
	case approval:
		reason, msg = c4v1.NeedsApproval, "capacity that would restore its deadline waits for approval"
	}
	if reason == "" {
		meta.RemoveStatusCondition(&st.Conditions, c4v1.ConditionNeedsPerson)
		return
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: c4v1.ConditionNeedsPerson, Status: metav1.ConditionTrue,
		Reason: reason, Message: msg, ObservedGeneration: gen})
}

// approvals are the jobs a capacity request waits on a person for.
func approvals(crs []c4v1.CapacityRequest) map[string]bool {
	out := map[string]bool{}
	for _, cr := range crs {
		if cr.Spec.Approved || !strings.Contains(cr.Name, "-approve-") {
			continue
		}
		for _, j := range cr.Spec.Jobs {
			out[j] = true
		}
	}
	return out
}

// escalatedTo names who flagged units wait for, as " to lead" or "".
func escalatedTo(m *jobMem) string {
	var to []string
	for i, r := range m.reviews {
		if m.flagged(i) && r.To != "" && !slices.Contains(to, r.To) {
			to = append(to, r.To)
		}
	}
	if len(to) == 0 {
		return ""
	}
	sort.Strings(to)
	return " to " + strings.Join(to, ", ")
}
