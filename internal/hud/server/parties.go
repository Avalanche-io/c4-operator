package server

// Parties (accountability ORGANIZATION 1 and 2; internal/hud/API.md,
// Parties): the farm's party list. A Party is a person or an organisation,
// by the bare name records use, kept as a cluster-scoped Party that only
// the HUD writes (config/policy/parties.yaml) and nobody deletes. Who may
// act for an organisation is not in the object: each membership, by a
// person's name or by a group of the studio's own, with the purposes it
// holds, is a signed control record booked in the farm book, and what is
// in force is the book's one rule (farmbook.InForceAt, ForceMembership).
// So a farm without a farm book has no Parties, and its party: waits take
// the c4:party:<name> group, as before.
//
// Who may do what:
//   - a farm admin keeps the party list (DESIGN D8): makes a party, and
//     may set a person's sign-ins. Whoever makes an organisation is its
//     first admin;
//   - an organisation's admin changes its names and policy, and adds,
//     changes and ends its members. An admin never grants for
//     themselves, and the last named admin is never removed;
//   - a person changes their own party, and adds to their sign-ins only
//     the one they are signed in with;
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
	// studio's own sign-in, and c4:party:<name> holds member. Off, every
	// member is named in C4: group rules are refused and read as nothing,
	// and the c4:party: group no longer speaks for a Party.
	MembersByGroup = true
	// ViewPeopleNeedsPolicy (9.3): view-people is its own purpose, which
	// view-spend does not imply, and it is in effect only where the
	// organisation's policy turns per-person views on (perPersonViews).
	// Off, a granted view-people is in effect at once.
	ViewPeopleNeedsPolicy = true
)

// firstAdmin are the purposes whoever makes an organisation holds.
var firstAdmin = []string{PurposeMember, PurposeAdmin}

var (
	// partyName is a party's bare name (c4v1.PartyName).
	partyName = c4v1.PartyName
	// personName is a person as a membership names them: their party's
	// name or their sign-in name.
	personName = regexp.MustCompile(`^[^\s:/,]{1,253}$`)
	// subjectName is a sign-in subject, and groupName a studio group.
	subjectName = regexp.MustCompile(`^[^\s,]{1,253}$`)
)

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

// partyPerson is the person v is for a Party's purposes: the person Party
// one of whose sign-ins v signed in with, else whom v's own work belongs
// to (ownerFor: on a one-person farm, its person for a role's sign-in).
// "" for a unit, which is never a member.
func (s *Server) partyPerson(ctx context.Context, v *api.Viewer, ps []c4v1.Party) string {
	if v == nil || v.Unit != nil || v.User == "" {
		return ""
	}
	var held []string
	for i := range ps {
		if ps[i].Spec.Kind == c4v1.PartyPerson && slices.Contains(ps[i].Spec.SignIns, c4v1.PartySignIn{Subject: v.User}) {
			held = append(held, ps[i].Name)
		}
	}
	switch len(held) {
	case 0:
		return s.ownerFor(ctx, v)
	case 1:
		return held[0]
	}
	// one sign-in is one person's: in two people's, it is no one
	return ""
}

// partyBook is the farm book as a Party's members are judged by: the
// sealed book and the entries pending its seal, now.
func (s *Server) partyBook(ctx context.Context) (*farmbook.View, error) {
	if s.FarmBookReader == nil {
		return nil, errors.New("this farm keeps no farm book (its install names no --system-store), and an organisation's members are the book's")
	}
	return s.FarmBookReader.View(ctx, time.Now())
}

// inForce is what put key in force in view, now: the book's one rule.
func inForce(view *farmbook.View, kind, key string) *farmbook.Sealed {
	on, _ := view.InForceAt(kind, key, view.Position(), view.Now)
	return on
}

// purposesOf reads the purposes the record that opened a membership
// grants: the record from the book's store, verified as the HUD's and as
// saying what its entry says, since an entry pending the seal is not yet
// checked by the queue.
func (s *Server) purposesOf(ctx context.Context, on *farmbook.Sealed) ([]string, error) {
	return farmbook.MemberPurposes(ctx, s.FarmBookReader.Store, s.trust(ctx).roots, s.FarmBookReader.Farm, on)
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

// membership is one membership of a Party in force.
type membership struct {
	person, group string
	purposes      []string
	on            *farmbook.Sealed
}

// membersOf are p's memberships in force in view: by name, then by group,
// each in the order first booked. The c4:party: group, being the studio's
// own sign-in, is not among them.
func (s *Server) membersOf(ctx context.Context, view *farmbook.View, p *c4v1.Party) ([]membership, error) {
	party := farmbook.PartyKey(p.Name)
	var keys []string
	seen := map[string]bool{}
	made := false
	es := append(view.Book.Entries(0), view.Pending()...)
	for _, s := range es {
		e := s.Entry
		key := ""
		switch {
		case e.Kind != farmbook.KindControl:
		case e.Act == "make-party" && e.Subject == party && !made:
			made = true
			if e.Person != "" {
				key = farmbook.MemberKey(p.Name, e.Person)
			}
		case strings.HasPrefix(e.Subject, party+"/"):
			key = e.Subject
		}
		if key != "" && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	var out []membership
	for _, key := range keys {
		on := inForce(view, farmbook.ForceMembership, key)
		if on == nil {
			continue
		}
		purposes, err := s.purposesOf(ctx, on)
		if err != nil {
			return nil, err
		}
		m := membership{purposes: purposes, on: on}
		rest := strings.TrimPrefix(key, party+"/")
		if g, ok := strings.CutPrefix(rest, "group:"); ok {
			m.group = g
		} else {
			m.person = strings.TrimPrefix(rest, "person:")
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
	// parties are the farm's Parties, and person whom the viewer is for
	// their purposes (partyPerson).
	parties []c4v1.Party
	person  string
	// held are, by party, the purposes the viewer holds in effect, each
	// with the record that put it in force ("" for the c4:party: group).
	held map[string]map[string]string
	// err is why the farm book could not be read: the viewer then holds
	// nothing for any organisation.
	err error
}

// holds says whether the viewer holds purpose for party, in effect.
func (st *standing) holds(party, purpose string) bool {
	_, ok := st.held[party][purpose]
	return ok
}

// standingOf reads what the farm's Parties say of v: for each Party, the
// purposes v holds in effect. An organisation's are its memberships in
// force for v, by name and by v's groups, and the c4:party: group's
// member; a person's party gives the person every purpose (ORGANIZATION
// 5).
func (s *Server) standingOf(ctx context.Context, v *api.Viewer) *standing {
	st := &standing{held: map[string]map[string]string{}}
	ps, err := s.parties(ctx)
	if err != nil {
		st.err = err
		return st
	}
	st.parties = ps
	if len(ps) == 0 {
		return st
	}
	st.person = s.partyPerson(ctx, v, ps)
	if st.person == "" {
		return st
	}
	var view *farmbook.View
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
			if p.Name == st.person {
				grant(p, Purposes, p.Spec.Made.Record)
			}
			continue
		}
		if view == nil && st.err == nil {
			view, st.err = s.partyBook(ctx)
		}
		if st.err != nil {
			continue
		}
		keys := []string{farmbook.MemberKey(p.Name, st.person)}
		if MembersByGroup {
			for _, g := range v.Groups {
				keys = append(keys, farmbook.GroupKey(p.Name, g))
			}
		}
		for _, key := range keys {
			on := inForce(view, farmbook.ForceMembership, key)
			if on == nil {
				continue
			}
			purposes, err := s.purposesOf(ctx, on)
			if err != nil {
				s.Log.Error(err, "a membership's record", "party", p.Name, "key", key)
				continue
			}
			grant(p, purposes, on.Entry.Record)
		}
		if MembersByGroup && slices.Contains(v.Parties, p.Name) {
			// The studio's c4:party:<name> group: member, and nothing more.
			grant(p, []string{PurposeMember}, "")
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
	out.Booked = inForce(view, farmbook.ForceParty, farmbook.PartyKey(p.Name)) != nil
	if p.Spec.Kind != c4v1.PartyOrganisation {
		return out, nil
	}
	ms, err := s.membersOf(ctx, view, p)
	if err != nil {
		return out, err
	}
	for _, m := range ms {
		if m.group != "" && !MembersByGroup {
			continue
		}
		e := api.Member{Person: m.person, Group: m.group, Purposes: m.purposes, InEffect: inEffect(p, m.purposes),
			By: m.on.Entry.By, Record: m.on.Entry.Record, Since: m.on.Time, Pending: m.on.Pending}
		if m.group != "" {
			out.Groups = append(out.Groups, e)
		} else {
			out.Members = append(out.Members, e)
		}
	}
	if MembersByGroup {
		out.Groups = append(out.Groups, api.Member{Group: partyGroups(p.Name)[0], Purposes: []string{PurposeMember}, InEffect: []string{PurposeMember}})
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
// unit.
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
// guard every party write passes: a person, signed in or on the
// installer's secret, never a unit, an agent or a tracker's sync; on a
// farm that keeps a farm book and signs.
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
// status, or 0: the guard every write passes, and what the viewer's
// makeParties and makeOwnParty say.
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

// farmAdminMakes is the farm admin's right make-party takes (DESIGN D8):
// create on parties, or for a development viewer the farm-admin role or
// every role (devrights). name "" asks of any party.
func (s *Server) farmAdminMakes(ctx context.Context, r *http.Request, v *api.Viewer, name string) error {
	return s.authorize(ctx, r, v, "", "create", "parties", name)
}

// withPartyRights is v as the HUD serves it (snapshot, events, needs-me),
// a copy, with what it may do about the party list: makeParties, by the
// guard and right make-party takes; bindingOwner, the farm binding's
// owner; and makeOwnParty, that owner while their own Party doesn't exist,
// by ownerMakesOwn's rule.
func (s *Server) withPartyRights(r *http.Request, v *api.Viewer) *api.Viewer {
	out := *v
	ctx := r.Context()
	ps, err := s.parties(ctx)
	if err != nil {
		return &out
	}
	b, err := s.farmBinding(ctx)
	person := s.partyPerson(ctx, v, ps)
	out.BindingOwner = err == nil && b != nil && person != "" && person == b.Spec.Owner
	if code, _ := s.partyWriteRefusal(r, v); code != 0 {
		return &out
	}
	out.MakeParties = s.farmAdminMakes(ctx, r, v, "") == nil
	out.MakeOwnParty = out.BindingOwner && s.ownerMakesOwn(ctx, ps, person, &api.PartyRequest{Name: person, Kind: c4v1.PartyPerson}, nil)
	return &out
}

// validSignIns checks a list of sign-in subjects: each well formed, a
// provider's at an http(s) issuer URL, none twice. It returns them sorted
// by issuer, then subject.
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
	slices.SortFunc(out, func(a, b c4v1.PartySignIn) int {
		if c := strings.Compare(a.Issuer, b.Issuer); c != 0 {
			return c
		}
		return strings.Compare(a.Subject, b.Subject)
	})
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
// subjects among its sign-ins, and that subject.
func claimed(ps []c4v1.Party, except string, subjects []c4v1.PartySignIn) (string, string) {
	for i := range ps {
		if ps[i].Name == except || ps[i].Spec.Kind != c4v1.PartyPerson {
			continue
		}
		for _, x := range subjects {
			if slices.Contains(ps[i].Spec.SignIns, x) {
				return ps[i].Name, signInWords(x)
			}
		}
	}
	return "", ""
}

// ownerMakesOwn says whether the farm binding's owner may make body
// without a farm admin's right: their own person Party, while it doesn't
// exist, signed in only as themselves. So the owner can always make the
// owner's Party, which the people's issuer's admins then come from.
func (s *Server) ownerMakesOwn(ctx context.Context, ps []c4v1.Party, person string, body *api.PartyRequest, signIns []c4v1.PartySignIn) bool {
	b, err := s.farmBinding(ctx)
	if err != nil || b == nil || person == "" || person != b.Spec.Owner || body.Name != b.Spec.Owner ||
		body.Kind != c4v1.PartyPerson || findParty(ps, body.Name) != nil {
		return false
	}
	for _, x := range signIns {
		// a provider's subject is a farm admin's to add
		if x.Issuer != "" {
			return false
		}
	}
	return true
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

// bookParty books a party act's control record in the farm book, now:
// for a membership the book is the state, so an act it did not take did
// nothing.
func (s *Server) bookParty(ctx context.Context, record string) error {
	if s.FarmBook == nil {
		return errors.New("this farm keeps no farm book")
	}
	rec, err := s.FarmBook.Store.Get(ctx, record)
	if err != nil {
		return err
	}
	e, err := farmbook.ControlEntry(rec, time.Now())
	if err != nil {
		return err
	}
	_, err = s.FarmBook.Append(ctx, farmbook.Submission{Entry: e})
	return err
}

// handleMakeParty answers POST /api/v1/parties {name, kind, displayName,
// legalName?, signIns?}: a farm admin makes a party (make-party). Whoever
// makes an organisation is its first admin, holding admin and member.
func (s *Server) handleMakeParty(w http.ResponseWriter, r *http.Request) {
	v := s.partyWriter(w, r)
	if v == nil {
		return
	}
	var body api.PartyRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {name, kind, displayName, legalName?, signIns?}: %v", err)
		return
	}
	body.Name, body.DisplayName, body.LegalName = strings.TrimSpace(body.Name), strings.TrimSpace(body.DisplayName), strings.TrimSpace(body.LegalName)
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
	}
	ctx := r.Context()
	ps, err := s.parties(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	person := s.partyPerson(ctx, v, ps)
	authz := s.authorization(v, "create", "parties", "", body.Name)
	if err := s.farmAdminMakes(ctx, r, v, body.Name); err != nil {
		if !s.ownerMakesOwn(ctx, ps, person, &body, signIns) {
			fail(w, http.StatusForbidden, "a farm admin keeps the party list: %v", err)
			return
		}
		authz = "the farm binding's owner, making their own party"
	}
	if old := findParty(ps, body.Name); old != nil {
		s.partyExists(w, r, v, old, &body, signIns)
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
	if body.Kind == c4v1.PartyOrganisation && person == "" {
		fail(w, http.StatusForbidden, "whoever makes an organisation is its first admin, and %s is no person", v.User)
		return
	}
	change := map[string]any{"name": body.Name, "kind": body.Kind, "displayName": body.DisplayName}
	if body.LegalName != "" {
		change["legalName"] = body.LegalName
	}
	if len(signIns) > 0 {
		change["signIns"] = signIns
	}
	if body.Kind == c4v1.PartyOrganisation {
		// Whoever makes an organisation is its first admin.
		change["member"], change["purposes"] = person, firstAdmin
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
	if err := s.bookParty(ctx, id); err != nil {
		fail(w, http.StatusServiceUnavailable, "party %s is made, and the farm book did not take its record (%v), so it has no members yet: make it again the same way to book it", body.Name, err)
		return
	}
	msg := fmt.Sprintf("%s makes the %s %s (%s)", v.User, body.Kind, body.Name, body.DisplayName)
	if body.Kind == c4v1.PartyOrganisation {
		msg += fmt.Sprintf(", with %s its first admin", person)
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
	if inForce(view, farmbook.ForceParty, farmbook.PartyKey(old.Name)) == nil {
		if err := s.bookParty(ctx, old.Spec.Made.Record); err != nil {
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

// adminOf says how v may change p: as its admin (an organisation's admin,
// or the person whose party it is), or as a farm admin; "" when v may
// not.
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

// handleSignIns answers PUT /api/v1/parties/{name}/sign-ins {signIns}
// (set-sign-ins): a person's sign-in subjects, set by the person or a farm
// admin. The person adds only the sign-in they are using; a farm admin,
// who keeps the party list, may add any not already another's.
func (s *Server) handleSignIns(w http.ResponseWriter, r *http.Request) {
	v := s.partyWriter(w, r)
	if v == nil {
		return
	}
	var body api.SignInsRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {signIns}: %v", err)
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
	authz := s.adminOf(r, v, st, p)
	if authz == "" {
		fail(w, http.StatusForbidden, "%s may not change %s's sign-ins: the person, or a farm admin, does", v.User, p.Name)
		return
	}
	if !s.farmAdmin(r.Context(), r, v, p.Name) {
		// the person themselves
		for _, x := range signIns {
			if x != (c4v1.PartySignIn{Subject: v.User}) && !slices.Contains(p.Spec.SignIns, x) {
				fail(w, http.StatusForbidden, "signIns: %s adds only the sign-in they are using (%s); a farm admin adds %s", v.User, v.User, signInWords(x))
				return
			}
		}
	}
	if who, x := claimed(st.parties, p.Name, signIns); who != "" {
		fail(w, http.StatusConflict, "signIns: %s is %s's already; one sign-in is one person's", x, who)
		return
	}
	if who, x, err := s.standInHolder(r.Context(), st.parties, p.Name, signIns); err != nil || who != "" {
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

// handleMember answers the three membership writes, each {person | group,
// purposes?}:
//   - POST /api/v1/parties/{name}/members: add-member;
//   - PUT /api/v1/parties/{name}/members: set-purposes, of a membership in
//     force;
//   - POST /api/v1/parties/{name}/members/end: end-member. A membership
//     ends, it is never deleted.
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
			fail(w, http.StatusBadRequest, "name a person or a group of the studio's own, not both")
			return
		case body.Person != "" && !personName.MatchString(body.Person):
			fail(w, http.StatusBadRequest, "person: a person's name, their party's or their sign-in's, without spaces, colons or slashes")
			return
		case body.Person != "" && findParty(st.parties, body.Person) != nil && findParty(st.parties, body.Person).Spec.Kind != c4v1.PartyPerson:
			fail(w, http.StatusBadRequest, "person: %s is an organisation, and only a person is a member", body.Person)
			return
		case body.Group != "" && !MembersByGroup:
			fail(w, http.StatusBadRequest, "group: on this farm every member is named (membership by the studio's groups is off)")
			return
		case body.Group != "" && !subjectName.MatchString(body.Group):
			fail(w, http.StatusBadRequest, "group: a group of the studio's sign-in, without spaces or commas")
			return
		case body.Group != "" && slices.Contains(partyGroups(p.Name), body.Group):
			fail(w, http.StatusBadRequest, "group: the studio's %s group holds member and nothing more, as its sign-in says who is in it", body.Group)
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
		ms, err := s.membersOf(ctx, view, p)
		if err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		var cur *membership
		admins := 0
		for i := range ms {
			if ms[i].person != "" && slices.Contains(ms[i].purposes, PurposeAdmin) {
				admins++
			}
			if ms[i].person == body.Person && ms[i].group == body.Group {
				cur = &ms[i]
			}
		}
		self := body.Person != "" && body.Person == st.person || body.Group != "" && slices.Contains(v.Groups, body.Group)
		authz := ""
		switch {
		case st.holds(p.Name, PurposeAdmin):
			authz = "an admin of " + p.Name
		case action == "end-member" && body.Person != "" && self:
			// a member ends their own membership: it only narrows
			authz = "the member, leaving"
		default:
			fail(w, http.StatusForbidden, "%s is not an admin of %s: its admins change its members", v.User, p.Name)
			return
		}
		switch {
		case action == "add-member" && cur != nil:
			fail(w, http.StatusConflict, "%s is a member of %s already, holding %s: change its purposes (PUT)", who, p.Name, strings.Join(cur.purposes, ", "))
			return
		case action != "add-member" && cur == nil:
			fail(w, http.StatusNotFound, "%s is not a member of %s", who, p.Name)
			return
		case action == "set-purposes" && slices.Equal(cur.purposes, purposes):
			s.answerParty(w, r, v, p, fmt.Sprintf("%s holds %s for %s already", who, strings.Join(purposes, ", "), p.Name), "", true)
			return
		}
		var had []string
		if cur != nil {
			had = cur.purposes
		}
		if self {
			// Admins revoke, never grant for themselves: a membership that
			// is the admin's own gives nothing they don't hold already.
			for _, x := range purposes {
				if !slices.Contains(had, x) && !st.holds(p.Name, x) {
					fail(w, http.StatusForbidden, "%s may not grant %s to %s: an admin never grants for themselves; another admin of %s does", v.User, x, who, p.Name)
					return
				}
			}
		}
		if body.Person != "" && slices.Contains(had, PurposeAdmin) && !slices.Contains(purposes, PurposeAdmin) && admins <= 1 {
			fail(w, http.StatusConflict, "%s is the last admin of %s: add another admin first", body.Person, p.Name)
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
		if cur != nil {
			change["follows"] = cur.on.Entry.Record
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
		if err := s.bookParty(ctx, id); err != nil {
			fail(w, http.StatusServiceUnavailable, "the farm book did not take the %s record (%v), so nothing changed: try again", action, err)
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
