package server

// The people map, confirmed (internal/hud/API.md, People). A Tracker's
// spec.people offers its logins as C4 people; that is the project's word,
// and alone it lets an admin make any login decide as anyone. So a mapping
// counts once the person it names confirms it, as themselves: the HUD signs
// the offer and the confirmation as one control record (kind
// people-confirm) and writes the TrackerPerson, the only writer of one.
// For party:<name>, a member of the party confirms. A relayed decision
// names the confirmation it rests on; one by an unconfirmed login is
// relayed marked unconfirmed (relay.go).

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// speaksAs says whether v is person: the person themselves, or a member of
// the party.
func speaksAs(v *api.Viewer, person string) bool {
	if p, ok := strings.CutPrefix(person, "party:"); ok {
		return slices.Contains(v.Parties, p)
	}
	return person != "" && person == v.User
}

func peopleAct(a *c4v1.TrackerPersonAct) *api.PeopleAct {
	if a == nil {
		return nil
	}
	return &api.PeopleAct{By: a.By, At: a.At.UTC(), Record: a.Record}
}

func peopleEntry(tp *c4v1.TrackerPerson, project string, v *api.Viewer) api.PeopleEntry {
	e := api.PeopleEntry{Namespace: tp.Namespace, Project: project, Tracker: tp.Spec.Tracker, Login: tp.Spec.Login,
		Person: tp.Spec.Person, State: "confirmed", Name: tp.Name, Confirmed: peopleAct(&tp.Spec.Accepted),
		Revoked: peopleAct(tp.Spec.Revoked), Yours: speaksAs(v, tp.Spec.Person), Lets: lets(tp.Spec.Tracker, tp.Spec.Login, tp.Spec.Person)}
	if tp.Spec.Revoked != nil {
		e.State = "revoked"
	}
	return e
}

// handlePeople lists the people maps of the Trackers in projects the
// viewer sees: every offer, confirmed or not. ?tracker=ns/name for one
// Tracker's; ?mine=1 for the viewer's own; ?all=1 adds revoked entries.
func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	ctx := r.Context()
	out := api.PeopleList{Entries: []api.PeopleEntry{}}
	var trs c4v1.TrackerList
	if err := s.Client.List(ctx, &trs); err != nil {
		if noGrants(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	var tps c4v1.TrackerPersonList
	if err := s.Client.List(ctx, &tps); err != nil && !noGrants(err) {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	q := r.URL.Query()
	keep := func(e api.PeopleEntry) {
		if (q.Get("tracker") == "" || q.Get("tracker") == e.Namespace+"/"+e.Tracker) && (q.Get("mine") == "" || e.Yours) &&
			(e.State != "revoked" || q.Get("all") != "") {
			out.Entries = append(out.Entries, e)
		}
	}
	confirmed := map[string]bool{}
	for i := range tps.Items {
		tp := &tps.Items[i]
		project, ok := m.nsProject[tp.Namespace]
		if !ok || !v.Sees(project) {
			continue
		}
		if tp.Spec.Revoked == nil {
			confirmed[tp.Namespace+"/"+tp.Name] = true
		}
		keep(peopleEntry(tp, project, v))
	}
	for i := range trs.Items {
		t := &trs.Items[i]
		project, ok := m.nsProject[t.Namespace]
		if !ok || !v.Sees(project) {
			continue
		}
		for login, person := range t.Spec.People {
			if confirmed[t.Namespace+"/"+c4v1.TrackerPersonName(t.Name, login, person)] {
				continue
			}
			keep(api.PeopleEntry{Namespace: t.Namespace, Project: project, Tracker: t.Name, Login: login, Person: person,
				State: "offered", Yours: speaksAs(v, person), Lets: lets(t.Name, login, person)})
		}
	}
	slices.SortFunc(out.Entries, func(a, b api.PeopleEntry) int {
		return strings.Compare(a.Namespace+"/"+a.Tracker+"/"+a.Login+"/"+a.State, b.Namespace+"/"+b.Tracker+"/"+b.Login+"/"+b.State)
	})
	writeJSON(w, http.StatusOK, out)
}

// handlePeopleConfirm is a person confirming that a login the Tracker
// offers as them is them. Confirming a login replaces (revokes) another
// person's confirmation for the same login; confirming again renews it.
func (s *Server) handlePeopleConfirm(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	var body api.PeopleConfirm
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	project, ns, err := s.project(m, v, body.Project, body.Namespace)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	if s.isSync(v) || v.Unit != nil || via(r) == "mcp" || r.Header.Get("X-C4-Agent") != "" {
		fail(w, http.StatusForbidden, "only the person confirms that a tracker login is them, as themselves in the HUD; a tracker's sync, a unit or an agent may not")
		return
	}
	ctx := r.Context()
	var tr c4v1.Tracker
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: body.Tracker}, &tr); err != nil {
		fail(w, http.StatusNotFound, "no tracker %s in project %s", body.Tracker, project)
		return
	}
	person, ok := tr.Spec.People[body.Login]
	if !ok || body.Login == "" {
		fail(w, http.StatusNotFound, "tracker %s offers no person for login %q", tr.Name, body.Login)
		return
	}
	if !speaksAs(v, person) {
		fail(w, http.StatusForbidden, "tracker %s offers @%s as %s: only %s confirms it", tr.Name, body.Login, person, whoIs(person))
		return
	}
	if s.Signer == nil && s.DevViewers == nil {
		fail(w, http.StatusServiceUnavailable, "a confirmation is a signed record, and this HUD has no signer")
		return
	}
	name := c4v1.TrackerPersonName(tr.Name, body.Login, person)
	tp := &c4v1.TrackerPerson{}
	err = s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, tp)
	exists := err == nil
	switch {
	case noGrants(err):
		fail(w, http.StatusNotImplemented, "this farm has no tracker people installed")
		return
	case err != nil && !apierrors.IsNotFound(err):
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	offer := c4v1.TrackerPersonOffer{From: "spec.people", Generation: tr.Generation}
	target := "trackerperson:" + ns + "/" + name
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r), Via: via(r),
		Authorization: "the person the login is offered as (" + person + ")", Action: "people-confirm", Target: target,
		Change: map[string]any{"tracker": tr.Name, "login": body.Login, "person": person, "offer": offer}}
	id, err := s.signControl(ctx, []string{project}, provenance.Subject{Name: target,
		Digest: map[string]string{"sha256": sha256JSON(pred.Change)}}, pred)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	tp.Namespace, tp.Name = ns, name
	tp.Spec = c4v1.TrackerPersonSpec{Tracker: tr.Name, Login: body.Login, Person: person, Offer: offer,
		Accepted: c4v1.TrackerPersonAct{By: v.User, At: now, Record: id}}
	if exists {
		err = s.Client.Update(ctx, tp)
	} else {
		err = s.Client.Create(ctx, tp)
	}
	if err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	// One person per login: another confirmation for it no longer stands.
	var tps c4v1.TrackerPersonList
	if err := s.Client.List(ctx, &tps, client.InNamespace(ns)); err == nil {
		for i := range tps.Items {
			o := &tps.Items[i]
			if o.Name == name || o.Spec.Tracker != tr.Name || o.Spec.Login != body.Login || o.Spec.Revoked != nil {
				continue
			}
			o.Spec.Revoked = &c4v1.TrackerPersonAct{By: v.User, At: now, Record: id}
			if err := s.Client.Update(ctx, o); err != nil {
				s.Log.Error(err, "revoke a replaced tracker login", "entry", ns+"/"+o.Name)
			}
		}
	}
	msg := fmt.Sprintf("%s confirmed that @%s on tracker %s is %s", v.User, body.Login, tr.Name, person)
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	s.record(ctx, tp, "TrackerPerson", v.User, "Confirmed", note, bookNote{actor: actorOf(r, v), kind: "people-confirm", record: id})
	writeJSON(w, http.StatusOK, api.PeopleResponse{OK: true, Message: msg, Entry: peopleEntry(tp, project, v), Record: id})
}

// lets says what a confirmed login does: what the person grants by
// confirming it.
func lets(tracker, login, person string) string {
	p := strings.TrimPrefix(person, "party:")
	return fmt.Sprintf("decisions @%s makes in tracker %s are relayed as %s's, and serve %s's waits", login, tracker, p, p)
}

// whoIs is whom a person names, in words, for a refusal.
func whoIs(person string) string {
	if p, ok := strings.CutPrefix(person, "party:"); ok {
		return "a member of " + p
	}
	return person
}

// handlePeopleRevoke takes a confirmation back: the person, a member of
// the party, or a project admin (one allowed to delete trackerpeople
// there). The entry is kept, marked, with the revocation's record.
func (s *Server) handlePeopleRevoke(w http.ResponseWriter, r *http.Request) {
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
		fail(w, http.StatusNotFound, "no confirmed login %s/%s", ns, name)
		return
	}
	ctx := r.Context()
	tp := &c4v1.TrackerPerson{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, tp); err != nil {
		fail(w, http.StatusNotFound, "no confirmed login %s/%s", ns, name)
		return
	}
	authz := "the person the login is (" + tp.Spec.Person + ")"
	if !speaksAs(v, tp.Spec.Person) {
		if s.DevViewers != nil && !v.All {
			fail(w, http.StatusForbidden, "only %s, or a project admin, revokes it", whoIs(tp.Spec.Person))
			return
		}
		if err := s.authorize(ctx, r, v, ns, "delete", "trackerpeople", name); err != nil {
			fail(w, http.StatusForbidden, "only %s, or a project admin, revokes it: %v", whoIs(tp.Spec.Person), err)
			return
		}
		authz = s.authorization("delete", "trackerpeople", ns, name)
	}
	if tp.Spec.Revoked != nil {
		fail(w, http.StatusConflict, "%s revoked it already", tp.Spec.Revoked.By)
		return
	}
	target := "trackerperson:" + ns + "/" + name
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r), Via: via(r), Authorization: authz,
		Action: "people-revoke", Target: target,
		Change: map[string]any{"tracker": tp.Spec.Tracker, "login": tp.Spec.Login, "person": tp.Spec.Person, "confirmation": tp.Spec.Accepted.Record}}
	id, err := s.signControl(ctx, []string{project}, provenance.Subject{Name: target,
		Digest: map[string]string{"sha256": sha256JSON(tp.Spec)}}, pred)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	tp.Spec.Revoked = &c4v1.TrackerPersonAct{By: v.User, At: metav1.NewTime(time.Now().UTC().Truncate(time.Second)), Record: id}
	if err := s.Client.Update(ctx, tp); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s revoked that @%s on tracker %s is %s", v.User, tp.Spec.Login, tp.Spec.Tracker, tp.Spec.Person)
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	s.record(ctx, tp, "TrackerPerson", v.User, "Revoked", note, bookNote{actor: actorOf(r, v), kind: "people-revoke", record: id})
	writeJSON(w, http.StatusOK, api.PeopleResponse{OK: true, Message: msg, Entry: peopleEntry(tp, project, v), Record: id})
}

// confirmedLogin is the standing confirmation that tracker t's login is
// person, if any: what a relayed decision rests on.
func (s *Server) confirmedLogin(ctx context.Context, ns, tracker, login, person string) *c4v1.TrackerPerson {
	tp := &c4v1.TrackerPerson{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: c4v1.TrackerPersonName(tracker, login, person)}, tp); err != nil {
		return nil
	}
	if tp.Spec.Revoked != nil || tp.Spec.Tracker != tracker || tp.Spec.Login != login || tp.Spec.Person != person {
		return nil
	}
	return tp
}
