package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// The farm binding (DESIGN.md section 9.2): the installer names the farm's
// owner once (POST /api/v1/farm/binding), and the HUD binds every
// deployment of the farm to them, so the records the farm signs accrue to
// someone. It is a signed control record (action bind) and a
// DeploymentBinding named farm, which only the HUD writes and nobody
// changes. Binding again to the same owner is a no-op; a different owner
// is an owner change, a new binding that follows this one, which is not
// built yet.
//
// Bound to someone is not one person: only a binding the installer marks
// onePerson (a local farm's bind request) makes its owner the farm's one
// person, who holds every role. A shared farm bound to lee leaves lee in
// lee's own roles, and stand-in to the farm's policy.

// farmTo is a farm binding's deployments: every one of the farm.
var farmTo = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/\*$`)

// boundKinds are the record kinds every deployment signs under the farm
// binding.
var boundKinds = []string{"task/v1", "resolution/v1", "decision/v1", "job/v1", "control/v1", "import/v1", "ingest/v1"}

// The words a farm binding's view gives its onePerson, in effect or not.
const (
	onePersonInEffect    = "while this HUD is reached only with this machine's installer token"
	onePersonNotInEffect = "not in effect: this HUD has no installer token"
)

// bindingView is the binding as the HUD shows it, with whether its
// onePerson holds now: only on a HUD guarded by an installer token.
func (s *Server) bindingView(b *c4v1.DeploymentBinding) *api.FarmBinding {
	out := &api.FarmBinding{Owner: b.Spec.Owner, To: b.Spec.To, RunFor: b.Spec.RunFor, OnePerson: b.Spec.OnePerson, For: b.Spec.For,
		Start: b.Spec.Start.Time, By: b.Spec.By, Record: b.Spec.Record, Follows: b.Spec.Follows}
	if b.Spec.SingleOwner() != "" {
		out.OnePersonInEffect, out.OnePersonNote = s.installerGuarded(), onePersonNotInEffect
		if out.OnePersonInEffect {
			out.OnePersonNote = onePersonInEffect
		}
	}
	return out
}

// farmBinding reads the farm binding: nil, nil when there is none.
func (s *Server) farmBinding(ctx context.Context) (*c4v1.DeploymentBinding, error) {
	if s.Client == nil {
		return nil, nil
	}
	var b c4v1.DeploymentBinding
	err := s.Client.Get(ctx, types.NamespacedName{Name: c4v1.FarmBindingName}, &b)
	switch {
	case apierrors.IsNotFound(err) || noGrants(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &b, nil
}

// singlePerson is the person who holds every role on this farm: the farm
// binding's owner where the installer bound it as one person's
// (onePerson) and the HUD is guarded by an installer token; no one on a
// farm bound any other way, or on a HUD without the token; and
// --single-person-farm (singlePersonFlag) on a farm with no binding.
func (s *Server) singlePerson(ctx context.Context) string {
	b, err := s.farmBinding(ctx)
	if err != nil || b == nil {
		return s.singlePersonFlag()
	}
	if !s.installerGuarded() {
		return ""
	}
	return b.Spec.SingleOwner()
}

// handleFarmBinding answers GET /api/v1/farm/binding.
func (s *Server) handleFarmBinding(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	b, err := s.farmBinding(r.Context())
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, "%v", err)
	case b == nil:
		fail(w, http.StatusNotFound, "this farm has no owner bound yet: the installer binds it (POST /api/v1/farm/binding)")
	default:
		writeJSON(w, http.StatusOK, api.FarmBindingResponse{OK: true, Binding: s.bindingView(b), Record: b.Spec.Record})
	}
}

// handleBind answers POST /api/v1/farm/binding {owner, to, runFor?,
// onePerson?}.
func (s *Server) handleBind(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	var body api.FarmBindingRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {owner, to, runFor?, onePerson?}: %v", err)
		return
	}
	body.Owner, body.RunFor = strings.TrimSpace(body.Owner), strings.TrimSpace(body.RunFor)
	switch {
	case v.Unit != nil:
		fail(w, http.StatusForbidden, "%s is a unit: the installer binds a farm to its owner", v.User)
		return
	case agentActs(r) || s.isSync(v):
		fail(w, http.StatusForbidden, "an agent cannot bind a farm to an owner: the installer does, as themselves")
		return
	case isDev(v) && !byInstaller(v):
		fail(w, http.StatusForbidden, "without this machine's installer token, a development viewer is no one: sign in to bind the farm, or bind it with the installer token")
		return
	case body.OnePerson && !s.installerGuarded():
		fail(w, http.StatusForbidden, "onePerson: this HUD has no installer token, so the farm is not one person's")
		return
	case body.Owner == "" || len(body.Owner) > 253 || strings.ContainsAny(body.Owner, " \t\r\n"):
		fail(w, http.StatusBadRequest, "owner: the farm's owner, a person or an organization, without spaces")
		return
	case !farmTo.MatchString(body.To):
		fail(w, http.StatusBadRequest, "to: <farm>/*, every deployment of the farm, such as laptop-1/*")
		return
	case len(body.RunFor) > 253 || strings.ContainsAny(body.RunFor, " \t\r\n"):
		fail(w, http.StatusBadRequest, "runFor: the party a managed farm is run for, without spaces")
		return
	case body.OnePerson && body.RunFor != "":
		fail(w, http.StatusBadRequest, "onePerson: a farm run for %s is not one person's", body.RunFor)
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, "", "create", "deploymentbindings", c4v1.FarmBindingName); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	old, err := s.farmBinding(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if old != nil {
		if old.Spec.Owner == body.Owner && old.Spec.To == body.To && old.Spec.RunFor == body.RunFor && old.Spec.OnePerson == body.OnePerson {
			writeJSON(w, http.StatusOK, api.FarmBindingResponse{OK: true, Existing: true, Binding: s.bindingView(old), Record: old.Spec.Record,
				Message: fmt.Sprintf("this farm is already bound to %s", old.Spec.Owner)})
			return
		}
		fail(w, http.StatusConflict, "this farm is bound to %s since %s (record %s): a new owner takes an owner change, a new binding that follows this one, which this farm cannot make yet",
			describeOwner(old), old.Spec.Start.UTC().Format(time.RFC3339), shortID(old.Spec.Record))
		return
	}
	if s.Signer == nil {
		fail(w, http.StatusServiceUnavailable, "a farm binding is a signed record, and this HUD has no signer")
		return
	}
	b := &c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: c4v1.FarmBindingName}, Spec: c4v1.DeploymentBindingSpec{
		Owner: body.Owner, To: body.To, RunFor: body.RunFor, OnePerson: body.OnePerson, For: boundKinds,
		Start: metav1.NewTime(time.Now().UTC().Truncate(time.Second)), By: actorOf(r, v)}}
	custody := CustodyDirect
	if isDev(v) {
		// a development viewer on the installer's secret: their word
		custody = CustodyClaimed
	}
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Agent: claimedAgent(r),
		Authorization: s.authorization(v, "create", "deploymentbindings", "", c4v1.FarmBindingName), Action: "bind",
		Target: "deploymentbinding:" + c4v1.FarmBindingName, Custody: custody,
		Change: map[string]any{"owner": b.Spec.Owner, "to": b.Spec.To, "runFor": b.Spec.RunFor, "onePerson": b.Spec.OnePerson, "right": "sign", "for": b.Spec.For, "start": b.Spec.Start.Time}}
	id, rec, err := s.signRecord(ctx, provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": sha256JSON(b.Spec)}}, pred)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	b.Spec.Record, b.Spec.Signed = id, rec
	if err := s.Client.Create(ctx, b); err != nil {
		code := http.StatusInternalServerError
		if apierrors.IsAlreadyExists(err) {
			// another bind won the race: answer as if this one came second
			code = http.StatusConflict
		}
		fail(w, code, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s binds every deployment of %s to %s", v.User, strings.TrimSuffix(b.Spec.To, "/*"), describeOwner(b))
	s.record(ctx, b, "DeploymentBinding", v.User, "Bound", msg+"; record "+shortID(id), bookNote{actor: actorOf(r, v), kind: "bind", record: id})
	writeJSON(w, http.StatusOK, api.FarmBindingResponse{OK: true, Message: msg, Binding: s.bindingView(b), Record: id})
}

func describeOwner(b *c4v1.DeploymentBinding) string {
	switch {
	case b.Spec.RunFor != "":
		return b.Spec.Owner + ", who runs it for " + b.Spec.RunFor
	case b.Spec.OnePerson:
		return b.Spec.Owner + ", its one person"
	}
	return b.Spec.Owner
}

// signRecord signs a control record that belongs to the farm rather than
// a project: it returns the record, and keeps a copy in every project's
// store there is.
func (s *Server) signRecord(ctx context.Context, subject provenance.Subject, pred controlPredicate) (string, []byte, error) {
	if pred.Agent != "" {
		pred.AgentGrade = CustodyClaimed
	}
	rec, err := provenance.SignStatement(ctx, s.Signer, provenance.PredicateControl, []provenance.Subject{subject}, pred, time.Now())
	if err != nil {
		return "", nil, err
	}
	id := c4.Identify(bytes.NewReader(rec))
	if m := s.current(); m != nil && s.Stores != nil {
		for _, p := range m.projects {
			if st, err := s.Stores(ctx, p.Name); err == nil && st != nil {
				st.Put(ctx, id, bytes.NewReader(rec), int64(len(rec)))
			}
		}
	}
	return id.String(), rec, nil
}
