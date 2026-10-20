package server

import (
	"bytes"
	"encoding/json"
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
// A refused submission creates nothing: every job is checked as POST
// /jobs would check it before the first is made.
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
		out.Errors = errs
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
			refuse(http.StatusNotFound, api.FieldError{Path: "project", Message: err.Error(), Fix: "c4 farm projects lists the ones you can submit to"})
			return
		}
	}
	// The farm reads trees by C4 ID; a folder is the front end's to upload.
	var views []api.FieldError
	trees := map[string]string{"tree": sub.Tree}
	for name, t := range sub.Read {
		trees["read."+name] = t
	}
	paths := make([]string, 0, len(trees))
	for p := range trees {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		t := trees[path]
		switch {
		case t == "" || strings.HasPrefix(t, "job:") || strings.HasPrefix(t, "git:"):
		case isC4ID(t):
			views = append(views, api.FieldError{Path: path, Message: t})
		default:
			refuse(http.StatusUnprocessableEntity, api.FieldError{Path: path,
				Message: fmt.Sprintf("%q is a folder on your machine, which the farm cannot read", t),
				Fix:     "upload it and give its C4 ID (POST /objects and /views); c4 farm submit does both"})
			return
		}
	}
	ctx := r.Context()
	plan, lerrs := authoring.Lower(sub, s.farmFor(ctx, m))
	if len(lerrs) > 0 {
		refuse(http.StatusUnprocessableEntity, errs(lerrs)...)
		return
	}
	if !body.DryRun {
		// A dry run plans without the tree's bytes; a job needs them.
		for _, f := range views {
			if !s.viewStored(r, plan.Project, f.Message) {
				refuse(http.StatusUnprocessableEntity, api.FieldError{Path: f.Path,
					Message: fmt.Sprintf("%s is not in %s's store", f.Message, plan.Project),
					Fix:     "upload the folder first (POST /objects and /views); c4 farm submit does"})
				return
			}
		}
	}
	opts := func(j authoring.PlannedJob) createOpts {
		o := createOpts{Labels: j.Labels, Annotations: map[string]string{}}
		for k, val := range j.Annotations {
			o.Annotations[k] = val
		}
		if out.Submission != "" {
			o.Annotations[authoring.AnnotationSubmission] = out.Submission
		}
		return o
	}
	jobErr := func(j authoring.PlannedJob, code int, err error) {
		if code == http.StatusBadRequest {
			code = http.StatusUnprocessableEntity
		}
		refuse(code, api.FieldError{Path: "jobs." + j.Key, Message: err.Error(),
			Fix: "the farm refused the job as lowered; explain: true shows what it was"})
	}
	// Every job is checked before any is created.
	for _, j := range plan.Jobs {
		o := opts(j)
		o.CheckOnly = true
		if _, code, err := s.createJob(r, v, m, *j.Request, o); err != nil {
			jobErr(j, code, err)
			return
		}
	}
	out.Warnings = plan.Warnings
	if body.Explain {
		out.Explain = authoring.Explain(plan)
	}
	for _, j := range plan.Jobs {
		req := *j.Request
		req.DryRun = body.DryRun
		o := opts(j)
		o.EstimateOptional = true
		resp, code, err := s.createJob(r, v, m, req, o)
		if err != nil {
			if len(out.Jobs) > 0 {
				out.Message = fmt.Sprintf("%d of %d jobs created before %s was refused", len(out.Jobs), len(plan.Jobs), j.Key)
			}
			jobErr(j, code, err)
			return
		}
		out.Jobs = append(out.Jobs, api.SubmissionJob{Key: j.Key, Namespace: resp.Namespace, Name: resp.Name,
			Tool: j.Tool.Name, Version: j.Tool.Version, What: j.What(), Estimate: resp.Estimate,
			EstimateError: resp.EstimateError, After: j.After, Link: resp.Link})
	}
	out.OK = true
	out.Estimate = submissionEstimate(out.Jobs)
	code, verb := http.StatusCreated, "submitted"
	if body.DryRun {
		code, verb = http.StatusOK, "planned, not submitted"
	}
	names := make([]string, len(out.Jobs))
	for i, j := range out.Jobs {
		names[i] = j.Name
	}
	out.Message = fmt.Sprintf("%s: %s", strings.Join(names, ", "), verb)
	writeJSON(w, code, out)
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
