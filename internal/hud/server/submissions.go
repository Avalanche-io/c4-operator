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
		jobs, record, pending, err := s.keyed(ctx, plan.Namespace, label)
		switch {
		case err != nil:
			refuse(http.StatusServiceUnavailable, api.FieldError{Path: "key", Message: "the farm could not look this key up just now",
				Fix: "try again in a moment, with the same key"})
			return
		case pending:
			refuse(http.StatusConflict, api.FieldError{Path: "key", Message: "the submission sent with this key is still being made",
				Fix: "ask again in a moment, with the same key"})
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
	var taken []string
	for attempt := 0; ; attempt++ {
		jobs, code, ferr := s.makeSubmission(r, v, m, plan, &body, &out)
		if ferr == nil {
			out.Jobs, out.OK = jobs, true
			break
		}
		var te takenError
		if !errors.As(ferr.err, &te) || attempt >= 4 {
			fe := leftWords(s.farmWords(ferr.key, plan.Project, code, ferr.err), ferr.left, ferr.until)
			if errors.As(ferr.err, &te) {
				fe = fieldRefusal{api.FieldError{Path: "jobs." + ferr.key, Message: "job names kept being taken while the submission was made",
					Fix: "submit it again"}, http.StatusConflict}
			}
			refuse(fe.code, fe.FieldError)
			return
		}
		// A name taken meanwhile: number the submission past it.
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

// jobFailure is the job a submission failed at, and why.
type jobFailure struct {
	key, name string
	err       error
	// left are the jobs a rollback could not delete; until is when the
	// farm deletes those still held (submission_commit.go).
	left  []string
	until string
}

// makeSubmission checks every job of a plan as POST /jobs would, then
// plans them together (a dry run) or creates them in order. A create that
// fails deletes the jobs this attempt made before it returns.
func (s *Server) makeSubmission(r *http.Request, v *api.Viewer, m *model, plan *authoring.Plan, body *api.SubmissionRequest, out *api.SubmissionResponse) ([]api.SubmissionJob, int, *jobFailure) {
	ctx := r.Context()
	opts := func(j authoring.PlannedJob, record string) createOpts {
		o := createOpts{Labels: j.Labels, Annotations: map[string]string{}}
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
				resp.EstimateError = estErr.Error()
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
// admission policy, access and the API's own errors, never raw.
func (s *Server) farmWords(key, project string, code int, err error) fieldRefusal {
	path, msg := "jobs."+key, err.Error()
	switch {
	case strings.Contains(msg, "denied the request:"):
		_, why, _ := strings.Cut(msg, "denied the request:")
		return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("the farm's admission policy refused %s: %s", key, strings.TrimSpace(why)),
			Fix: "change the job as it says, or ask a farm admin"}, http.StatusUnprocessableEntity}
	case code == http.StatusForbidden || strings.Contains(msg, "is forbidden"):
		return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("you may not create jobs in %s", project),
			Fix: "ask a farm admin for access to " + project}, http.StatusForbidden}
	case code == http.StatusNotFound:
		return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("no project %q you can submit to", project),
			Fix: "c4 farm projects lists the ones you can submit to"}, http.StatusNotFound}
	case code >= 500 || errors.Is(err, ErrNotPlanned):
		return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("the farm could not make %s just now", key),
			Fix: "try again in a moment; nothing of the submission was kept"}, http.StatusServiceUnavailable}
	}
	return fieldRefusal{api.FieldError{Path: path, Message: fmt.Sprintf("the farm refused %s as lowered: %s", key, msg),
		Fix: "explain: true shows what it lowered to; a farm admin can say why the farm refused it"}, http.StatusUnprocessableEntity}
}

// what is a job in a line, with what reuse will skip.
func what(j authoring.PlannedJob, e *api.SubmitEstimate) string {
	s := j.What()
	if e != nil && e.Reused > 0 && j.Units > 0 {
		s += fmt.Sprintf("; %d of %d units reused", e.Reused, j.Units)
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
