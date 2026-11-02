package server

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// actBook is a grant farm whose HUD keeps a farm book on the scratch farm
// its acts are signed on, with an issuer to sign device records.
type actBook struct {
	*bookHUD
	g      *actRig
	issuer *provenance.Signer
}

func newActBook(t *testing.T) *actBook {
	t.Helper()
	s := grantFarm(t)
	s.DevViewers["noor"] = &api.Viewer{User: "noor", Projects: []string{"show-a"}}
	f := provtest.NewFarm(t, time.Now())
	sign := func(component string) *provenance.Signer {
		k, err := provenance.NewSigner(context.Background(), provenance.LocalIssuer(f.CA, provenance.ComponentIdentity(component), time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	// the HUD signs on the farm's own CA, as the people's devices are
	s.Signer = sign("hud")
	g := newActRigOn(t, s, f)
	b := &bookHUD{s: s, roots: f.Roots, farm: f.ID, store: &farmbook.MemoryStore{}, ptr: &farmbook.MemoryPointer{}, own: &farmbook.MemorySealer{}}
	s.FarmBook = &farmbook.Writer{Store: b.store, Pointer: b.ptr}
	s.FarmBookReader = &farmbook.Reader{Farm: b.farm, Roots: b.roots, Store: b.store, Pointer: b.ptr, Own: b.own}
	b.sealer = &farmbook.Sealer{Farm: b.farm, Roots: b.roots, Store: b.store, Pointer: b.ptr, Own: b.own, Signer: sign("queue"), Log: logr.Discard(),
		Checkers: func() []farmbook.Checker {
			return []farmbook.Checker{&farmbook.ControlChecker{Farm: b.farm, Roots: b.roots, Store: b.store}, devices{}}
		}}
	return &actBook{bookHUD: b, g: g, issuer: sign("issuer")}
}

// enrol books k's device record for person, as the people's issuer
// would: a device entry pending, citing the record it stores.
func (a *actBook) enrol(t *testing.T, k *provtest.Key, person, kind, label, protection string) {
	t.Helper()
	a.enrolKey(t, k, "", person, kind, label, protection)
}

// enrolTouch books k's touch key's device record, a touch key for k.
func (a *actBook) enrolTouch(t *testing.T, k *provtest.Key, person string) {
	t.Helper()
	a.enrolKey(t, k.Touch, k.Device, person, "mac", "", schema.ProtectionHardware)
}

func (a *actBook) enrolKey(t *testing.T, k *provtest.Key, touchFor, person, kind, label, protection string) {
	t.Helper()
	ctx := context.Background()
	spki, err := x509.MarshalPKIXPublicKey(&k.P256.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	p := schema.DevicePredicate{Farm: a.farm, Person: person, Key: base64.RawURLEncoding.EncodeToString(spki), Protection: protection,
		Kind: kind, Label: label, TouchFor: touchFor, Evidence: schema.EvidenceInstaller, Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		RequestedAt: time.Now().UTC().Format(time.RFC3339)}
	proof, err := provenance.SignP256(k.P256, p.ProofMessage())
	if err != nil {
		t.Fatal(err)
	}
	p.Proof = base64.RawURLEncoding.EncodeToString(proof)
	rec, err := provenance.SignStatement(ctx, a.issuer, provenance.PredicateDevice,
		[]provenance.Subject{{Name: "device:" + k.Device, Digest: map[string]string{"c4": k.Device}}}, p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, errs := schema.ParseDevice(payloadOf(t, rec)); errs != nil {
		t.Fatalf("the device record: %v", errs)
	}
	a.book(t, &people.Entry{Kind: people.KindDevice, Device: k.Device, Person: person, Record: c4id(rec)}, rec)
}

// book writes an entry pending, storing the record it cites.
func (a *actBook) book(t *testing.T, e *people.Entry, rec []byte) {
	t.Helper()
	e.Order, e.At = farmbook.OrderPending, time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := a.s.FarmBook.Append(context.Background(), farmbook.Submission{Entry: e, Record: rec}); err != nil {
		t.Fatal(err)
	}
}

func payloadOf(t *testing.T, rec []byte) []byte {
	t.Helper()
	var b provenance.Bundle
	if err := json.Unmarshal(rec, &b); err != nil {
		t.Fatal(err)
	}
	p, err := base64.StdEncoding.DecodeString(b.DSSEEnvelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// signed is k's act on on, as X-C4-Act carries it, naming the role as.
func (a *actBook) signed(t *testing.T, k *provtest.Key, on *api.ActOn, verb string, params any, confirmed bool, as string) string {
	t.Helper()
	grade := schema.GradeSigned
	if confirmed {
		grade = schema.GradeConfirmed
	}
	payload := a.g.f.Statement(t, k, provtest.Act{Subject: provenance.Subject{Name: on.Name, Digest: on.Digest}, Act: verb, Params: params,
		SignedAt: time.Now(), Confirmed: confirmed,
		Chain: &schema.ActChain{By: schema.Actor{Person: "noor", Grade: grade}, For: schema.For{Party: "noor", Grade: grade}}})
	if as != "" {
		var st schema.Act
		if err := json.Unmarshal(payload, &st); err != nil {
			t.Fatal(err)
		}
		st.Predicate.As = as
		payload, _ = json.Marshal(st)
	}
	return base64.StdEncoding.EncodeToString(provtest.Sign(t, payload, k, confirmed))
}

// acts is each entry of a page read as viewer, by what it books: a
// control entry's act and subject, else its kind.
func (a *actBook) acts(t *testing.T, as string) map[string]*api.Act {
	t.Helper()
	p := a.page(t, 0, "&as="+as, nil)
	if _, err := farmbook.CheckPage(a.farm, a.roots, farmbook.LastHead{}, p); err != nil {
		t.Errorf("a client's check of the page, acts beside its entries: %v", err)
	}
	out := map[string]*api.Act{}
	for _, es := range [][]farmbook.PageEntry{p.Entries, p.Pending} {
		for _, e := range es {
			var en farmbook.Entry
			json.Unmarshal(e.Entry, &en)
			key := en.Kind
			if en.Kind == farmbook.KindControl {
				key = en.Act + " " + en.Subject
			}
			out[key] = e.Act
		}
	}
	return out
}

func sameAct(got *api.Act, want api.Act) bool { return got != nil && *got == want }

// A book entry whose record cites a person's act is served with it: the
// act's grade and role, and the device record in force for the device
// that signed it, pending and once sealed. The grant and its revocation
// show the same object. A record that cites no act has none.
func TestBookEntriesShowTheirAct(t *testing.T) {
	a := newActBook(t)
	s := a.s
	noor := a.g.f.Person(t, "noor", true)
	a.enrol(t, noor, "noor", "mac", "noor's laptop", schema.ProtectionHardware)
	a.enrolTouch(t, noor, "noor")
	a.seal(t)

	on := projectActOn(t, s, "noor", "show-a")
	req := api.GrantRequest{Project: "show-a", Tracker: "github", Pools: []string{"cpu"}, Until: "7d"}
	code, out, p := a.g.send(t, http.MethodPost, "/api/v1/grants?as=noor", req, a.signed(t, noor, on, "grant", req, true, ""), "show-a", nil)
	cites(t, "grant", code, out, p, on, CustodyConfirmed)
	confirmed := api.Act{Grade: schema.GradeConfirmed, Kind: "mac", Label: "noor's laptop", Protection: schema.ProtectionHardware}
	var resp api.GrantResponse
	b, _ := json.Marshal(out)
	json.Unmarshal(b, &resp)
	if !sameAct(resp.Grant.Act, confirmed) {
		t.Errorf("the grant's answer shows act %+v, want %+v", resp.Grant.Act, confirmed)
	}
	target := "grant submitgrant:show-a/" + resp.Grant.Name
	if got := a.acts(t, "noor"); !sameAct(got[target], confirmed) || got[people.KindDevice] != nil {
		t.Errorf("pending: the grant's entry shows %+v, the device's %+v", got[target], got[people.KindDevice])
	}
	a.seal(t)
	if got := a.acts(t, "noor")[target]; !sameAct(got, confirmed) {
		t.Errorf("sealed: the grant's entry shows %+v, want %+v", got, confirmed)
	}
	if gs := grants(t, s, "?as=noor"); len(gs) != 1 || !sameAct(gs[0].Act, confirmed) {
		t.Errorf("GET /grants: %+v", gs)
	}

	// noor revokes it, signed, as artist
	withUID(t, s, &c4v1.SubmitGrant{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: resp.Grant.Name}}, "9a7e0000-0001")
	revokeOn := grants(t, s, "?as=noor")[0].ActOn
	signed := api.Act{Grade: schema.GradeSigned, Kind: "mac", Label: "noor's laptop", Protection: schema.ProtectionHardware, As: "artist"}
	code, out, _ = a.g.send(t, http.MethodDelete, "/api/v1/grants/show-a/"+resp.Grant.Name+"?as=noor", nil,
		a.signed(t, noor, revokeOn, "revoke", map[string]any{}, false, "artist"), "show-a", nil)
	if code != http.StatusOK {
		t.Fatalf("revoke: %d %v", code, out)
	}
	if gs := grants(t, s, "?as=noor"); len(gs) != 1 || gs[0].Revoked == nil || !sameAct(gs[0].Revoked.Act, signed) || !sameAct(gs[0].Act, confirmed) {
		t.Errorf("GET /grants after the revoke: %+v", gs)
	}
	if got := a.acts(t, "noor")["revoke submitgrant:show-a/"+resp.Grant.Name]; !sameAct(got, signed) {
		t.Errorf("the revoke's entry shows %+v, want %+v", got, signed)
	}

	// lee grants with no act: a record made as before personal keys
	if code, out, _ := a.g.send(t, http.MethodPost, "/api/v1/grants?as=lee", req, "", "show-a", nil); code != http.StatusOK {
		t.Fatalf("lee's grant: %d %v", code, out)
	}
	for _, g := range grants(t, s, "?as=lee") {
		if g.Act != nil {
			t.Errorf("lee's grant cites no act, and shows %+v", g.Act)
		}
		if got, ok := a.acts(t, "lee")["grant submitgrant:show-a/"+g.Name]; !ok || got != nil {
			t.Errorf("lee's grant entry: booked %v, act %+v", ok, got)
		}
	}
}

// A confirmed act is shown confirmed only if its touch key was in force
// for its device at the entry's position, by the rule the HUD's live check
// applies (CheckTouchKey): a touch key never enrolled shows the act signed.
func TestBookActConfirmedOnlyWithItsTouchKeyInForce(t *testing.T) {
	a := newActBook(t)
	s := a.s
	noor := a.g.f.Person(t, "noor", true)
	a.enrol(t, noor, "noor", "mac", "noor's laptop", schema.ProtectionHardware)
	a.seal(t)

	on := projectActOn(t, s, "noor", "show-a")
	req := api.GrantRequest{Project: "show-a", Tracker: "github", Pools: []string{"cpu"}, Until: "7d"}
	code, out, _ := a.g.send(t, http.MethodPost, "/api/v1/grants?as=noor", req, a.signed(t, noor, on, "grant", req, true, ""), "show-a", nil)
	if code != http.StatusOK {
		t.Fatalf("grant: %d %v", code, out)
	}
	a.seal(t)
	signed := api.Act{Grade: schema.GradeSigned, Kind: "mac", Label: "noor's laptop", Protection: schema.ProtectionHardware}
	gs := grants(t, s, "?as=noor")
	if len(gs) != 1 || !sameAct(gs[0].Act, signed) {
		t.Fatalf("a confirmed act whose touch key isn't enrolled: %+v", gs)
	}
	if got := a.acts(t, "noor")["grant submitgrant:show-a/"+gs[0].Name]; !sameAct(got, signed) {
		t.Errorf("its book entry shows %+v, want %+v", got, signed)
	}
}

// With no device record in force for the device that signed, at the
// entry's position, the entry shows no act rather than a guess: a key
// never enrolled, and one removed before the act was booked.
func TestBookActLeftOutWithoutItsDevice(t *testing.T) {
	a := newActBook(t)
	s := a.s
	noor := a.g.f.Person(t, "noor", true)
	a.enrol(t, noor, "noor", "mac", "noor's laptop", schema.ProtectionHardware)
	a.enrolTouch(t, noor, "noor")
	stranger := a.g.f.Person(t, "noor", true) // certified, no device record
	on := projectActOn(t, s, "noor", "show-a")
	req := api.GrantRequest{Project: "show-a", Tracker: "github", Until: "7d"}
	code, out, _ := a.g.send(t, http.MethodPost, "/api/v1/grants?as=noor", req, a.signed(t, stranger, on, "grant", req, true, ""), "show-a", nil)
	if code != http.StatusOK {
		t.Fatalf("the stranger's grant: %d %v", code, out)
	}
	if gs := grants(t, s, "?as=noor"); len(gs) != 1 || gs[0].Act != nil {
		t.Errorf("a grant signed by a device with no record: %+v", gs)
	}
	// noor's device removed, then her grant: no act at its position
	a.book(t, &people.Entry{Kind: people.KindRemoval, Device: noor.Device, Person: "noor"}, nil)
	req.Pools = []string{"cpu"}
	code, out, _ = a.g.send(t, http.MethodPost, "/api/v1/grants?as=noor", req, a.signed(t, noor, on, "grant", req, true, ""), "show-a", nil)
	if code != http.StatusOK {
		t.Fatalf("the grant after the removal: %d %v", code, out)
	}
	if gs := grants(t, s, "?as=noor"); len(gs) != 1 || gs[0].Act != nil {
		t.Errorf("a grant signed by a removed device: %+v", gs)
	}
}

// An entry whose record is the act itself (the people's issuer's) shows
// it the same way. An entry withheld whole from a reader shows no act
// either; to a reader who may read it, it does.
func TestWithheldEntryShowsNoAct(t *testing.T) {
	a := newActBook(t)
	// the installer's secret reads as the farm's one person, noor; an
	// open development viewer without it reads as no one
	a.s.InstallerToken, a.s.SinglePerson = []byte(testInstallerToken), "noor"
	noor := a.g.f.Person(t, "noor", true)
	laptop := a.g.f.Person(t, "noor", true)
	a.enrol(t, noor, "noor", "mac", "noor's laptop", schema.ProtectionHardware)
	a.enrolTouch(t, noor, "noor")
	a.enrol(t, laptop, "noor", "linux", "noor's workstation", schema.ProtectionFile)
	a.seal(t)
	act := a.g.f.SignAct(t, noor, provtest.Act{Subject: provenance.Subject{Name: "device:" + laptop.Device, Digest: map[string]string{"c4": laptop.Device}},
		Act: "remove-device", SignedAt: time.Now(),
		Chain: &schema.ActChain{By: schema.Actor{Person: "noor", Grade: schema.GradeSigned}, For: schema.For{Party: "noor", Grade: schema.GradeSigned}}})
	// from before private parts: its address in the entry
	a.book(t, &people.Entry{Kind: people.KindRemoval, Device: laptop.Device, Person: "noor", By: "noor", Record: c4id(act), Addr: "192.0.2.10"}, act)
	read := func(headers map[string]string) (*api.Act, []string) {
		p := a.page(t, 0, "&as=lee", headers)
		for _, e := range p.Pending {
			return e.Act, e.Redacted
		}
		t.Fatal("no pending entry")
		return nil, nil
	}
	if got, red := read(nil); got != nil || len(red) != 1 || red[0] != farmbook.RedactedEntry {
		t.Errorf("withheld whole from no one in particular: act %+v, redacted %v", got, red)
	}
	want := api.Act{Grade: schema.GradeSigned, Kind: "mac", Label: "noor's laptop", Protection: schema.ProtectionHardware}
	if got, red := read(map[string]string{InstallerHeader: testInstallerToken}); !sameAct(got, want) || len(red) != 0 {
		t.Errorf("to noor: act %+v, redacted %v; want %+v", got, red, want)
	}
	// an entry whose record is an act must name its person as by: one
	// that names another shows none
	other := a.g.f.SignAct(t, noor, provtest.Act{Subject: provenance.Subject{Name: "device:" + laptop.Device, Digest: map[string]string{"c4": laptop.Device}},
		Act: "remove-device", SignedAt: time.Now(),
		Chain: &schema.ActChain{By: schema.Actor{Person: "noor", Grade: schema.GradeSigned}, For: schema.For{Party: "noor", Grade: schema.GradeSigned}}})
	a.book(t, &people.Entry{Kind: people.KindRemoval, Device: laptop.Device, Person: "noor", By: "dana", Record: c4id(other)}, other)
	p := a.page(t, 0, "&as=lee", nil)
	if n := len(p.Pending); n != 2 || p.Pending[1].Act != nil {
		t.Errorf("an act booked as dana's, signed by noor: %d pending, the second's act %+v", n, p.Pending[n-1].Act)
	}
}

// A people-map confirmation and its revocation show their acts as a grant
// does: the same object, from the same entries.
func TestConfirmationShowsItsAct(t *testing.T) {
	a := newActBook(t)
	s, ctx := a.s, context.Background()
	noor := a.g.f.Person(t, "noor", true)
	a.enrol(t, noor, "noor", "mac", "noor's laptop", schema.ProtectionHardware)
	a.enrolTouch(t, noor, "noor")
	var tr c4v1.Tracker
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "github"}, &tr); err != nil {
		t.Fatal(err)
	}
	tr.Spec.People = map[string]string{"noor-gh": "noor"}
	if err := s.Client.Update(ctx, &tr); err != nil {
		t.Fatal(err)
	}
	withUID(t, s, &tr, "7c0e0000-0002")
	entries := func() []api.PeopleEntry {
		var out api.PeopleList
		if code := call(t, s, http.MethodGet, "/api/v1/people?as=noor&mine=1&all=1", nil, &out); code != http.StatusOK {
			t.Fatalf("GET /people: %d", code)
		}
		return out.Entries
	}
	confirm := api.PeopleConfirm{Project: "show-a", Tracker: "github", Login: "noor-gh"}
	code, out, _ := a.g.send(t, http.MethodPost, "/api/v1/people?as=noor", confirm, a.signed(t, noor, entries()[0].ConfirmOn, "people-confirm", confirm, true, ""), "show-a", nil)
	if code != http.StatusOK {
		t.Fatalf("confirm: %d %v", code, out)
	}
	confirmed := api.Act{Grade: schema.GradeConfirmed, Kind: "mac", Label: "noor's laptop", Protection: schema.ProtectionHardware}
	e := entries()[0]
	if e.Confirmed == nil || !sameAct(e.Confirmed.Act, confirmed) {
		t.Fatalf("the confirmation: %+v", e.Confirmed)
	}
	withUID(t, s, &c4v1.TrackerPerson{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: e.Name}}, "7c0e0000-0003")
	e = entries()[0]
	code, out, _ = a.g.send(t, http.MethodDelete, "/api/v1/people/show-a/"+e.Name+"?as=noor", nil, a.signed(t, noor, e.RevokeOn, "people-revoke", map[string]any{}, false, ""), "show-a", nil)
	if code != http.StatusOK {
		t.Fatalf("revoke: %d %v", code, out)
	}
	signed := confirmed
	signed.Grade = schema.GradeSigned
	var revoked *api.PeopleEntry
	for _, e := range entries() {
		if e.State == "revoked" {
			revoked = &e
		}
	}
	if revoked == nil || revoked.Revoked == nil || !sameAct(revoked.Revoked.Act, signed) || !sameAct(revoked.Confirmed.Act, confirmed) {
		t.Errorf("the revoked confirmation: %+v", revoked)
	}
}
