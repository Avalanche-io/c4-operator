package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// A lead's job (DESIGN 10.6, A lead's job). A lead is a person's own
// Claude Code session that puts workers on the farm with c4_agent; its job
// is a graph job made for that session (c4.avalanche.io/lead-session), and
// each c4_agent call appends workers to it (appends.go). A lead's workers
// come and go between its turns, so the job does not finish when they
// have: it stays Running, with nothing to place, until it is closed. It
// is closed by its person or their lead (POST /jobs/{ns}/{name}/close,
// signed by the HUD and carried in c4.avalanche.io/close), or by the farm
// once it has been idle for the farm's idle close (24h by default) with no
// worker added; never by the session ending, since a session is resumed
// often. The queue signs each close (decision/v1, kind close), the farm's
// as its own act. Once closed the job takes no more workers and finishes
// as any job does: Succeeded if every unit did, else as its units say.
// status.lead and the Open condition say which of these it is.

// DefaultLeadIdleClose is how long a lead's job may stay idle, every
// worker ended and none added, before the farm closes it.
const DefaultLeadIdleClose = 24 * time.Hour

// leadMem is what the queue keeps of a lead's job: since when it has been
// idle, and its closing once closed.
type leadMem struct {
	idleSince time.Time
	closed    *c4v1.WorkClose
}

// leadSession is the session a lead's job was made for, "" for any other
// job.
func leadSession(j *c4v1.WorkJob) string {
	return j.Annotations[c4v1.AnnotationLeadSession]
}

// leadIdleClose is the farm's idle close.
func (q *Queue) leadIdleClose() time.Duration {
	if q.LeadIdleClose <= 0 {
		return DefaultLeadIdleClose
	}
	return q.LeadIdleClose
}

// questionFor is whose a job's units' questions are: its lead's when its
// lead answers them, else its owner's ("" for a job with neither).
func questionFor(j *c4v1.WorkJob) string {
	if s := leadSession(j); s != "" && j.Spec.Answers != nil && j.Spec.Answers.By == c4v1.AnswersByLead {
		return "lead:" + s
	}
	if o := j.Annotations[c4v1.AnnotationOwner]; o != "" {
		return "person:" + o
	}
	return ""
}

func (m *jobMem) loadLead(st *c4v1.WorkJobStatus) {
	l := st.Lead
	if l == nil {
		return
	}
	if l.IdleSince != nil && l.State == c4v1.LeadIdle {
		m.lead.idleSince = l.IdleSince.Time
	}
	if l.Closed != nil {
		c := *l.Closed
		m.lead.closed = &c
	}
}

// closeRequest is the first of a job's close requests, in token order.
func closeRequest(j *c4v1.WorkJob) (c4v1.WorkCloseRequest, bool) {
	var reqs []c4v1.WorkCloseRequest
	if err := json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationClose]), &reqs); err != nil || len(reqs) == 0 {
		return c4v1.WorkCloseRequest{}, false
	}
	sort.SliceStable(reqs, func(x, y int) bool { return tokenValue(reqs[x].Token) < tokenValue(reqs[y].Token) })
	return reqs[0], true
}

// applyClose closes a lead's job that is still open: on its first close
// request the queue trusts (ctl: the job with only those), else once it
// has been idle for the farm's idle close.
func (q *Queue) applyClose(ctx context.Context, v *view, ctl *c4v1.WorkJob, now time.Time) {
	m := v.mem
	if leadSession(v.job) == "" || m.lead.closed != nil {
		return
	}
	if r, ok := closeRequest(ctl); ok {
		why := r.Why
		if why == "" {
			why = "closed by " + r.Actor
		}
		q.closeLead(ctx, v, c4v1.WorkClose{By: r.Actor, Why: why, Control: r.Record}, now)
		return
	}
	idle := q.leadIdleClose()
	if since := m.lead.idleSince; !since.IsZero() && now.Sub(since) >= idle {
		q.closeLead(ctx, v, c4v1.WorkClose{By: "c4",
			Why: fmt.Sprintf("idle %s, no worker since %s", span(idle), since.UTC().Format(time.RFC3339))}, now)
	}
}

// closeLead closes the job, signing the close (decision/v1, kind close).
func (q *Queue) closeLead(ctx context.Context, v *view, c c4v1.WorkClose, now time.Time) {
	c.At = metav1.Time{Time: now.UTC().Truncate(time.Second)}
	c.Record = q.recordClose(ctx, v, c, now)
	v.mem.lead.closed = &c
	v.mem.lead.idleSince = time.Time{}
	q.Log.Info("lead's job closed", "job", v.key, "by", c.By, "why", c.Why)
}

// recordClose signs a lead's job's close: who closed it and why, and the
// HUD's record of the request, or the farm's idle close it was closed
// under.
func (q *Queue) recordClose(ctx context.Context, v *view, c c4v1.WorkClose, now time.Time) string {
	if q.Signer == nil {
		return ""
	}
	pred := map[string]any{"decision": "close", "job": v.key, "session": leadSession(v.job), "by": c.By, "reason": c.Why,
		"policy": q.PolicyName, "at": now.UTC().Format(time.RFC3339)}
	if c.Control != "" {
		pred["control"] = c.Control
	} else {
		pred["idleClose"] = span(q.leadIdleClose())
	}
	rec, err := provenance.SignStatement(ctx, q.Signer, provenance.PredicateDecision, []provenance.Subject{jobSubject(v.job)}, pred, now)
	if err == nil {
		var id string
		if id, err = q.storeRecord(ctx, v, rec); err == nil {
			return id
		}
	}
	q.Log.Error(err, "close record", "job", v.key)
	return ""
}

// leadStatus keeps a lead's job open until it is closed: a job whose
// units have all ended is Running with nothing to place (Idle), not
// finished, and one with a question open beside a failed unit is Held on
// the question. It writes status.lead and the Open condition.
func (q *Queue) leadStatus(st *c4v1.WorkJobStatus, v *view, now time.Time) {
	s := leadSession(v.job)
	if s == "" || isForecast(v.job) {
		return
	}
	m := v.mem
	ts := func(t time.Time) *metav1.Time { return &metav1.Time{Time: t.UTC().Truncate(time.Second)} }
	l := &c4v1.WorkLead{Session: s}
	cond := metav1.Condition{Type: c4v1.ConditionOpen, Status: metav1.ConditionTrue, ObservedGeneration: v.job.Generation}
	switch {
	case m.lead.closed != nil:
		c := *m.lead.closed
		l.State, l.Closed = c4v1.LeadClosed, &c
		cond.Status, cond.Reason = metav1.ConditionFalse, c4v1.LeadClosed
		cond.Message = fmt.Sprintf("closed %s by %s: %s; it takes no more workers and finishes once its units have",
			c.At.UTC().Format(time.RFC3339), c.By, c.Why)
	case (st.Phase == "Succeeded" || st.Phase == "Failed") && st.Units.Needs > 0:
		// A failed unit beside an open question: the lead answers it, or
		// adds what it needs.
		st.Phase = "Held"
		m.lead.idleSince = time.Time{}
		l.State, cond.Reason = c4v1.LeadWorking, c4v1.LeadWorking
		cond.Message = "open: a worker waits for an answer"
	case st.Phase == "Succeeded" || st.Phase == "Failed":
		// Every unit has ended: nothing to place, and its lead may add
		// more.
		st.Phase = "Running"
		if m.lead.idleSince.IsZero() {
			m.lead.idleSince = now
		}
		closes := m.lead.idleSince.Add(q.leadIdleClose())
		l.State, l.IdleSince, l.ClosesAt = c4v1.LeadIdle, ts(m.lead.idleSince), ts(closes)
		cond.Reason = c4v1.LeadIdle
		cond.Message = fmt.Sprintf("every worker has ended: nothing to place, waiting for its lead to add workers or close it; it closes by itself at %s if none is added",
			closes.UTC().Format(time.RFC3339))
	default:
		m.lead.idleSince = time.Time{}
		l.State, cond.Reason = c4v1.LeadWorking, c4v1.LeadWorking
		cond.Message = "open: its lead's workers run, and its lead may add more until it closes it"
	}
	st.Lead = l
	meta.SetStatusCondition(&st.Conditions, cond)
}
