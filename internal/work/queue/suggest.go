package queue

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Suggested deadlines (DESIGN.md section 10.7). A job with no deadline of
// its own, whose policy is not a latency SLO, gets one from the farm:
//
//   - achievable: the earliest time, rounded up, by which it finishes at
//     the policy's quantile without making any job that is on time now
//     late, found by running the policy on copies of the farm with the
//     job's deadline moved (forecast.FirstAchievable);
//   - sessions: the next review session; a job not expected to make it
//     keeps it and is offered the following one (status.suggestion
//     .nextSession);
//   - turnaround: submission plus the turnaround.
//
// An achievable suggestion is revised only at binary order's information
// points and only when the estimate has narrowed (forecast.Suggestion),
// at most MaxRevisions times, then settles. Every value is signed into
// the book.

const (
	// whatIfPerTick bounds the jobs whose achievable deadline is searched
	// in one tick; each search runs the policy about a dozen times.
	whatIfPerTick = 2
	// whatIfHorizon is the latest deadline searched.
	whatIfHorizon = 7 * 24 * time.Hour
	// whatIfTolerance is the search's resolution.
	whatIfTolerance = 5 * time.Minute
)

const (
	modePinned    = "Pinned"
	modeSuggested = "Suggested"
	modeTracking  = "Tracking"
)

// suggestMem is a job's suggestion between ticks.
type suggestMem struct {
	s        forecast.Suggestion
	policy   forecast.Policy
	records  []string // signed record per History entry
	lastInfo int      // finished count of the last information point used
	next     time.Time
	atRisk   bool // the band at the policy quantile ends after the deadline
}

// classPolicy is a class's default deadline policy for a job.
func classPolicy(j *c4v1.WorkJob) forecast.Policy {
	p := forecast.DefaultPolicy()
	switch j.Spec.Class {
	case "ci":
		p.Kind = forecast.Latency
		p.Turnaround = 10 * time.Minute
	default:
		if t := j.Spec.Params["turnaround"]; t != "" {
			if d, err := time.ParseDuration(t); err == nil && d > 0 {
				p.Kind, p.Turnaround, p.MaxRevisions = forecast.Turnaround, d, 0
			}
		}
	}
	return p
}

// projectPolicy converts a project's policy for a class.
func projectPolicy(dp c4v1.DeadlinePolicy) (forecast.Policy, error) {
	p := forecast.DefaultPolicy()
	p.Kind = forecast.Kind(dp.Kind)
	p.Sessions = dp.Sessions
	if dp.TimeZone != "" {
		loc, err := time.LoadLocation(dp.TimeZone)
		if err != nil {
			return p, fmt.Errorf("timeZone: %w", err)
		}
		p.Location = loc
	}
	for _, w := range dp.Weekdays {
		d, ok := weekdays[strings.ToLower(strings.TrimSpace(w))]
		if !ok {
			return p, fmt.Errorf("weekday %q", w)
		}
		p.Weekdays = append(p.Weekdays, d)
	}
	for _, f := range []struct {
		s   string
		dst *time.Duration
	}{{dp.Turnaround, &p.Turnaround}, {dp.Round, &p.Round}, {dp.MinLead, &p.MinLead}} {
		if f.s == "" {
			continue
		}
		d, err := time.ParseDuration(f.s)
		if err != nil {
			return p, err
		}
		*f.dst = d
	}
	if dp.Quantile != "" {
		q, err := strconv.ParseFloat(dp.Quantile, 64)
		if err != nil {
			return p, fmt.Errorf("quantile: %w", err)
		}
		p.Quantile = q
	}
	if dp.MaxRevisions != nil {
		p.MaxRevisions = int(*dp.MaxRevisions)
	}
	if p.Kind != forecast.Achievable {
		// A session or a turnaround is fixed once chosen: a job that will
		// miss its session keeps it and is offered the next one.
		p.MaxRevisions = 0
	}
	return p, p.Validate()
}

var weekdays = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

// policyFor returns the deadline policy of a job: its project's for its
// class, else the class's.
func (q *Queue) policyFor(v *view) forecast.Policy { return q.policyForJob(v.project, v.job) }

func (q *Queue) policyForJob(project string, j *c4v1.WorkJob) forecast.Policy {
	if pp, ok := q.policies[project][j.Spec.Class]; ok {
		return pp
	}
	return classPolicy(j)
}

// loadPolicies reads every project's deadline policies.
func (q *Queue) loadPolicies(projects []c4v1.Project) {
	q.policies = map[string]map[string]forecast.Policy{}
	for _, pr := range projects {
		if pr.Spec.Work == nil {
			continue
		}
		for _, dp := range pr.Spec.Work.DeadlinePolicies {
			p, err := projectPolicy(dp)
			if err != nil {
				q.Log.Error(err, "deadline policy", "project", pr.Name, "class", dp.Class)
				continue
			}
			if q.policies[pr.Name] == nil {
				q.policies[pr.Name] = map[string]forecast.Policy{}
			}
			q.policies[pr.Name][dp.Class] = p
		}
	}
}

// mode is who owns the job's deadline.
func mode(j *c4v1.WorkJob, p forecast.Policy) string {
	switch {
	case j.Spec.DeadlineMode != "":
		return j.Spec.DeadlineMode
	case j.Spec.Deadline != nil:
		return modePinned
	case p.Hard():
		return ""
	}
	return modeSuggested
}

// applySuggestion makes a settled-or-not suggestion the job's deadline
// when the job's deadline is the farm's to suggest.
func (q *Queue) applySuggestion(v *view) {
	m := v.mem
	v.source = "class"
	switch {
	case v.job.Spec.Deadline != nil:
		v.source = "spec"
	case v.milestone != "":
		// Whoever owns the Milestone owns the date.
		v.source = "milestone"
		return
	}
	if mode(v.job, q.policyFor(v)) != modeSuggested || m.sugg == nil || m.sugg.s.Deadline.IsZero() {
		return
	}
	v.obj.Type, v.obj.Deadline, v.source = "deadline", m.sugg.s.Deadline, "suggested"
}

// suggest updates suggestions after the policy has run.
func (q *Queue) suggest(ctx context.Context, now time.Time, snap sched.Snapshot, dec sched.Decisions, views []*view) {
	budget := whatIfPerTick
	for _, v := range views {
		m := v.mem
		if m.plan == nil || !m.finished.IsZero() {
			continue
		}
		p := q.policyFor(v)
		if mode(v.job, p) != modeSuggested || v.milestone != "" {
			continue
		}
		if m.sugg == nil {
			m.sugg = &suggestMem{}
		}
		sg := m.sugg
		sg.policy = p
		finished, total := len(m.millis), len(m.plan.Units)
		js := dec.Jobs[v.key]
		band := q.shown(v, now, forecast.Display(now, v.sum, js.ETA50, js.ETA90, forecast.Tail(v.mem.plan.Units[0].Slots, js.GroupSlots)))
		info := forecast.InfoPoint(finished) && finished > sg.lastInfo
		ev := forecast.Event{Now: now, Finished: finished, Total: total, Band: band, Candidate: sg.s.Deadline}
		switch {
		case sg.s.State == forecast.Settled:
		case sg.s.Deadline.IsZero() || (info && p.Kind == forecast.Achievable && sg.s.AtRisk(p.Quantile, band)):
			// The first suggestion, or one at risk at an information
			// point: the only times the policy is searched. The system
			// never moves a suggestion earlier (forecast.Suggestion).
			if p.Kind == forecast.Achievable {
				if budget == 0 {
					continue
				}
				budget--
			}
			c, _ := q.candidate(now, snap, v, p)
			if c.IsZero() {
				continue
			}
			ev.Candidate, ev.Info = c, info
		}
		if info {
			sg.lastInfo = finished
		}
		before := len(sg.s.History)
		changed := sg.s.Observe(p, ev)
		for i := before; i < len(sg.s.History); i++ {
			sg.records = append(sg.records, q.recordSuggestion(ctx, v, sg, sg.s.History[i], now))
		}
		// Status reports the deadline the next decision plans on.
		q.applySuggestion(v)
		sg.next = time.Time{}
		// A suggestion set this tick was not what this tick's decision
		// planned on; its risk is judged from the next one.
		sg.atRisk = !changed && sg.s.AtRisk(p.Quantile, band)
		if p.Kind == forecast.Sessions && sg.atRisk {
			sg.next = p.NextSession(sg.s.Deadline.Add(time.Minute))
		}
	}
}

// candidate is the policy's deadline for a job now, and the band it was
// computed with. It is zero when the job has no forecast.
func (q *Queue) candidate(now time.Time, snap sched.Snapshot, v *view, p forecast.Policy) (time.Time, forecast.Band) {
	return candidateIn(q.Policy, q.schedCfg(), now, snap, v.key, v.job.CreationTimestamp.Time, v.mem.readyAt, p, q.adjustFor(v))
}

// candidateIn is the policy's deadline for job key of snap: for an
// achievable policy, the earliest deadline by which it finishes at the
// quantile without making a job that is on time late.
func candidateIn(policy sched.Policy, cfg sched.Config, now time.Time, snap sched.Snapshot, key string, submit, ready time.Time,
	p forecast.Policy, adjust func(time.Time, forecast.Band) forecast.Band) (time.Time, forecast.Band) {
	if p.Kind != forecast.Achievable {
		return p.Earliest(now, now, submit, ready), forecast.Band{}
	}
	w := newWhatIf(policy, cfg, snap, key)
	if w == nil {
		return time.Time{}, forecast.Band{}
	}
	w.adjust = adjust
	hi := now.Add(whatIfHorizon)
	base := w.run(hi, 0)
	band := w.band(base)
	if band.IsZero() {
		return time.Time{}, band
	}
	d := w.achievable(p.Quantile, w.onTime(base), now.Add(p.MinLead), hi)
	if d.Equal(hi) {
		// Not achievable within the horizon without making another job
		// late: suggest the finish at the quantile with no deadline
		// pressure, and let the feasibility warning speak.
		d = band.At(p.Quantile)
	}
	return p.Earliest(d, now, submit, ready), band
}

// recordSuggestion signs one value of a job's suggestion into the book.
func (q *Queue) recordSuggestion(ctx context.Context, v *view, sg *suggestMem, r forecast.Revision, now time.Time) string {
	if q.Signer == nil {
		return ""
	}
	pred := map[string]any{"decision": "suggest-deadline", "job": v.key, "deadline": r.Deadline.UTC().Format(time.RFC3339), "reason": r.Reason,
		"state": string(sg.s.State), "revision": sg.s.Revisions, "unitsMeasured": r.Finished,
		"policy": string(sg.policy.Kind), "quantile": sg.policy.Quantile, "at": now.UTC().Format(time.RFC3339)}
	rec, err := provenance.SignStatement(ctx, q.Signer, provenance.PredicateDecision, []provenance.Subject{jobSubject(v.job)}, pred, now)
	if err != nil {
		q.Log.Error(err, "suggestion record", "job", v.key)
		return ""
	}
	id, err := q.storeRecord(ctx, v, rec)
	if err != nil {
		q.Log.Error(err, "suggestion record", "job", v.key)
	}
	return id
}

// suggestionStatus is the job's suggestion as status reports it.
func suggestionStatus(sg *suggestMem) *c4v1.WorkSuggestion {
	if sg == nil || sg.s.Deadline.IsZero() {
		return nil
	}
	out := &c4v1.WorkSuggestion{
		Deadline: metav1.Time{Time: sg.s.Deadline}, State: string(sg.s.State), Policy: string(sg.policy.Kind),
		Quantile: strconv.FormatFloat(sg.policy.Quantile, 'f', -1, 64), Revisions: int32(sg.s.Revisions),
		MaxRevisions: int32(sg.policy.MaxRevisions), AtRisk: sg.atRisk,
	}
	for i, r := range sg.s.History {
		h := c4v1.WorkSuggestionRevision{At: metav1.Time{Time: r.At.Truncate(time.Second)}, Deadline: metav1.Time{Time: r.Deadline},
			UnitsMeasured: int32(r.Finished), Reason: r.Reason}
		if i < len(sg.records) {
			h.Record = sg.records[i]
		}
		out.History = append(out.History, h)
	}
	if !sg.next.IsZero() {
		out.NextSession = &metav1.Time{Time: sg.next}
	}
	return out
}

// loadSuggestion restores a suggestion from status after a restart.
func loadSuggestion(st *c4v1.WorkSuggestion) *suggestMem {
	if st == nil || st.Deadline.IsZero() {
		return nil
	}
	sg := &suggestMem{}
	sg.s.Deadline, sg.s.State, sg.s.Revisions = st.Deadline.Time, forecast.State(st.State), int(st.Revisions)
	for _, h := range st.History {
		sg.s.History = append(sg.s.History, forecast.Revision{At: h.At.Time, Deadline: h.Deadline.Time, Finished: int(h.UnitsMeasured), Reason: h.Reason})
		sg.records = append(sg.records, h.Record)
		sg.lastInfo = max(sg.lastInfo, int(h.UnitsMeasured))
	}
	if len(sg.s.History) > 0 {
		sg.s.Start = sg.s.History[0].At
	}
	if st.NextSession != nil {
		sg.next = st.NextSession.Time
	}
	sg.atRisk = st.AtRisk
	return sg
}
