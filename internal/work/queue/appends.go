package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
)

// Appended units (DESIGN 10.6, Appending units). A graph job's owner adds
// units to it while it runs (POST /jobs/{ns}/{name}/units): the HUD
// stores the units as an expansion document, the same a generator writes,
// signs the request and lists it in c4.avalanche.io/append under its
// idempotency key. The queue applies each key once, by the path a
// generator's expansion takes: the document is checked as one
// (checkExpansion: names, edges, the lead-only rule, the unit bound), and
// its units go at the end of the unit table with the next indexes. Their
// parent is the job's root, unit 0, one level down as the root's own
// children are; nothing that waited on the root waits on them, and only
// the job's review comes after them. Each key's outcome is signed
// (decision/v1, kind append) and listed in status.appends, so a restart
// replays the same documents to the same table and applies no key twice.
// A refused append adds nothing and says why there.

// appendRoot is the unit appended units hang under: the job's root.
const appendRoot = 0

// appendRec is one key applied (c4v1.WorkAppend).
type appendRec struct {
	Key, Doc, Control, Record, Error string
	First, Units                     int
	At                               time.Time
}

// appended says whether key was applied, added or refused.
func (m *jobMem) appended(key string) bool {
	for _, a := range m.appends {
		if a.Key == key {
			return true
		}
	}
	return false
}

// appendParent is the unit appended units are evaluated under: the root,
// as a plain generator (never a round's, whose children are named for
// the round).
func appendParent(p *class.Plan) class.Unit {
	u := p.Units[appendRoot]
	u.Rounds = nil
	return u
}

// appendRequests are a job's append requests, in token order.
func appendRequests(j *c4v1.WorkJob) []c4v1.WorkAppendRequest {
	raw := j.Annotations[c4v1.AnnotationAppend]
	if raw == "" {
		return nil
	}
	var reqs []c4v1.WorkAppendRequest
	if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
		return nil
	}
	sort.SliceStable(reqs, func(x, y int) bool { return tokenValue(reqs[x].Token) < tokenValue(reqs[y].Token) })
	return reqs
}

// applyAppends applies the job's append requests whose keys it has not
// (ctl: the job with only the requests the queue trusts). A document the
// store does not give back yet stops the pass, so keys apply in order;
// it is tried next tick.
func (q *Queue) applyAppends(ctx context.Context, v *view, ctl *c4v1.WorkJob, now time.Time) {
	m := v.mem
	for _, r := range appendRequests(ctl) {
		if r.Key == "" || m.appended(r.Key) {
			continue
		}
		var doc []byte
		if _, err := c4.Parse(r.Document); err == nil {
			b, err := q.expansionDoc(ctx, v.project, m, r.Document)
			if err != nil {
				q.Log.Error(err, "append document", "job", v.key, "key", r.Key)
				return
			}
			doc = b
		}
		q.appendWith(ctx, v, r, doc, now)
	}
}

// appendWith adds request r's units (doc, nil if its document is no C4
// ID), signs and lists the outcome, and returns it; its Error is set if
// the units were refused, and then nothing was added.
func (q *Queue) appendWith(ctx context.Context, v *view, r c4v1.WorkAppendRequest, doc []byte, now time.Time) appendRec {
	m := v.mem
	x := appendRec{Key: r.Key, Doc: r.Document, Control: r.Record, First: len(m.plan.Units), At: now}
	var lead leadOnlyError
	var err error
	switch phase := v.job.Status.Phase; {
	case doc == nil:
		err = fmt.Errorf("the document %q is not a C4 ID", r.Document)
	case v.driver.Name() != "graph":
		err = fmt.Errorf("%s is a %s job: only a graph job takes units while it runs", v.job.Name, v.job.Spec.Class)
	case m.lead.closed != nil:
		err = fmt.Errorf("%s is closed (%s): a closed lead's job takes no more workers; make a new job for them", v.job.Name, m.lead.closed.Why)
	case phase == "Succeeded" || phase == "Failed":
		err = fmt.Errorf("%s has finished (%s): a finished job takes no more units", v.job.Name, strings.ToLower(phase))
	default:
		var ex *class.Expansion
		if ex, err = q.checkUnits(v, appendRoot, appendParent(m.plan), doc, true); err == nil {
			m.plan.Apply(appendRoot, ex)
			x.Units = len(ex.Units)
			// a lead's job is not idle while its new workers run (lead.go)
			m.lead.idleSince = time.Time{}
		}
	}
	if err != nil {
		x.Error = err.Error()
		if errors.As(err, &lead) {
			x.Error = fmt.Sprintf("%s (%s would run agents, and this job was submitted by a unit): %s",
				c4v1.LeadOnlyMessage, strings.Join(lead.units, ", "), c4v1.LeadOnlyFix)
		}
	}
	x.Record = q.recordAppend(ctx, v, x, now, lead)
	m.appends = append(m.appends, x)
	q.Log.Info("appended", "job", v.key, "key", x.Key, "units", x.Units, "document", x.Doc, "error", x.Error)
	return x
}

// recordAppend signs an append: the request's key and control record, its
// document and what it added (decision/v1, kind append). A refusal of a
// unit's job's agent work also names the agent units, the fix and the
// policy value, as an expansion's does.
func (q *Queue) recordAppend(ctx context.Context, v *view, x appendRec, now time.Time, lead leadOnlyError) string {
	if q.Signer == nil {
		return ""
	}
	pred := map[string]any{"decision": "append", "job": v.key, "key": x.Key, "parent": appendRoot,
		"first": x.First, "units": x.Units, "policy": q.PolicyName, "at": now.UTC().Format(time.RFC3339)}
	subjects := []provenance.Subject{jobSubject(v.job)}
	if cid, err := c4.Parse(x.Doc); err == nil {
		pred["document"] = x.Doc
		subjects = append(subjects, provenance.SubjectFor("document", cid))
	}
	if x.Control != "" {
		pred["control"] = x.Control
	}
	if x.Error != "" {
		pred["error"] = x.Error
	}
	if len(lead.units) > 0 {
		pred["agentUnits"], pred["fix"] = lead.units, c4v1.LeadOnlyFix
		pred["policy"], pred["scheduler"] = policy.Record(lead.use), q.PolicyName
		pred["submittedByUnit"] = v.job.Annotations[c4v1.AnnotationSubmittedByUnit]
	}
	rec, err := provenance.SignStatement(ctx, q.Signer, provenance.PredicateDecision, subjects, pred, now)
	if err == nil {
		var id string
		if id, err = q.storeRecord(ctx, v, rec); err == nil {
			return id
		}
	}
	q.Log.Error(err, "append record", "job", v.key, "key", x.Key)
	return ""
}

func appendStatus(as []appendRec) []c4v1.WorkAppend {
	var out []c4v1.WorkAppend
	for _, a := range as {
		out = append(out, c4v1.WorkAppend{Key: a.Key, Document: a.Doc, First: int32(a.First), Units: int32(a.Units),
			At: metav1.Time{Time: a.At.Truncate(time.Second)}, Control: a.Control, Record: a.Record, Error: a.Error})
	}
	return out
}

func (m *jobMem) loadAppends(st *c4v1.WorkJobStatus) {
	for _, a := range st.Appends {
		m.appends = append(m.appends, appendRec{Key: a.Key, Doc: a.Document, Control: a.Control, Record: a.Record,
			Error: a.Error, First: int(a.First), Units: int(a.Units), At: a.At.Time})
	}
}

// replayAppend adds a recorded append's units to plan p again, from its
// document.
func replayAppend(d class.Driver, j *c4v1.WorkJob, p *class.Plan, a appendRec, doc []byte) error {
	if len(p.Units) == 0 || a.First != len(p.Units) {
		return fmt.Errorf("append %s does not fit the plan (it began at %d, the plan has %d units)", a.Key, a.First, len(p.Units))
	}
	ex, err := class.Expand(d, j, appendParent(p), doc, p.Names(), a.First)
	if err != nil {
		return fmt.Errorf("replaying append %s: %w", a.Key, err)
	}
	ex.Dependents = nil
	if len(ex.Units) != a.Units {
		return fmt.Errorf("replaying append %s: %d units, recorded %d", a.Key, len(ex.Units), a.Units)
	}
	p.Apply(appendRoot, ex)
	return nil
}
