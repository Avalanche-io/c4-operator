package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// A lead's job (DESIGN 10.6, A lead's job): the graph job the HUD makes
// for a person's own Claude Code session (POST /jobs with leadSession),
// which its c4_agent adds workers to (POST /jobs/{ns}/{name}/units). It
// stays open between its workers until it is closed: by its person or
// their lead (POST /jobs/{ns}/{name}/close, here), or by the farm once
// idle too long (queue/lead.go). The lead speaks through the person's own
// client with the agent header (X-C4-Agent) naming its session: the
// session's word, graded claimed, and the person's sign-in the identity.
// A header naming another session is another lead's, and is refused.

// maxCloseWhy bounds a close's reason, in bytes.
const maxCloseWhy = 1000

// leadActRefusal says why request r, made as viewer v, may not act for job
// j's lead or its person ("" when it may): act is what it would do, as in
// "closes it". The person may, directly or with the agent header naming
// the job's session; no one else, and no other session, may. The person is
// v as ownerFor resolves it, so a role's sign-in on a one-person farm acts
// for that farm's person.
func (s *Server) leadActRefusal(r *http.Request, v *api.Viewer, j *c4v1.WorkJob, act string) string {
	owner, session := j.Annotations[c4v1.AnnotationOwner], j.Annotations[c4v1.AnnotationLeadSession]
	switch h := claimedAgent(r); {
	case v.Unit != nil:
		return fmt.Sprintf("a unit is not %s's lead: only its person, or that lead, %s", j.Name, act)
	case owner == "":
		return fmt.Sprintf("%s has no owner on record, so no one %s", j.Name, act)
	case s.ownerFor(r.Context(), v) != owner:
		return fmt.Sprintf("%s is %s's job, made for their lead (session %s): only %s, or that lead, %s", j.Name, owner, session, owner, act)
	case h != "" && h != session:
		return fmt.Sprintf("%s is the job of lead session %s, and the agent header (X-C4-Agent) names session %s: only that lead, or %s with no agent header, %s",
			j.Name, session, h, owner, act)
	}
	return ""
}

// leadQuestionRefusal says why request r, made as viewer v, may not
// answer (act: "answers it") the question unit ref of job j stopped for,
// when the question is j's lead's (spec.answers.by lead): "" when it may,
// or when the question is a person's, whom the control's own rules serve.
// A lead's question is answered by that lead, through its person's client
// with the agent header naming its session, or by the person directly
// (as ownerFor resolves the viewer).
func (s *Server) leadQuestionRefusal(r *http.Request, v *api.Viewer, j *c4v1.WorkJob, ref, act string) string {
	owner, session := j.Annotations[c4v1.AnnotationOwner], j.Annotations[c4v1.AnnotationLeadSession]
	if session == "" || j.Spec.Answers == nil || j.Spec.Answers.By != c4v1.AnswersByLead {
		return ""
	}
	switch h := claimedAgent(r); {
	case owner == "" || s.ownerFor(r.Context(), v) != owner:
		return fmt.Sprintf("unit %q's question is for %s's lead (session %s): only %s, or that lead, %s", ref, or(owner, "its person"), session, or(owner, "its person"), act)
	case h != "" && h != session:
		return fmt.Sprintf("unit %q's question is for the lead of session %s, and the agent header (X-C4-Agent) names session %s: only that lead, or %s with no agent header, %s",
			ref, session, h, owner, act)
	}
	return ""
}

// or is s, else alt.
func or(s, alt string) string {
	if s != "" {
		return s
	}
	return alt
}

// closing is a lead's job's close request the queue has not carried out
// yet, if any.
func closing(j *c4v1.WorkJob) *c4v1.WorkCloseRequest {
	if l := j.Status.Lead; l != nil && l.Closed != nil {
		return nil
	}
	var reqs []c4v1.WorkCloseRequest
	if json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationClose]), &reqs) != nil || len(reqs) == 0 {
		return nil
	}
	return &reqs[0]
}

// closedRefusal says why a lead's job takes no more workers ("" when it
// does): it is closed, or closing.
func closedRefusal(j *c4v1.WorkJob) string {
	if l := j.Status.Lead; l != nil && l.Closed != nil {
		return fmt.Sprintf("%s is closed (%s): a closed lead's job takes no more workers; make a new job for them", j.Name, l.Closed.Why)
	}
	if c := closing(j); c != nil {
		return fmt.Sprintf("%s is closing (%s asked): a closed lead's job takes no more workers; make a new job for them", j.Name, c.Actor)
	}
	return ""
}

// handleClose is POST /jobs/{ns}/{name}/close: a lead's job's person, or
// their lead, closes it. The close is signed (action close) and carried
// to the queue in c4.avalanche.io/close; the queue closes the job, which
// then takes no more workers and finishes once its units have. Closing a
// job closed or closing already answers as it stands.
func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	var body api.CloseRequest
	if err := decodeBody(r, &body); err != nil && !errors.Is(err, io.EOF) {
		failBody(w, err)
		return
	}
	why := strings.TrimSpace(body.Why)
	if len(why) > maxCloseWhy {
		fail(w, http.StatusBadRequest, "why: %d bytes, over %d", len(why), maxCloseWhy)
		return
	}
	ctx := r.Context()
	for range 4 {
		j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
		if err != nil {
			fail(w, http.StatusNotFound, "no job %s/%s: %v", rec.job.Namespace, rec.job.Name, err)
			return
		}
		if j.Annotations[c4v1.AnnotationLeadSession] == "" {
			fail(w, http.StatusUnprocessableEntity, "%s is not a lead's job: it finishes once its units have, and is never closed", j.Name)
			return
		}
		if why := s.leadActRefusal(r, v, j, "closes it"); why != "" {
			fail(w, http.StatusForbidden, "%s", why)
			return
		}
		if err := s.authorize(ctx, r, v, j.Namespace, "patch", "workjobs", j.Name); err != nil {
			fail(w, http.StatusForbidden, "%v", err)
			return
		}
		if l := j.Status.Lead; l != nil && l.Closed != nil {
			writeJSON(w, http.StatusOK, api.CloseResponse{OK: true, State: "closed", Record: l.Closed.Control,
				Message: fmt.Sprintf("%s is closed: %s (%s, %s)", j.Name, l.Closed.Why, l.Closed.By, l.Closed.At.UTC().Format(time.RFC3339))})
			return
		}
		if c := closing(j); c != nil {
			writeJSON(w, http.StatusOK, api.CloseResponse{OK: true, State: "closing", Record: c.Record,
				Message: fmt.Sprintf("%s is closing, as %s asked", j.Name, c.Actor)})
			return
		}
		patch := client.MergeFromWithOptions(j.DeepCopy(), client.MergeFromWithOptimisticLock{})
		before := queue.SpecDigest(j)
		now := time.Now()
		token := strconv.FormatInt(now.UnixNano(), 36)
		change := map[string]any{"token": token}
		if why != "" {
			change["why"] = why
		}
		// Signed first, so the request names its record (controltrust.go).
		id, err := s.controlJob(ctx, r, v, rec, j, before, "close", change)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "control record: %v", err)
			return
		}
		b, _ := json.Marshal([]c4v1.WorkCloseRequest{{Token: token, Why: why, Actor: actorOf(r, v),
			At: metav1.Time{Time: now.UTC().Truncate(time.Second)}, Record: id}})
		j.Annotations[c4v1.AnnotationClose] = string(b)
		if err := s.Client.Patch(ctx, j, patch); err != nil {
			if apierrors.IsConflict(err) {
				continue // the job changed since it was read: decide again
			}
			fail(w, http.StatusServiceUnavailable, "writing the request: %v", err)
			return
		}
		msg := fmt.Sprintf("%s: closing; it takes no more workers and finishes once its units have", j.Name)
		note := msg
		if why != "" {
			note += ": " + why
		}
		if id != "" {
			note += "; record " + shortID(id)
		}
		if via(r) == "mcp" {
			note += "; via MCP"
		}
		s.record(ctx, j, "WorkJob", v.User, "Closing", note, bookNote{actor: actorOf(r, v), kind: "close", record: id})
		writeJSON(w, http.StatusOK, api.CloseResponse{OK: true, State: "closing", Record: id, Message: msg})
		return
	}
	fail(w, http.StatusConflict, "%s kept changing while it was closed: try again", rec.job.Name)
}

// leadOf is a lead's job's state as the HUD shows it (Job.Lead).
func leadOf(l *c4v1.WorkLead) *api.Lead {
	if l == nil {
		return nil
	}
	at := func(t *metav1.Time) *time.Time {
		if t == nil {
			return nil
		}
		u := t.Time.UTC()
		return &u
	}
	out := &api.Lead{Session: l.Session, State: l.State, IdleSince: at(l.IdleSince), ClosesAt: at(l.ClosesAt)}
	if c := l.Closed; c != nil {
		out.Closed = &api.Closed{At: c.At.Time.UTC(), By: c.By, Why: c.Why, Control: c.Control, Record: c.Record}
	}
	return out
}
