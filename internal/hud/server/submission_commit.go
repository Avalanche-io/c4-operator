package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
)

// A submission is all or nothing (API.md, POST /submissions), whatever
// its client does and even if the HUD stops midway:
//
//  1. every job is created held (spec.nudge.hold), marked pending until a
//     time (submission.pendingFor) and with the attempt it belongs to, so
//     none of its units runs before the whole submission is made;
//  2. the submission is signed for once, when every job exists;
//  3. its jobs are released together: the hold and the pending mark come
//     off, the record goes on;
//  4. if any of that fails, the jobs made are deleted (each only while it
//     is still the job made, by UID), and a submission already signed
//     gets a signed withdrawal. A job that cannot be deleted is named in
//     the refusal.
//
// Steps 1 to 4 run on a context the client's hanging up does not end. A
// HUD that stops midway leaves jobs pending past their time; the sweep
// deletes them with every job of their attempt. A request with a key
// (SubmissionRequest.Key) is answered once: a retry is given the jobs the
// first made, not a second set.
const (
	// AnnotationSubmissionPending on a WorkJob is when its submission
	// gives up on it (RFC 3339): set while the submission makes its jobs,
	// taken off when it releases them.
	AnnotationSubmissionPending = "c4.avalanche.io/submission-pending"
	// AnnotationSubmissionAttempt is the attempt the job was made in: the
	// jobs a sweep deletes together.
	AnnotationSubmissionAttempt = "c4.avalanche.io/submission-attempt"
	// AnnotationSubmissionHoldAsked marks a job its submission asked to be
	// held (spec.nudge.hold): its release leaves it held.
	AnnotationSubmissionHoldAsked = "c4.avalanche.io/submission-hold-asked"
	// AnnotationSubmissionJob is the job's line in the submission's answer
	// (submissionLine), so a retry is answered as the first was.
	AnnotationSubmissionJob = "c4.avalanche.io/submission-job"
	// LabelSubmissionKey is the hash of the submitter and the request's
	// key (keyLabel): the jobs a retry is answered with.
	LabelSubmissionKey = "c4.avalanche.io/submission-key"
)

// submissionWindow bounds making a submission (its creates, record and
// release) once its client is gone; rollbackWindow its rollback.
const (
	submissionWindow = 2 * time.Minute
	rollbackWindow   = 30 * time.Second
)

// errTransient is a failure that is no fault of the submission: try again.
var errTransient = errors.New("the farm could not finish it just now")

// madeJob is a job a submission made: as created, its answer and plan.
type madeJob struct {
	job  *c4v1.WorkJob
	resp api.SubmitResponse
	plan authoring.PlannedJob
}

// submissionLine is what AnnotationSubmissionJob keeps of a job's line.
type submissionLine struct {
	I       int      `json:"i"`
	Key     string   `json:"key"`
	Tool    string   `json:"tool,omitempty"`
	Version string   `json:"version,omitempty"`
	What    string   `json:"what,omitempty"`
	After   []string `json:"after,omitempty"`
}

func newAttempt() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// keyLabel is the label value for a submitter's key: a hash, since a key
// is the client's text.
func keyLabel(user, key string) string {
	h := sha256.Sum256([]byte(user + "\x00" + key))
	return hex.EncodeToString(h[:16])
}

// projectObject is a project by name, for its policy values; nil when the
// model has none by that name.
func projectObject(m *model, name string) *c4v1.Project {
	if m == nil {
		return nil
	}
	for i := range m.projects {
		if m.projects[i].Name == name {
			return &m.projects[i]
		}
	}
	return nil
}

// commitSubmission makes a checked plan's jobs, as the comment above
// says. opts gives a job's create options; line its answer.
func (s *Server) commitSubmission(r *http.Request, v *api.Viewer, m *model, plan *authoring.Plan, body *api.SubmissionRequest,
	out *api.SubmissionResponse, opts func(authoring.PlannedJob, string) createOpts,
	line func(authoring.PlannedJob, api.SubmitResponse) api.SubmissionJob) ([]api.SubmissionJob, int, *jobFailure) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), submissionWindow)
	defer cancel()
	dr := r.WithContext(ctx)
	pendingFor, _ := policy.Duration(nil, projectObject(m, plan.Project), -1, policy.SubmissionPendingFor)
	until := time.Now().Add(pendingFor).UTC().Format(time.RFC3339)
	attempt := newAttempt()
	var made []madeJob
	for i, j := range plan.Jobs {
		o := opts(j, "")
		o.EstimateOptional, o.Hold, o.Made = true, true, &c4v1.WorkJob{}
		o.Annotations[AnnotationSubmissionPending] = until
		o.Annotations[AnnotationSubmissionAttempt] = attempt
		if l, err := json.Marshal(submissionLine{I: i, Key: j.Key, Tool: j.Tool.Name, Version: j.Tool.Version, What: j.What(), After: j.After}); err == nil {
			o.Annotations[AnnotationSubmissionJob] = string(l)
		}
		if body.Key != "" {
			if o.Labels == nil {
				o.Labels = map[string]string{}
			}
			o.Labels[LabelSubmissionKey] = keyLabel(v.User, body.Key)
		}
		resp, code, err := s.createJob(dr, v, m, *j.Request, o)
		if err != nil {
			left := s.rollback(ctx, dr, v, plan, made, j.Key, "")
			return nil, code, &jobFailure{key: j.Key, name: j.Request.Name, err: err, left: left, until: until}
		}
		made = append(made, madeJob{job: o.Made, resp: resp, plan: j})
	}
	// The submission's record, once, when its jobs exist: who submitted
	// which file, and the jobs.
	names := make([]string, len(made))
	for i, mj := range made {
		names[i] = mj.job.Name
	}
	sum := sha256.Sum256(body.Submission)
	record, err := s.signControl(ctx, []string{plan.Project}, provenance.Subject{Name: "submission:" + out.Submission,
		Digest: map[string]string{"sha256": hex.EncodeToString(sum[:])}},
		controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r), Via: via(r),
			Authorization: s.authorization("create", "workjobs", plan.Namespace, ""), Action: "submit",
			Target: "submission:" + out.Submission, Change: map[string]any{"project": plan.Project, "jobs": names}})
	if err != nil {
		s.Log.Error(err, "sign a submission", "submission", out.Submission)
		left := s.rollback(ctx, dr, v, plan, made, plan.Jobs[0].Key, "")
		return nil, http.StatusServiceUnavailable, &jobFailure{key: plan.Jobs[0].Key, name: names[0], err: errTransient, left: left, until: until}
	}
	for _, mj := range made {
		if err := s.release(ctx, mj.job, record); err != nil {
			s.Log.Error(err, "release a submission's job", "job", mj.job.Namespace+"/"+mj.job.Name)
			left := s.rollback(ctx, dr, v, plan, made, mj.plan.Key, record)
			return nil, http.StatusServiceUnavailable, &jobFailure{key: mj.plan.Key, name: mj.job.Name, err: errTransient, left: left, until: until}
		}
	}
	out.Record = record
	jobs := make([]api.SubmissionJob, len(made))
	for i, mj := range made {
		jobs[i] = line(mj.plan, mj.resp)
	}
	return jobs, http.StatusCreated, nil
}

// retrying runs f until it succeeds, a try says stop, or ctx ends: four
// tries, waiting 100ms, then 200ms, then 400ms.
func retrying(ctx context.Context, f func() (stop bool, err error)) error {
	var err error
	wait := 100 * time.Millisecond
	for try := 0; try < 4; try++ {
		var stop bool
		if stop, err = f(); err == nil || stop {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 2
	}
	return err
}

// release lets a submission's held job run: the hold (unless the file
// asked for it) and the pending mark come off, the record goes on. Only
// the job made is touched (its UID), and only as read (its resource
// version), tried again on a conflict.
func (s *Server) release(ctx context.Context, made *c4v1.WorkJob, record string) error {
	return retrying(ctx, func() (bool, error) {
		var cur c4v1.WorkJob
		if err := s.Client.Get(ctx, client.ObjectKeyFromObject(made), &cur); err != nil {
			return apierrors.IsNotFound(err), err
		}
		if cur.UID != made.UID {
			return true, fmt.Errorf("%s is no longer the job the submission made", made.Name)
		}
		patch := client.MergeFromWithOptions(cur.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if cur.Annotations[AnnotationSubmissionHoldAsked] == "" {
			cur.Spec.Nudge.Hold = false
		}
		delete(cur.Annotations, AnnotationSubmissionPending)
		delete(cur.Annotations, AnnotationSubmissionHoldAsked)
		if record != "" {
			cur.Annotations[authoring.AnnotationSubmissionRecord] = record
		}
		return false, s.Client.Patch(ctx, &cur, patch)
	})
}

// rollback deletes the jobs a submission made, each only while it is the
// job made (its UID), tried again within rollbackWindow, and books each.
// A submission already signed for (record) gets a signed withdrawal, which
// the book entries name. It returns the jobs it could not delete. The
// Materializations of their input views stay: one view is one
// Materialization, which other jobs may share.
func (s *Server) rollback(ctx context.Context, r *http.Request, v *api.Viewer, plan *authoring.Plan, made []madeJob, failed, record string) []string {
	ctx, cancel := context.WithTimeout(ctx, rollbackWindow)
	defer cancel()
	withdrawal := ""
	if record != "" {
		names := make([]string, len(made))
		for i, mj := range made {
			names[i] = mj.job.Name
		}
		var err error
		withdrawal, err = s.signControl(ctx, []string{plan.Project}, provenance.Subject{Name: "submission-record:" + record},
			controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r), Via: via(r),
				Authorization: s.authorization("delete", "workjobs", plan.Namespace, ""), Action: "withdraw",
				Target: "submission-record:" + record, Change: map[string]any{"jobs": names, "failed": failed}})
		if err != nil {
			s.Log.Error(err, "sign a submission's withdrawal", "record", record)
		}
	}
	var left []string
	for _, mj := range made {
		uid := mj.job.UID
		err := retrying(ctx, func() (bool, error) {
			err := s.Client.Delete(ctx, mj.job.DeepCopy(), client.Preconditions{UID: &uid})
			// Gone, or another job by that name now: not ours to delete.
			if err == nil || apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
				return true, nil
			}
			return false, err
		})
		if err != nil {
			s.Log.Error(err, "delete a refused submission's job", "job", mj.job.Namespace+"/"+mj.job.Name)
			left = append(left, mj.job.Name)
			continue
		}
		s.record(ctx, mj.job, "WorkJob", v.User, "Withdrawn", mj.job.Name+": deleted, since its submission's "+failed+" was refused",
			bookNote{actor: actorOf(r, v), kind: "submit", record: withdrawal})
	}
	return left
}

// leftWords adds to a refusal the jobs its rollback could not delete.
func leftWords(fr fieldRefusal, left []string, until string) fieldRefusal {
	if len(left) == 0 {
		return fr
	}
	them := "them"
	if len(left) == 1 {
		them = "it"
	}
	fr.Message += fmt.Sprintf("; %s could not be removed", strings.Join(left, ", "))
	fr.Fix = strings.Replace(fr.Fix, "; nothing of the submission was kept", "", 1)
	fr.Fix += fmt.Sprintf("; delete %s before submitting again (what is still held, the farm deletes after %s)", them, until)
	return fr
}

// keyLocks serializes requests with the same key on this replica, so a
// retry waits for the first and is answered with its jobs.
type keyLocks struct{ m sync.Map }

func (k *keyLocks) lock(label string) func() {
	mu, _ := k.m.LoadOrStore(label, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

// keyed is what a key already made in ns: the jobs, in the order made,
// and their record; pending when they are still being made.
func (s *Server) keyed(ctx context.Context, ns, label string) (jobs []api.SubmissionJob, record string, pending bool, err error) {
	var list c4v1.WorkJobList
	if err := s.Client.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{LabelSubmissionKey: label}); err != nil {
		return nil, "", false, err
	}
	type row struct {
		i   int
		job api.SubmissionJob
	}
	var rows []row
	for _, j := range list.Items {
		if j.DeletionTimestamp != nil {
			continue
		}
		if j.Annotations[AnnotationSubmissionPending] != "" {
			pending = true
		}
		var l submissionLine
		_ = json.Unmarshal([]byte(j.Annotations[AnnotationSubmissionJob]), &l)
		rows = append(rows, row{l.I, api.SubmissionJob{Key: l.Key, Namespace: j.Namespace, Name: j.Name, Tool: l.Tool, Version: l.Version,
			What: l.What, After: l.After, Link: "#/job/" + j.Namespace + "/" + j.Name}})
		record = j.Annotations[authoring.AnnotationSubmissionRecord]
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].i < rows[b].i })
	for _, r := range rows {
		jobs = append(jobs, r.job)
	}
	return jobs, record, pending, nil
}

// sweepSubmissions deletes the jobs of submissions that never finished
// (the HUD making them stopped): any job still pending past its time,
// with every job of its attempt, released or not, so nothing is held for
// good and nothing is half made. Every submission.sweepEvery.
func (s *Server) sweepSubmissions(ctx context.Context, now time.Time) {
	every, _ := policy.Duration(nil, nil, -1, policy.SubmissionSweepEvery)
	s.sweepMu.Lock()
	if now.Before(s.sweptAt.Add(every)) {
		s.sweepMu.Unlock()
		return
	}
	s.sweptAt = now
	s.sweepMu.Unlock()
	m := s.current()
	if m == nil || s.Client == nil {
		return
	}
	expired := map[string]bool{}
	for _, rec := range m.jobs {
		at, err := time.Parse(time.RFC3339, rec.job.Annotations[AnnotationSubmissionPending])
		if a := rec.job.Annotations[AnnotationSubmissionAttempt]; err == nil && a != "" && now.After(at) {
			expired[rec.job.Namespace+"/"+a] = true
		}
	}
	for _, rec := range m.jobs {
		a := rec.job.Annotations[AnnotationSubmissionAttempt]
		if a == "" || !expired[rec.job.Namespace+"/"+a] {
			continue
		}
		j := rec.job.DeepCopy()
		uid := j.UID
		err := s.Client.Delete(ctx, j, client.Preconditions{UID: &uid})
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			s.Log.Error(err, "delete an unfinished submission's job", "job", j.Namespace+"/"+j.Name)
			continue
		}
		s.record(ctx, j, "WorkJob", "c4-hud", "Withdrawn", j.Name+": deleted, since its submission never finished", bookNote{kind: "submit"})
	}
}
