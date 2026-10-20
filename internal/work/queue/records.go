package queue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// The queue signs what it decides (DESIGN.md section 9.2): every
// preemption or yield, when it happens, and each job's end, sealing its
// output view, chain and record index.

// decisionRecord is one decision as the queue keeps it.
type decisionRecord struct {
	Unit   int
	At     time.Time
	Kind   string // yield, stop, safepoint-stop, safepoint-continue or safepoint-move
	Reason string
	For    string
	Record string
}

// SpecDigest names a job's spec as it is now: the SHA-256 of its JSON.
func SpecDigest(j *c4v1.WorkJob) string { return specDigest(j) }

// specDigest names a job's spec: the SHA-256 of its JSON.
func specDigest(j *c4v1.WorkJob) string {
	b, _ := json.Marshal(j.Spec)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func jobSubject(j *c4v1.WorkJob) provenance.Subject {
	return provenance.Subject{Name: "workjob:" + j.Namespace + "/" + j.Name, Digest: map[string]string{"sha256": specDigest(j)}}
}

// store puts a record into the job's project store and returns its ID.
func (q *Queue) storeRecord(ctx context.Context, v *view, rec []byte) (string, error) {
	if q.Stores == nil {
		return "", nil
	}
	st, err := q.Stores(ctx, v.project)
	if err != nil || st == nil {
		return "", err
	}
	id := c4.Identify(bytes.NewReader(rec))
	if err := st.Put(ctx, id, bytes.NewReader(rec), int64(len(rec))); err != nil {
		return "", err
	}
	return id.String(), nil
}

// recordDecision signs a preemption, a yield or a decision at a safe point
// and remembers it; extra adds to the signed predicate.
func (q *Queue) recordDecision(ctx context.Context, v *view, idx int, kind, reason, forJob string, now time.Time, extra ...map[string]any) {
	d := decisionRecord{Unit: idx, At: now, Kind: kind, Reason: reason, For: forJob}
	if q.Signer != nil {
		pred := map[string]any{"decision": kind, "job": v.key, "unit": idx, "reason": reason, "for": forJob,
			"policy": q.PolicyName, "at": now.UTC().Format(time.RFC3339)}
		for _, x := range extra {
			for k, val := range x {
				pred[k] = val
			}
		}
		rec, err := provenance.SignStatement(ctx, q.Signer, provenance.PredicateDecision, []provenance.Subject{jobSubject(v.job)}, pred, now)
		if err == nil {
			d.Record, err = q.storeRecord(ctx, v, rec)
		}
		if err != nil {
			q.Log.Error(err, "decision record", "job", v.key, "unit", idx)
		}
	}
	v.mem.decisions = append(v.mem.decisions, d)
}

// sealJob signs the job record once the job has ended.
func (q *Queue) sealJob(ctx context.Context, v *view, st *c4v1.WorkJobStatus, now time.Time) {
	m := v.mem
	if q.Signer == nil || m.jobRecord != "" || st.Output == nil || st.Output.View == "" {
		return
	}
	var subjects []provenance.Subject
	for name, id := range map[string]string{"output": st.Output.View, "chain": st.Output.Chain, "index": st.Output.Provenance} {
		if cid, err := c4.Parse(id); err == nil {
			subjects = append(subjects, provenance.SubjectFor(name, cid))
		}
	}
	subjects = append(subjects, jobSubject(v.job))
	var decisions []map[string]any
	for _, d := range m.decisions {
		decisions = append(decisions, map[string]any{"unit": d.Unit, "kind": d.Kind, "reason": d.Reason, "for": d.For, "record": d.Record})
	}
	pred := map[string]any{
		"job": v.key, "project": v.project, "class": v.job.Spec.Class, "phase": st.Phase,
		"units": st.Units, "changesets": st.Output.Changesets, "conflicts": st.Output.Conflicts,
		"decisions": decisions, "policy": q.PolicyName,
	}
	if c := v.job.Annotations[c4v1.AnnotationControlRecords]; c != "" {
		pred["controls"] = strings.Split(c, ",")
	}
	if st.Deadline != nil {
		pred["deadline"] = st.Deadline.Time.UTC().Format(time.RFC3339)
	}
	if st.Milestone != "" {
		pred["milestone"] = st.Milestone
	}
	// The tracker entity it was made for, and the version it follows, so
	// lineage can answer which task a version answered.
	if ref := v.job.Annotations[c4v1.AnnotationTrackerRef]; ref != "" {
		pred["trackerRef"] = ref
	}
	if f := v.job.Spec.Follows; f != "" {
		pred["follows"] = v.job.Namespace + "/" + f
	}
	if st.Finished != nil {
		pred["finished"] = st.Finished.Time.UTC().Format(time.RFC3339)
	}
	rec, err := provenance.SignStatement(ctx, q.Signer, provenance.PredicateJob, subjects, pred, now)
	if err == nil {
		m.jobRecord, err = q.storeRecord(ctx, v, rec)
	}
	if err != nil {
		q.Log.Error(err, "job record", "job", v.key)
	}
}
