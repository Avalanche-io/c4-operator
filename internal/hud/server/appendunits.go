package server

import (
	"context"
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
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// Appending units (DESIGN 10.6, Appending units): a graph job's owner, or
// an agent of theirs, adds units to it while it runs. The units are an
// expansion document's, the same a generator writes; the HUD checks them
// as the queue will, stores the document in the project's store, signs
// the request (action append) and lists it in c4.avalanche.io/append
// under the caller's idempotency key. The queue applies each key once,
// after the job's existing units (queue/appends.go). A request again with
// a key already listed adds nothing and answers as the first did, with
// the units' indexes once the queue has given them.

// maxPendingAppends bounds the requests a job holds that the queue has not
// applied yet.
const maxPendingAppends = 64

// maxAppendKey bounds an idempotency key, in bytes.
const maxAppendKey = 128

// handleAppendUnits is POST /jobs/{ns}/{name}/units.
func (s *Server) handleAppendUnits(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, jobBody))
	if err != nil {
		failBody(w, err)
		return
	}
	var body api.AppendUnitsRequest
	if err := class.Strict(raw, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	out, code, err := s.appendUnits(r.Context(), r, v, r.PathValue("ns"), r.PathValue("name"), body)
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

// appendUnits adds req's units to job ns/name for viewer v, on request r
// (whose headers say the client and the agent): 201 with the request
// listed, 200 for a key already listed, or the refusal's status and
// words. Another route that adds a worker to a lead's job calls it as
// POST /jobs/{ns}/{name}/units does.
func (s *Server) appendUnits(ctx context.Context, r *http.Request, v *api.Viewer, ns, name string, req api.AppendUnitsRequest) (api.AppendUnitsResponse, int, error) {
	var none api.AppendUnitsResponse
	m := s.current()
	if m == nil {
		return none, http.StatusServiceUnavailable, errors.New("the HUD is still reading the farm")
	}
	rec, err := s.lookup(ctx, m, ns, name)
	if err != nil {
		return none, http.StatusServiceUnavailable, fmt.Errorf("could not read job %s/%s: %v", ns, name, err)
	}
	if rec == nil || !v.Sees(rec.project) {
		return none, http.StatusNotFound, fmt.Errorf("no job %s/%s", ns, name)
	}
	units, err := appendDocUnits(req)
	if err != nil {
		return none, http.StatusBadRequest, err
	}
	project := projectObject(m, rec.project)
	agents := appendAgentUnits(&rec.job, units)
	if v.Unit != nil {
		// A worker may not start workers; and a unit edits no job's
		// table, its own or another's: its lead adds what it needs.
		if len(agents) > 0 && leadOnly(v, project) {
			return none, http.StatusForbidden, leadOnlyError{}
		}
		return none, http.StatusForbidden, errors.New("a unit cannot add units to a job: its lead does")
	}
	if rec.driver == nil || rec.driver.Name() != "graph" {
		return none, http.StatusUnprocessableEntity, fmt.Errorf("%s is a %s job, whose units its class makes: only a graph job takes units while it runs", name, rec.job.Spec.Class)
	}
	for range 4 {
		out, code, err := s.appendOnce(ctx, r, v, m, rec, project, units, agents, req)
		if !apierrors.IsConflict(err) {
			return out, code, err
		}
		// The job changed since it was read (the queue's status, another
		// request): read it again and decide again.
	}
	return none, http.StatusConflict, fmt.Errorf("%s kept changing while the units were added: try again", name)
}

// appendOnce is one try of appendUnits on the job as it is now; a
// conflict writing it is returned as the API's, to try again.
func (s *Server) appendOnce(ctx context.Context, r *http.Request, v *api.Viewer, m *model, rec *jobRec, project *c4v1.Project,
	units []class.DocUnit, agents []string, req api.AppendUnitsRequest) (api.AppendUnitsResponse, int, error) {
	var none api.AppendUnitsResponse
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		return none, http.StatusNotFound, fmt.Errorf("no job %s/%s: %v", rec.job.Namespace, rec.job.Name, err)
	}
	switch owner := j.Annotations[c4v1.AnnotationOwner]; {
	case owner == "":
		return none, http.StatusForbidden, fmt.Errorf("%s has no owner on record, so no one adds units to it", j.Name)
	case owner != v.User:
		return none, http.StatusForbidden, fmt.Errorf("%s is %s's job: only %s, or an agent of theirs, adds units to it", j.Name, owner, owner)
	}
	if err := s.authorize(ctx, r, v, j.Namespace, "patch", "workjobs", j.Name); err != nil {
		return none, http.StatusForbidden, err
	}
	reqs := pendingAppends(j)
	if out, ok := appendedBefore(j, reqs, s.planFor(rec, j), req.Key); ok {
		return out, http.StatusOK, nil
	}
	if p := j.Status.Phase; p == "Succeeded" || p == "Failed" {
		return none, http.StatusConflict, fmt.Errorf("%s has finished (%s): a finished job takes no more units; submit another job for them", j.Name, strings.ToLower(p))
	}
	if len(agents) > 0 && j.Annotations[c4v1.AnnotationSubmittedByUnit] != "" {
		// A unit's job grows no agent units, whoever asks: the queue
		// would refuse them as it refuses its generator's.
		if who, _ := policy.Value(j, project, -1, policy.AgentsSubmit); who == policy.SubmitLead {
			return none, http.StatusForbidden, leadOnlyError{}
		}
	}
	if len(reqs) >= maxPendingAppends {
		return none, http.StatusConflict, fmt.Errorf("%s has %d additions the queue has not read yet: try again in a moment", j.Name, len(reqs))
	}
	doc, err := json.Marshal(class.Doc{Version: 1, Units: units, Dependents: json.RawMessage(`[]`)})
	if err != nil {
		return none, http.StatusBadRequest, fmt.Errorf("units: %v", err)
	}
	if err := checkAppend(j, s.planFor(rec, j), reqs, doc); err != nil {
		return none, http.StatusUnprocessableEntity, fmt.Errorf("units: %v", err)
	}
	docID, err := s.storeDoc(ctx, rec.project, doc)
	if err != nil {
		return none, http.StatusServiceUnavailable, fmt.Errorf("storing the units: %v", err)
	}
	names := make([]string, len(units))
	for i, u := range units {
		names[i] = u.Name
	}
	var turn *int32
	if req.Turn != nil {
		t := int32(*req.Turn)
		turn = &t
	}
	patch := client.MergeFromWithOptions(j.DeepCopy(), client.MergeFromWithOptimisticLock{})
	before := queue.SpecDigest(j)
	now := time.Now()
	token := strconv.FormatInt(now.UnixNano(), 36)
	// Signed first, so the request names its record (controltrust.go).
	id, err := s.controlJob(ctx, r, v, rec, j, before, "append",
		map[string]any{"token": token, "key": req.Key, "document": docID, "units": names, "turn": turn})
	if err != nil {
		return none, http.StatusServiceUnavailable, fmt.Errorf("control record: %v", err)
	}
	reqs = append(reqs, c4v1.WorkAppendRequest{Token: token, Key: req.Key, Document: docID, Units: names, Turn: turn,
		Actor: actorOf(r, v), At: metav1.Time{Time: now.UTC().Truncate(time.Second)}, Record: id})
	b, _ := json.Marshal(reqs)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationAppend] = string(b)
	if err := s.Client.Patch(ctx, j, patch); err != nil {
		if apierrors.IsConflict(err) {
			return none, http.StatusConflict, err
		}
		return none, http.StatusServiceUnavailable, fmt.Errorf("writing the request: %v", err)
	}
	msg := fmt.Sprintf("%s: %s to be added", j.Name, strings.Join(names, ", "))
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	s.record(ctx, j, "WorkJob", v.User, "UnitsAppended", note, bookNote{actor: actorOf(r, v), kind: "append", record: id})
	out := api.AppendUnitsResponse{Record: id, Document: docID, State: "pending", Message: msg}
	for _, n := range names {
		out.Units = append(out.Units, api.AppendedUnit{Name: n})
	}
	return out, http.StatusCreated, nil
}

// appendDocUnits reads and checks a request's units and key, and carries
// its turn onto the units: each without a group is grouped as "turn N",
// and each carries param turn.
func appendDocUnits(req api.AppendUnitsRequest) ([]class.DocUnit, error) {
	switch k := req.Key; {
	case strings.TrimSpace(k) == "":
		return nil, errors.New("key: an idempotency key, the same each time the request is sent")
	case len(k) > maxAppendKey:
		return nil, fmt.Errorf("key: %d bytes, over %d", len(k), maxAppendKey)
	}
	if req.Turn != nil && (*req.Turn < 0 || *req.Turn > 1<<30) {
		return nil, fmt.Errorf("turn: %d, want a turn number from 0", *req.Turn)
	}
	var units []class.DocUnit
	if err := class.Strict(req.Units, &units); err != nil {
		return nil, fmt.Errorf("units: an expansion document's units (version 1): %v", err)
	}
	if len(units) == 0 {
		return nil, errors.New("units: at least one unit, as an expansion document lists them")
	}
	if req.Turn == nil {
		return units, nil
	}
	t := strconv.Itoa(*req.Turn)
	for i := range units {
		u := &units[i]
		if u.Group == "" {
			u.Group = "turn " + t
		}
		if u.Params == nil {
			u.Params = map[string]string{}
		}
		if _, ok := u.Params["turn"]; !ok {
			u.Params["turn"] = t
		}
	}
	return units, nil
}

// appendAgentUnits are the units that would run agents: a bundle member
// or a block, by their environment or their job's template, as the queue
// tells them (queue/leadonly.go).
func appendAgentUnits(j *c4v1.WorkJob, units []class.DocUnit) []string {
	var out []string
	template := agentSpec(c4v1.WorkJobSpec{Template: j.Spec.Template})
	for _, u := range units {
		if template || u.Env["C4_AGENT_MEMBER"] != "" || u.Env["C4_AGENT_BLOCK"] != "" {
			out = append(out, u.Name)
		}
	}
	return out
}

// pendingAppends are the job's append requests the queue has not applied
// (status.appends); the applied leave the annotation when it is next
// written.
func pendingAppends(j *c4v1.WorkJob) []c4v1.WorkAppendRequest {
	var reqs []c4v1.WorkAppendRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationAppend]), &reqs)
	out := reqs[:0]
	for _, r := range reqs {
		if appliedAppend(j, r.Key) == nil {
			out = append(out, r)
		}
	}
	return out
}

func appliedAppend(j *c4v1.WorkJob, key string) *c4v1.WorkAppend {
	for i := range j.Status.Appends {
		if j.Status.Appends[i].Key == key {
			return &j.Status.Appends[i]
		}
	}
	return nil
}

// appendedBefore answers a request whose key is already listed: pending,
// or as the queue applied it, with the units' indexes.
func appendedBefore(j *c4v1.WorkJob, pending []c4v1.WorkAppendRequest, plan *class.Plan, key string) (api.AppendUnitsResponse, bool) {
	if a := appliedAppend(j, key); a != nil {
		out := api.AppendUnitsResponse{Record: a.Control, Document: a.Document, State: "added"}
		if a.Error != "" {
			out.State, out.Error = "refused", a.Error
			out.Message = fmt.Sprintf("%s: the queue refused these units: %s", j.Name, a.Error)
			return out, true
		}
		// The units are named as the request named them, in its order;
		// once the request has left the list, the table names them.
		var listed []string
		var all []c4v1.WorkAppendRequest
		_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationAppend]), &all)
		for _, r := range all {
			if r.Key == key {
				listed = r.Units
			}
		}
		var names []string
		for i := range int(a.Units) {
			idx := int(a.First) + i
			n := strconv.Itoa(idx)
			switch {
			case len(listed) == int(a.Units):
				n = listed[i]
			case plan != nil && idx < len(plan.Units) && plan.Units[idx].Name != "":
				n = plan.Units[idx].Name
			}
			names = append(names, n)
			out.Units = append(out.Units, api.AppendedUnit{Name: n, Index: &idx})
		}
		out.Message = fmt.Sprintf("%s: %s added", j.Name, strings.Join(names, ", "))
		return out, true
	}
	for _, r := range pending {
		if r.Key != key {
			continue
		}
		out := api.AppendUnitsResponse{Record: r.Record, Document: r.Document, State: "pending",
			Message: fmt.Sprintf("%s: %s to be added", j.Name, strings.Join(r.Units, ", "))}
		for _, n := range r.Units {
			out.Units = append(out.Units, api.AppendedUnit{Name: n})
		}
		return out, true
	}
	return api.AppendUnitsResponse{}, false
}

// planFor is the job's unit table as the queue has it now, else as the
// HUD last read it; nil if that is of another generation, or behind what
// the job's status says was added (the units of the latest expansion or
// append are not in it yet).
func (s *Server) planFor(rec *jobRec, j *c4v1.WorkJob) *class.Plan {
	p, gen := s.planOf(rec.key)
	if p == nil {
		p, gen = rec.plan, rec.job.Generation
		if rec.planErr != nil {
			p = nil
		}
	}
	if p == nil || gen != j.Generation {
		return nil
	}
	n := len(p.Units)
	for _, x := range j.Status.Expansions {
		if int(x.First+x.Units) > n {
			return nil
		}
	}
	for _, a := range j.Status.Appends {
		if int(a.First+a.Units) > n {
			return nil
		}
	}
	return p
}

// checkAppend checks document doc as the queue will before it adds its
// units after plan's and those pending: names, edges and inputs, the
// unit bound. The queue checks again on its own table, and is the one
// that decides.
func checkAppend(j *c4v1.WorkJob, plan *class.Plan, pending []c4v1.WorkAppendRequest, doc []byte) error {
	if plan == nil || len(plan.Units) == 0 {
		return nil
	}
	names := plan.Names()
	n := len(plan.Units)
	for _, r := range pending {
		for _, u := range r.Units {
			names[u] = n
			n++
		}
	}
	root := plan.Units[0]
	root.Rounds = nil
	ex, err := class.ExpandDocument(j, root, doc, names, n)
	if err != nil {
		return err
	}
	if n+len(ex.Units) > class.MaxUnits {
		return fmt.Errorf("the job would have %d units, over %d", n+len(ex.Units), class.MaxUnits)
	}
	return nil
}

// storeDoc stores a document in the project's store and returns its C4
// ID.
func (s *Server) storeDoc(ctx context.Context, project string, doc []byte) (string, error) {
	if s.Stores == nil {
		return "", errors.New("this HUD has no stores")
	}
	st, err := s.Stores(ctx, project)
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", fmt.Errorf("project %s has no store", project)
	}
	id, err := st.PutBytes(ctx, doc)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}
