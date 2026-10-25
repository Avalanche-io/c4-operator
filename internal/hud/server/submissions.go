package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// handleSubmission is POST /submissions: a submission checked, lowered to
// one job per key, and planned (dryRun) or created in the order written.
// It is all or nothing: every job is checked as POST /jobs would check it
// before the first is made, and if any create fails the jobs already made
// are deleted. A name taken meanwhile is numbered past and the jobs made
// again. Every refusal is a path, a message and a fix in the farm's words.
func (s *Server) handleSubmission(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, jobBody))
	if err != nil {
		failBody(w, err)
		return
	}
	var body api.SubmissionRequest
	if err := class.Strict(raw, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	if len(bytes.TrimSpace(body.Submission)) == 0 {
		fail(w, http.StatusBadRequest, "body: submission is required: the submission as JSON")
		return
	}
	out := api.SubmissionResponse{DryRun: body.DryRun}
	var doc bytes.Buffer
	if err := json.Compact(&doc, body.Submission); err == nil {
		out.Submission = c4.Identify(bytes.NewReader(doc.Bytes())).String()
	}
	refuse := func(code int, errs ...api.FieldError) {
		sort.SliceStable(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
		out.OK, out.Jobs, out.Estimate, out.Record, out.Errors = false, nil, nil, "", errs
		writeJSON(w, code, out)
	}
	sub, perrs := authoring.ParseSubmission(body.Submission)
	if len(perrs) > 0 {
		refuse(http.StatusUnprocessableEntity, errs(perrs)...)
		return
	}
	if sub.Project == "" && len(v.Projects) == 1 {
		sub.Project = v.Projects[0]
	}
	if sub.Project != "" {
		if _, _, err := s.project(m, v, sub.Project, ""); err != nil {
			refuse(http.StatusNotFound, api.FieldError{Path: "project", Message: fmt.Sprintf("no project %q you can submit to", sub.Project),
				Fix: "c4 farm projects lists the ones you can submit to"})
			return
		}
	}
	ctx := r.Context()
	farm := s.farmFor(ctx, m)
	plan, lerrs := authoring.Lower(sub, farm)
	// The farm reads trees by C4 ID; a folder is the front end's to upload.
	// It is refused beside the file's other errors, not before them.
	refusals := errs(lerrs)
	var views []api.FieldError
	trees := map[string]string{"tree": sub.Tree}
	for name, t := range sub.Read {
		trees["read."+name] = t
	}
	for path, t := range trees {
		switch {
		case t == "" || strings.HasPrefix(t, "job:") || strings.HasPrefix(t, "git:"):
		case isC4ID(t):
			views = append(views, api.FieldError{Path: path, Message: t})
		default:
			refusals = append(refusals, api.FieldError{Path: path,
				Message: fmt.Sprintf("%q is a folder on your machine, which the farm cannot read", t),
				Fix:     "upload it and give its C4 ID (POST /objects and /views); c4 farm submit does both"})
		}
	}
	if len(refusals) > 0 {
		refuse(http.StatusUnprocessableEntity, refusals...)
		return
	}
	if !body.DryRun {
		// A dry run plans without the tree's bytes; a job needs them.
		for _, f := range views {
			if !s.viewStored(r, plan.Project, f.Message) {
				refusals = append(refusals, api.FieldError{Path: f.Path,
					Message: fmt.Sprintf("%s is not in %s's store", f.Message, plan.Project),
					Fix:     "upload the folder first (POST /objects and /views); c4 farm submit does"})
			}
		}
		if len(refusals) > 0 {
			refuse(http.StatusUnprocessableEntity, refusals...)
			return
		}
	}
	out.Warnings = plan.Warnings
	if !body.DryRun && body.Key != "" {
		// A request with a key is answered once: a retry is given the
		// jobs the first made (submission_commit.go).
		label := keyLabel(v.User, body.Key)
		defer s.submitKeys.lock(label)()
		keyFor, _ := policy.Duration(nil, projectObject(m, plan.Project), -1, policy.SubmissionKeyFor)
		jobs, record, made, pending, err := s.keyed(ctx, plan.Namespace, label, keyFor)
		switch {
		case err != nil:
			refuse(http.StatusServiceUnavailable, api.FieldError{Path: "key", Message: "the farm could not look this key up just now",
				Fix: "try again in a moment, with the same key"})
			return
		case pending:
			refuse(http.StatusConflict, api.FieldError{Path: "key", Message: "the submission sent with this key is still being made",
				Fix: "ask again in a moment, with the same key"})
			return
		case len(jobs) > 0 && made != out.Submission:
			refuse(http.StatusConflict, api.FieldError{Path: "key", Message: fmt.Sprintf("this key was sent with another submission (%s)", made),
				Fix: "give this submission a key of its own; a key names one submission, sent again only on a retry"})
			return
		case len(jobs) > 0:
			out.Jobs, out.OK, out.Record = jobs, true, record
			names := make([]string, len(jobs))
			for i, j := range jobs {
				names[i] = plan.Project + "/" + j.Name
			}
			out.Message = fmt.Sprintf("%s: submitted already, with this key", strings.Join(names, ", "))
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	var taken, leftover []string
	for attempt := 0; ; attempt++ {
		jobs, code, ferr := s.makeSubmission(r, v, m, plan, &body, &out)
		if ferr == nil {
			out.Jobs, out.OK = jobs, true
			break
		}
		var te takenError
		if errors.As(ferr.err, &te) && pinned(plan, ferr.key) {
			// a version the file gives is never numbered again
			refuse(http.StatusConflict, api.FieldError{Path: "jobs." + ferr.key + ".version",
				Message: fmt.Sprintf("%s exists; leave version out for the next one", ferr.name),
				Fix:     "leave version out, or give a later one"})
			return
		}
		if !errors.As(ferr.err, &te) || attempt >= 4 {
			fe := leftWords(s.farmWords(ferr.key, ferr.name, plan.Project, code, ferr.err), append(leftover, ferr.left...), ferr.until, ferr.unsigned)
			if errors.As(ferr.err, &te) {
				fe = fieldRefusal{api.FieldError{Path: "jobs." + ferr.key, Message: "job names kept being taken while the submission was made",
					Fix: "submit it again"}, http.StatusConflict}
			}
			refuse(fe.code, fe.FieldError)
			return
		}
		// A name taken meanwhile: number the submission past it. What the
		// attempt could not delete is reported, not dropped.
		leftover = append(leftover, ferr.left...)
		if len(ferr.left) > 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s, from an attempt numbered again, could not be removed: delete it (the farm deletes what is still held after %s)",
				strings.Join(ferr.left, ", "), ferr.until))
		}
		taken = append(taken, ferr.name)
		var again []authoring.Error
		if plan, again = authoring.Lower(sub, authoring.Taking(farm, plan.Namespace, taken)); len(again) > 0 {
			refuse(http.StatusUnprocessableEntity, errs(again)...)
			return
		}
	}
	if body.Explain {
		out.Explain = authoring.Explain(plan)
	}
	out.Estimate = submissionEstimate(out.Jobs)
	code, verb := http.StatusCreated, "submitted"
	if body.DryRun {
		code, verb = http.StatusOK, "planned, not submitted"
	}
	names := make([]string, len(out.Jobs))
	for i, j := range out.Jobs {
		names[i] = plan.Project + "/" + j.Name
	}
	out.Message = fmt.Sprintf("%s: %s", strings.Join(names, ", "), verb)
	writeJSON(w, code, out)
}

// pinned says whether the file gives a job's version.
func pinned(plan *authoring.Plan, key string) bool {
	for _, j := range plan.Jobs {
		if j.Key == key {
			return j.Pinned
		}
	}
	return false
}

// jobFailure is the job a submission failed at, and why.
type jobFailure struct {
	key, name string
	err       error
	// left are the jobs a rollback could not delete; until is when the
	// farm deletes those still held (submission_commit.go).
	left  []string
	until string
	// unsigned says a signed submission's withdrawal could not be signed.
	unsigned bool
}

// makeSubmission checks every job of a plan as POST /jobs would, then
// plans them together (a dry run) or creates them in order. A create that
// fails deletes the jobs this attempt made before it returns.
func (s *Server) makeSubmission(r *http.Request, v *api.Viewer, m *model, plan *authoring.Plan, body *api.SubmissionRequest, out *api.SubmissionResponse) ([]api.SubmissionJob, int, *jobFailure) {
	ctx := r.Context()
	opts := func(j authoring.PlannedJob, record string) createOpts {
		// The request is authoring.Lower's, made here: its spec.tool is
		// the farm's.
		o := createOpts{Labels: j.Labels, Annotations: map[string]string{}, Lowered: true}
		for k, val := range j.Annotations {
			o.Annotations[k] = val
		}
		var book []string
		if out.Submission != "" {
			o.Annotations[authoring.AnnotationSubmission] = out.Submission
			book = append(book, "submission "+out.Submission)
		}
		if record != "" {
			o.Annotations[authoring.AnnotationSubmissionRecord] = record
		}
		if j.Note != "" {
			o.Annotations[authoring.AnnotationNote] = j.Note
			book = append(book, "note: "+j.Note)
		}
		o.Book = strings.Join(book, "; ")
		return o
	}
	// Every job is checked before any is made.
	drafts := make([]*c4v1.WorkJob, len(plan.Jobs))
	views := map[string]string{}
	for i, j := range plan.Jobs {
		o := opts(j, "")
		o.CheckOnly, o.Draft = true, &c4v1.WorkJob{}
		if _, code, err := s.createJob(r, v, m, *j.Request, o); err != nil {
			return nil, code, &jobFailure{key: j.Key, name: j.Request.Name, err: err}
		}
		drafts[i] = o.Draft
		for _, in := range j.Request.Inputs {
			if in.View != "" {
				views[inputMaterialization(in.View)] = in.View
			}
		}
	}
	job := func(j authoring.PlannedJob, resp api.SubmitResponse) api.SubmissionJob {
		return api.SubmissionJob{Key: j.Key, Namespace: resp.Namespace, Name: resp.Name, Tool: j.Tool.Name, Version: j.Tool.Version,
			What: what(j, resp.Estimate), Estimate: resp.Estimate, EstimateError: resp.EstimateError, After: j.After, Link: resp.Link}
	}
	var jobs []api.SubmissionJob
	if body.DryRun {
		ests, estErr := s.planTogether(ctx, drafts, plan.Project, views)
		for i, j := range plan.Jobs {
			resp := api.SubmitResponse{Namespace: plan.Namespace, Name: j.Request.Name, Link: "#/job/" + plan.Namespace + "/" + j.Request.Name}
			if estErr != nil {
				resp.EstimateError = s.estimateWords(estErr)
			} else {
				e := ests[i]
				resp.Estimate = &e
			}
			jobs = append(jobs, job(j, resp))
		}
		return jobs, http.StatusOK, nil
	}
	// Made held, signed for once, released together; rolled back if
	// any of it fails, whatever the client does (submission_commit.go).
	return s.commitSubmission(r, v, m, plan, body, out, opts, job)
}

// planTogether plans a submission's drafts together (DryRunAll), else
// each alone.
func (s *Server) planTogether(ctx context.Context, drafts []*c4v1.WorkJob, project string, views map[string]string) ([]api.SubmitEstimate, error) {
	if s.DryRunAll != nil {
		return s.DryRunAll(ctx, drafts, project, views)
	}
	out := make([]api.SubmitEstimate, len(drafts))
	for i, d := range drafts {
		e, err := s.dryRun(ctx, d, project)
		if err != nil {
			return nil, err
		}
		out[i] = *e
	}
	return out, nil
}

// fieldRefusal is a refusal and its status.
type fieldRefusal struct {
	api.FieldError
	code int
}

// farmWords is a create's failure as the farm says it, at jobs.<key>:
// admission policy, access, a grant, the API's own errors, never raw.
func (s *Server) farmWords(key, name, project string, code int, err error) fieldRefusal {
	path, msg := "jobs."+key, err.Error()
	var gr *grantRefused
	if errors.As(err, &gr) {
		// A submission for a person, refused as POST /jobs refuses it,
		// with the same reason, so one handler serves both: at for:, or
		// at tracker: for a farm with no trackers.
		at := "for"
		if gr.reason == api.RefusedNotInstalled && strings.Contains(gr.msg, "trackers") || gr.reason == api.RefusedNoTracker {
			at = "tracker"
		}
		m := gr.msg
		if gr.code == http.StatusNotImplemented {
			// said once, as what this farm lacks, never as a thing to retry
			m += ", so submitting for a person is not available on this farm"
		}
		return fieldRefusal{api.FieldError{Path: at, Message: m, Fix: grantFix[gr.reason], Reason: gr.reason}, gr.code}
	}
	var ge *errGrant
	if errors.As(err, &ge) {
		// the grant's own refusal, at the field that asked for it
		at, m, fix := "for", ge.msg, "leave for: out, or ask a farm admin about the grant"
		if ge.code == http.StatusNotImplemented {
			at, m, fix = "tracker", ge.msg+", so trackers are not available on this farm", "leave tracker: out on this farm"
		}
		return fieldRefusal{api.FieldError{Path: at, Message: m, Fix: fix}, ge.code}
	}
	var ie identityError
	if errors.As(err, &ie) {
		return fieldRefusal{api.FieldError{Path: path, Message: ie.Error(),
			Fix: "a unit submits only what its tool grants, for its own job's owner"}, http.StatusForbidden}
	}
	switch {
	case strings.Contains(msg, "RFC 1123"):
		return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("%s can't name a job: use letters, digits and -", name),
			Fix: "set name: to override"}, http.StatusUnprocessableEntity}
	case strings.Contains(msg, "denied the request:"):
		_, why, _ := strings.Cut(msg, "denied the request:")
		return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("the farm's admission policy refused %s: %s", key, strings.TrimSpace(why)),
			Fix: "change the job as it says, or ask a farm admin"}, http.StatusUnprocessableEntity}
	case transient(err, code):
		return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("the farm could not make %s just now", key),
			Fix: "try again in a moment"}, http.StatusServiceUnavailable}
	case code == http.StatusForbidden || apierrors.IsForbidden(err):
		return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("you may not create jobs in %s", project),
			Fix: "ask a farm admin for access to " + project}, http.StatusForbidden}
	case code == http.StatusNotFound:
		return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("no project %q you can submit to", project),
			Fix: "c4 farm projects lists the ones you can submit to"}, http.StatusNotFound}
	}
	// Anything else is the farm's to explain, not the caller's to read.
	s.Log.Error(err, "a submission's job refused as lowered", "job", key, "code", code)
	return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("the farm could not make %s as lowered", key),
		Fix: "explain: true shows what it lowered to; the HUD's log says why the farm refused it"}, http.StatusUnprocessableEntity}
}

// transient says whether a failure is the API's for the moment (too many
// requests, a timeout, a server error), not the submission's.
func transient(err error, code int) bool {
	return errors.Is(err, ErrNotPlanned) || errors.Is(err, context.DeadlineExceeded) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) || code >= 500 && code != http.StatusNotImplemented
}

// estimateWords is why a dry run has no estimate, in the farm's words.
func (s *Server) estimateWords(err error) string {
	if errors.Is(err, ErrNotPlanned) {
		return "the farm has not planned its work yet; ask again in a moment"
	}
	s.Log.Error(err, "a submission's dry run")
	return "the farm could not estimate it just now"
}

// what is a job in a line, with what reuse will skip: the estimate's
// units, which a once job has too.
func what(j authoring.PlannedJob, e *api.SubmitEstimate) string {
	s := j.What()
	if e != nil && e.Reused > 0 && e.Units > 0 {
		s += fmt.Sprintf("; %d of %s reused", e.Reused, authoring.Count(e.Units, "unit"))
	}
	return s
}

// submissionEstimate is the jobs' estimates as one: the latest finish,
// the earliest deadline, the least slack.
func submissionEstimate(jobs []api.SubmissionJob) *api.SubmissionEstimate {
	var out *api.SubmissionEstimate
	later := func(a, b *time.Time) *time.Time {
		if a == nil || b != nil && b.After(*a) {
			return b
		}
		return a
	}
	for _, j := range jobs {
		e := j.Estimate
		if e == nil {
			continue
		}
		if out == nil {
			out = &api.SubmissionEstimate{SlackSeconds: e.SlackSeconds, Feasible: true}
		}
		out.Jobs++
		out.ETA50, out.ETA90 = later(out.ETA50, e.ETA50), later(out.ETA90, e.ETA90)
		if e.Deadline != nil && (out.Deadline == nil || e.Deadline.Before(*out.Deadline)) {
			out.Deadline = e.Deadline
		}
		out.SlackSeconds = min(out.SlackSeconds, e.SlackSeconds)
		out.Feasible = out.Feasible && e.Feasible
		out.AtRisk = out.AtRisk || e.AtRisk
	}
	return out
}

// viewStored says whether a view's record is in the project's store.
func (s *Server) viewStored(r *http.Request, project, view string) bool {
	id, err := c4.Parse(view)
	if err != nil || s.Stores == nil {
		return false
	}
	st, err := s.Stores(r.Context(), project)
	if err != nil || st == nil {
		return false
	}
	_, err = st.Head(r.Context(), id)
	return err == nil
}

func isC4ID(s string) bool {
	_, err := c4.Parse(s)
	return err == nil
}

// handleProject is GET /projects/{project}: the project as a submitter
// writes for it: where its trees mount, its time zone and due time, and
// the milestones a submission may be due at.
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	project, ns, err := s.project(m, v, r.PathValue("project"), "")
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	out := api.ProjectDetail{Name: project, Root: c4v1.DefaultProjectRoot, TimeZone: "UTC", DueTime: "18:00", Milestones: []api.ProjectMilestone{}}
	for _, p := range m.projects {
		if p.Name != project {
			continue
		}
		out.Root = p.Spec.TreeRoot()
		if p.Spec.Work != nil && p.Spec.Work.DueTime != nil {
			out.DueTime = p.Spec.Work.DueTime.At
			if tz := p.Spec.Work.DueTime.TimeZone; tz != "" {
				out.TimeZone = tz
			}
		}
	}
	var l c4v1.MilestoneList
	if s.Client != nil && s.Client.List(r.Context(), &l) == nil {
		for _, ms := range l.Items {
			if ms.Namespace != ns {
				continue
			}
			pm := api.ProjectMilestone{Name: ms.Name, At: ms.Spec.Due.Time}
			if src := ms.Spec.Source; src != nil {
				pm.Label = src.Tracker + "/" + src.Ref
			}
			out.Milestones = append(out.Milestones, pm)
		}
	}
	sort.Slice(out.Milestones, func(i, j int) bool {
		a, b := out.Milestones[i], out.Milestones[j]
		return a.At.Before(b.At) || a.At.Equal(b.At) && a.Name < b.Name
	})
	writeJSON(w, http.StatusOK, out)
}

// grantFix is what to do about each grant refusal of a submission for a
// person.
var grantFix = map[string]string{
	api.RefusedNoGrant:        "the person lets the tracker submit for them with c4 farm grant <tracker>",
	api.RefusedGrantLapsed:    "the person grants the tracker again with c4 farm grant <tracker>",
	api.RefusedGrantUncovered: "the person widens their grant's pools or classes with c4 farm grant <tracker>, or the job narrows to them",
	api.RefusedNoTracker:      "name a tracker of the project in tracker:",
	api.RefusedNotSync:        "leave for: out: only a tracker's sync submits for another person",
	api.RefusedNotInstalled:   "leave for: out: this farm has no trackers or grants",
}
