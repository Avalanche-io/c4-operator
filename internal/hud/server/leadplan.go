package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// A lead's plan (DESIGN 10.6, A lead's plan; LEAD.md stage 3): a lead
// edits its own job's plan with one call (c4_plan, POST
// /jobs/{ns}/{name}/plan). The call's edits are written as one plan
// document, the expansion document a generator writes extended with a
// lead's edits (class/planedit.go): each unit it adds (a worker, lowered
// as c4_agent's are, or an expansion document's unit) held until a
// release names it, and its splits, merges, withdrawals, edges, releases
// and order. The HUD checks the document as the queue will, stores it in
// the project's store, signs the call (action plan) and lists it in
// c4.avalanche.io/plan under the caller's key; the queue applies each
// call once, in order, whole or not at all (queue/plans.go). Who may is
// who may add workers: the lead's own session, or its person, on a lead's
// job. A call again with a key already listed changes nothing and answers
// as the first did: pending, then applied, with the indexes of the units
// it added, or refused.

// maxPlanEdits bounds the edits of one call.
const maxPlanEdits = 256

// maxPlanWorkers bounds the workers one call adds.
const maxPlanWorkers = 64

// maxPendingPlans bounds the calls a job holds that the queue has not
// applied yet.
const maxPendingPlans = 64

// handlePlan is POST /jobs/{ns}/{name}/plan.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, jobBody))
	if err != nil {
		failBody(w, err)
		return
	}
	var body api.PlanRequest
	if err := class.Strict(raw, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	out, code, err := s.planEdits(r.Context(), r, v, r.PathValue("ns"), r.PathValue("name"), body)
	if errors.As(err, new(leadOnlyError)) {
		writeJSON(w, code, map[string]string{"error": leadOnlyMessage, "fix": leadOnlyFix})
		return
	}
	if err != nil {
		fail(w, code, "%s", err.Error())
		return
	}
	writeJSON(w, code, out)
}

// planEdits makes req's edits to job ns/name for viewer v, on request r:
// 201 with the call listed, 200 for a key already listed, or the
// refusal's status and words.
func (s *Server) planEdits(ctx context.Context, r *http.Request, v *api.Viewer, ns, name string, req api.PlanRequest) (api.PlanResponse, int, error) {
	var none api.PlanResponse
	m := s.current()
	if m == nil {
		return none, http.StatusServiceUnavailable, errors.New("the HUD is still reading the farm")
	}
	if err := checkPlanRequest(req); err != nil {
		return none, http.StatusBadRequest, err
	}
	rec, err := s.lookup(ctx, m, ns, name)
	if err != nil {
		return none, http.StatusServiceUnavailable, fmt.Errorf("could not read job %s/%s: %v", ns, name, err)
	}
	if rec == nil || !v.Sees(rec.project) {
		return none, http.StatusNotFound, fmt.Errorf("no job %s/%s", ns, name)
	}
	project := projectObject(m, rec.project)
	if v.Unit != nil {
		// A worker may not start workers, nor plan a job: its lead does.
		if planHasWorkers(req) && leadOnly(v, project) {
			return none, http.StatusForbidden, leadOnlyError{}
		}
		return none, http.StatusForbidden, errors.New("a unit cannot plan a job: its lead does")
	}
	if rec.job.Annotations[c4v1.AnnotationLeadSession] == "" || rec.driver == nil || rec.driver.Name() != "graph" {
		return none, http.StatusUnprocessableEntity, fmt.Errorf("%s is not a lead's job: only a lead plans its own job (c4_plan); add units to it with POST /jobs/%s/%s/units", name, ns, name)
	}
	doc, names, err := s.planDocument(ctx, m, v, rec, req)
	if err != nil {
		return none, http.StatusUnprocessableEntity, err
	}
	for range 4 {
		out, code, err := s.planOnce(ctx, r, v, rec, req.Key, doc, names)
		if !apierrors.IsConflict(err) {
			return out, code, err
		}
		// The job changed since it was read (the queue's status, another
		// call): read it again and decide again.
	}
	return none, http.StatusConflict, fmt.Errorf("%s kept changing while it was planned: try again", name)
}

// planOnce is one try of planEdits on the job as it is now; a conflict
// writing it is returned as the API's, to try again.
func (s *Server) planOnce(ctx context.Context, r *http.Request, v *api.Viewer, rec *jobRec, key string, doc []byte, names []string) (api.PlanResponse, int, error) {
	var none api.PlanResponse
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		return none, http.StatusNotFound, fmt.Errorf("no job %s/%s: %v", rec.job.Namespace, rec.job.Name, err)
	}
	if why := s.leadActRefusal(r, v, j, "plans it"); why != "" {
		return none, http.StatusForbidden, errors.New(why)
	}
	if err := s.authorize(ctx, r, v, j.Namespace, "patch", "workjobs", j.Name); err != nil {
		return none, http.StatusForbidden, err
	}
	reqs := pendingPlans(j)
	if out, ok := s.plannedBefore(ctx, rec, j, reqs, key); ok {
		return out, http.StatusOK, nil
	}
	if why := closedRefusal(j); why != "" {
		return none, http.StatusConflict, errors.New(strings.Replace(why, "takes no more workers", "takes no more edits", 1))
	}
	if p := j.Status.Phase; p == "Succeeded" || p == "Failed" {
		return none, http.StatusConflict, fmt.Errorf("%s has finished (%s): a finished job takes no more edits", j.Name, strings.ToLower(p))
	}
	if len(reqs) >= maxPendingPlans {
		return none, http.StatusConflict, fmt.Errorf("%s has %d plan calls the queue has not read yet: try again in a moment", j.Name, len(reqs))
	}
	if len(reqs) == 0 && len(pendingAppends(j)) == 0 {
		// As the queue will check it; with calls or additions it has not
		// read yet, the queue alone decides.
		if p := s.planFor(rec, j); p != nil {
			if _, err := class.ReadPlan(j, queuedRoot(p), doc, p, key, statusStarted(j, len(p.Units))); err != nil {
				return none, http.StatusUnprocessableEntity, err
			}
		}
	}
	docID, err := s.storeDoc(ctx, rec.project, doc)
	if err != nil {
		return none, http.StatusServiceUnavailable, fmt.Errorf("storing the plan: %v", err)
	}
	patch := client.MergeFromWithOptions(j.DeepCopy(), client.MergeFromWithOptimisticLock{})
	before := queue.SpecDigest(j)
	now := time.Now()
	token := strconv.FormatInt(now.UnixNano(), 36)
	// Signed first, so the request names its record (controltrust.go).
	id, err := s.controlJob(ctx, r, v, rec, j, before, "plan",
		map[string]any{"token": token, "key": key, "document": docID, "units": names})
	if err != nil {
		return none, http.StatusServiceUnavailable, fmt.Errorf("control record: %v", err)
	}
	all := append(reqs, c4v1.WorkPlanRequest{Token: token, Key: key, Document: docID, Units: names,
		Actor: actorOf(r, v), At: metav1.Time{Time: now.UTC().Truncate(time.Second)}, Record: id})
	b, _ := json.Marshal(all)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationPlan] = string(b)
	if err := s.Client.Patch(ctx, j, patch); err != nil {
		if apierrors.IsConflict(err) {
			return none, http.StatusConflict, err
		}
		return none, http.StatusServiceUnavailable, fmt.Errorf("writing the request: %v", err)
	}
	msg := fmt.Sprintf("%s: plan to be applied", j.Name)
	if len(names) > 0 {
		msg += ", adding " + strings.Join(names, ", ")
	}
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	s.record(ctx, j, "WorkJob", v.User, "Planned", note, bookNote{actor: actorOf(r, v), kind: "plan", record: id})
	out := api.PlanResponse{Record: id, Document: docID, State: "pending", Message: msg}
	for _, n := range names {
		out.Units = append(out.Units, api.AppendedUnit{Name: n})
	}
	return out, http.StatusCreated, nil
}

// checkPlanRequest checks a call's key and the shape of its edits: each
// gives exactly one edit.
func checkPlanRequest(req api.PlanRequest) error {
	switch k := req.Key; {
	case strings.TrimSpace(k) == "":
		return errors.New("key: an idempotency key, the same each time the call is sent")
	case len(k) > maxAppendKey:
		return fmt.Errorf("key: %d bytes, over %d", len(k), maxAppendKey)
	case len(req.Edits) == 0:
		return errors.New("edits: at least one edit (add, split, merge, order, withdraw, release or edge)")
	case len(req.Edits) > maxPlanEdits:
		return fmt.Errorf("edits: %d in one call; give at most %d, and call again for more", len(req.Edits), maxPlanEdits)
	}
	if msg := checkLeadRoot(req.Root); msg != "" {
		return errors.New(msg)
	}
	for i, e := range req.Edits {
		n := 0
		for _, set := range []bool{e.Add != nil, e.Split != nil, e.Merge != nil, e.Order != nil, e.Withdraw != nil, e.Release != nil, e.Edge != nil} {
			if set {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("edits[%d]: give exactly one of add, split, merge, order, withdraw, release or edge (it gives %d)", i, n)
		}
	}
	return nil
}

// planHasWorkers says whether a call adds workers (agent work).
func planHasWorkers(req api.PlanRequest) bool {
	for _, e := range req.Edits {
		for _, u := range planUnits(e) {
			if u.Worker != nil {
				return true
			}
		}
	}
	return false
}

// planUnits are the units an edit adds.
func planUnits(e api.PlanEdit) []api.PlanUnit {
	switch {
	case e.Add != nil:
		return []api.PlanUnit{*e.Add}
	case e.Split != nil:
		return e.Split.Into
	case e.Merge != nil:
		return []api.PlanUnit{e.Merge.Into}
	}
	return nil
}

// planDocument writes a call's edits as one plan document, and names the
// units it adds, in its order. Workers are lowered as c4_agent's are, so
// the farm checks them as it checks any agent work; an added unit is held
// until a release names it.
func (s *Server) planDocument(ctx context.Context, m *model, v *api.Viewer, rec *jobRec, req api.PlanRequest) ([]byte, []string, error) {
	lw := api.LeadWorkersRequest{Project: rec.project, Root: req.Root, Key: req.Key}
	for _, e := range req.Edits {
		for _, u := range planUnits(e) {
			if u.Worker != nil {
				lw.Workers = append(lw.Workers, *u.Worker)
			}
		}
	}
	var workers []class.DocUnit
	if len(lw.Workers) > 0 {
		if msg := checkLeadWorkers(lw.Workers, maxPlanWorkers); msg != "" {
			return nil, nil, errors.New(msg)
		}
		sub, in, refusals := leadSubmission(v, lw)
		if len(refusals) > 0 {
			return nil, nil, fieldErrors(refusals)
		}
		pl, lerrs := authoring.LowerWith(sub, s.farmFor(ctx, m), in)
		if len(lerrs) > 0 {
			return nil, nil, fieldErrors(errs(lerrs))
		}
		if len(pl.Jobs) != 1 || pl.Jobs[0].Table == nil {
			return nil, nil, errors.New("the plan's workers did not lower to one table")
		}
		var table struct {
			Units json.RawMessage `json:"units"`
		}
		if err := json.Unmarshal(pl.Jobs[0].Table, &table); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(table.Units, &workers); err != nil || len(workers) != len(lw.Workers) {
			return nil, nil, fmt.Errorf("the plan's workers lowered to %d units, not %d: %v", len(workers), len(lw.Workers), err)
		}
	}
	d := class.Doc{Version: 1, Dependents: json.RawMessage(`[]`)}
	var names []string
	unit := func(pu api.PlanUnit, held bool, at string) (string, error) {
		var du class.DocUnit
		switch {
		case pu.Worker != nil && pu.Unit != nil:
			return "", fmt.Errorf("%s: give a worker or a unit, not both", at)
		case pu.Worker != nil:
			du, workers = workers[0], workers[1:]
			// Lowering keeps only the edges between this call's workers;
			// the job's units it names, the queue resolves.
			for _, a := range pu.Worker.After {
				if !slices.Contains(du.After, a) {
					du.After = append(du.After, a)
				}
			}
			if pu.Work < 0 {
				return "", fmt.Errorf("%s: negative work", at)
			}
			if pu.Work > 0 {
				du.Work = pu.Work
			}
		case pu.Unit != nil:
			if err := class.Strict(pu.Unit, &du); err != nil {
				return "", fmt.Errorf("%s.unit: an expansion document's unit (version 1): %v", at, err)
			}
		default:
			return "", fmt.Errorf("%s: give the worker or the unit it adds", at)
		}
		du.Held = held
		d.Units = append(d.Units, du)
		names = append(names, du.Name)
		return du.Name, nil
	}
	for i, e := range req.Edits {
		at := "edits[" + strconv.Itoa(i) + "]"
		switch {
		case e.Add != nil:
			if _, err := unit(*e.Add, true, at+".add"); err != nil {
				return nil, nil, err
			}
		case e.Split != nil:
			sp := class.DocSplit{Unit: e.Split.Unit, Why: e.Split.Why}
			for k, pu := range e.Split.Into {
				n, err := unit(pu, false, at+".split.into["+strconv.Itoa(k)+"]")
				if err != nil {
					return nil, nil, err
				}
				sp.Into = append(sp.Into, n)
			}
			d.Split = append(d.Split, sp)
		case e.Merge != nil:
			n, err := unit(e.Merge.Into, false, at+".merge.into")
			if err != nil {
				return nil, nil, err
			}
			d.Merge = append(d.Merge, class.DocMerge{Units: e.Merge.Units, Into: n, Why: e.Merge.Why})
		case e.Withdraw != nil:
			d.Withdraw = append(d.Withdraw, class.DocWithdraw{Unit: e.Withdraw.Unit, Why: e.Withdraw.Why})
		case e.Order != nil:
			if len(d.Order) > 0 {
				return nil, nil, fmt.Errorf("%s: one order per call; give every unit to order in it", at)
			}
			d.Order = e.Order
		case e.Release != nil:
			d.Release = append(d.Release, e.Release...)
		case e.Edge != nil:
			d.Edges = append(d.Edges, class.DocEdge{From: e.Edge.From, To: e.Edge.To, Remove: e.Edge.Remove})
		}
	}
	doc, err := json.Marshal(d)
	if err != nil {
		return nil, nil, err
	}
	return doc, names, nil
}

// fieldErrors is the farm's refusals of a call's workers as one error.
func fieldErrors(fes []api.FieldError) error {
	msgs := make([]string, 0, len(fes))
	for _, e := range fes {
		m := e.Message
		if e.Path != "" {
			m = e.Path + ": " + m
		}
		if e.Fix != "" {
			m += " (" + e.Fix + ")"
		}
		msgs = append(msgs, m)
	}
	return errors.New(strings.Join(msgs, "; "))
}

// queuedRoot is the unit a plan's units are added under: the job's root,
// as a plain generator, as the queue reads it.
func queuedRoot(p *class.Plan) class.Unit {
	u := p.Units[0]
	u.Rounds = nil
	return u
}

// statusStarted says, from the job's status, why unit i of its n can no
// longer be planned (class.Started), as the queue says it.
func statusStarted(j *c4v1.WorkJob, n int) class.Started {
	states := unitStateMap(&j.Status, n)
	return func(i int) string {
		if i < 0 || i >= n {
			return ""
		}
		switch states[i] {
		case "preflight", "running", "retrying", "backoff", "egress":
			return "is running"
		case "succeeded":
			return "has succeeded"
		case "failed", "flagged":
			return "has failed"
		case "accepted":
			return "failed, and the failure was accepted"
		case "needs":
			return "stopped on a question"
		case "superseded":
			return "was superseded"
		case "waits":
			return "is waiting for its decision"
		}
		return ""
	}
}

// pendingPlans are the job's plan requests the queue has not carried out:
// after the last token status names (the carried out leave the annotation
// when it is next written).
func pendingPlans(j *c4v1.WorkJob) []c4v1.WorkPlanRequest {
	var reqs []c4v1.WorkPlanRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationPlan]), &reqs)
	var last int64
	if p := j.Status.Plan; p != nil {
		last, _ = strconv.ParseInt(p.Token, 36, 64)
	}
	out := reqs[:0]
	for _, r := range reqs {
		if t, _ := strconv.ParseInt(r.Token, 36, 64); t > last {
			out = append(out, r)
		}
	}
	return out
}

// plannedBefore answers a call whose key is already listed: pending, or
// as the queue applied it, with the indexes of the units it added. A call
// older than status's recent list is found in the stored log.
func (s *Server) plannedBefore(ctx context.Context, rec *jobRec, j *c4v1.WorkJob, pending []c4v1.WorkPlanRequest, key string) (api.PlanResponse, bool) {
	for _, r := range pending {
		if r.Key != key {
			continue
		}
		out := api.PlanResponse{Record: r.Record, Document: r.Document, State: "pending", Message: fmt.Sprintf("%s: plan to be applied", j.Name)}
		for _, n := range r.Units {
			out.Units = append(out.Units, api.AppendedUnit{Name: n})
		}
		return out, true
	}
	x := s.planEditOf(ctx, rec, j, key)
	if x == nil {
		return api.PlanResponse{}, false
	}
	out := api.PlanResponse{Record: x.Control, Document: x.Document, State: "applied"}
	if x.Error != "" {
		out.State, out.Error = "refused", x.Error
		out.Message = fmt.Sprintf("%s: the queue refused this plan, and made none of its edits: %s", j.Name, x.Error)
		return out, true
	}
	// The units are named as the call named them; once it has left the
	// list, the table names them.
	var listed []string
	var all []c4v1.WorkPlanRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationPlan]), &all)
	for _, r := range all {
		if r.Key == key {
			listed = r.Units
		}
	}
	plan := s.planFor(rec, j)
	var names []string
	for i := range int(x.Units) {
		idx := int(x.First) + i
		n := strconv.Itoa(idx)
		switch {
		case len(listed) == int(x.Units):
			n = listed[i]
		case plan != nil && idx < len(plan.Units) && plan.Units[idx].Name != "":
			n = plan.Units[idx].Name
		}
		names = append(names, n)
		out.Units = append(out.Units, api.AppendedUnit{Name: n, Index: &idx})
	}
	out.Message = fmt.Sprintf("%s: plan applied", j.Name)
	if len(names) > 0 {
		out.Message += ", adding " + strings.Join(names, ", ")
	}
	return out, true
}

// planEditOf is the job's applied call with key: from status's recent
// list, else from the stored log; nil if neither has it.
func (s *Server) planEditOf(ctx context.Context, rec *jobRec, j *c4v1.WorkJob, key string) *c4v1.WorkPlanEdit {
	p := j.Status.Plan
	if p == nil {
		return nil
	}
	for i := range p.Recent {
		if p.Recent[i].Key == key {
			return &p.Recent[i]
		}
	}
	if p.Log == "" || int(p.Edits) <= len(p.Recent) || s.Stores == nil {
		return nil
	}
	cid, err := c4.Parse(p.Log)
	if err != nil {
		return nil
	}
	st, err := s.Stores(ctx, rec.project)
	if err != nil || st == nil {
		return nil
	}
	data, err := c4view.ReadObject(st, cid)
	if err != nil {
		return nil
	}
	var log struct {
		Edits []c4v1.WorkPlanEdit `json:"edits"`
	}
	if json.Unmarshal(data, &log) != nil {
		return nil
	}
	for i := range log.Edits {
		if log.Edits[i].Key == key {
			return &log.Edits[i]
		}
	}
	return nil
}

// leadPlanOf is a lead's job's plan as the HUD shows it (Job.Plan).
func leadPlanOf(p *c4v1.WorkPlan) *api.LeadPlan {
	if p == nil {
		return nil
	}
	out := &api.LeadPlan{Edits: int(p.Edits), Log: p.Log, Order: p.Order}
	for _, x := range p.Recent {
		out.Recent = append(out.Recent, api.PlanCall{Key: x.Key, Document: x.Document, First: int(x.First), Units: int(x.Units),
			At: x.At.Time.UTC(), Control: x.Control, Record: x.Record, Error: x.Error})
	}
	for i, w := range p.Withdrawn {
		if out.Withdrawn == nil {
			out.Withdrawn = map[string]api.WithdrawnUnit{}
		}
		out.Withdrawn[i] = api.WithdrawnUnit{Why: w.Why, Replacements: w.Replacements, Edit: w.Edit}
	}
	return out
}
