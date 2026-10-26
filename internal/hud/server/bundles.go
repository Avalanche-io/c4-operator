package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/controller"
	"github.com/Avalanche-io/c4-operator/internal/when"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// A person runs their own agents on the farm through these, without
// kubectl (`c4 farm agents`, MCP c4_run_bundle): POST makes an
// AgentBundle owned by the viewer, so the pools and host runners that
// serve that person run its members; GET lists and reads bundles; DELETE
// and suspend are controls, signed and booked like the others.

// nodeMembers on a runner's Node lists the members registered on it,
// comma-separated, as its heartbeat writes them.
const nodeMembers = "c4.avalanche.io/members"

// defaultBundleName is a bundle's name when the request gives none.
const defaultBundleName = "agents"

var memberName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func bundleLink(ns, name string) string { return "#/job/" + ns + "/" + name }

// handleBundleCreate is POST /bundles/{project}.
func (s *Server) handleBundleCreate(w http.ResponseWriter, r *http.Request) {
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
	var body api.BundleRequest
	if err := class.Strict(raw, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	project := r.PathValue("project")
	out := api.BundleResponse{DryRun: body.DryRun, Project: project, Owner: v.User}
	refuse := func(code int, errs ...api.FieldError) {
		out.Errors = errs
		writeJSON(w, code, out)
	}
	_, ns, err := s.project(m, v, project, "")
	if err != nil {
		refuse(http.StatusNotFound, api.FieldError{Path: "project", Message: err.Error(), Fix: "c4 farm projects lists the ones you can use"})
		return
	}
	out.Namespace = ns
	if leadOnly(v, projectObject(m, project)) {
		// a bundle starts agents: a worker may not start workers, by any door
		refuse(http.StatusForbidden, api.FieldError{Path: "members", Message: leadOnlyMessage, Fix: leadOnlyFix})
		return
	}
	ctx := r.Context()
	b, errs, warnings := s.checkBundle(r, m, v, ns, body)
	out.Warnings = warnings
	if len(errs) > 0 {
		refuse(http.StatusUnprocessableEntity, errs...)
		return
	}
	if err := s.authorize(ctx, r, v, ns, "create", "agentbundles", ""); err != nil {
		refuse(http.StatusForbidden, api.FieldError{Path: "project", Message: "you may not run agents in " + project + ": " + err.Error(),
			Fix: "ask a project admin for the c4-artist role in " + project})
		return
	}
	if b.Name, err = s.bundleName(ctx, ns, body.Name); err != nil {
		refuse(http.StatusConflict, api.FieldError{Path: "name", Message: err.Error(), Fix: "choose another name, or leave it empty"})
		return
	}
	out.Name, out.Link = b.Name, bundleLink(ns, b.Name)
	if b.Spec.Deadline != nil {
		d := b.Spec.Deadline.Time.UTC()
		out.Due = &d
	}
	for _, mb := range b.Spec.Members {
		out.Members = append(out.Members, api.BundleMemberInfo{Name: mb.Name, State: "queued"})
	}
	if body.DryRun {
		out.OK = true
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err := s.Client.Create(ctx, b); err != nil {
		code := http.StatusConflict
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			code = http.StatusUnprocessableEntity
		}
		refuse(code, api.FieldError{Path: "name", Message: "the farm did not make the bundle: " + err.Error(), Fix: "try again; if it persists, c4 farm diagnose"})
		return
	}
	s.record(ctx, b, "AgentBundle", v.User, "Created", fmt.Sprintf("%s: %d agents, by %s", b.Name, len(b.Spec.Members), v.User),
		bookNote{actor: actorOf(r, v), kind: "bundle"})
	out.OK = true
	writeJSON(w, http.StatusCreated, out)
}

// checkBundle builds the AgentBundle a request asks for, owned by the
// viewer, with every refusal in the farm's words and the warnings a dry
// run gives.
func (s *Server) checkBundle(r *http.Request, m *model, v *api.Viewer, ns string, body api.BundleRequest) (*c4v1.AgentBundle, []api.FieldError, []string) {
	var errs []api.FieldError
	var warnings []string
	b := &c4v1.AgentBundle{ObjectMeta: metav1.ObjectMeta{Namespace: ns,
		// The owner is the person asking: the pools and host runners that
		// serve them run its members (JobForBundle copies it to the job).
		Annotations: map[string]string{c4v1.AnnotationOwner: v.User, c4v1.AnnotationSubmittedVia: submittedVia(r)}},
		Spec: c4v1.AgentBundleSpec{MaxRunning: body.MaxRunning, Pools: body.Pools, Suspend: body.Suspend}}
	owner := s.ownerFor(r.Context(), v)
	if owner != v.User {
		// a one-person farm's person, as a role, as a job is (createJob)
		b.Annotations[c4v1.AnnotationOwner] = owner
		b.Annotations[c4v1.AnnotationSubmittedAs] = devRole(v)
	}
	if len(body.Members) == 0 {
		errs = append(errs, api.FieldError{Path: "members", Message: "a bundle needs at least one agent",
			Fix: "name the agents to run (c4 farm agents ls lists them)"})
	}
	seen := map[string]bool{}
	for i, mb := range body.Members {
		path := "members[" + strconv.Itoa(i) + "]"
		switch {
		case !memberName.MatchString(mb.Name):
			errs = append(errs, api.FieldError{Path: path, Message: fmt.Sprintf("%q is not an agent's name on this farm", mb.Name),
				Fix: "use the name it was added under (c4 farm agents ls)"})
			continue
		case seen[mb.Name]:
			errs = append(errs, api.FieldError{Path: path, Message: mb.Name + " is named twice", Fix: "name each agent once"})
			continue
		case mb.Kind != "" && mb.Kind != "resident" && mb.Kind != "task":
			errs = append(errs, api.FieldError{Path: path + ".kind", Message: fmt.Sprintf("kind %q: an agent is resident or task", mb.Kind),
				Fix: "resident lives as long as the bundle; task has one job"})
			continue
		}
		seen[mb.Name] = true
		b.Spec.Members = append(b.Spec.Members, c4v1.AgentMemberSpec{Name: mb.Name, Kind: mb.Kind, Prompt: mb.Prompt})
	}
	if body.MaxRunning < 0 {
		errs = append(errs, api.FieldError{Path: "maxRunning", Message: "maxRunning cannot be negative", Fix: "leave it out to run them all"})
	}
	if body.Due != "" {
		t, err := s.bundleDue(body.Due)
		if err != nil {
			errs = append(errs, api.FieldError{Path: "due", Message: err.Error(), Fix: "an RFC 3339 time, a date, or words like friday 18:00"})
		} else {
			b.Spec.Deadline = &metav1.Time{Time: t}
		}
	}
	// Where its members would run: pools that exist and serve this person.
	pools := body.Pools
	if len(pools) == 0 {
		if d, err := class.Lookup("agent"); err == nil {
			pools = d.Pools()
		}
	}
	var serving []c4v1.Pool
	var refused []string
	for _, name := range pools {
		i := slices.IndexFunc(m.pools, func(p c4v1.Pool) bool { return p.Name == name })
		switch {
		case i < 0:
			refused = append(refused, "pool "+name+" does not exist")
		case len(m.pools[i].Spec.Owners) > 0 && !slices.Contains(m.pools[i].Spec.Owners, owner):
			refused = append(refused, "pool "+name+" runs only the agents of "+strings.Join(m.pools[i].Spec.Owners, ", "))
		default:
			serving = append(serving, m.pools[i])
		}
	}
	if len(serving) == 0 {
		errs = append(errs, api.FieldError{Path: "pools", Message: "no pool runs agents for " + owner + " yet (" + strings.Join(refused, "; ") + ")",
			Fix: "c4 farm up makes one for this machine"})
		return b, errs, warnings
	}
	// The members the runners serving those pools have registered, where
	// they say: a member none of them knows would wait for ever.
	known, registered := map[string]bool{}, false
	for _, p := range serving {
		sel := labels.SelectorFromSet(p.Spec.NodeSelector)
		for _, n := range m.nodes {
			list, ok := n.Annotations[nodeMembers]
			if !ok || !sel.Matches(labels.Set(n.Labels)) {
				continue
			}
			registered = true
			for _, name := range strings.Split(list, ",") {
				known[strings.TrimSpace(name)] = true
			}
		}
	}
	if registered {
		for _, mb := range b.Spec.Members {
			if !known[mb.Name] {
				warnings = append(warnings, fmt.Sprintf("no runner serving %s has an agent named %s: it waits until one does (c4 farm agents add %s on that machine)",
					strings.Join(pools, ", "), mb.Name, mb.Name))
			}
		}
	}
	return b, errs, warnings
}

// bundleDue reads a bundle's due: RFC 3339, a date, or words on the
// farm's clock.
func (s *Server) bundleDue(due string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, due); err == nil {
		return t, nil
	}
	loc := s.Loc
	if loc == nil {
		loc = time.UTC
	}
	t, err := when.Parse(due, time.Now(), loc)
	if err != nil || t.IsZero() {
		return time.Time{}, fmt.Errorf("due %q is not a time this farm reads", due)
	}
	return t, nil
}

// bundleName is the request's name, or "agents" numbered past any bundle
// or job already called that (a bundle's job takes its name).
func (s *Server) bundleName(ctx context.Context, ns, want string) (string, error) {
	taken := func(name string) bool {
		var b c4v1.AgentBundle
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &b); err == nil {
			return true
		}
		var j c4v1.WorkJob
		return s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &j) == nil
	}
	if want != "" {
		if !memberName.MatchString(want) || strings.ToLower(want) != want {
			return "", fmt.Errorf("%q is not a bundle name: lower case letters, digits, . and -", want)
		}
		if taken(want) {
			return "", fmt.Errorf("%s already has a bundle or job called %s", ns, want)
		}
		return want, nil
	}
	name := defaultBundleName
	for i := 2; taken(name); i++ {
		name = fmt.Sprintf("%s-%d", defaultBundleName, i)
	}
	return name, nil
}

// handleBundles is GET /bundles/{project}: the project's bundles.
func (s *Server) handleBundles(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	project := r.PathValue("project")
	_, ns, err := s.project(m, v, project, "")
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	var list c4v1.AgentBundleList
	if err := s.Client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		fail(w, http.StatusServiceUnavailable, "reading the bundles: %v", err)
		return
	}
	out := []api.BundleResponse{}
	for i := range list.Items {
		out = append(out, bundleInfo(project, &list.Items[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleBundle is GET /bundles/{project}/{name}.
func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request) {
	b, project, _ := s.liveBundle(w, r)
	if b == nil {
		return
	}
	writeJSON(w, http.StatusOK, bundleInfo(project, b))
}

func bundleInfo(project string, b *c4v1.AgentBundle) api.BundleResponse {
	out := api.BundleResponse{OK: true, Name: b.Name, Project: project, Namespace: b.Namespace, Owner: b.Annotations[c4v1.AnnotationOwner],
		Link: bundleLink(b.Namespace, b.Name), Phase: b.Status.Phase}
	if b.Spec.Deadline != nil {
		d := b.Spec.Deadline.Time.UTC()
		out.Due = &d
	}
	states := map[string]c4v1.AgentMemberStatus{}
	for _, st := range b.Status.Members {
		states[st.Name] = st
	}
	for _, mb := range b.Spec.Members {
		st := states[mb.Name]
		state := st.State
		if state == "" {
			state = "queued"
		}
		out.Members = append(out.Members, api.BundleMemberInfo{Name: mb.Name, State: state, Node: st.Node})
	}
	// Why its members cannot start, if they cannot (the job's waits).
	for _, c := range b.Status.Conditions {
		if c.Type == "Ready" && c.Status == metav1.ConditionFalse {
			out.Warnings = append(out.Warnings, c.Message)
		}
	}
	return out
}

// liveBundle reads the bundle a request names, from the API server.
func (s *Server) liveBundle(w http.ResponseWriter, r *http.Request) (*c4v1.AgentBundle, string, *api.Viewer) {
	v := s.withViewer(w, r)
	if v == nil {
		return nil, "", nil
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return nil, "", nil
	}
	project := r.PathValue("project")
	_, ns, err := s.project(m, v, project, "")
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return nil, "", nil
	}
	var b c4v1.AgentBundle
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: r.PathValue("name")}, &b); err != nil {
		fail(w, http.StatusNotFound, "no bundle %s in %s", r.PathValue("name"), project)
		return nil, "", nil
	}
	return &b, project, v
}

// handleBundleDelete is DELETE /bundles/{project}/{name}: its members
// stop at their next turn's end and the bundle goes.
func (s *Server) handleBundleDelete(w http.ResponseWriter, r *http.Request) {
	b, _, v := s.liveBundle(w, r)
	if b == nil || unitOnOthersBundle(w, v, b) {
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, b.Namespace, "delete", "agentbundles", b.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	id := s.bundleControl(ctx, r, v, b, "delete-bundle", map[string]any{})
	if err := s.Client.Delete(ctx, b, client.Preconditions{UID: &b.UID}); err != nil && !apierrors.IsNotFound(err) {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	s.record(ctx, b, "AgentBundle", v.User, "Deleted", b.Name+": stopped and removed by "+v.User, bookNote{actor: actorOf(r, v), kind: "bundle", record: id})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": b.Name + ": stopped and removed", "record": id})
}

const errUnitBundles = "a unit cannot hold or remove a person's agents: the person, or a project admin, does"

// unitOnOthersBundle refuses a unit's hold or removal of a bundle that is
// not its own, as a unit revokes no person's grant: a block pod would
// otherwise stop what a person started. It says whether it refused.
func unitOnOthersBundle(w http.ResponseWriter, v *api.Viewer, b *c4v1.AgentBundle) bool {
	if v == nil || v.Unit == nil || b.Annotations[c4v1.AnnotationOwner] == v.User {
		return false
	}
	fail(w, http.StatusForbidden, errUnitBundles)
	return true
}

// handleBundleSuspend is POST /bundles/{project}/{name}/suspend
// {suspend}: hold the bundle (its members stop at their next turn's end)
// or let it run again.
func (s *Server) handleBundleSuspend(w http.ResponseWriter, r *http.Request) {
	b, _, v := s.liveBundle(w, r)
	if b == nil || unitOnOthersBundle(w, v, b) {
		return
	}
	var body struct {
		Suspend bool `json:"suspend"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, b.Namespace, "patch", "agentbundles", b.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	id := s.bundleControl(ctx, r, v, b, "suspend-bundle", body)
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"suspend": body.Suspend}})
	if err := s.Client.Patch(ctx, b, client.RawPatch(types.MergePatchType, patch)); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := b.Name + ": held"
	if !body.Suspend {
		msg = b.Name + ": running again"
	}
	s.record(ctx, b, "AgentBundle", v.User, "Suspend", msg+" by "+v.User, bookNote{actor: actorOf(r, v), kind: "bundle", record: id})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "record": id})
}

// bundleControl signs a control on the bundle's job, when the HUD has
// read it, and returns the record's ID ("" if it has not, or the farm
// signs nothing).
func (s *Server) bundleControl(ctx context.Context, r *http.Request, v *api.Viewer, b *c4v1.AgentBundle, action string, change any) string {
	m := s.current()
	if m == nil {
		return ""
	}
	rec := m.byKey[b.Namespace+"/"+controller.JobForBundle(b).Name]
	if rec == nil {
		return ""
	}
	j := rec.job.DeepCopy()
	id, err := s.controlJob(ctx, r, v, rec, j, "", action, change, func(p *controlPredicate) {
		p.Target = "agentbundle:" + b.Namespace + "/" + b.Name
		verb := "patch"
		if action == "delete-bundle" {
			verb = "delete"
		}
		p.Authorization = s.authorization(v, verb, "agentbundles", b.Namespace, b.Name)
	})
	if err != nil {
		return ""
	}
	return id
}
