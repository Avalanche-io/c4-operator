package server

// Parties (accountability ORGANIZATION 1 and 2; internal/hud/API.md,
// Parties): the farm's party list. A Party is a person or an organisation,
// by the bare name records use, kept as a cluster-scoped Party that only
// the HUD writes (config/policy/parties.yaml) and nobody deletes. Who may
// act for an organisation is not in the object: each membership, of a
// Party by its name or of a group of the studio's own, with the purposes
// it holds, is a signed control record booked in the farm book, and what
// is in force is the book's one rule (farmbook.InForceAt,
// ForceMembership). So a farm without a farm book has no Parties, and its
// party: waits take the c4:party:<name> group, as before.
//
// Who is who (PEOPLE-KEYS-2 10.4 item 27):
//   - a viewer is a person Party's person only through a sign-in the Party
//     lists by name: nothing falls back to the user name (R1);
//   - a sign-in goes on a Party only by signing in with it: the name the
//     HUD signs the person in as, or a provider subject a fresh ID token
//     proves (R2). So a person makes their own person Party, and adds
//     their own sign-ins; they, or a farm admin, remove one;
//   - a membership names a Party, never a bare sign-in name (R3);
//   - making an organisation never makes its maker an admin: it names its
//     first admin, who counts once they accept with their own touched act
//     (R4);
//   - an organisation's admin adds, changes and ends its members, never
//     grants for themselves (a purpose held only by a group is never
//     copied into their named membership), and never removes the last
//     named admin, each judged inside the book's append (R5);
//   - a member may end their own membership;
//   - an agent, a unit and a tracker's sync never change a party, and are
//     never members.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

// The purposes a member may hold (ORGANIZATION 2).
const (
	// PurposeMember: act for the organisation where a wait or a rule
	// names it (a party: wait).
	PurposeMember = "member"
	// PurposeAdmin: change the organisation's members and its policy, and
	// own its projects.
	PurposeAdmin = "admin"
	// PurposeApproveSpend: approve spend past a cap at its scope (SPEND).
	PurposeApproveSpend = "approve-spend"
	// PurposeViewSpend: see its spend by show, project and team (SPEND).
	PurposeViewSpend = "view-spend"
	// PurposeViewPeople: see spend per person, where its policy turns
	// per-person views on (SPEND 5).
	PurposeViewPeople = "view-people"
	// PurposeApproveEgress: sign a project's standing approval to send
	// content off the farm (PROVIDERS 3.5, DESIGN D10).
	PurposeApproveEgress = "approve-egress"
)

// Purposes are the purposes, in the order the HUD shows them.
var Purposes = []string{PurposeMember, PurposeAdmin, PurposeApproveSpend, PurposeViewSpend, PurposeViewPeople, PurposeApproveEgress}

// ORGANIZATION section 9's recommendations, built as recommended, each
// one switch.
var (
	// MembersByGroup (9.1): a member may hold purposes by a group of the
	// studio's own sign-in, through a group rule an admin signed. Membership
	// is issuance (Joshua, 11-03): the signed rule issues it, and the group
	// is only the evidence that a sign-in falls under it. Off, every member
	// is named in C4: group rules are refused and read as nothing. Either
	// way, the provider's c4:party:<name> group alone holds nothing in a
	// Party the farm keeps; it speaks only for a party the farm keeps none
	// of, as before the farm book.
	MembersByGroup = true
	// ViewPeopleNeedsPolicy (9.3): view-people is its own purpose, which
	// view-spend does not imply, and it is in effect only where the
	// organisation's policy turns per-person views on (perPersonViews).
	// Off, a granted view-people is in effect at once.
	ViewPeopleNeedsPolicy = true
)

// firstAdmin are the purposes an organisation's first admin holds once
// they accept.
var firstAdmin = []string{PurposeMember, PurposeAdmin}

var (
	// partyName is a party's bare name (c4v1.PartyName).
	partyName = c4v1.PartyName
	// subjectName is a sign-in subject, and groupName a studio group.
	subjectName = regexp.MustCompile(`^[^\s,]{1,253}$`)
)

// partyCache keeps what reading memberships costs, so a request doesn't
// rescan: each record's verified purposes, by its C4 ID and the act and
// subject of the entry citing it (a record never changes), and each key's
// in-force entry at one view of the book, by its position and last entry.
type partyCache struct {
	mu       sync.Mutex
	purposes map[string][]string
	at       string
	memo     map[string]*farmbook.Sealed
}

// parties are the farm's Parties: none on a farm without the kind.
func (s *Server) parties(ctx context.Context) ([]c4v1.Party, error) {
	if s.Client == nil {
		return nil, nil
	}
	var l c4v1.PartyList
	if err := s.reader().List(ctx, &l); err != nil {
		if noGrants(err) {
			return nil, nil
		}
		return nil, err
	}
	slices.SortFunc(l.Items, func(a, b c4v1.Party) int { return strings.Compare(a.Name, b.Name) })
	return l.Items, nil
}

// findParty is the Party named name among ps.
func findParty(ps []c4v1.Party, name string) *c4v1.Party {
	for i := range ps {
		if ps[i].Name == name {
			return &ps[i]
		}
	}
	return nil
}

// partySubject is a Party as an act changing it, or its members, names it.
func partySubject(p *c4v1.Party) provenance.Subject {
	// the farm's, in no project: party:<name>@<uid>
	return provenance.Subject{Name: "party:" + p.Name + "@" + string(p.UID), Digest: map[string]string{"sha256": sha256JSON(p.Spec)}}
}

// partyPerson is the person Party v is (R1): the one person Party that
// lists the name v signed in with among its sign-ins. Nothing else makes
// v a Party's person: not v's user name, nor whom its work belongs to.
// "" for a unit, for no Party's sign-in, and for one two Parties list.
func (s *Server) partyPerson(v *api.Viewer, ps []c4v1.Party) string {
	if v == nil || v.Unit != nil || v.User == "" {
		return ""
	}
	var held []string
	for i := range ps {
		if ps[i].Spec.Kind == c4v1.PartyPerson && slices.Contains(ps[i].Spec.SignIns, c4v1.PartySignIn{Subject: v.User}) {
			held = append(held, ps[i].Name)
		}
	}
	if len(held) == 1 {
		return held[0]
	}
	// one sign-in is one person's: in two people's, it is no one
	return ""
}

// partyPersonOf is partyPerson, the Parties read.
func (s *Server) partyPersonOf(ctx context.Context, v *api.Viewer) string {
	ps, err := s.parties(ctx)
	if err != nil {
		return ""
	}
	return s.partyPerson(v, ps)
}

// partyBook is the farm book as a Party's members are judged by: the
// sealed book and the entries pending its seal, now.
func (s *Server) partyBook(ctx context.Context) (*farmbook.View, error) {
	if s.FarmBookReader == nil {
		return nil, errors.New(noBookParties)
	}
	return s.FarmBookReader.View(ctx, time.Now())
}

// viewKey names one view of the book: its position and its last entry.
func viewKey(view *farmbook.View) string {
	es := view.All()
	if len(es) == 0 {
		return "0"
	}
	return fmt.Sprint(view.Position(), es[len(es)-1].ID)
}

// inForce is what put key in force in view, now: the book's one rule,
// kept per view.
func (s *Server) inForce(view *farmbook.View, kind, key string) *farmbook.Sealed {
	c := &s.partyMemo
	at := viewKey(view)
	c.mu.Lock()
	if c.at != at {
		c.at, c.memo = at, map[string]*farmbook.Sealed{}
	}
	on, ok := c.memo[kind+" "+key]
	c.mu.Unlock()
	if ok {
		return on
	}
	on, _ = view.InForceAt(kind, key, view.Position(), view.Now)
	c.mu.Lock()
	if c.at == at {
		c.memo[kind+" "+key] = on
	}
	c.mu.Unlock()
	return on
}

// purposesOf reads the purposes the record that opened a membership
// grants (farmbook.MemberPurposes), once per record and the entry citing
// it: the check binds the record to the entry's act and subject, so an
// entry citing another's record is checked anew.
func (s *Server) purposesOf(ctx context.Context, on *farmbook.Sealed) ([]string, error) {
	c := &s.partyMemo
	key := on.Entry.Record + " " + on.Entry.Act + " " + on.Entry.Subject
	c.mu.Lock()
	got, ok := c.purposes[key]
	c.mu.Unlock()
	if ok {
		return got, nil
	}
	got, err := farmbook.MemberPurposes(ctx, s.FarmBookReader.Store, s.trust(ctx).roots, s.FarmBookReader.Farm, on)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.purposes == nil {
		c.purposes = map[string][]string{}
	}
	c.purposes[key] = got
	c.mu.Unlock()
	return got, nil
}

// inEffect are the purposes of a membership of p that hold now.
func inEffect(p *c4v1.Party, purposes []string) []string {
	out := []string{}
	for _, x := range purposes {
		if x == PurposeViewPeople && ViewPeopleNeedsPolicy && !p.Spec.PerPersonViews {
			continue
		}
		out = append(out, x)
	}
	return out
}

// partyGroups are the studio's groups that stand for party: c4:party:<name>
// and c4-party-<name>, which hold member and nothing more (ORGANIZATION 2).
func partyGroups(party string) []string {
	return []string{"c4:party:" + party, "c4-party-" + party}
}

// madeBy is party's first make-party in view, and the first admin it
// names; nil when the book has none.
func madeBy(view *farmbook.View, party string) (*farmbook.Sealed, string) {
	for _, s := range view.All() {
		if s.Entry.Kind == farmbook.KindControl && s.Entry.Act == "make-party" && s.Entry.Subject == farmbook.PartyKey(party) {
			return s, s.Entry.Person
		}
	}
	return nil, ""
}

// membership is one membership of a Party in force.
type membership struct {
	person, group string
	purposes      []string
	on            *farmbook.Sealed
	// lapsed says why it holds nothing: it names no Party (R3), or its
	// record does not verify.
	lapsed string
}

// membersOf are p's memberships in force in view: by name, then by group,
// each in the order first booked. A named membership whose name is no
// Party among ps is lapsed, and holds nothing. The c4:party: group, being
// the studio's own sign-in, is not among them.
func (s *Server) membersOf(ctx context.Context, view *farmbook.View, p *c4v1.Party, ps []c4v1.Party) ([]membership, error) {
	party := farmbook.PartyKey(p.Name)
	var keys []string
	seen := map[string]bool{}
	for _, s := range view.All() {
		e := s.Entry
		if e.Kind == farmbook.KindControl && strings.HasPrefix(e.Subject, party+"/") && !seen[e.Subject] {
			seen[e.Subject] = true
			keys = append(keys, e.Subject)
		}
	}
	var out []membership
	for _, key := range keys {
		on := s.inForce(view, farmbook.ForceMembership, key)
		if on == nil {
			continue
		}
		m := membership{on: on}
		purposes, err := s.purposesOf(ctx, on)
		if err != nil {
			// An entry pending the seal can cite anything: it holds
			// nothing, and shows why, rather than hiding the Party.
			s.Log.Error(err, "a membership's record", "party", p.Name, "key", key)
			m.lapsed = "its record does not verify"
		}
		m.purposes = purposes
		rest := strings.TrimPrefix(key, party+"/")
		if g, ok := strings.CutPrefix(rest, "group:"); ok {
			m.group = g
		} else {
			m.person = strings.TrimPrefix(rest, "person:")
			if findParty(ps, m.person) == nil && m.lapsed == "" {
				m.lapsed = "names no Party"
			}
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b membership) int {
		switch {
		case a.group == "" && b.group != "":
			return -1
		case a.group != "" && b.group == "":
			return 1
		}
		return 0
	})
	return out, nil
}

// standing is what the farm's Parties say of one viewer.
type standing struct {
	// parties are the farm's Parties, and person the person Party the
	// viewer is (partyPerson), "" for none.
	parties []c4v1.Party
	person  string
	// held are, by party, the purposes the viewer holds in effect, each
	// with the record that put it in force ("" for the c4:party: group);
	// named those its person's own named membership grants, granted, in
	// effect or not.
	held  map[string]map[string]string
	named map[string][]string
	// err is why the farm book could not be read: the viewer then holds
	// nothing for any organisation.
	err error
}

// holds says whether the viewer holds purpose for party, in effect.
func (st *standing) holds(party, purpose string) bool {
	_, ok := st.held[party][purpose]
	return ok
}

// standingOf reads what the farm's Parties say of v, now.
func (s *Server) standingOf(ctx context.Context, v *api.Viewer) *standing {
	ps, err := s.parties(ctx)
	if err != nil {
		return &standing{err: err, held: map[string]map[string]string{}}
	}
	var view *farmbook.View
	var verr error
	if len(ps) > 0 {
		view, verr = s.partyBook(ctx)
	}
	return s.standingAt(ctx, v, ps, view, verr)
}

// standingAt is what ps and view say of v: for each Party, the purposes v
// holds in effect. An organisation's are its memberships in force naming
// v's person Party and those of v's groups by a signed group rule, never
// the provider's c4:party: group alone (membership is issuance, 9.1); a
// person's own Party gives them every purpose (ORGANIZATION 5).
func (s *Server) standingAt(ctx context.Context, v *api.Viewer, ps []c4v1.Party, view *farmbook.View, verr error) *standing {
	st := &standing{parties: ps, held: map[string]map[string]string{}, named: map[string][]string{}, err: verr}
	if len(ps) == 0 || v == nil || v.Unit != nil || v.User == "" {
		return st
	}
	st.person = s.partyPerson(v, ps)
	grant := func(p *c4v1.Party, purposes []string, record string) {
		h := st.held[p.Name]
		if h == nil {
			h = map[string]string{}
			st.held[p.Name] = h
		}
		for _, x := range inEffect(p, purposes) {
			if _, ok := h[x]; !ok {
				h[x] = record
			}
		}
	}
	for i := range ps {
		p := &ps[i]
		if p.Spec.Kind == c4v1.PartyPerson {
			if st.person != "" && p.Name == st.person {
				grant(p, Purposes, p.Spec.Made.Record)
			}
			continue
		}
		if view == nil {
			continue
		}
		var keys []string
		if st.person != "" {
			keys = append(keys, farmbook.MemberKey(p.Name, st.person))
		}
		if MembersByGroup {
			for _, g := range v.Groups {
				keys = append(keys, farmbook.GroupKey(p.Name, g))
			}
		}
		for k, key := range keys {
			on := s.inForce(view, farmbook.ForceMembership, key)
			if on == nil {
				continue
			}
			purposes, err := s.purposesOf(ctx, on)
			if err != nil {
				s.Log.Error(err, "a membership's record", "party", p.Name, "key", key)
				continue
			}
			if k == 0 && st.person != "" {
				st.named[p.Name] = purposes
			}
			grant(p, purposes, on.Entry.Record)
		}
	}
	if st.err != nil {
		s.Log.Error(st.err, "read an organisation's members")
	}
	return st
}

// holdsFor says whether v holds purpose for party, in effect: the one
// question SPEND, FRAME-ORDER and an organisation's approvals ask.
func (s *Server) holdsFor(ctx context.Context, v *api.Viewer, party, purpose string) bool {
	return s.standingOf(ctx, v).holds(party, purpose)
}

// keptParty says whether the farm keeps party as a Party.
func (s *Server) keptParty(ctx context.Context, party string) bool {
	ps, err := s.parties(ctx)
	return err != nil || findParty(ps, party) != nil
}

// decider is deciderOf with what the farm's Parties say of v: a party:
// wait for a Party the farm keeps takes a member holding member.
func (s *Server) decider(r *http.Request, v *api.Viewer) Decider {
	d := deciderOf(r, v)
	if v == nil || v.Unit != nil || d.Agent != "" {
		return d
	}
	st := s.standingOf(r.Context(), v)
	if st.err != nil && st.parties == nil {
		// The parties could not be read: every party is taken as kept,
		// so no c4:party: group decides for one that might be.
		d.AllKept = true
	}
	for i := range st.parties {
		if d.Kept == nil {
			d.Kept, d.Members = map[string]bool{}, map[string]string{}
		}
		name := st.parties[i].Name
		d.Kept[name] = true
		if rec, ok := st.held[name][PurposeMember]; ok {
			d.Members[name] = rec
		}
	}
	if st.person != "" && st.person != v.User {
		d.Person = st.person
	}
	return d
}

// memberWords say, for a decision's record, why the decider may decide a
// wait for a Party: a member holding member, under which record.
func memberWords(server string, d Decider) string {
	kind, name, _ := strings.Cut(server, ":")
	rec, ok := d.Members[name]
	if kind != "party" || !d.Kept[name] || !ok {
		return ""
	}
	if rec == "" {
		return fmt.Sprintf("; a member of %s by the studio's group c4:party:%s", name, name)
	}
	return fmt.Sprintf("; a member of %s holding member (record %s)", name, shortID(rec))
}

// partyView is a Party as the HUD shows it to v.
func (s *Server) partyView(ctx context.Context, p *c4v1.Party, view *farmbook.View, st *standing, signIns bool) (api.Party, error) {
	out := api.Party{Name: p.Name, Kind: p.Spec.Kind, DisplayName: p.Spec.DisplayName, LegalName: p.Spec.LegalName,
		PerPersonViews: p.Spec.PerPersonViews, Made: partyAct(&p.Spec.Made), Changed: nil, Members: []api.Member{},
		ActOn: actOn(partySubject(p))}
	if p.Spec.Changed != nil {
		a := partyAct(p.Spec.Changed)
		out.Changed = &a
	}
	if signIns {
		for _, x := range p.Spec.SignIns {
			out.SignIns = append(out.SignIns, api.SignIn{Issuer: x.Issuer, Subject: x.Subject})
		}
	}
	for _, x := range Purposes {
		if st != nil && st.holds(p.Name, x) {
			out.Yours = append(out.Yours, x)
		}
	}
	if view == nil {
		return out, nil
	}
	out.Booked = s.inForce(view, farmbook.ForceParty, farmbook.PartyKey(p.Name)) != nil
	if p.Spec.Kind != c4v1.PartyOrganisation {
		return out, nil
	}
	if _, fa := madeBy(view, p.Name); fa != "" {
		out.FirstAdmin = &api.FirstAdmin{Person: fa}
		if q := findParty(st.parties, fa); st != nil && q != nil && slices.Contains(q.Spec.SignIns, c4v1.PartySignIn{Subject: p.Spec.Made.By}) {
			// the farm admin who made it named themselves
			out.FirstAdmin.SelfNamed = true
		}
		if acc := farmbook.AdminAccepted(view.All(), p.Name); acc != nil {
			out.FirstAdmin.Accepted, out.FirstAdmin.Record, out.FirstAdmin.At = true, acc.Entry.Record, acc.Time
		}
	}
	var ps []c4v1.Party
	if st != nil {
		ps = st.parties
	}
	ms, err := s.membersOf(ctx, view, p, ps)
	if err != nil {
		return out, err
	}
	for _, m := range ms {
		if m.group != "" && !MembersByGroup {
			continue
		}
		e := api.Member{Person: m.person, Group: m.group, Purposes: m.purposes, InEffect: inEffect(p, m.purposes),
			By: m.on.Entry.By, Record: m.on.Entry.Record, Since: m.on.Time, Pending: m.on.Pending, Lapsed: m.lapsed}
		if m.lapsed != "" {
			e.InEffect = []string{}
		}
		if m.group != "" {
			out.Groups = append(out.Groups, e)
		} else {
			out.Members = append(out.Members, e)
		}
	}
	return out, nil
}

func partyAct(a *c4v1.PartyAct) api.PartyAct {
	return api.PartyAct{By: a.By, At: a.At.UTC(), Record: a.Record}
}

// farmAdmin says whether v keeps the party list: may change parties.
func (s *Server) farmAdmin(ctx context.Context, r *http.Request, v *api.Viewer, name string) bool {
	return s.authorize(ctx, r, v, "", "update", "parties", name) == nil
}

// seesSignIns says whether v sees p's sign-ins: the person, or a farm
// admin.
func (s *Server) seesSignIns(ctx context.Context, r *http.Request, v *api.Viewer, st *standing, p *c4v1.Party) bool {
	return p.Spec.Kind == c4v1.PartyPerson && (st.person == p.Name || s.farmAdmin(ctx, r, v, p.Name))
}

// handleParties answers GET /api/v1/parties.
func (s *Server) handleParties(w http.ResponseWriter, r *http.Request) {
	v := s.partyReader(w, r)
	if v == nil {
		return
	}
	ctx := r.Context()
	st := s.standingOf(ctx, v)
	if st.err != nil && st.parties == nil {
		fail(w, http.StatusInternalServerError, "%v", st.err)
		return
	}
	view, _ := s.partyBook(ctx)
	out := api.PartyList{Parties: []api.Party{}, Purposes: Purposes}
	for i := range st.parties {
		p := &st.parties[i]
		pv, err := s.partyView(ctx, p, view, st, s.seesSignIns(ctx, r, v, st, p))
		if err != nil {
			fail(w, http.StatusInternalServerError, "party %s: %v", p.Name, err)
			return
		}
		out.Parties = append(out.Parties, pv)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleParty answers GET /api/v1/parties/{name}.
func (s *Server) handleParty(w http.ResponseWriter, r *http.Request) {
	v := s.partyReader(w, r)
	if v == nil {
		return
	}
	ctx := r.Context()
	st := s.standingOf(ctx, v)
	p := findParty(st.parties, r.PathValue("name"))
	if p == nil {
		fail(w, http.StatusNotFound, "no party %s on this farm", r.PathValue("name"))
		return
	}
	view, _ := s.partyBook(ctx)
	pv, err := s.partyView(ctx, p, view, st, s.seesSignIns(ctx, r, v, st, p))
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, api.PartyResponse{OK: true, Party: &pv, Record: p.Spec.Made.Record})
}

// partyReader is the viewer of a party route that reads: a person, not a
// unit, on a farm that keeps a farm book.
func (s *Server) partyReader(w http.ResponseWriter, r *http.Request) *api.Viewer {
	v := s.withViewer(w, r)
	if v == nil {
		return nil
	}
	switch {
	case v.Unit != nil:
		fail(w, http.StatusForbidden, "%s is a unit: the party list is read by people", v.User)
		return nil
	case s.FarmBookReader == nil:
		// as the writes: a farm without a book has no Parties, and the HUD
		// shows the tab only where HEAD /parties answers
		fail(w, http.StatusNotImplemented, noBookParties)
		return nil
	}
	return v
}

// noBookParties is why a farm without a farm book has no Parties.
const noBookParties = "this farm keeps no farm book (its install names no --system-store), and an organisation's members are the book's: it has no parties"

// partyWriter is the viewer of a party route that writes, after the
// guard every party write passes.
func (s *Server) partyWriter(w http.ResponseWriter, r *http.Request) *api.Viewer {
	v := s.withViewer(w, r)
	if v == nil {
		return nil
	}
	if code, msg := s.partyWriteRefusal(r, v); code != 0 {
		fail(w, code, "%s", msg)
		return nil
	}
	return v
}

// partyWriteRefusal is why v may make no party write here, with its
// status, or 0: a person, signed in or on the installer's secret, never a
// unit, an agent or a tracker's sync; on a farm that keeps a farm book and
// signs. The viewer's makeParties and makeOwnParty pass it too.
func (s *Server) partyWriteRefusal(r *http.Request, v *api.Viewer) (int, string) {
	switch {
	case v.Unit != nil:
		return http.StatusForbidden, v.User + " is a unit: people keep the party list, and a unit is never a member"
	case agentActs(r) || s.isSync(v):
		return http.StatusForbidden, "an agent cannot change a party or its members: a person does, as themselves, and an agent is never a member"
	case isDev(v) && !byInstaller(v):
		return http.StatusForbidden, "without this machine's installer token, a development viewer is no one: sign in to change a party"
	case s.FarmBook == nil || s.FarmBookReader == nil:
		return http.StatusNotImplemented, noBookParties
	case s.Signer == nil:
		return http.StatusServiceUnavailable, "a party is a signed record, and this HUD has no signer"
	}
	return 0, ""
}

// managedFarm says whether the farm is run for another party: its binding
// names a runFor (ORGANIZATION 6). There a farm admin who makes an
// organisation never names themselves its first admin; on a farm its
// owner runs they may, since a small studio's farm admin is often its one
// admin, and they still count only once they accept, booked.
func (s *Server) managedFarm(ctx context.Context) bool {
	b, err := s.farmBinding(ctx)
	return err != nil || b != nil && b.Spec.RunFor != ""
}

// farmAdminMakes is the farm admin's right making an organisation takes
// (DESIGN D8): create on parties, or for a development viewer the
// farm-admin role or every role (devrights). name "" asks of any party.
func (s *Server) farmAdminMakes(ctx context.Context, r *http.Request, v *api.Viewer, name string) error {
	return s.authorize(ctx, r, v, "", "create", "parties", name)
}

// withPartyRights is v as the HUD serves it (snapshot, events, needs-me),
// a copy, with what it may do about the party list:
//   - makeParties: make an organisation, by the guard and the right
//     make-party takes for one;
//   - makeOwnParty: make their own person Party now, by the guard, no
//     Party listing their sign-in yet. One who signs in by the
//     c4-sign-ins Secret proves their subject as they do;
//   - bindingOwner: the farm binding's owner: the person of the owner's
//     person Party, through its sign-ins; no one before it exists;
//   - party: the viewer's own person Party, by a sign-in it lists (R1),
//     or "".
func (s *Server) withPartyRights(r *http.Request, v *api.Viewer) *api.Viewer {
	out := *v
	ctx := r.Context()
	ps, err := s.parties(ctx)
	if err != nil {
		return &out
	}
	person := s.partyPerson(v, ps)
	out.Party = person
	if b, err := s.farmBinding(ctx); err == nil && b != nil && v.Unit == nil {
		// through the owner's person Party's sign-ins alone (R1)
		owner := findParty(ps, b.Spec.Owner)
		out.BindingOwner = owner != nil && owner.Spec.Kind == c4v1.PartyPerson && person == owner.Name
	}
	if code, _ := s.partyWriteRefusal(r, v); code != 0 {
		return &out
	}
	out.MakeParties = s.farmAdminMakes(ctx, r, v, "") == nil
	if who, _ := claimed(ps, "", []c4v1.PartySignIn{{Subject: v.User}}); who == "" && person == "" {
		out.MakeOwnParty = subjectName.MatchString(v.User)
	}
	return &out
}

// validSignIns checks a list of sign-in subjects: each well formed, a
// provider's at an http(s) issuer URL, kept in its one spelling, none
// twice. It returns them sorted by issuer, then subject.
func validSignIns(in []api.SignIn) ([]c4v1.PartySignIn, error) {
	if len(in) > 32 {
		return nil, errors.New("signIns: at most 32")
	}
	out := []c4v1.PartySignIn{}
	for _, x := range in {
		y := c4v1.PartySignIn{Issuer: people.CanonicalIssuer(strings.TrimSpace(x.Issuer)), Subject: strings.TrimSpace(x.Subject)}
		if !subjectName.MatchString(y.Subject) {
			return nil, fmt.Errorf("signIns: %q is not a sign-in subject (no spaces or commas, at most 253 characters)", y.Subject)
		}
		if u, err := url.Parse(y.Issuer); y.Issuer != "" && (err != nil || len(y.Issuer) > 253 || u.Host == "" || u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("signIns: %q is not a sign-in provider's issuer URL", y.Issuer)
		}
		if !slices.Contains(out, y) {
			out = append(out, y)
		}
	}
	sortSignIns(out)
	return out, nil
}

// signInWords is a sign-in in the farm's words.
func signInWords(x c4v1.PartySignIn) string {
	if x.Issuer == "" {
		return x.Subject
	}
	return x.Subject + " at " + x.Issuer
}

// validPurposes checks a membership's purposes: some, each known, none
// twice. It returns them in Purposes' order.
func validPurposes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("purposes: name at least one of %s; to end a membership, end it", strings.Join(Purposes, ", "))
	}
	for _, x := range in {
		if !slices.Contains(Purposes, x) {
			return nil, fmt.Errorf("purposes: %q is not a purpose: %s", x, strings.Join(Purposes, ", "))
		}
	}
	var out []string
	for _, x := range Purposes {
		if slices.Contains(in, x) {
			out = append(out, x)
		}
	}
	return out, nil
}

// claimed is the person Party other than except that already has one of
// subjects among its sign-ins, and that subject, each compared in its one
// spelling.
func claimed(ps []c4v1.Party, except string, subjects []c4v1.PartySignIn) (string, string) {
	for i := range ps {
		if ps[i].Name == except || ps[i].Spec.Kind != c4v1.PartyPerson {
			continue
		}
		for _, x := range subjects {
			if slices.ContainsFunc(ps[i].Spec.SignIns, func(y c4v1.PartySignIn) bool {
				return y.Subject == x.Subject && people.CanonicalIssuer(y.Issuer) == people.CanonicalIssuer(x.Issuer)
			}) {
				return ps[i].Name, signInWords(x)
			}
		}
	}
	return "", ""
}

// added are the sign-ins next has that old doesn't.
func added(old, next []c4v1.PartySignIn) []c4v1.PartySignIn {
	var out []c4v1.PartySignIn
	for _, x := range next {
		if !slices.Contains(old, x) {
			out = append(out, x)
		}
	}
	return out
}

// provedSignIns checks each sign-in a write takes next to over old is the
// writer's own, just proved (R2), and adds the one idToken proves: a name
// the HUD signs people in as only by the viewer signed in as it; a
// provider subject only from idToken, a fresh ID token the people's
// issuer checks as whoami checks one, the server taking its issuer, in
// its one spelling, and its subject from the verified token. A client
// sends idToken alone; a provider subject it sends besides must be the
// one the token proves (400). It returns next with the proved subject,
// and that subject, if any.
func (s *Server) provedSignIns(ctx context.Context, v *api.Viewer, old, next []c4v1.PartySignIn, idToken string) ([]c4v1.PartySignIn, *c4v1.PartySignIn, *errGrant) {
	var claims []c4v1.PartySignIn
	for _, x := range added(old, next) {
		switch {
		case x.Issuer == "" && x.Subject != v.User:
			return nil, nil, &errGrant{http.StatusForbidden, fmt.Sprintf("signIns: %s is added only by someone signed in as it, and you are signed in as %s", x.Subject, v.User)}
		case x.Issuer != "":
			claims = append(claims, x)
		}
	}
	if idToken == "" {
		if len(claims) > 0 {
			return nil, nil, &errGrant{http.StatusForbidden, fmt.Sprintf("signIns: %s goes on a Party only on a fresh ID token for it: send idToken, from signing in at the farm's provider", signInWords(claims[0]))}
		}
		return next, nil, nil
	}
	if s.ProveSignIn == nil {
		return nil, nil, &errGrant{http.StatusNotImplemented, "this HUD has no people's issuer to check an ID token with, so it adds no provider subject"}
	}
	got, err := s.ProveSignIn(ctx, idToken)
	if err != nil {
		return nil, nil, &errGrant{http.StatusForbidden, fmt.Sprintf("idToken: %v", err)}
	}
	proved := c4v1.PartySignIn{Issuer: people.CanonicalIssuer(got.Issuer), Subject: got.Subject}
	for _, x := range claims {
		if x != proved {
			return nil, nil, &errGrant{http.StatusBadRequest, fmt.Sprintf("signIns: %s is not what idToken proves (%s): send idToken alone, and the HUD takes the subject from it", signInWords(x), signInWords(proved))}
		}
	}
	if !slices.Contains(next, proved) {
		next = append(slices.Clone(next), proved)
		sortSignIns(next)
	}
	return next, &proved, nil
}

// sortSignIns sorts sign-ins by issuer, then subject.
func sortSignIns(ins []c4v1.PartySignIn) {
	slices.SortFunc(ins, func(a, b c4v1.PartySignIn) int {
		if c := strings.Compare(a.Issuer, b.Issuer); c != 0 {
			return c
		}
		return strings.Compare(a.Subject, b.Subject)
	})
}

// standInHolder is who else the c4-sign-ins Secret (K2's stand-in) gives
// one of a person Party's provider subjects: a person with no person
// Party of their own, who so still signs in by it. "" for no one.
func (s *Server) standInHolder(ctx context.Context, ps []c4v1.Party, party string, signIns []c4v1.PartySignIn) (string, string, error) {
	if s.StandInSignIns == nil {
		return "", "", nil
	}
	stand, err := s.StandInSignIns.Load(ctx)
	if err != nil {
		return "", "", err
	}
	for _, x := range signIns {
		if x.Issuer == "" {
			continue
		}
		for _, who := range stand.People(x.Issuer, x.Subject) {
			if q := findParty(ps, who); who != party && (q == nil || q.Spec.Kind != c4v1.PartyPerson) {
				return who, signInWords(x), nil
			}
		}
	}
	return "", "", nil
}

// standInRefusal answers a sign-in the stand-in Secret gives another.
func standInRefusal(w http.ResponseWriter, who, x string, err error) {
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "the c4-sign-ins Secret: %v", err)
		return
	}
	fail(w, http.StatusConflict, "signIns: %s is %s's in the c4-sign-ins Secret; one sign-in is one person's", x, who)
}

// nameIsTheirs checks a person Party's name is its maker's (R2, R6): the
// name they sign in as; or, where the c4-sign-ins Secret signs in a person
// of that name, the name of a subject they prove, one of its lines for
// them, which also keeps them signed in once the Party makes the Secret's
// lines for them count for nothing. Names are never reused, so nobody
// takes another's. It returns why not, or "".
func (s *Server) nameIsTheirs(ctx context.Context, v *api.Viewer, name string, proved *c4v1.PartySignIn) (string, error) {
	var lines []people.SignIn
	if s.StandInSignIns != nil {
		stand, err := s.StandInSignIns.Load(ctx)
		if err != nil {
			return "", err
		}
		lines = stand[name]
	}
	switch {
	case len(lines) > 0 && proved != nil && slices.ContainsFunc(lines, func(y people.SignIn) bool {
		return y.Subject == proved.Subject && people.CanonicalIssuer(y.Issuer) == proved.Issuer
	}):
		return "", nil
	case len(lines) > 0:
		return fmt.Sprintf("%s signs in by the c4-sign-ins Secret, which a Party of that name replaces: list one of its subjects among signIns and prove it (idToken), so the Party is theirs and keeps them signed in", name), nil
	case name != v.User:
		return fmt.Sprintf("name: a person's Party takes the name they sign in as (%s), or the name the c4-sign-ins Secret gives a subject they prove", v.User), nil
	}
	return "", nil
}

// partyPredicate is a party write's control record, before its act.
func (s *Server) partyPredicate(r *http.Request, v *api.Viewer, authz, action, target string, change map[string]any) controlPredicate {
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Authorization: authz,
		Action: action, Target: target, Custody: CustodyDirect, Change: change}
	if isDev(v) {
		// the installer's word, on their secret
		pred.Custody = CustodyClaimed
	}
	return pred
}

// errPartyMoved is a write the book's state moved under too often.
var errPartyMoved = errors.New("the farm book moved under this write too often: try again")

// bookChecked books a party act's control record in the farm book, now,
// judging check on the book as it stands at the State the entry is
// submitted from (R5): when another writer moved it first, it reads the
// book again and judges again, as the people's issuer's append does, so
// two writes that each pass alone can't both be booked when together
// they break a rule. For a membership the book is the state, so an act it
// did not take did nothing. A refusal from check comes back as it is.
func (s *Server) bookChecked(ctx context.Context, record string, check func(view *farmbook.View) *errGrant) error {
	if s.FarmBook == nil || s.FarmBookReader == nil {
		return errors.New(noBookParties)
	}
	rec, err := s.FarmBook.Store.Get(ctx, record)
	if err != nil {
		return err
	}
	for try := 0; try < 8; try++ {
		view, err := s.FarmBookReader.View(ctx, time.Now())
		if err != nil {
			return err
		}
		if check != nil {
			if g := check(view); g != nil {
				return g
			}
		}
		e, err := farmbook.ControlEntry(rec, time.Now())
		if err != nil {
			return err
		}
		_, err = farmbook.Submit(ctx, s.FarmBook.Store, s.FarmBook.Pointer, view.State, []farmbook.Submission{{Entry: e}})
		if !errors.Is(err, farmbook.ErrMoved) {
			return err
		}
	}
	return errPartyMoved
}

// bookFailed answers a party write the book did not take.
func bookFailed(w http.ResponseWriter, err error, what string) {
	var g *errGrant
	if errors.As(err, &g) {
		fail(w, g.code, "%s", g.msg)
		return
	}
	fail(w, http.StatusServiceUnavailable, "the farm book did not take %s (%v), so nothing changed: try again", what, err)
}

// handleMakeParty answers POST /api/v1/parties {name, kind, displayName,
// legalName?, signIns?, idToken?, firstAdmin?} (make-party):
//   - an organisation: a farm admin makes it, naming its first admin, a
//     person Party other than their own, who counts once they accept;
//   - a person: the person makes their own, signed in with the name it
//     lists and proving any provider subject it lists.
func (s *Server) handleMakeParty(w http.ResponseWriter, r *http.Request) {
	v := s.partyWriter(w, r)
	if v == nil {
		return
	}
	var body api.PartyRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {name, kind, displayName, legalName?, signIns?, idToken?, firstAdmin?}: %v", err)
		return
	}
	body.Name, body.DisplayName, body.LegalName, body.FirstAdmin = strings.TrimSpace(body.Name), strings.TrimSpace(body.DisplayName), strings.TrimSpace(body.LegalName), strings.TrimSpace(body.FirstAdmin)
	signIns, err := validSignIns(body.SignIns)
	switch {
	case !partyName.MatchString(body.Name):
		fail(w, http.StatusBadRequest, "name: a party's bare name, as records use it (kestrel): lower case letters, digits, dots and dashes, at most 63")
		return
	case body.Kind != c4v1.PartyPerson && body.Kind != c4v1.PartyOrganisation:
		fail(w, http.StatusBadRequest, "kind: person or organisation; nothing else is a party (an agent, a unit, a tracker and a deployment never are)")
		return
	case body.DisplayName == "" || len(body.DisplayName) > 200:
		fail(w, http.StatusBadRequest, "displayName: how the HUD shows the party (Kestrel Productions), at most 200 characters")
		return
	case len(body.LegalName) > 200:
		fail(w, http.StatusBadRequest, "legalName: at most 200 characters")
		return
	case body.LegalName != "" && body.Kind != c4v1.PartyOrganisation:
		fail(w, http.StatusBadRequest, "legalName: only an organisation has one")
		return
	case err != nil:
		fail(w, http.StatusBadRequest, "%v", err)
		return
	case len(signIns) > 0 && body.Kind != c4v1.PartyPerson:
		fail(w, http.StatusBadRequest, "signIns: only a person signs in; an organisation's members do")
		return
	case body.FirstAdmin != "" && body.Kind != c4v1.PartyOrganisation:
		fail(w, http.StatusBadRequest, "firstAdmin: an organisation's; a person's Party has no members")
		return
	}
	ctx := r.Context()
	ps, err := s.parties(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if old := findParty(ps, body.Name); old != nil {
		s.partyExists(w, r, v, old, &body, signIns)
		return
	}
	change := map[string]any{"name": body.Name, "kind": body.Kind, "displayName": body.DisplayName}
	authz := ""
	if body.Kind == c4v1.PartyOrganisation {
		person := s.partyPerson(v, ps)
		fa := findParty(ps, body.FirstAdmin)
		switch err := s.farmAdminMakes(ctx, r, v, body.Name); {
		case err != nil:
			fail(w, http.StatusForbidden, "a farm admin keeps the party list: %v", err)
			return
		case body.FirstAdmin == "":
			fail(w, http.StatusBadRequest, "firstAdmin: name the person Party that is %s's first admin once they accept; making it makes no one its admin", body.Name)
			return
		case fa == nil || fa.Spec.Kind != c4v1.PartyPerson:
			fail(w, http.StatusBadRequest, "firstAdmin: %s is no person Party: the first admin is a person, with a Party of their own", body.FirstAdmin)
			return
		case person != "" && body.FirstAdmin == person && s.managedFarm(ctx):
			// a provider's admin is never thereby the studio's admin
			fail(w, http.StatusForbidden, "firstAdmin: this farm is run for another party (its binding's runFor), and making a Party never makes its maker its admin: name the person who is")
			return
		}
		authz = s.authorization(v, "create", "parties", "", body.Name)
		change["firstAdmin"] = body.FirstAdmin
		if body.LegalName != "" {
			change["legalName"] = body.LegalName
		}
	} else {
		if !slices.Contains(signIns, c4v1.PartySignIn{Subject: v.User}) {
			fail(w, http.StatusForbidden, "signIns: a person's Party is made by the person, signed in with it: list the sign-in you are using, %s", v.User)
			return
		}
		var proved *c4v1.PartySignIn
		var g *errGrant
		if signIns, proved, g = s.provedSignIns(ctx, v, nil, signIns, body.IDToken); g != nil {
			fail(w, g.code, "%s", g.msg)
			return
		}
		if who, x := claimed(ps, "", signIns); who != "" {
			fail(w, http.StatusConflict, "signIns: %s is %s's already; one sign-in is one person's", x, who)
			return
		}
		if who, x, err := s.standInHolder(ctx, ps, body.Name, signIns); err != nil || who != "" {
			standInRefusal(w, who, x, err)
			return
		}
		switch why, err := s.nameIsTheirs(ctx, v, body.Name, proved); {
		case err != nil:
			fail(w, http.StatusServiceUnavailable, "the c4-sign-ins Secret: %v", err)
			return
		case why != "":
			fail(w, http.StatusForbidden, "%s", why)
			return
		}
		authz = "the person, making their own party"
		change["signIns"] = signIns
	}
	pred := s.partyPredicate(r, v, authz, "make-party", farmbook.PartyKey(body.Name), change)
	// making a party is an act on the farm itself
	var act *citedAct
	if on, ok := s.farmSubject(ctx); ok {
		act, err = s.citeAct(ctx, r, v, on, "make-party")
	} else if r.Header.Get(ActHeader) != "" {
		err = fmt.Errorf("%w: this HUD has no farm CA to name the farm by", errAct)
	}
	if err != nil {
		fail(w, controlStatus(err), "%v", err)
		return
	}
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	p := &c4v1.Party{ObjectMeta: metav1.ObjectMeta{Name: body.Name}, Spec: c4v1.PartySpec{Kind: body.Kind, DisplayName: body.DisplayName,
		LegalName: body.LegalName, SignIns: signIns, Made: c4v1.PartyAct{By: v.User, At: now}}}
	if len(signIns) == 0 {
		p.Spec.SignIns = nil
	}
	subject := onAct(act, provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": sha256JSON(change)}}, &pred)
	id, _, err := s.signRecord(ctx, subject, pred)
	if err != nil {
		fail(w, controlStatus(err), "control record: %v", err)
		return
	}
	p.Spec.Made.Record = id
	if err := s.Client.Create(ctx, p); err != nil {
		code := http.StatusInternalServerError
		if apierrors.IsAlreadyExists(err) {
			code = http.StatusConflict
		}
		fail(w, code, "%v", err)
		return
	}
	if err := s.bookChecked(ctx, id, nil); err != nil {
		fail(w, http.StatusServiceUnavailable, "party %s is made, and the farm book did not take its record (%v): make it again the same way to book it", body.Name, err)
		return
	}
	msg := fmt.Sprintf("%s makes the %s %s (%s)", v.User, body.Kind, body.Name, body.DisplayName)
	if body.Kind == c4v1.PartyOrganisation {
		msg += fmt.Sprintf(", naming %s its first admin, once they accept", body.FirstAdmin)
	}
	s.record(ctx, p, "Party", v.User, "Made", msg+"; record "+shortID(id), bookNote{actor: actorOf(r, v), kind: "make-party", record: id, booked: true})
	s.answerParty(w, r, v, p, msg, id, false)
}

// partyExists answers a make-party for a name the farm has: the same party
// made again by its maker is an answer, and books its record if the book
// did not take it; anything else is refused, since a name is never
// reused.
func (s *Server) partyExists(w http.ResponseWriter, r *http.Request, v *api.Viewer, old *c4v1.Party, body *api.PartyRequest, signIns []c4v1.PartySignIn) {
	ctx := r.Context()
	same := old.Spec.Kind == body.Kind && old.Spec.DisplayName == body.DisplayName && old.Spec.LegalName == body.LegalName &&
		(slices.Equal(old.Spec.SignIns, signIns) || len(old.Spec.SignIns)+len(signIns) == 0) && old.Spec.Made.By == v.User
	if !same {
		fail(w, http.StatusConflict, "party %s is the %s %s since %s (made by %s, record %s): a party's name is never reused",
			old.Name, old.Spec.Kind, old.Spec.DisplayName, old.Spec.Made.At.UTC().Format(time.RFC3339), old.Spec.Made.By, shortID(old.Spec.Made.Record))
		return
	}
	view, err := s.partyBook(ctx)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	msg := fmt.Sprintf("party %s is made already", old.Name)
	if s.inForce(view, farmbook.ForceParty, farmbook.PartyKey(old.Name)) == nil {
		if err := s.bookChecked(ctx, old.Spec.Made.Record, nil); err != nil {
			fail(w, http.StatusServiceUnavailable, "party %s is made, and the farm book did not take its record: %v", old.Name, err)
			return
		}
		msg += "; its record is booked now"
	}
	s.answerParty(w, r, v, old, msg, old.Spec.Made.Record, true)
}

// answerParty answers a party write with the party as v sees it now.
func (s *Server) answerParty(w http.ResponseWriter, r *http.Request, v *api.Viewer, p *c4v1.Party, msg, record string, existing bool) {
	ctx := r.Context()
	st := s.standingOf(ctx, v)
	if q := findParty(st.parties, p.Name); q != nil && q.ResourceVersion == p.ResourceVersion {
		p = q
	}
	view, _ := s.partyBook(ctx)
	pv, err := s.partyView(ctx, p, view, st, s.seesSignIns(ctx, r, v, st, p))
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, api.PartyResponse{OK: true, Message: msg, Existing: existing, Party: &pv, Record: record})
}

// partyFor reads the party a write names, and v's standing.
func (s *Server) partyFor(w http.ResponseWriter, r *http.Request, v *api.Viewer) (*c4v1.Party, *standing) {
	st := s.standingOf(r.Context(), v)
	if st.err != nil && st.parties == nil {
		fail(w, http.StatusInternalServerError, "%v", st.err)
		return nil, nil
	}
	p := findParty(st.parties, r.PathValue("name"))
	if p == nil {
		fail(w, http.StatusNotFound, "no party %s on this farm", r.PathValue("name"))
		return nil, nil
	}
	return p, st
}

// adminOf says how v may change p's names and policy: as its admin (an
// organisation's admin, or the person whose party it is), or as a farm
// admin for a person's; "" when v may not.
func (s *Server) adminOf(r *http.Request, v *api.Viewer, st *standing, p *c4v1.Party) string {
	switch {
	case p.Spec.Kind == c4v1.PartyOrganisation && st.holds(p.Name, PurposeAdmin):
		return "an admin of " + p.Name
	case p.Spec.Kind == c4v1.PartyPerson && st.person == p.Name:
		return "the person " + p.Name
	case p.Spec.Kind == c4v1.PartyPerson && s.farmAdmin(r.Context(), r, v, p.Name):
		return s.authorization(v, "update", "parties", "", p.Name)
	}
	return ""
}

// handleChangeParty answers PATCH /api/v1/parties/{name} {displayName?,
// legalName?, perPersonViews?} (change-party): the party's admin changes
// its names, and an organisation's per-person views policy.
func (s *Server) handleChangeParty(w http.ResponseWriter, r *http.Request) {
	v := s.partyWriter(w, r)
	if v == nil {
		return
	}
	var body api.PartyChange
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {displayName?, legalName?, perPersonViews?}: %v", err)
		return
	}
	p, st := s.partyFor(w, r, v)
	if p == nil {
		return
	}
	authz := s.adminOf(r, v, st, p)
	if authz == "" {
		fail(w, http.StatusForbidden, "%s may not change party %s: its admin does", v.User, p.Name)
		return
	}
	next := p.Spec.DeepCopy()
	if body.DisplayName != nil {
		next.DisplayName = strings.TrimSpace(*body.DisplayName)
	}
	if body.LegalName != nil {
		next.LegalName = strings.TrimSpace(*body.LegalName)
	}
	if body.PerPersonViews != nil {
		next.PerPersonViews = *body.PerPersonViews
	}
	switch {
	case next.DisplayName == "" || len(next.DisplayName) > 200:
		fail(w, http.StatusBadRequest, "displayName: how the HUD shows the party, at most 200 characters")
		return
	case len(next.LegalName) > 200:
		fail(w, http.StatusBadRequest, "legalName: at most 200 characters")
		return
	case next.LegalName != "" && p.Spec.Kind != c4v1.PartyOrganisation:
		fail(w, http.StatusBadRequest, "legalName: only an organisation has one")
		return
	case next.PerPersonViews && p.Spec.Kind != c4v1.PartyOrganisation:
		fail(w, http.StatusBadRequest, "perPersonViews: an organisation's policy; a person sees their own")
		return
	}
	if next.DisplayName == p.Spec.DisplayName && next.LegalName == p.Spec.LegalName && next.PerPersonViews == p.Spec.PerPersonViews {
		s.answerParty(w, r, v, p, "party "+p.Name+" is so already", "", true)
		return
	}
	change := map[string]any{"displayName": next.DisplayName, "legalName": next.LegalName, "perPersonViews": next.PerPersonViews}
	s.changeParty(w, r, v, p, next, authz, "change-party", change)
}

// handleSignIns answers PUT /api/v1/parties/{name}/sign-ins {signIns,
// idToken?} (set-sign-ins): a person's sign-in subjects, all of them. The
// person adds their own, each just proved (R2): the name they are signed
// in with, and a provider subject idToken proves. The person, or a farm
// admin, removes one; nobody adds one for someone else.
func (s *Server) handleSignIns(w http.ResponseWriter, r *http.Request) {
	v := s.partyWriter(w, r)
	if v == nil {
		return
	}
	var body api.SignInsRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {signIns, idToken?}: %v", err)
		return
	}
	p, st := s.partyFor(w, r, v)
	if p == nil {
		return
	}
	if p.Spec.Kind != c4v1.PartyPerson {
		fail(w, http.StatusBadRequest, "party %s is an organisation: its members sign in, as themselves", p.Name)
		return
	}
	signIns, err := validSignIns(body.SignIns)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	ctx := r.Context()
	// an idToken is a provider subject to add
	adding := len(added(p.Spec.SignIns, signIns)) > 0 || body.IDToken != ""
	authz := ""
	switch {
	case st.person == p.Name:
		authz = "the person " + p.Name
	case !adding && s.farmAdmin(ctx, r, v, p.Name):
		authz = s.authorization(v, "update", "parties", "", p.Name)
	case adding:
		fail(w, http.StatusForbidden, "%s may not add to %s's sign-ins: the person adds their own, each proved by signing in with it", v.User, p.Name)
		return
	default:
		fail(w, http.StatusForbidden, "%s may not change %s's sign-ins: the person, or a farm admin, removes one", v.User, p.Name)
		return
	}
	signIns, _, g := s.provedSignIns(ctx, v, p.Spec.SignIns, signIns, body.IDToken)
	if g != nil {
		fail(w, g.code, "%s", g.msg)
		return
	}
	adds := added(p.Spec.SignIns, signIns)
	if who, x := claimed(st.parties, p.Name, adds); who != "" {
		fail(w, http.StatusConflict, "signIns: %s is %s's already; one sign-in is one person's", x, who)
		return
	}
	if who, x, err := s.standInHolder(ctx, st.parties, p.Name, adds); err != nil || who != "" {
		standInRefusal(w, who, x, err)
		return
	}
	if slices.Equal(signIns, p.Spec.SignIns) || len(signIns) == 0 && len(p.Spec.SignIns) == 0 {
		s.answerParty(w, r, v, p, p.Name+"'s sign-ins are so already", "", true)
		return
	}
	next := p.Spec.DeepCopy()
	next.SignIns = signIns
	if len(signIns) == 0 {
		next.SignIns = nil
	}
	s.changeParty(w, r, v, p, next, authz, "set-sign-ins", map[string]any{"signIns": signIns})
}

// changeParty signs and carries out a change to the Party itself, as
// action, then books it.
func (s *Server) changeParty(w http.ResponseWriter, r *http.Request, v *api.Viewer, p *c4v1.Party, next *c4v1.PartySpec, authz, action string, change map[string]any) {
	ctx := r.Context()
	act, err := s.citeAct(ctx, r, v, partySubject(p), action)
	if err != nil {
		fail(w, controlStatus(err), "%v", err)
		return
	}
	pred := s.partyPredicate(r, v, authz, action, farmbook.PartyKey(p.Name), change)
	subject := onAct(act, provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": sha256JSON(next)}}, &pred)
	id, _, err := s.signRecord(ctx, subject, pred)
	if err != nil {
		fail(w, controlStatus(err), "control record: %v", err)
		return
	}
	next.Changed = &c4v1.PartyAct{By: v.User, At: metav1.NewTime(time.Now().UTC().Truncate(time.Second)), Record: id}
	q := p.DeepCopy()
	q.Spec = *next
	if err := s.Client.Update(ctx, q); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s changes party %s (%s)", v.User, p.Name, action)
	s.record(ctx, q, "Party", v.User, "Changed", msg+"; record "+shortID(id), bookNote{actor: actorOf(r, v), kind: action, record: id})
	s.answerParty(w, r, v, q, msg, id, false)
}

// handleAccept answers POST /api/v1/parties/{name}/accept (accept-admin):
// the first admin an organisation's make-party named accepts, as
// themselves, with their own touched act (R4). Until they do, the
// organisation has no admin.
func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request) {
	v := s.partyWriter(w, r)
	if v == nil {
		return
	}
	p, st := s.partyFor(w, r, v)
	if p == nil {
		return
	}
	ctx := r.Context()
	if p.Spec.Kind != c4v1.PartyOrganisation {
		fail(w, http.StatusBadRequest, "party %s is a person: it has no admin to accept", p.Name)
		return
	}
	view, err := s.partyBook(ctx)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	made, fa := madeBy(view, p.Name)
	judge := func(view *farmbook.View) *errGrant {
		_, now := madeBy(view, p.Name)
		switch {
		case now == "" || now != fa:
			return &errGrant{http.StatusConflict, fmt.Sprintf("%s's make-party names no first admin to accept", p.Name)}
		case farmbook.AdminAccepted(view.All(), p.Name) != nil:
			return &errGrant{http.StatusConflict, fmt.Sprintf("%s accepted as %s's first admin already", fa, p.Name)}
		}
		return nil
	}
	if g := judge(view); g != nil {
		fail(w, g.code, "%s", g.msg)
		return
	}
	if st.person == "" || st.person != fa {
		fail(w, http.StatusForbidden, "only %s, the first admin %s's make-party names, accepts, as themselves", fa, p.Name)
		return
	}
	act, err := s.citeAct(ctx, r, v, partySubject(p), "accept-admin")
	switch {
	case err != nil:
		fail(w, controlStatus(err), "%v", err)
		return
	case act == nil || act.grade != CustodyConfirmed:
		fail(w, http.StatusForbidden, "accepting %s's admin takes your own touched act: sign accept-admin on the party with your device and touch key (X-C4-Act)", p.Name)
		return
	}
	key := farmbook.MemberKey(p.Name, fa)
	change := map[string]any{"party": p.Name, "member": fa, "purposes": firstAdmin, "follows": made.Entry.Record}
	pred := s.partyPredicate(r, v, "the first admin "+p.Name+"'s make-party names, accepting", "accept-admin", key, change)
	subject := onAct(act, provenance.Subject{Name: key, Digest: map[string]string{"sha256": sha256JSON(change)}}, &pred)
	id, _, err := s.signRecord(ctx, subject, pred)
	if err != nil {
		fail(w, controlStatus(err), "control record: %v", err)
		return
	}
	if err := s.bookChecked(ctx, id, judge); err != nil {
		bookFailed(w, err, "the accept-admin record")
		return
	}
	msg := fmt.Sprintf("%s accepts as %s's first admin", fa, p.Name)
	s.record(ctx, p, "Party", v.User, "Membership", msg+"; record "+shortID(id), bookNote{actor: actorOf(r, v), kind: "accept-admin", record: id, booked: true})
	s.answerParty(w, r, v, p, msg, id, false)
}

// memberWrite is one membership write, as judged against one view of the
// book.
type memberWrite struct {
	action        string
	party         *c4v1.Party
	person, group string
	purposes      []string
}

// judgeMember judges w against view, the actor v: who may make it, what
// it changes, and the rules on top (R3, R5): an admin never grants for
// themselves, nor copies a purpose they hold only by a group into their
// own named membership; the last named admin is never removed. It
// returns the membership the write changes, if any, and how v may.
func (s *Server) judgeMember(ctx context.Context, v *api.Viewer, ps []c4v1.Party, view *farmbook.View, w *memberWrite) (*membership, string, *errGrant) {
	p := w.party
	st := s.standingAt(ctx, v, ps, view, nil)
	ms, err := s.membersOf(ctx, view, p, ps)
	if err != nil {
		return nil, "", &errGrant{http.StatusInternalServerError, err.Error()}
	}
	who := w.person
	if w.group != "" {
		who = "group " + w.group
	}
	var cur *membership
	admins := 0
	for i := range ms {
		if ms[i].person != "" && ms[i].lapsed == "" && slices.Contains(ms[i].purposes, PurposeAdmin) {
			admins++
		}
		if ms[i].person == w.person && ms[i].group == w.group {
			cur = &ms[i]
		}
	}
	named := w.person != "" && st.person != "" && w.person == st.person
	byGroup := w.group != "" && slices.Contains(v.Groups, w.group)
	authz := ""
	switch {
	case st.holds(p.Name, PurposeAdmin):
		authz = "an admin of " + p.Name
	case w.action == "end-member" && named:
		// a member ends their own membership: it only narrows
		authz = "the member, leaving"
	default:
		return nil, "", &errGrant{http.StatusForbidden, fmt.Sprintf("%s is not an admin of %s: its admins change its members", v.User, p.Name)}
	}
	switch {
	case w.action == "add-member" && cur != nil:
		return nil, "", &errGrant{http.StatusConflict, fmt.Sprintf("%s is a member of %s already, holding %s: change its purposes (PUT)", who, p.Name, strings.Join(cur.purposes, ", "))}
	case w.action != "add-member" && cur == nil:
		return nil, "", &errGrant{http.StatusNotFound, fmt.Sprintf("%s is not a member of %s", who, p.Name)}
	}
	var had []string
	if cur != nil {
		had = cur.purposes
	}
	for _, x := range w.purposes {
		switch {
		case slices.Contains(had, x):
		case named:
			// Admins revoke, never grant for themselves: their own named
			// membership gains nothing, and a purpose they hold only by a
			// group is never copied into it.
			return nil, "", &errGrant{http.StatusForbidden, fmt.Sprintf("%s may not grant %s to %s: an admin never grants for themselves; another admin of %s does", v.User, x, who, p.Name)}
		case byGroup && !slices.Contains(st.named[p.Name], x):
			// a group they are in gains only what they hold by name
			return nil, "", &errGrant{http.StatusForbidden, fmt.Sprintf("%s may not grant %s to %s, a group they are in: an admin never grants for themselves; another admin of %s does", v.User, x, who, p.Name)}
		}
	}
	if w.person != "" && slices.Contains(had, PurposeAdmin) && !slices.Contains(w.purposes, PurposeAdmin) && admins <= 1 {
		return nil, "", &errGrant{http.StatusConflict, fmt.Sprintf("%s is the last admin of %s: add another admin first", w.person, p.Name)}
	}
	return cur, authz, nil
}

// handleMember answers the three membership writes, each {person | group,
// purposes?}, a person by their Party's name (R3):
//   - POST /api/v1/parties/{name}/members: add-member;
//   - PUT /api/v1/parties/{name}/members: set-purposes, of a membership in
//     force;
//   - POST /api/v1/parties/{name}/members/end: end-member. A membership
//     ends, it is never deleted.
//
// Each is judged once to sign it, and again inside the book's append, on
// the book as it then stands (R5).
func (s *Server) handleMember(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v := s.partyWriter(w, r)
		if v == nil {
			return
		}
		var body api.MemberRequest
		if err := decodeBody(r, &body); err != nil {
			fail(w, http.StatusBadRequest, "send {person | group, purposes}: %v", err)
			return
		}
		body.Person, body.Group = strings.TrimSpace(body.Person), strings.TrimSpace(body.Group)
		p, st := s.partyFor(w, r, v)
		if p == nil {
			return
		}
		ctx := r.Context()
		key, who := "", ""
		switch {
		case p.Spec.Kind != c4v1.PartyOrganisation:
			fail(w, http.StatusBadRequest, "party %s is a person: a person's party has no members, and every purpose is theirs", p.Name)
			return
		case (body.Person == "") == (body.Group == ""):
			fail(w, http.StatusBadRequest, "name a person's Party or a group of the studio's own, not both")
			return
		case body.Person != "" && !partyName.MatchString(body.Person):
			fail(w, http.StatusBadRequest, "person: %s is no Party's name", body.Person)
			return
		case body.Person != "" && action != "end-member" && findParty(st.parties, body.Person) == nil:
			fail(w, http.StatusBadRequest, "person: %s is no Party: a membership names a Party, person or organisation, never a bare sign-in name", body.Person)
			return
		case body.Group != "" && !MembersByGroup:
			fail(w, http.StatusBadRequest, "group: on this farm every member is named (membership by the studio's groups is off)")
			return
		case body.Group != "" && !subjectName.MatchString(body.Group):
			fail(w, http.StatusBadRequest, "group: a group of the studio's sign-in, without spaces or commas")
			return
		case body.Person != "":
			key, who = farmbook.MemberKey(p.Name, body.Person), body.Person
		default:
			key, who = farmbook.GroupKey(p.Name, body.Group), "group "+body.Group
		}
		var purposes []string
		if action != "end-member" {
			var err error
			if purposes, err = validPurposes(body.Purposes); err != nil {
				fail(w, http.StatusBadRequest, "%v", err)
				return
			}
		} else if len(body.Purposes) > 0 {
			fail(w, http.StatusBadRequest, "purposes: ending a membership ends all of them; to keep some, change its purposes")
			return
		}
		view, err := s.partyBook(ctx)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "%v", err)
			return
		}
		mw := &memberWrite{action: action, party: p, person: body.Person, group: body.Group, purposes: purposes}
		cur, authz, g := s.judgeMember(ctx, v, st.parties, view, mw)
		if g != nil {
			fail(w, g.code, "%s", g.msg)
			return
		}
		if action == "set-purposes" && slices.Equal(cur.purposes, purposes) {
			s.answerParty(w, r, v, p, fmt.Sprintf("%s holds %s for %s already", who, strings.Join(purposes, ", "), p.Name), "", true)
			return
		}
		change := map[string]any{"party": p.Name, "purposes": purposes}
		if body.Person != "" {
			change["member"] = body.Person
		} else {
			change["group"] = body.Group
		}
		if purposes == nil {
			change["purposes"] = []string{}
		}
		follows := ""
		if cur != nil {
			follows = cur.on.Entry.Record
			change["follows"] = follows
		}
		act, err := s.citeAct(ctx, r, v, partySubject(p), action)
		if err != nil {
			fail(w, controlStatus(err), "%v", err)
			return
		}
		pred := s.partyPredicate(r, v, authz, action, key, change)
		subject := onAct(act, provenance.Subject{Name: key, Digest: map[string]string{"sha256": sha256JSON(change)}}, &pred)
		id, _, err := s.signRecord(ctx, subject, pred)
		if err != nil {
			fail(w, controlStatus(err), "control record: %v", err)
			return
		}
		err = s.bookChecked(ctx, id, func(view *farmbook.View) *errGrant {
			// the same judgement, on the book as it stands now
			now, _, g := s.judgeMember(ctx, v, st.parties, view, mw)
			if g != nil {
				return g
			}
			if (now == nil) != (cur == nil) || now != nil && now.on.Entry.Record != follows {
				return &errGrant{http.StatusConflict, fmt.Sprintf("%s's membership of %s changed while you acted: look again", who, p.Name)}
			}
			return nil
		})
		if err != nil {
			bookFailed(w, err, "the "+action+" record")
			return
		}
		var msg string
		switch action {
		case "add-member":
			msg = fmt.Sprintf("%s makes %s a member of %s, holding %s", v.User, who, p.Name, strings.Join(purposes, ", "))
		case "set-purposes":
			msg = fmt.Sprintf("%s changes %s's purposes for %s to %s", v.User, who, p.Name, strings.Join(purposes, ", "))
		default:
			msg = fmt.Sprintf("%s ends %s's membership of %s", v.User, who, p.Name)
		}
		s.record(ctx, p, "Party", v.User, "Membership", msg+"; record "+shortID(id), bookNote{actor: actorOf(r, v), kind: action, record: id, booked: true})
		s.answerParty(w, r, v, p, msg, id, false)
	}
}
