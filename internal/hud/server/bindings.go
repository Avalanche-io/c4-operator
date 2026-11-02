package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/bindings"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// Bindings for one deployment (accountability DESIGN 3.3, S2b): a farm
// admin binds a node, a host runner, a tracker's sync, a project's
// importer or one of the manager's components to its owner, overriding
// the farm binding for that deployment. Each is a signed bind act (on the
// touch list, so confirmed, where the request carries one), booked in the
// farm book, in force from its booked position (farmbook.InForceAt).
//   - Its to names exactly one deployment, the identity the farm CA
//     certifies, and its for only narrows what the signer policy lets
//     that deployment sign: a node never signs control records.
//   - Two bindings for one deployment never overlap: a new owner is a
//     binding that follows the one in force (rotate), ending it at the
//     same instant, the changeover.
//   - A binding ends early by end-binding, by its owner (silent, like a
//     revoke) or by a farm admin (a touch: it changes who answers for the
//     deployment). The issuer stops certifying under it at once;
//     certificates already issued live out their hour or day, and a
//     record booked after the end is labelled revoked (DESIGN.md 9.2).
//   - The manager's components (component/hud, queue, controller) are the
//     farm owner's on a farm its owner runs. A managed farm's, the
//     provider's with the farm owner's countersignature, are ORGANIZATION
//     O6's.

// oneDeployment is a binding's to that names one deployment, not the
// whole farm.
var oneDeployment = regexp.MustCompile(c4v1.BindingToPattern)

// bindingAdmin is the one rule for who may bind a deployment, rotate a
// binding or end another owner's: until Parties (ORGANIZATION O1), the
// farm binding's owner, the farm's one admin. O1 replaces it with a member
// of the owner party holding admin (farmAdmins); nothing else decides it.
func bindingAdmin(farm *c4v1.DeploymentBinding, person string) error {
	if farm == nil {
		return fmt.Errorf("this farm has no owner bound yet: the installer binds it first (POST /api/v1/farm/binding)")
	}
	if person == "" || person != farm.Spec.Owner {
		return fmt.Errorf("%s may not bind the farm's deployments: that is the farm owner's, %s", orNoOne(person), farm.Spec.Owner)
	}
	return nil
}

func orNoOne(p string) string {
	if p == "" {
		return "no one signed in"
	}
	return p
}

// bindingLookup reads the bindings and the farm book as of now.
func (s *Server) bindingLookup(ctx context.Context) (*bindings.Lookup, error) {
	return (&bindings.Resolver{Client: s.Client, Book: s.FarmBookReader}).Lookup(ctx)
}

// bindingSubject is a binding as an act on it names it: rotating or
// ending it, by name and UID, at its spec's digest.
func bindingSubject(b *c4v1.DeploymentBinding) provenance.Subject {
	return provenance.Subject{Name: "deploymentbinding:" + b.Name + "@" + string(b.UID), Digest: map[string]string{"sha256": sha256JSON(b.Spec)}}
}

// bindingName is the name of a new binding for a deployment: its
// deployment and a digest of its spec, so each binding of it over time is
// its own object.
func bindingName(spec c4v1.DeploymentBindingSpec) string {
	dep := strings.ReplaceAll(c4v1.DeploymentOf(spec.To), "/", "-")
	if len(dep) > 200 {
		dep = strings.TrimRight(dep[:200], "-.")
	}
	return dep + "-" + sha256JSON(spec)[:10]
}

// mayBind refuses the viewers who never bind: a unit, an agent, a
// tracker's sync, a development viewer without the installer's token.
func (s *Server) mayBind(r *http.Request, v *api.Viewer) error {
	switch {
	case v.Unit != nil:
		return fmt.Errorf("%s is a unit: a farm admin binds a deployment", v.User)
	case agentActs(r) || s.isSync(v):
		return fmt.Errorf("an agent cannot bind a deployment: a farm admin does, as themselves")
	case isDev(v) && !byInstaller(v):
		return fmt.Errorf("without this machine's installer token, a development viewer is no one: sign in to bind a deployment")
	}
	return nil
}

// bindingFor checks a binding's for against what the signer policy lets
// its deployment sign, and returns it sorted; none is all of it.
func bindingFor(dep string, kinds []string) ([]string, error) {
	if len(kinds) == 0 {
		kinds = provenance.DeploymentKinds(dep)
	}
	if wider := provenance.NarrowsOnly(dep, kinds); len(wider) > 0 {
		return nil, fmt.Errorf("for: %s may not sign %s: a binding only narrows what the signer policy lets a deployment sign (%s)",
			dep, strings.Join(wider, ", "), strings.Join(provenance.DeploymentKinds(dep), ", "))
	}
	out := slices.Clone(kinds)
	slices.Sort(out)
	return slices.Compact(out), nil
}

// handleBindings answers GET /api/v1/bindings.
func (s *Server) handleBindings(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	if s.Client == nil {
		writeJSON(w, http.StatusOK, api.BindingsResponse{OK: true, Bindings: []api.FarmBinding{}})
		return
	}
	l, err := s.bindingLookup(r.Context())
	if err != nil {
		if noGrants(err) {
			writeJSON(w, http.StatusOK, api.BindingsResponse{OK: true, Bindings: []api.FarmBinding{}})
			return
		}
		fail(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	out := api.BindingsResponse{OK: true, Bindings: []api.FarmBinding{}}
	if f := l.Farm(); f != nil {
		out.Farm = c4v1.FarmOf(f.Spec.To)
	}
	for i := range l.Items {
		b := &l.Items[i]
		view := s.bindingView(b)
		view.InForce = l.InForce(b)
		out.Bindings = append(out.Bindings, *view)
	}
	sort.SliceStable(out.Bindings, func(i, j int) bool {
		a, b := out.Bindings[i], out.Bindings[j]
		if (a.Deployment == "*") != (b.Deployment == "*") {
			return a.Deployment == "*"
		}
		if a.Deployment != b.Deployment {
			return a.Deployment < b.Deployment
		}
		return a.Start.Before(b.Start)
	})
	writeJSON(w, http.StatusOK, out)
}

// handleMakeBinding answers POST /api/v1/bindings {owner, to, for?, end?}.
func (s *Server) handleMakeBinding(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	var body api.BindingRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {owner, to, for?, end?}: %v", err)
		return
	}
	s.bindDeployment(w, r, v, body.Owner, body.To, body.For, body.End, nil)
}

// handleRotateBinding answers POST /api/v1/bindings/{name}/rotate {owner,
// for?, end?}: a binding for the same deployment that follows this one.
func (s *Server) handleRotateBinding(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	var body api.RotateBindingRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {owner, for?, end?}: %v", err)
		return
	}
	old, ok := s.bindingNamed(w, r)
	if !ok {
		return
	}
	s.bindDeployment(w, r, v, body.Owner, old.Spec.To, body.For, body.End, old)
}

// bindingNamed reads the binding the path names.
func (s *Server) bindingNamed(w http.ResponseWriter, r *http.Request) (*c4v1.DeploymentBinding, bool) {
	name := r.PathValue("name")
	if s.Client == nil {
		fail(w, http.StatusNotFound, "no binding %s", name)
		return nil, false
	}
	var b c4v1.DeploymentBinding
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: name}, &b); err != nil {
		if apierrors.IsNotFound(err) || noGrants(err) {
			fail(w, http.StatusNotFound, "no binding %s", name)
			return nil, false
		}
		fail(w, http.StatusInternalServerError, "%v", err)
		return nil, false
	}
	return &b, true
}

// bindDeployment makes a binding for one deployment, or, with old, the
// binding that follows old and ends it at its start.
func (s *Server) bindDeployment(w http.ResponseWriter, r *http.Request, v *api.Viewer, owner, to string, kinds []string, end *time.Time, old *c4v1.DeploymentBinding) {
	ctx := r.Context()
	owner = strings.TrimSpace(owner)
	dep := c4v1.DeploymentOf(to)
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.mayBind(r, v); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	switch {
	case owner == "" || len(owner) > 253 || strings.ContainsAny(owner, " \t\r\n"):
		fail(w, http.StatusBadRequest, "owner: the deployment's owner, a person or an organization, without spaces")
		return
	case !oneDeployment.MatchString(to) || dep == "*":
		fail(w, http.StatusBadRequest, "to: one deployment of the farm, such as laptop-1/node/laptop-1: <farm>/node/<name>, <farm>/runner/<name>, <farm>/sync/<tracker>, <farm>/import/<project> or <farm>/component/{hud,queue,controller}; the farm binding (<farm>/*) is POST /api/v1/farm/binding")
		return
	case end != nil && !end.After(now):
		fail(w, http.StatusBadRequest, "end: a time after now, or none for a standing binding")
		return
	}
	kinds, err := bindingFor(dep, kinds)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err := s.authorize(ctx, r, v, "", "create", "deploymentbindings", ""); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	if s.Client == nil || s.Signer == nil {
		fail(w, http.StatusServiceUnavailable, "a binding is a signed record, and this HUD has no signer")
		return
	}
	l, err := s.bindingLookup(ctx)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	farm := l.Farm()
	person := s.ownerFor(ctx, v)
	if err := bindingAdmin(farm, person); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	switch {
	case c4v1.FarmOf(to) != c4v1.FarmOf(farm.Spec.To):
		fail(w, http.StatusBadRequest, "to: %s is not a deployment of this farm, %s", to, c4v1.FarmOf(farm.Spec.To))
		return
	case strings.HasPrefix(dep, "component/") && farm.Spec.RunFor != "":
		fail(w, http.StatusConflict, "%s is run for %s: its components are the provider's with the farm owner's countersignature, which this farm cannot make yet", c4v1.FarmOf(farm.Spec.To), farm.Spec.RunFor)
		return
	case strings.HasPrefix(dep, "component/") && owner != farm.Spec.Owner:
		fail(w, http.StatusBadRequest, "owner: %s is the farm owner's, %s, on a farm its owner runs", dep, farm.Spec.Owner)
		return
	}
	var current *c4v1.DeploymentBinding
	for i := range l.Items {
		if b := &l.Items[i]; b.Spec.To == to && l.InForce(b) {
			current = b
		}
	}
	switch {
	case old == nil && current != nil:
		fail(w, http.StatusConflict, "%s is bound to %s since %s (binding %s): a new owner follows it (POST /api/v1/bindings/%s/rotate)",
			dep, current.Spec.Owner, current.Spec.Start.UTC().Format(time.RFC3339), current.Name, current.Name)
		return
	case old != nil && (current == nil || current.Name != old.Name):
		fail(w, http.StatusConflict, "binding %s is not in force: only the binding in force for %s is followed", old.Name, dep)
		return
	}
	b := &c4v1.DeploymentBinding{Spec: c4v1.DeploymentBindingSpec{Owner: owner, To: to, For: kinds, Start: metav1.NewTime(now), By: actorOf(r, v)}}
	if end != nil {
		t := metav1.NewTime(end.UTC().Truncate(time.Second))
		b.Spec.End = &t
	}
	change := map[string]any{"owner": owner, "to": to, "right": "sign", "for": kinds, "start": now, "by": person}
	if end != nil {
		change["end"] = b.Spec.End.Time.Format(time.RFC3339Nano)
	}
	on, onOK := s.farmSubject(ctx)
	if old != nil {
		b.Spec.Follows = old.Spec.Record
		change["follows"], change["ends"] = old.Spec.Record, farmbook.BindingSubject(old.Name)
		on, onOK = bindingSubject(old), true
	}
	b.Name = bindingName(b.Spec)
	custody := CustodyDirect
	if isDev(v) {
		custody = CustodyClaimed
	}
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Agent: claimedAgent(r),
		Authorization: s.authorization(v, "create", "deploymentbindings", "", b.Name), Action: "bind",
		Target: farmbook.BindingSubject(b.Name), Custody: custody, Change: change}
	var act *citedAct
	if onOK {
		act, err = s.citeAct(ctx, r, v, on, "bind")
	} else if r.Header.Get(ActHeader) != "" {
		err = fmt.Errorf("%w: this HUD has no farm CA to name the farm by", errAct)
	}
	if err != nil {
		fail(w, controlStatus(err), "%v", err)
		return
	}
	subject := onAct(act, provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": sha256JSON(b.Spec)}}, &pred)
	id, rec, err := s.signRecord(ctx, subject, pred)
	if err != nil {
		fail(w, controlStatus(err), "control record: %v", err)
		return
	}
	b.Spec.Record, b.Spec.Signed = id, rec
	if err := s.Client.Create(ctx, b); err != nil {
		code := http.StatusInternalServerError
		if apierrors.IsAlreadyExists(err) {
			code = http.StatusConflict
		}
		fail(w, code, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s binds %s to %s", v.User, dep, owner)
	reason := "Bound"
	if old != nil {
		msg = fmt.Sprintf("%s binds %s to %s, following %s's binding %s, which ends now", v.User, dep, owner, old.Spec.Owner, old.Name)
		reason = "Rotated"
	}
	// the book is the binding's truth: its bind, and for a rotation the
	// old binding's end at the same position
	s.record(ctx, b, "DeploymentBinding", v.User, reason, msg+"; record "+shortID(id), bookNote{actor: actorOf(r, v), kind: "bind", record: id})
	if old != nil {
		old.Ended = &c4v1.DeploymentBindingEnd{At: b.Spec.Start, By: actorOf(r, v), Record: id, FollowedBy: id}
		if err := s.Client.Update(ctx, old); err != nil {
			// the book has ended it; the object says so at its next write
			s.Log.Error(err, "mark a followed binding ended", "binding", old.Name)
		}
	}
	writeJSON(w, http.StatusOK, api.FarmBindingResponse{OK: true, Message: msg, Binding: s.bindingView(b), Record: id})
}

// handleEndBinding answers POST /api/v1/bindings/{name}/end {owner}.
func (s *Server) handleEndBinding(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	var body api.EndBindingRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {owner}: %v", err)
		return
	}
	if err := s.mayBind(r, v); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	b, ok := s.bindingNamed(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, "", "create", "deploymentbindings", b.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	if s.Signer == nil {
		fail(w, http.StatusServiceUnavailable, "ending a binding is a signed record, and this HUD has no signer")
		return
	}
	l, err := s.bindingLookup(ctx)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	person := s.ownerFor(ctx, v)
	switch {
	case b.Spec.FarmWide():
		fail(w, http.StatusConflict, "the farm binding is not ended: a new farm owner follows it, which this farm cannot make yet")
		return
	case !l.InForce(b):
		fail(w, http.StatusConflict, "binding %s is not in force", b.Name)
		return
	case body.Owner != b.Spec.Owner:
		fail(w, http.StatusBadRequest, "owner: binding %s is %s's, and the act names %q", b.Name, b.Spec.Owner, body.Owner)
		return
	case person != b.Spec.Owner:
		// a farm admin ends another's binding: a touch (ActNeedsBy)
		if err := bindingAdmin(l.Farm(), person); err != nil {
			fail(w, http.StatusForbidden, "binding %s is %s's: its owner or the farm owner ends it; %v", b.Name, b.Spec.Owner, err)
			return
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Agent: claimedAgent(r),
		Authorization: s.authorization(v, "create", "deploymentbindings", "", b.Name), Action: "end-binding",
		Target: farmbook.BindingSubject(b.Name), Custody: CustodyDirect,
		Change: map[string]any{"binding": b.Spec.Record, "owner": b.Spec.Owner, "to": b.Spec.To, "by": person, "at": now}}
	if isDev(v) {
		pred.Custody = CustodyClaimed
	}
	act, err := s.citeAct(ctx, r, v, bindingSubject(b), "end-binding")
	if err != nil {
		fail(w, controlStatus(err), "%v", err)
		return
	}
	subject := onAct(act, provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": sha256JSON(b.Spec)}}, &pred)
	id, _, err := s.signRecord(ctx, subject, pred)
	if err != nil {
		fail(w, controlStatus(err), "control record: %v", err)
		return
	}
	b.Ended = &c4v1.DeploymentBindingEnd{At: metav1.NewTime(now), By: actorOf(r, v), Record: id}
	if err := s.Client.Update(ctx, b); err != nil {
		code := http.StatusInternalServerError
		if apierrors.IsConflict(err) {
			code = http.StatusConflict
		}
		fail(w, code, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s ends %s's binding of %s: the farm certifies it under it no more, its certificates already issued live out their hour or day, and a record booked after now is labelled revoked",
		v.User, b.Spec.Owner, c4v1.DeploymentOf(b.Spec.To))
	s.record(ctx, b, "DeploymentBinding", v.User, "BindingEnded", msg+"; record "+shortID(id), bookNote{actor: actorOf(r, v), kind: "end-binding", record: id})
	writeJSON(w, http.StatusOK, api.FarmBindingResponse{OK: true, Message: msg, Binding: s.bindingView(b), Record: id})
}
