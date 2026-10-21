package server

import (
	"bytes"
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
//     none of its units runs before the whole submission is made; while it
//     is pending nobody else changes its hold (handleNudge refuses it);
//  2. the submission is signed for once, when every job exists;
//  3. its jobs are released in two passes: the record goes on every job
//     while all are still held, then the holds and the pending marks come
//     off;
//  4. if any of that fails, the jobs made are deleted (each only while it
//     is still the job made, by UID), and a submission already signed
//     gets a signed withdrawal naming what was deleted and what was left.
//     A job that cannot be deleted is named in the refusal, and marked
//     failed so a retry does not wait for it.
//
// Steps 1 to 4 run on a context the client's hanging up does not end, and
// read the jobs they write from the API server, not the manager's cache. A
// HUD that stops midway leaves jobs pending past their time; the sweep
// deletes them with every job of their attempt, and withdraws a signed
// one. A request with a key (SubmissionRequest.Key) is answered once, for
// submission.keyFor: a retry is given the jobs the first made, not a
// second set, and the same key with another file is refused.
const (
	// AnnotationSubmissionPending on a WorkJob is when its submission
	// gives up on it (RFC 3339): set while the submission makes its jobs,
	// taken off when it releases them.
	AnnotationSubmissionPending = "c4.avalanche.io/submission-pending"
	// AnnotationSubmissionFailed marks a pending job its submission gave
	// up on and could not delete: the sweep takes it, and a retry with the
	// key does not wait for it.
	AnnotationSubmissionFailed = "c4.avalanche.io/submission-failed"
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
// release) once its client is gone; rollbackWindow its rollback, on a
// window of its own. submission.pendingFor is at least both (policy).
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

// reader reads what the HUD has just written: the API server when the
// manager gives one (Reader), not its cache, which may not have it yet.
func (s *Server) reader() client.Reader {
	if s.Reader != nil {
		return s.Reader
	}
	return s.Client
}

// pendingJob says whether a job is held by its submission still being
// made: marked pending, and not given up on.
func pendingJob(j *c4v1.WorkJob) bool {
	return j.Annotations[AnnotationSubmissionPending] != "" && j.Annotations[AnnotationSubmissionFailed] == ""
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
	label := ""
	if body.Key != "" {
		label = keyLabel(v.User, body.Key)
	}
	var made []madeJob
	fail := func(key, name string, code int, err error, record string) ([]api.SubmissionJob, int, *jobFailure) {
		left := s.rollback(r, v, plan, made, key, record, attempt)
		return nil, code, &jobFailure{key: key, name: name, err: err, left: left.left, until: until, unsigned: left.unsigned}
	}
	for i, j := range plan.Jobs {
		o := opts(j, "")
		o.EstimateOptional, o.Hold, o.Made = true, true, &c4v1.WorkJob{}
		o.Annotations[AnnotationSubmissionPending] = until
		o.Annotations[AnnotationSubmissionAttempt] = attempt
		if l, err := json.Marshal(submissionLine{I: i, Key: j.Key, Tool: j.Tool.Name, Version: j.Tool.Version, What: j.What(), After: j.After}); err == nil {
			o.Annotations[AnnotationSubmissionJob] = string(l)
		}
		if label != "" {
			if o.Labels == nil {
				o.Labels = map[string]string{}
			}
			o.Labels[LabelSubmissionKey] = label
		}
		resp, code, err := s.createJob(dr, v, m, *j.Request, o)
		if err != nil {
			return fail(j.Key, j.Request.Name, code, err, "")
		}
		made = append(made, madeJob{job: o.Made, resp: resp, plan: j})
	}
	// The submission's record, once, when its jobs exist: who submitted
	// which file (its C4 ID and the same bytes' SHA-256), with which key in
	// which attempt, and the jobs.
	names := make([]string, len(made))
	for i, mj := range made {
		names[i] = mj.job.Name
	}
	var doc bytes.Buffer
	if err := json.Compact(&doc, body.Submission); err != nil {
		doc.Reset()
		doc.Write(body.Submission)
	}
	sum := sha256.Sum256(doc.Bytes())
	change := map[string]any{"project": plan.Project, "jobs": names, "attempt": attempt}
	if label != "" {
		change["key"] = label
	}
	record, err := s.signControl(ctx, []string{plan.Project}, provenance.Subject{Name: "submission:" + out.Submission,
		Digest: map[string]string{"sha256": hex.EncodeToString(sum[:])}},
		controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Agent: claimedAgent(r),
			Authorization: s.authorization(v, "create", "workjobs", plan.Namespace, ""), Action: "submit",
			Target: "submission:" + out.Submission, Change: change})
	if err != nil {
		s.Log.Error(err, "sign a submission", "submission", out.Submission)
		return fail(plan.Jobs[0].Key, names[0], http.StatusServiceUnavailable, errTransient, "")
	}
	// Two passes, so a failure leaves every job held: the record on each,
	// then the holds off.
	for pass := range 2 {
		for _, mj := range made {
			if err := s.release(ctx, mj.job, record, pass == 1); err != nil {
				s.Log.Error(err, "release a submission's job", "job", mj.job.Namespace+"/"+mj.job.Name, "pass", pass)
				return fail(mj.plan.Key, mj.job.Name, http.StatusServiceUnavailable, errTransient, record)
			}
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

// release changes a submission's job as made (its UID), read from the API
// server and patched at the version read, tried again on a conflict.
// The first pass puts the record on; the second (run) takes the hold
// (unless the file asked for it) and the pending mark off.
func (s *Server) release(ctx context.Context, made *c4v1.WorkJob, record string, run bool) error {
	return retrying(ctx, func() (bool, error) {
		var cur c4v1.WorkJob
		if err := s.reader().Get(ctx, client.ObjectKeyFromObject(made), &cur); err != nil {
			return apierrors.IsNotFound(err), err
		}
		if cur.UID != made.UID {
			return true, fmt.Errorf("%s is no longer the job the submission made", made.Name)
		}
		patch := client.MergeFromWithOptions(cur.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if !run {
			if record == "" {
				return true, nil
			}
			cur.Annotations[authoring.AnnotationSubmissionRecord] = record
			return false, s.Client.Patch(ctx, &cur, patch)
		}
		if cur.Annotations[AnnotationSubmissionHoldAsked] == "" {
			cur.Spec.Nudge.Hold = false
		}
		delete(cur.Annotations, AnnotationSubmissionPending)
		delete(cur.Annotations, AnnotationSubmissionHoldAsked)
		return false, s.Client.Patch(ctx, &cur, patch)
	})
}

// rolledBack is what a rollback left: the jobs it could not delete, and
// whether a signed submission's withdrawal could not be signed.
type rolledBack struct {
	left     []string
	unsigned bool
}

// rollback deletes the jobs a submission made, each only while it is the
// job made (its UID), tried again within rollbackWindow (its own, so a
// submission that ran out of time can still be rolled back), and books
// each. A job it cannot delete is marked failed, so the sweep takes it
// and a retry does not wait for it. A submission already signed for
// (record) then gets a signed withdrawal by the farm, naming what was
// deleted and what was left. The Materializations of the jobs' input
// views stay: one view is one Materialization, which other jobs may share.
func (s *Server) rollback(r *http.Request, v *api.Viewer, plan *authoring.Plan, made []madeJob, failed, record, attempt string) rolledBack {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), rollbackWindow)
	defer cancel()
	var out rolledBack
	var deleted []*c4v1.WorkJob
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
			out.left = append(out.left, mj.job.Name)
			s.markFailed(ctx, mj.job)
			continue
		}
		deleted = append(deleted, mj.job)
	}
	withdrawal := ""
	if record != "" {
		var err error
		withdrawal, err = s.withdraw(ctx, plan.Project, plan.Namespace, record, attempt, failed, v.User, deleted, out.left)
		if err != nil {
			s.Log.Error(err, "sign a submission's withdrawal", "record", record)
			out.unsigned = true
		}
	}
	for _, j := range deleted {
		s.record(ctx, j, "WorkJob", v.User, "Withdrawn", j.Name+": deleted, since its submission's "+failed+" was refused",
			bookNote{actor: actorOf(r, v), kind: "submit", record: withdrawal})
	}
	return out
}

// withdraw signs, as the farm, that a signed submission's jobs were taken
// back: those deleted, and those it could not delete.
func (s *Server) withdraw(ctx context.Context, project, ns, record, attempt, why, submitter string, deleted []*c4v1.WorkJob, left []string) (string, error) {
	names := make([]string, len(deleted))
	for i, j := range deleted {
		names[i] = j.Name
	}
	change := map[string]any{"deleted": names, "attempt": attempt, "why": why, "submitter": submitter}
	if len(left) > 0 {
		change["left"] = left
	}
	return s.signControl(ctx, []string{project}, provenance.Subject{Name: "submission-record:" + record},
		controlPredicate{Actor: "c4-hud", Authenticated: "the farm", Via: "submission",
			Authorization: "the HUD's own account deletes workjobs in " + ns + " to take back a submission it could not finish", Action: "withdraw",
			Target: "submission-record:" + record, Change: change})
}

// markFailed marks a job its submission gave up on, best effort: the
// delete that failed may fail this too, and the sweep still takes it.
func (s *Server) markFailed(ctx context.Context, made *c4v1.WorkJob) {
	var cur c4v1.WorkJob
	if err := s.reader().Get(ctx, client.ObjectKeyFromObject(made), &cur); err != nil || cur.UID != made.UID {
		return
	}
	patch := client.MergeFrom(cur.DeepCopy())
	cur.Annotations[AnnotationSubmissionFailed] = "true"
	_ = s.Client.Patch(ctx, &cur, patch)
}

// leftWords adds to a refusal the jobs its rollback could not delete, and
// a withdrawal it could not sign.
func leftWords(fr fieldRefusal, left []string, until string, unsigned bool) fieldRefusal {
	if len(left) > 0 {
		them := "them"
		if len(left) == 1 {
			them = "it"
		}
		fr.Message += fmt.Sprintf("; %s could not be removed", strings.Join(left, ", "))
		fr.Fix = strings.Replace(fr.Fix, "; nothing of the submission was kept", "", 1)
		fr.Fix += fmt.Sprintf("; delete %s before submitting again (what is still held, the farm deletes after %s)", them, until)
	}
	if unsigned {
		fr.Message += "; the farm could not sign the withdrawal of the record it had signed (its book records the deletes)"
	}
	return fr
}

// keyLocks serializes requests with the same key on this replica, so a
// retry waits for the first and is answered with its jobs; a key's entry
// goes when nobody holds or waits for it.
type keyLocks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	n int
}

func (k *keyLocks) lock(label string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyLock{}
	}
	l := k.m[label]
	if l == nil {
		l = &keyLock{}
		k.m[label] = l
	}
	l.n++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.n--; l.n == 0 {
			delete(k.m, label)
		}
		k.mu.Unlock()
	}
}

func (k *keyLocks) size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}

// keyed is what a key made in ns within keyFor, read from the API server:
// the jobs its submission committed, in the order made, their record and
// the submission's C4 ID; pending when one is still being made (younger
// than the submission window and not given up on). Jobs a failed attempt
// left are neither.
func (s *Server) keyed(ctx context.Context, ns, label string, keyFor time.Duration) (jobs []api.SubmissionJob, record, submission string, pending bool, err error) {
	var list c4v1.WorkJobList
	if err := s.reader().List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{LabelSubmissionKey: label}); err != nil {
		return nil, "", "", false, err
	}
	type row struct {
		i   int
		job api.SubmissionJob
	}
	var rows []row
	now := time.Now()
	for _, j := range list.Items {
		// The API server stamps every object; an unstamped one is new.
		age := time.Duration(0)
		if !j.CreationTimestamp.IsZero() {
			age = now.Sub(j.CreationTimestamp.Time)
		}
		if j.DeletionTimestamp != nil || age > keyFor {
			continue
		}
		if j.Annotations[AnnotationSubmissionPending] != "" {
			if pendingJob(&j) && age < submissionWindow {
				pending = true
			}
			continue
		}
		var l submissionLine
		_ = json.Unmarshal([]byte(j.Annotations[AnnotationSubmissionJob]), &l)
		rows = append(rows, row{l.I, api.SubmissionJob{Key: l.Key, Namespace: j.Namespace, Name: j.Name, Tool: l.Tool, Version: l.Version,
			What: l.What, After: l.After, Link: "#/job/" + j.Namespace + "/" + j.Name}})
		record, submission = j.Annotations[authoring.AnnotationSubmissionRecord], j.Annotations[authoring.AnnotationSubmission]
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].i < rows[b].i })
	for _, r := range rows {
		jobs = append(jobs, r.job)
	}
	return jobs, record, submission, pending, nil
}

// sweepSubmissions deletes the jobs of submissions that never finished
// (the HUD making them stopped, or a rollback could not delete them): any
// job still pending past its time, with every job of its attempt, released
// or not, so nothing is held for good and nothing is half made. An
// attempt that was signed for is withdrawn, signed by the farm. Every
// submission.sweepEvery.
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
	type swept struct {
		project, ns, record string
		deleted             []*c4v1.WorkJob
		left                []string
	}
	attempts := map[string]*swept{}
	var order []string
	for _, rec := range m.jobs {
		a := rec.job.Annotations[AnnotationSubmissionAttempt]
		key := rec.job.Namespace + "/" + a
		if a == "" || !expired[key] {
			continue
		}
		sw := attempts[key]
		if sw == nil {
			sw = &swept{project: rec.project, ns: rec.job.Namespace}
			attempts[key] = sw
			order = append(order, key)
		}
		if r := rec.job.Annotations[authoring.AnnotationSubmissionRecord]; r != "" {
			sw.record = r
		}
		j := rec.job.DeepCopy()
		uid := j.UID
		err := s.Client.Delete(ctx, j, client.Preconditions{UID: &uid})
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			s.Log.Error(err, "delete an unfinished submission's job", "job", j.Namespace+"/"+j.Name)
			sw.left = append(sw.left, j.Name)
			continue
		}
		sw.deleted = append(sw.deleted, j)
	}
	for _, key := range order {
		sw := attempts[key]
		if len(sw.deleted) == 0 {
			continue
		}
		withdrawal := ""
		if sw.record != "" {
			_, a, _ := strings.Cut(key, "/")
			var err error
			if withdrawal, err = s.withdraw(ctx, sw.project, sw.ns, sw.record, a, "its submission never finished", "", sw.deleted, sw.left); err != nil {
				s.Log.Error(err, "sign an unfinished submission's withdrawal", "record", sw.record)
			}
		}
		for _, j := range sw.deleted {
			s.record(ctx, j, "WorkJob", "c4-hud", "Withdrawn", j.Name+": deleted, since its submission never finished",
				bookNote{kind: "submit", record: withdrawal})
		}
	}
}
