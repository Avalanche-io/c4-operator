package server

// Submit grants (internal/hud/API.md, Grants). A person lets a Tracker
// submit work for them, as themselves: the HUD writes the SubmitGrant, the
// only writer of one (config/policy/submit-grants.yaml), with a signed
// control record of kind grant. A tracker's sync then submits with
// `for: <person>` and the person owns the job, so what is bound to them (a
// lease of their own sign-in) serves it. The person, or a project admin,
// revokes it; nobody grants for someone else.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

const (
	grantDefault = 30 * 24 * time.Hour
	grantMax     = 90 * 24 * time.Hour
)

// grantUntil reads a grant's end: an RFC 3339 time, or a span from now
// ("30d", "72h"); empty is the default.
func grantUntil(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return now.Add(grantDefault), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n <= 0 {
			return time.Time{}, fmt.Errorf("%q: want a number of days, like 30d", s)
		}
		return now.Add(time.Duration(n) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("%q is neither an RFC 3339 time nor a span like 30d or 72h", s)
	}
	return now.Add(d), nil
}

// isSync says the viewer is a tracker's sync (c4-sync), the only caller
// that may submit for another person.
func (s *Server) isSync(v *api.Viewer) bool {
	return v.User == c4v1.SyncAccount || (isDev(v) && v.User == c4v1.SyncDevViewer)
}

func grantView(g *c4v1.SubmitGrant, project string, now time.Time) api.Grant {
	out := api.Grant{Namespace: g.Namespace, Name: g.Name, Project: project, Person: g.Spec.Person, Tracker: g.Spec.Tracker,
		Pools: g.Spec.Pools, Classes: g.Spec.Classes, Until: g.Spec.Until.UTC(), Record: g.Spec.Record}
	if r := g.Spec.Revoked; r != nil {
		out.Revoked = &api.GrantRevoked{By: r.By, At: r.At.UTC(), Record: r.Record}
	}
	out.Why = g.Lapsed(now)
	out.Active = out.Why == ""
	return out
}

// noGrants is a farm without the kind installed (submit grants, or the
// optional Tracker).
func noGrants(err error) bool { return meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) }

func (s *Server) handleGrant(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	var body api.GrantRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	project, ns, err := s.project(m, v, body.Project, body.Namespace)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	if s.isSync(v) {
		fail(w, http.StatusForbidden, "a tracker's sync cannot grant itself: a person grants, as themselves")
		return
	}
	ctx := r.Context()
	now := time.Now()
	until, err := grantUntil(body.Until, now)
	if err != nil {
		fail(w, http.StatusBadRequest, "until: %v", err)
		return
	}
	if !until.After(now) || until.Sub(now) > grantMax {
		fail(w, http.StatusBadRequest, "until: a grant ends after now and within 90 days")
		return
	}
	if body.Tracker == "" {
		fail(w, http.StatusBadRequest, "name the tracker to grant")
		return
	}
	var tr c4v1.Tracker
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: body.Tracker}, &tr); err != nil {
		fail(w, http.StatusNotFound, "no tracker %s in project %s", body.Tracker, project)
		return
	}
	// A grant lets the tracker do no more than the person could: submit
	// work in this project.
	if err := s.authorize(ctx, r, v, ns, "create", "workjobs", ""); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	if s.Signer == nil && s.DevViewers == nil {
		fail(w, http.StatusServiceUnavailable, "a grant is a signed record, and this HUD has no signer")
		return
	}
	g := &c4v1.SubmitGrant{}
	key := types.NamespacedName{Namespace: ns, Name: c4v1.SubmitGrantName(tr.Name, v.User)}
	err = s.Client.Get(ctx, key, g)
	exists := err == nil
	switch {
	case noGrants(err):
		fail(w, http.StatusNotImplemented, "this farm has no submit grants installed")
		return
	case err != nil && !apierrors.IsNotFound(err):
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	g.Namespace, g.Name = key.Namespace, key.Name
	g.Spec = c4v1.SubmitGrantSpec{Person: v.User, Tracker: tr.Name, Pools: body.Pools, Classes: body.Classes,
		Until: metav1.NewTime(until.UTC().Truncate(time.Second))}
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Agent: claimedAgent(r),
		Authorization: s.authorization(v, "create", "workjobs", ns, ""), Action: "grant",
		Target: "submitgrant:" + ns + "/" + g.Name,
		Change: map[string]any{"person": v.User, "tracker": tr.Name, "pools": body.Pools, "classes": body.Classes, "until": g.Spec.Until.Time}}
	id, err := s.signControl(ctx, []string{project}, provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": sha256JSON(g.Spec)}}, pred)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	g.Spec.Record = id
	if exists {
		err = s.Client.Update(ctx, g)
	} else {
		err = s.Client.Create(ctx, g)
	}
	if err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	scope := describeScope(g)
	msg := fmt.Sprintf("%s lets tracker %s submit work for them%s, until %s", v.User, tr.Name, scope, g.Spec.Until.UTC().Format(time.RFC3339))
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	s.record(ctx, g, "SubmitGrant", v.User, "Granted", note, bookNote{actor: actorOf(r, v), kind: "grant", record: id})
	writeJSON(w, http.StatusOK, api.GrantResponse{OK: true, Message: msg, Grant: grantView(g, project, now), Record: id})
}

func describeScope(g *c4v1.SubmitGrant) string {
	var parts []string
	if len(g.Spec.Pools) > 0 {
		parts = append(parts, "in "+strings.Join(g.Spec.Pools, ", "))
	}
	if len(g.Spec.Classes) > 0 {
		parts = append(parts, "of class "+strings.Join(g.Spec.Classes, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// handleGrants lists grants: by default the viewer's own; ?tracker=ns/name
// lists who granted that Tracker; ?person= another person's, in projects
// the viewer sees.
func (s *Server) handleGrants(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	q := r.URL.Query()
	person, tracker := q.Get("person"), q.Get("tracker")
	if person == "" && tracker == "" {
		person = v.User
	}
	var list c4v1.SubmitGrantList
	if err := s.Client.List(r.Context(), &list); err != nil {
		if noGrants(err) {
			writeJSON(w, http.StatusOK, api.GrantList{Grants: []api.Grant{}})
			return
		}
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	now := time.Now()
	out := api.GrantList{Grants: []api.Grant{}}
	for i := range list.Items {
		g := &list.Items[i]
		project, ok := m.nsProject[g.Namespace]
		if !ok || !v.Sees(project) {
			continue
		}
		if person != "" && g.Spec.Person != person {
			continue
		}
		if tracker != "" && g.Namespace+"/"+g.Spec.Tracker != tracker && g.Spec.Tracker != tracker {
			continue
		}
		out.Grants = append(out.Grants, grantView(g, project, now))
	}
	slices.SortFunc(out.Grants, func(a, b api.Grant) int { return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name) })
	writeJSON(w, http.StatusOK, out)
}

// handleRevoke takes a grant back: the person who gave it, or a project
// admin (one allowed to delete submit grants there). The grant is kept,
// marked, with the revocation's record.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	project, ok := m.nsProject[ns]
	if !ok || !v.Sees(project) {
		fail(w, http.StatusNotFound, "no grant %s/%s", ns, name)
		return
	}
	ctx := r.Context()
	g := &c4v1.SubmitGrant{}
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, g); err != nil {
		fail(w, http.StatusNotFound, "no grant %s/%s", ns, name)
		return
	}
	authz := "the person who gave it"
	if g.Spec.Person != v.User {
		// Development viewers are not checked; only a farm-wide one may
		// stand in for an admin there.
		if isDev(v) && !v.All {
			fail(w, http.StatusForbidden, "only %s, or a project admin, revokes their grant", g.Spec.Person)
			return
		}
		if err := s.authorize(ctx, r, v, ns, "delete", "submitgrants", name); err != nil {
			fail(w, http.StatusForbidden, "only %s, or a project admin, revokes their grant: %v", g.Spec.Person, err)
			return
		}
		authz = s.authorization(v, "delete", "submitgrants", ns, name)
	}
	if g.Spec.Revoked != nil {
		fail(w, http.StatusConflict, "%s revoked it already", g.Spec.Revoked.By)
		return
	}
	before := sha256JSON(g.Spec)
	now := time.Now()
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Agent: claimedAgent(r), Authorization: authz,
		Action: "revoke", Target: "submitgrant:" + ns + "/" + name,
		Change: map[string]any{"person": g.Spec.Person, "tracker": g.Spec.Tracker, "grant": g.Spec.Record}}
	id, err := s.signControl(ctx, []string{project}, provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": before}}, pred)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	g.Spec.Revoked = &c4v1.GrantRevocation{By: v.User, At: metav1.NewTime(now.UTC().Truncate(time.Second)), Record: id}
	if err := s.Client.Update(ctx, g); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s revoked the grant from %s to tracker %s", v.User, g.Spec.Person, g.Spec.Tracker)
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	s.record(ctx, g, "SubmitGrant", v.User, "Revoked", note, bookNote{actor: actorOf(r, v), kind: "revoke", record: id})
	writeJSON(w, http.StatusOK, api.GrantResponse{OK: true, Message: msg, Grant: grantView(g, project, now), Record: id})
}

// errGrant is a submission for a person refused, with its status.
type errGrant struct {
	code int
	msg  string
}

func (e *errGrant) Error() string { return e.msg }

// grantRefused is a submission for a person refused for a reason a
// tracker's sync tells the person about (api.SubmitRefused): it answers
// beside the error, and unwraps to its errGrant for the status.
type grantRefused struct {
	*errGrant
	reason string
}

func (e *grantRefused) Unwrap() error { return e.errGrant }

func refuseGrant(code int, reason, msg string) error {
	return &grantRefused{&errGrant{code, msg}, reason}
}

// grantFor checks a submission for another person: only a tracker's sync
// sends one, naming its Tracker in trackerRef, under the person's grant to
// that Tracker, which must not have lapsed and must cover the job's pools
// and class. It returns the grant.
func (s *Server) grantFor(ctx context.Context, v *api.Viewer, ns string, body *api.SubmitRequest, spec *c4v1.WorkJobSpec) (*c4v1.SubmitGrant, error) {
	if !s.isSync(v) {
		return nil, refuseGrant(http.StatusForbidden, api.RefusedNotSync, "only a tracker's sync submits work for another person")
	}
	tracker, _, _ := strings.Cut(body.TrackerRef, "/")
	if tracker == "" {
		return nil, &errGrant{http.StatusBadRequest, "work for a person names the tracker it is from (trackerRef)"}
	}
	var tr c4v1.Tracker
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: tracker}, &tr); err != nil {
		if noGrants(err) {
			return nil, refuseGrant(http.StatusNotImplemented, api.RefusedNotInstalled, "this farm has no trackers installed")
		}
		return nil, refuseGrant(http.StatusForbidden, api.RefusedNoTracker, fmt.Sprintf("no tracker %s in %s", tracker, ns))
	}
	g := &c4v1.SubmitGrant{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: c4v1.SubmitGrantName(tracker, body.For)}, g); err != nil {
		if noGrants(err) {
			return nil, refuseGrant(http.StatusNotImplemented, api.RefusedNotInstalled, "this farm has no submit grants installed")
		}
		if apierrors.IsNotFound(err) {
			return nil, refuseGrant(http.StatusForbidden, api.RefusedNoGrant, fmt.Sprintf("no grant from %s for tracker %s/%s", body.For, ns, tracker))
		}
		return nil, &errGrant{http.StatusInternalServerError, err.Error()}
	}
	if g.Spec.Person != body.For || g.Spec.Tracker != tracker {
		return nil, refuseGrant(http.StatusForbidden, api.RefusedNoGrant, fmt.Sprintf("no grant from %s for tracker %s/%s", body.For, ns, tracker))
	}
	if why := g.Lapsed(time.Now()); why != "" {
		return nil, refuseGrant(http.StatusForbidden, api.RefusedGrantLapsed, why)
	}
	if why := g.Uncovered(spec.Pools, spec.Class); why != "" {
		return nil, refuseGrant(http.StatusForbidden, api.RefusedGrantUncovered, why)
	}
	return g, nil
}

// grantRef is what a job submitted under g carries: the grant's record,
// or, unsigned (development), the grant itself.
func grantRef(g *c4v1.SubmitGrant) string {
	if g.Spec.Record != "" {
		return g.Spec.Record
	}
	return "submitgrant:" + g.Namespace + "/" + g.Name
}
