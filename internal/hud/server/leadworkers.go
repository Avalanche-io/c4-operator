package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
	"github.com/Avalanche-io/c4-operator/internal/plan"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
)

// A lead's workers (LEAD.md stage 2, c4_agent): the lead puts workers on
// the farm with one call, as it would start subagents. The first call
// makes the lead's job (a graph job named for the person and the session,
// owned by the person, answered by its lead); every later call adds to
// it. Each call's workers are lowered as a plan, exactly as a submission's
// plan is, so the farm checks a lead's workers as it checks any agent
// work: the job is made through the submission's own path (checked,
// signed, held and released), and later workers through the job's append
// (appendunits.go), signed once per call.

// maxLeadWorkers bounds the workers one call adds.
const maxLeadWorkers = 16

// maxLeadPrompt bounds a worker's brief, in bytes.
const maxLeadPrompt = 64 << 10

// leadPlanFile is the name a call's plan travels under.
const leadPlanFile = "lead-workers.json"

// unsafeName finds what a unit name may not hold.
var unsafeName = regexp.MustCompile(`[^a-z0-9-]+`)

// handleLeadWorkers is POST /lead/workers.
func (s *Server) handleLeadWorkers(w http.ResponseWriter, r *http.Request) {
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
	var req api.LeadWorkersRequest
	if err := class.Strict(raw, &req); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	session := claimedAgent(r)
	switch {
	case v.Unit != nil:
		fail(w, http.StatusForbidden, "%s", leadOnlyMessage)
		return
	case session == "":
		fail(w, http.StatusBadRequest, "a lead's workers come from the lead's own session: the request names none in its agent header (X-C4-Agent)")
		return
	case strings.TrimSpace(req.Key) == "":
		fail(w, http.StatusBadRequest, "key: give each call a key of its own, so a retry adds nothing twice")
		return
	}
	if msg := checkLeadRoot(req.Root); msg != "" {
		fail(w, http.StatusBadRequest, "%s", msg)
		return
	}
	if msg := checkLeadWorkers(req.Workers); msg != "" {
		fail(w, http.StatusBadRequest, "%s", msg)
		return
	}
	if req.Project == "" && len(v.Projects) == 1 {
		req.Project = v.Projects[0]
	}
	if req.Project == "" {
		fail(w, http.StatusBadRequest, "project: name the project the lead's job is in")
		return
	}
	if _, _, err := s.project(m, v, req.Project, ""); err != nil {
		fail(w, http.StatusNotFound, "no project %q you can submit to", req.Project)
		return
	}
	// one find-or-make at a time for a lead, so two first calls make one
	// job between them
	defer s.submitKeys.lock(keyLabel(v.User, "lead\x00"+session))()

	sub, in, refusals := leadSubmission(v, req)
	if len(refusals) > 0 {
		writeLeadRefusal(w, http.StatusUnprocessableEntity, refusals)
		return
	}
	pl, lerrs := authoring.LowerWith(sub, s.farmFor(r.Context(), m), in)
	if len(lerrs) > 0 {
		writeLeadRefusal(w, http.StatusUnprocessableEntity, errs(lerrs))
		return
	}
	// a call again with the key that made the job answers as it did
	keyFor, _ := policy.Duration(nil, projectObject(m, pl.Project), -1, policy.SubmissionKeyFor)
	made, record, _, pending, err := s.keyed(r.Context(), pl.Namespace, keyLabel(v.User, req.Key), keyFor)
	switch {
	case err != nil:
		fail(w, http.StatusServiceUnavailable, "the farm could not look this key up just now; try again in a moment, with the same key")
		return
	case pending:
		fail(w, http.StatusConflict, "the lead's job sent with this key is still being made; ask again in a moment, with the same key")
		return
	case len(made) == 1:
		out := api.LeadWorkersResponse{Job: made[0].Namespace + "/" + made[0].Name, Created: true, State: "added", Record: record,
			Units: leadUnits(req), Message: made[0].Namespace + "/" + made[0].Name + ": made already, with this key"}
		writeJSON(w, http.StatusOK, out)
		return
	}
	owner := s.ownerFor(r.Context(), v)
	open, n := openLeadJob(m, req.Project, owner, session)
	if open == nil {
		out, code, err := s.makeLeadJob(r, v, m, pl, req, owner, session, n)
		var te takenError
		if !errors.As(err, &te) {
			if err != nil {
				fail(w, code, "%v", err)
				return
			}
			writeJSON(w, code, out)
			return
		}
		// made by a call this model has not seen yet: add to it, if it is
		// this lead's
		name := leadJobName(owner, session, n)
		j, gerr := s.liveJob(r.Context(), pl.Namespace, name)
		if gerr != nil {
			fail(w, http.StatusConflict, "%s was made meanwhile but cannot be read: %v; try again", name, gerr)
			return
		}
		open = j
	}
	if msg := s.leadActRefusal(r, v, open, "adds workers to it"); msg != "" {
		fail(w, http.StatusForbidden, "%s", msg)
		return
	}
	if msg := closedRefusal(open); msg != "" {
		fail(w, http.StatusConflict, "%s", msg)
		return
	}
	out, code, err := s.addLeadWorkers(r, v, open, pl, req)
	if err != nil {
		fail(w, code, "%v", err)
		return
	}
	writeJSON(w, code, out)
}

// homeFolder finds a folder that is a person's home folder itself.
var homeFolder = regexp.MustCompile(`^(/Users|/home)/[^/]+/?$|^[A-Za-z]:[/\\]Users[/\\][^/\\]+[/\\]?$`)

// checkLeadRoot says what is wrong with the folder a lead names for its
// workers' worktrees, or "": an absolute folder, not a disk's root or a
// home folder. The host runner holds it to a project's folder with links
// resolved; this refuses the plain mistakes first.
func checkLeadRoot(root string) string {
	if root == "" {
		return ""
	}
	clean := path.Clean(strings.ReplaceAll(root, "\\", "/"))
	drive := len(clean) >= 2 && clean[1] == ':' // a Windows folder: C:/...
	switch {
	case !strings.HasPrefix(clean, "/") && !drive:
		return fmt.Sprintf("root: %q is not an absolute folder: name the lead's repository on its machine", root)
	case clean == "/" || (drive && len(clean) <= 3):
		return fmt.Sprintf("root: %q is a whole disk: name the lead's repository, not its disk", root)
	case homeFolder.MatchString(root) || homeFolder.MatchString(clean):
		return fmt.Sprintf("root: %q is a home folder: name the lead's repository inside it", root)
	}
	return ""
}

// checkLeadWorkers says what is wrong with a call's workers, or "".
func checkLeadWorkers(ws []api.LeadWorker) string {
	switch {
	case len(ws) == 0:
		return "workers: give at least one worker, its description and its prompt"
	case len(ws) > maxLeadWorkers:
		return fmt.Sprintf("workers: %d in one call; give at most %d, and call again for more", len(ws), maxLeadWorkers)
	}
	for i, w := range ws {
		switch {
		case strings.TrimSpace(w.Prompt) == "":
			return fmt.Sprintf("workers[%d].prompt: a worker needs its brief", i)
		case len(w.Prompt) > maxLeadPrompt || !utf8.ValidString(w.Prompt):
			return fmt.Sprintf("workers[%d].prompt: at most %d bytes of text; put the rest in a file in the repository and name it", i, maxLeadPrompt)
		case strings.TrimSpace(w.Description) == "":
			return fmt.Sprintf("workers[%d].description: a few words the HUD shows for it", i)
		}
	}
	return ""
}

// leadSubmission is a call's workers as a submission of one agent job
// whose plan is the workers: lowered the way any submission's plan is.
func leadSubmission(v *api.Viewer, req api.LeadWorkersRequest) (*authoring.Submission, authoring.Inputs, []api.FieldError) {
	tool := req.Tool
	if tool == "" {
		tool = "agent"
	}
	f := plan.File{Version: 1}
	no := false
	for i, w := range req.Workers {
		name := w.Name
		if name == "" {
			name = workerName(w.Description, req.Key, i)
		}
		params := map[string]string{"task": w.Prompt}
		for k, val := range map[string]string{"model": w.Model, "effort": w.Effort, "env": w.Agent, "base": w.Base} {
			if val != "" {
				params[k] = val
			}
		}
		b := plan.FileBlock{Name: name, Label: strings.TrimSpace(w.Description), Planned: &no, After: w.After, Params: params}
		if req.Turn != nil {
			t := strconv.Itoa(*req.Turn)
			b.Group, params["turn"] = "turn "+t, t
		}
		f.Blocks = append(f.Blocks, b)
	}
	pf, err := json.Marshal(f)
	if err != nil {
		return nil, authoring.Inputs{}, []api.FieldError{{Path: "workers", Message: err.Error()}}
	}
	d := map[string]any{"c4": "submit/1", "project": req.Project,
		"jobs": map[string]any{"lead": map[string]any{"tool": tool, "plan": leadPlanFile}}}
	if req.Root != "" {
		d["root"] = req.Root // where a host gives each worker its worktree
	}
	doc, _ := json.Marshal(d)
	sub, perrs := authoring.ParseSubmission(doc)
	if len(perrs) > 0 {
		return nil, authoring.Inputs{}, errs(perrs)
	}
	return sub, authoring.Inputs{Owner: v.User, Plans: map[string][]byte{leadPlanFile: pf}}, nil
}

// workerName is a worker's unit name made from its description, with a
// short mark of the call and its place so two calls never collide.
func workerName(desc, key string, i int) string {
	n := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(desc), "-"), "-")
	if len(n) > 32 {
		n = strings.TrimRight(n[:32], "-")
	}
	if n == "" {
		n = "worker"
	}
	h := sha256.Sum256([]byte(key + "\x00" + strconv.Itoa(i)))
	return n + "-" + hex.EncodeToString(h[:2])
}

// leadJobName is the name of a person's lead's nth job: from a hash of the
// person and the full session, so two people's leads never share a name,
// and numbered after the first, since a closed job is never reopened.
func leadJobName(owner, session string, n int) string {
	h := sha256.Sum256([]byte(owner + "\x00" + session))
	name := "lead-" + hex.EncodeToString(h[:5])
	if n > 1 {
		name += "-" + strconv.Itoa(n)
	}
	return name
}

// openLeadJob finds the person's open job for this lead in project, and the
// number the next one takes.
func openLeadJob(m *model, project, owner, session string) (open *c4v1.WorkJob, next int) {
	next = 1
	for _, rec := range m.byKey {
		j := &rec.job
		if rec.project != project || j.Annotations[c4v1.AnnotationLeadSession] != session || j.Annotations[c4v1.AnnotationOwner] != owner {
			continue
		}
		next++
		if closedRefusal(j) == "" && (open == nil || j.CreationTimestamp.After(open.CreationTimestamp.Time)) {
			open = j
		}
	}
	return open, next
}

// makeLeadJob makes the lead's job from the call's lowered plan through
// the submission's path: one job, named for the lead, its lead's session
// on it and its questions answered by that lead.
func (s *Server) makeLeadJob(r *http.Request, v *api.Viewer, m *model, pl *authoring.Plan, req api.LeadWorkersRequest, owner, session string, n int) (api.LeadWorkersResponse, int, error) {
	var none api.LeadWorkersResponse
	if len(pl.Jobs) != 1 || pl.Jobs[0].Request == nil {
		return none, http.StatusInternalServerError, errors.New("the lead's workers did not lower to one job")
	}
	j := &pl.Jobs[0]
	j.Request.Name = leadJobName(owner, session, n)
	j.Request.LeadSession = session
	var spec map[string]any
	if err := json.Unmarshal(j.Request.Spec, &spec); err != nil {
		return none, http.StatusInternalServerError, err
	}
	spec["answers"] = map[string]any{"by": c4v1.AnswersByLead}
	sb, err := json.Marshal(spec)
	if err != nil {
		return none, http.StatusInternalServerError, err
	}
	j.Request.Spec = sb
	body := api.SubmissionRequest{Key: req.Key}
	var out api.SubmissionResponse
	jobs, code, ferr := s.makeSubmission(r, v, m, pl, &body, &out)
	if ferr != nil {
		var te takenError
		if errors.As(ferr.err, &te) || apierrors.IsAlreadyExists(ferr.err) {
			return none, http.StatusConflict, takenError{ferr.err}
		}
		fe := s.farmWords(ferr.key, ferr.name, pl.Project, code, ferr.err)
		return none, fe.code, errors.New(fe.Message)
	}
	resp := api.LeadWorkersResponse{Job: pl.Namespace + "/" + j.Request.Name, Created: true, State: "added", Record: out.Record,
		Warnings: pl.Warnings, Units: leadUnits(req)}
	if len(jobs) == 1 {
		resp.Job = jobs[0].Namespace + "/" + jobs[0].Name
	}
	resp.Message = fmt.Sprintf("%s made for this lead with %d worker(s)", resp.Job, len(resp.Units))
	return resp, http.StatusCreated, nil
}

// leadUnits are the workers of the call that made a lead's job, at the
// indexes its table gave them.
func leadUnits(req api.LeadWorkersRequest) []api.AppendedUnit {
	out := make([]api.AppendedUnit, len(req.Workers))
	for i, w := range req.Workers {
		name := w.Name
		if name == "" {
			name = workerName(w.Description, req.Key, i)
		}
		out[i] = api.AppendedUnit{Name: name, Index: &i}
	}
	return out
}

// addLeadWorkers adds the call's workers to the lead's open job: the
// lowered plan's units, appended once for the call's key.
func (s *Server) addLeadWorkers(r *http.Request, v *api.Viewer, j *c4v1.WorkJob, pl *authoring.Plan, req api.LeadWorkersRequest) (api.LeadWorkersResponse, int, error) {
	var none api.LeadWorkersResponse
	if len(pl.Jobs) != 1 || pl.Jobs[0].Table == nil {
		return none, http.StatusInternalServerError, errors.New("the lead's workers did not lower to one table")
	}
	var table struct {
		Units json.RawMessage `json:"units"`
	}
	if err := json.Unmarshal(pl.Jobs[0].Table, &table); err != nil {
		return none, http.StatusInternalServerError, err
	}
	// the turn is already on each unit, as on the job's first workers
	ans, code, err := s.appendUnits(r.Context(), r, v, j.Namespace, j.Name, api.AppendUnitsRequest{Units: table.Units, Key: req.Key})
	if err != nil {
		return none, code, err
	}
	return api.LeadWorkersResponse{Job: j.Namespace + "/" + j.Name, Units: ans.Units, State: ans.State, Error: ans.Error,
		Record: ans.Record, Warnings: pl.Warnings, Message: ans.Message}, code, nil
}

// writeLeadRefusal answers a call whose workers the farm refuses, with
// each refusal at its path.
func writeLeadRefusal(w http.ResponseWriter, code int, errs []api.FieldError) {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		m := e.Message
		if e.Path != "" {
			m = e.Path + ": " + m
		}
		if e.Fix != "" {
			m += " (" + e.Fix + ")"
		}
		msgs = append(msgs, m)
	}
	writeJSON(w, code, map[string]any{"error": strings.Join(msgs, "; "), "errors": errs})
}
