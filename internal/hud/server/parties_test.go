package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// sso is the studio's sign-in provider in these tests (peopletest).
const sso = peopletest.Issuer

// partyRig is a HUD guarded by an installer token that signs under a
// scratch farm's CA, keeps a farm book the queue seals, trusts the farm's
// people's keys, and checks ID tokens at a scratch sign-in provider.
type partyRig struct {
	s      *Server
	f      *provtest.Farm
	g      *actRig
	idp    *peopletest.Farm
	store  *farmbook.MemoryStore
	ptr    *farmbook.MemoryPointer
	sealer *farmbook.Sealer
	uids   int
}

func newPartyRig(t *testing.T, objs ...runtime.Object) *partyRig {
	t.Helper()
	s := guarded(t)
	f := provtest.NewFarm(t, time.Now())
	ctx := context.Background()
	var err error
	if s.Signer, err = provenance.NewSigner(ctx, provenance.LocalIssuer(f.CA, provenance.ComponentIdentity("hud"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	queue, err := provenance.NewSigner(ctx, provenance.LocalIssuer(f.CA, provenance.ComponentIdentity("queue"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	p := &partyRig{s: s, f: f, store: &farmbook.MemoryStore{}, ptr: &farmbook.MemoryPointer{}}
	own := &farmbook.MemorySealer{}
	s.FarmBook = &farmbook.Writer{Store: p.store, Pointer: p.ptr}
	s.FarmBookReader = &farmbook.Reader{Farm: f.ID, Roots: f.Roots, Store: p.store, Pointer: p.ptr, Own: own}
	p.sealer = &farmbook.Sealer{Farm: f.ID, Roots: f.Roots, Store: p.store, Pointer: p.ptr, Own: own, Signer: queue, Log: logr.Discard(),
		Checkers: func() []farmbook.Checker {
			return []farmbook.Checker{&farmbook.ControlChecker{Farm: f.ID, Roots: f.Roots, Store: p.store}}
		}}
	p.g = newActRigOn(t, s, f)
	p.idp = peopletest.Studio(t)
	s.ProveSignIn = p.idp.Issuer.ProveSignIn
	for name, v := range map[string]*api.Viewer{
		"ines": {User: "ines", Role: "farm-admin", All: true},
		"lee":  {User: "lee", Role: "farm-admin", All: true},
		"dana": {User: "dana", Role: "artist", All: true},
		"noor": {User: "noor", Role: "artist", All: true},
		"maya": {User: "maya", Role: "artist", All: true},
		// the studio's c4:party:kestrel group, as a sign-in reports it
		"kim": {User: "kim", Role: "artist", All: true, Parties: []string{"kestrel"}, Groups: []string{"c4:party:kestrel"}},
		"sam": {User: "sam", Role: "artist", All: true, Groups: []string{"lighting-sups"}},
		// noor and dana, by the studio's sign-in
		"noor-sso": {User: "noor@studio.example", Role: "artist", All: true},
		"dana-sso": {User: "dana@studio.example", Role: "artist", All: true},
		"mallory":  {User: "mallory", Role: "artist", All: true},
	} {
		s.DevViewers[name] = v
	}
	return p
}

// call sends body to path as the development viewer as, on the
// installer's secret unless headers say otherwise.
func (p *partyRig) call(t *testing.T, method, path, as string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	if body == nil {
		raw = nil
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	r := httptest.NewRequest(method, path+sep+"as="+as, bytes.NewReader(raw))
	r.Header.Set(InstallerHeader, testInstallerToken)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	p.s.Handler().ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// party reads GET /parties/{name} as as.
func (p *partyRig) party(t *testing.T, name, as string) api.Party {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/parties/"+name+"?as="+as, nil)
	r.Header.Set(InstallerHeader, testInstallerToken)
	w := httptest.NewRecorder()
	p.s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET party %s as %s: %d %s", name, as, w.Code, w.Body)
	}
	var out api.PartyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return *out.Party
}

// viewer is the development viewer as, as a request makes it.
func (p *partyRig) viewer(as string) (*http.Request, *api.Viewer) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	v := *p.s.DevViewers[as]
	v.DevBy = byInstallerToken
	return r, &v
}

// token is a fresh ID token for subject at the studio's provider.
func (p *partyRig) token(subject string) string {
	return p.idp.SSO.Token(time.Now(), subject, "", nil)
}

// uid gives a Party the UID the API server would, so an act can name it.
func (p *partyRig) uid(t *testing.T, name string) {
	t.Helper()
	p.uids++
	withUID(t, p.s, &c4v1.Party{ObjectMeta: metav1.ObjectMeta{Name: name}}, "9b1e0000-0000-4000-8000-"+strconv.FormatInt(int64(100000000000+p.uids), 10))
}

// own makes as's own person Party, name, listing as's own sign-in and
// the provider subject given ("<issuer> <subject>"), proved.
func (p *partyRig) own(t *testing.T, as, name string, provider ...string) {
	t.Helper()
	body := map[string]any{"name": name, "kind": "person", "displayName": name, "signIns": ins(append([]string{p.s.DevViewers[as].User}, provider...)...)}
	if len(provider) == 1 {
		_, sub, _ := strings.Cut(provider[0], " ")
		body["idToken"] = p.token(sub)
	}
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", as, body, nil); code != http.StatusOK {
		t.Fatalf("%s makes their Party %s: %d %v", as, name, code, out)
	}
	p.uid(t, name)
}

// org has ines make the organisation name, naming first its first admin,
// and first accept as as, with their own touched act.
func (p *partyRig) org(t *testing.T, name, first, as string) {
	t.Helper()
	body := kestrel()
	body["name"], body["firstAdmin"] = name, first
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", body, nil); code != http.StatusOK {
		t.Fatalf("ines makes %s: %d %v", name, code, out)
	}
	p.uid(t, name)
	if code, out := p.accept(t, name, as, first, true); code != http.StatusOK {
		t.Fatalf("%s accepts %s: %d %v", first, name, code, out)
	}
}

// accept sends POST /parties/{name}/accept as as, with person's own act
// (touched, or not).
func (p *partyRig) accept(t *testing.T, name, as, person string, touched bool) (int, map[string]any) {
	t.Helper()
	on := p.party(t, name, as).ActOn
	act := p.g.act(t, p.f.Person(t, person, true), on, "accept-admin", map[string]any{}, touched)
	return p.call(t, http.MethodPost, "/api/v1/parties/"+name+"/accept", as, map[string]any{}, map[string]string{ActHeader: act})
}

// secret has the c4-sign-ins Secret sign noor and dana in, by their
// provider subjects, as a studio farm does before Parties.
func (p *partyRig) secret() {
	p.s.StandInSignIns = people.StaticSignIns{"noor": {{Issuer: sso, Subject: peopletest.NoorSub}}, "dana": {{Issuer: sso, Subject: peopletest.DanaSub}}}
}

// bind binds the farm to owner, run for runFor ("" for a farm its owner
// runs).
func (p *partyRig) bind(t *testing.T, owner, runFor string) {
	t.Helper()
	if err := p.s.Client.Create(context.Background(), &c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: c4v1.FarmBindingName},
		Spec: c4v1.DeploymentBindingSpec{Owner: owner, RunFor: runFor, To: "laptop-1/*", For: []string{"task/v1"}, By: owner}}); err != nil {
		t.Fatal(err)
	}
}

// people make the rig's usual persons' Parties, each signed in as their
// own name: those named, or ines, lee, dana, noor, maya, sam and kim.
func (p *partyRig) people(t *testing.T, who ...string) {
	t.Helper()
	if len(who) == 0 {
		who = []string{"ines", "lee", "dana", "noor", "maya", "sam", "kim"}
	}
	for _, x := range who {
		p.own(t, x, x)
	}
}

// seal seals the book and checks the queue took every entry.
func (p *partyRig) seal(t *testing.T) {
	t.Helper()
	res, err := p.sealer.Seal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res != nil && len(res.Refused) > 0 {
		t.Fatalf("the queue refused: %+v", res.Refused)
	}
}

func members(pv api.Party) map[string][]string {
	out := map[string][]string{}
	for _, m := range pv.Members {
		out[m.Person] = m.Purposes
	}
	for _, m := range pv.Groups {
		out["group:"+m.Group] = m.Purposes
	}
	return out
}

func says(out map[string]any, words string) bool {
	msg, _ := out["error"].(string)
	return strings.Contains(msg, words)
}

const kestrelBody = `{"name": "kestrel", "kind": "organisation", "displayName": "Kestrel Productions", "legalName": "Kestrel Productions LLC"}`

func kestrel() map[string]any {
	var m map[string]any
	json.Unmarshal([]byte(kestrelBody), &m)
	return m
}

// ins are sign-ins: a bare subject the HUD signs a person in as, or
// "<issuer URL> <subject>" for a provider's.
func ins(list ...string) []api.SignIn {
	out := []api.SignIn{}
	for _, x := range list {
		if iss, sub, ok := strings.Cut(x, " "); ok {
			out = append(out, api.SignIn{Issuer: iss, Subject: sub})
			continue
		}
		out = append(out, api.SignIn{Subject: x})
	}
	return out
}

// A farm admin makes an organisation naming its first admin, who counts
// once they accept; its admins add, change and end members, Parties each,
// by name and by group, each a signed record the queue books; an admin
// never grants for themselves, the last admin stays, a name is never
// reused, and view-people waits for the organisation's policy.
func TestPartiesAndMembers(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	// a managed farm: the provider runs it for kestrel
	p.bind(t, "avalanche", "kestrel")
	p.people(t)
	withFirst := func(first string) map[string]any { b := kestrel(); b["firstAdmin"] = first; return b }
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "maya", withFirst("noor"), nil); code != http.StatusForbidden || !says(out, "a farm admin keeps the party list") {
		t.Fatalf("an artist makes an organisation: %d %v", code, out)
	}
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), nil); code != http.StatusBadRequest || !says(out, "makes no one its admin") {
		t.Errorf("an organisation with no first admin: %d %v", code, out)
	}
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", withFirst("ines"), nil); code != http.StatusForbidden || !says(out, "never makes its maker its admin") {
		t.Errorf("ines names herself first admin on a managed farm: %d %v", code, out)
	}
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", withFirst("noor@studio.example"), nil); code != http.StatusBadRequest {
		t.Errorf("a first admin by a bare sign-in name: %d %v", code, out)
	}
	code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", withFirst("noor"), nil)
	if code != http.StatusOK {
		t.Fatalf("make kestrel: %d %v", code, out)
	}
	p.uid(t, "kestrel")
	pv := p.party(t, "kestrel", "maya")
	if !pv.Booked || len(pv.Members) != 0 || pv.FirstAdmin == nil || pv.FirstAdmin.Person != "noor" || pv.FirstAdmin.Accepted {
		t.Fatalf("kestrel before its first admin accepts: %+v", pv)
	}
	_, ines := p.viewer("ines")
	if p.s.holdsFor(ctx, ines, "kestrel", PurposeAdmin) {
		t.Fatal("ines, who made kestrel, is its admin")
	}
	p.seal(t)

	// a name is never reused; the same make again is an answer
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", withFirst("noor"), nil); code != http.StatusOK || out["existing"] != true {
		t.Errorf("make kestrel again: %d %v", code, out)
	}
	other := withFirst("noor")
	other["displayName"] = "Kestrel Films"
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", other, nil); code != http.StatusConflict || !says(out, "never reused") {
		t.Errorf("reuse kestrel: %d %v", code, out)
	}

	members := func(method, path, as string, body map[string]any) (int, map[string]any) {
		return p.call(t, method, "/api/v1/parties/kestrel/members"+path, as, body, nil)
	}
	if code, out := members(http.MethodPost, "", "noor", map[string]any{"person": "dana", "purposes": []string{"member"}}); code != http.StatusForbidden {
		t.Errorf("the named first admin acts before accepting: %d %v", code, out)
	}
	if code, out := p.accept(t, "kestrel", "noor", "noor", true); code != http.StatusOK {
		t.Fatalf("noor accepts: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "dana", map[string]any{"person": "maya", "purposes": []string{"member"}}); code != http.StatusForbidden || !says(out, "not an admin") {
		t.Errorf("a non-member adds one: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "noor", map[string]any{"person": "dana", "purposes": []string{"member", "approve-spend"}}); code != http.StatusOK {
		t.Fatalf("add dana: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "noor", map[string]any{"person": "dana", "purposes": []string{"member"}}); code != http.StatusConflict {
		t.Errorf("add dana twice: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "noor", map[string]any{"person": "kai", "purposes": []string{"member"}}); code != http.StatusBadRequest || !says(out, "never a bare sign-in name") {
		t.Errorf("a member by a name no Party has: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "dana", map[string]any{"person": "maya", "purposes": []string{"member"}}); code != http.StatusForbidden {
		t.Errorf("a member who is no admin adds one: %d %v", code, out)
	}
	// Admins revoke, never grant for themselves; the last admin stays.
	if code, out := members(http.MethodPut, "", "noor", map[string]any{"person": "noor", "purposes": []string{"member", "admin", "approve-spend"}}); code != http.StatusForbidden || !says(out, "never grants for themselves") {
		t.Errorf("noor grants herself: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "/end", "noor", map[string]any{"person": "noor"}); code != http.StatusConflict || !says(out, "last admin") {
		t.Errorf("the last admin leaves: %d %v", code, out)
	}
	if code, out := members(http.MethodPut, "", "noor", map[string]any{"person": "noor", "purposes": []string{"member"}}); code != http.StatusConflict || !says(out, "last admin") {
		t.Errorf("the last admin drops admin: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "noor", map[string]any{"person": "lee", "purposes": []string{"member", "admin"}}); code != http.StatusOK {
		t.Fatalf("add lee as an admin: %d %v", code, out)
	}
	if code, out := members(http.MethodPut, "", "lee", map[string]any{"person": "noor", "purposes": []string{"member", "admin", "approve-spend"}}); code != http.StatusOK {
		t.Errorf("another admin grants noor: %d %v", code, out)
	}
	// a member leaves on their own
	if code, out := members(http.MethodPost, "/end", "dana", map[string]any{"person": "dana"}); code != http.StatusOK {
		t.Errorf("dana leaves: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "/end", "dana", map[string]any{"person": "dana"}); code != http.StatusNotFound {
		t.Errorf("dana leaves twice: %d %v", code, out)
	}
	// by the studio's own groups
	if code, out := members(http.MethodPost, "", "noor", map[string]any{"group": "c4:party:kestrel", "purposes": []string{"member", "admin"}}); code != http.StatusBadRequest || !says(out, "member and nothing more") {
		t.Errorf("more for the c4:party: group: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "noor", map[string]any{"group": "lighting-sups", "purposes": []string{"member", "approve-spend"}}); code != http.StatusOK {
		t.Fatalf("lighting-sups: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "noor", map[string]any{"person": "maya", "group": "lighting-sups", "purposes": []string{"member"}}); code != http.StatusBadRequest {
		t.Errorf("a person and a group at once: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "noor", map[string]any{"person": "maya", "purposes": []string{"own"}}); code != http.StatusBadRequest {
		t.Errorf("an unknown purpose: %d %v", code, out)
	}
	pv = p.party(t, "kestrel", "maya")
	got := map[string][]string{}
	for _, m := range pv.Members {
		got[m.Person] = m.Purposes
	}
	for _, m := range pv.Groups {
		got["group:"+m.Group] = m.Purposes
	}
	want := map[string][]string{"noor": {"member", "admin", "approve-spend"}, "lee": {"member", "admin"},
		"group:lighting-sups": {"member", "approve-spend"}, "group:c4:party:kestrel": {"member"}}
	if len(got) != len(want) || !pv.FirstAdmin.Accepted {
		t.Errorf("kestrel's members: %v, want %v; first admin %+v", got, want, pv.FirstAdmin)
	}
	for k, w := range want {
		if !slices.Equal(got[k], w) {
			t.Errorf("kestrel's %s: %v, want %v", k, got[k], w)
		}
	}
	_, sam := p.viewer("sam")
	if !p.s.holdsFor(ctx, sam, "kestrel", PurposeApproveSpend) || p.s.holdsFor(ctx, sam, "kestrel", PurposeAdmin) {
		t.Error("sam does not hold lighting-sups' purposes, and only those")
	}
	// view-people is its own purpose, and off until the policy turns it on
	if code, out := members(http.MethodPost, "", "noor", map[string]any{"person": "maya", "purposes": []string{"view-spend", "view-people"}}); code != http.StatusOK {
		t.Fatalf("maya views: %d %v", code, out)
	}
	_, maya := p.viewer("maya")
	if !p.s.holdsFor(ctx, maya, "kestrel", PurposeViewSpend) || p.s.holdsFor(ctx, maya, "kestrel", PurposeViewPeople) {
		t.Error("view-people holds before the organisation's policy turns per-person views on")
	}
	if code, out := p.call(t, http.MethodPatch, "/api/v1/parties/kestrel", "maya", map[string]any{"perPersonViews": true}, nil); code != http.StatusForbidden {
		t.Errorf("a non-admin sets kestrel's policy: %d %v", code, out)
	}
	if code, out := p.call(t, http.MethodPatch, "/api/v1/parties/kestrel", "lee", map[string]any{"perPersonViews": true}, nil); code != http.StatusOK {
		t.Fatalf("an admin turns per-person views on: %d %v", code, out)
	}
	if !p.s.holdsFor(ctx, maya, "kestrel", PurposeViewPeople) {
		t.Error("view-people does not hold under the policy")
	}
	// every record the HUD wrote is booked as it says
	p.seal(t)
	if st, _ := p.ptr.Read(ctx); len(st.Pending) != 0 {
		t.Errorf("pending after the seal: %v", st.Pending)
	}
}

// An organisation's first admin counts only once they accept, with their
// own touched act, once; nobody else accepts for them (R4).
func TestPartyFirstAdminAccepts(t *testing.T) {
	p := newPartyRig(t)
	p.people(t)
	body := kestrel()
	body["firstAdmin"] = "noor"
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", body, nil); code != http.StatusOK {
		t.Fatalf("make kestrel: %d %v", code, out)
	}
	p.uid(t, "kestrel")
	if code, out := p.accept(t, "kestrel", "dana", "dana", true); code != http.StatusForbidden || !says(out, "only noor") {
		t.Errorf("dana accepts noor's place: %d %v", code, out)
	}
	if code, out := p.accept(t, "kestrel", "noor", "noor", false); code != http.StatusForbidden || !says(out, "touch") {
		t.Errorf("noor accepts with an act she didn't touch: %d %v", code, out)
	}
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/accept", "noor", map[string]any{}, nil); code != http.StatusForbidden {
		t.Errorf("noor accepts with no act: %d %v", code, out)
	}
	if code, out := p.accept(t, "kestrel", "noor", "noor", true); code != http.StatusOK {
		t.Fatalf("noor accepts: %d %v", code, out)
	}
	if code, out := p.accept(t, "kestrel", "noor", "noor", true); code != http.StatusConflict {
		t.Errorf("noor accepts twice: %d %v", code, out)
	}
	_, noor := p.viewer("noor")
	if !p.s.holdsFor(context.Background(), noor, "kestrel", PurposeAdmin) {
		t.Error("noor is no admin after accepting")
	}
	p.seal(t)
}

// Recommendations 1 and 3 each flip at one switch: with MembersByGroup off
// no group holds a purpose, the c4:party: group included; with
// ViewPeopleNeedsPolicy off, view-people holds once granted.
func TestPartyRecommendationsFlip(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	p.people(t)
	p.org(t, "kestrel", "noor", "noor")
	add := func(body map[string]any) {
		t.Helper()
		if code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "noor", body, nil); code != http.StatusOK {
			t.Fatalf("add %v: %d %v", body, code, out)
		}
	}
	add(map[string]any{"group": "lighting-sups", "purposes": []string{"member"}})
	add(map[string]any{"person": "maya", "purposes": []string{"view-people"}})
	_, sam := p.viewer("sam")
	_, kim := p.viewer("kim")
	_, maya := p.viewer("maya")
	if !p.s.holdsFor(ctx, sam, "kestrel", PurposeMember) || !p.s.holdsFor(ctx, kim, "kestrel", PurposeMember) {
		t.Fatal("groups hold nothing with MembersByGroup on")
	}
	defer func(was bool) { MembersByGroup = was }(MembersByGroup)
	MembersByGroup = false
	if p.s.holdsFor(ctx, sam, "kestrel", PurposeMember) || p.s.holdsFor(ctx, kim, "kestrel", PurposeMember) {
		t.Error("a group holds a purpose with MembersByGroup off")
	}
	if code, _ := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "noor", map[string]any{"group": "comp-sups", "purposes": []string{"member"}}, nil); code != http.StatusBadRequest {
		t.Errorf("a group rule with MembersByGroup off: %d", code)
	}
	MembersByGroup = true
	defer func(was bool) { ViewPeopleNeedsPolicy = was }(ViewPeopleNeedsPolicy)
	if p.s.holdsFor(ctx, maya, "kestrel", PurposeViewPeople) {
		t.Fatal("view-people holds without the policy")
	}
	ViewPeopleNeedsPolicy = false
	if !p.s.holdsFor(ctx, maya, "kestrel", PurposeViewPeople) {
		t.Error("view-people does not hold with ViewPeopleNeedsPolicy off")
	}
}

// The guard every party write passes: no unit, no agent, no tracker's
// sync, no development viewer without the installer's secret, and no
// farm without a farm book.
func TestPartyWriteGuard(t *testing.T) {
	p := newPartyRig(t)
	p.s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/sh040-light-v007/3", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "sh040-light-v007", Unit: "3", Grant: "job"}}
	p.s.DevViewers[c4v1.SyncDevViewer] = &api.Viewer{User: c4v1.SyncDevViewer, Role: "farm-admin", All: true}
	for _, c := range []struct {
		name, as string
		headers  map[string]string
		code     int
		says     string
	}{
		{"an agent through MCP", "ines", map[string]string{"X-C4-Client": "mcp"}, http.StatusForbidden, "an agent cannot change a party"},
		{"an agent named", "ines", map[string]string{"X-C4-Agent": "ra"}, http.StatusForbidden, "an agent cannot change a party"},
		{"a tracker's sync", c4v1.SyncDevViewer, nil, http.StatusForbidden, "an agent cannot change a party"},
		{"a unit", "unit", nil, http.StatusForbidden, "is a unit"},
		{"no installer token", "ines", map[string]string{InstallerHeader: ""}, http.StatusUnauthorized, ""},
	} {
		code, out := p.call(t, http.MethodPost, "/api/v1/parties", c.as, kestrel(), c.headers)
		if code != c.code || !says(out, c.says) {
			t.Errorf("%s: %d %v, want %d %q", c.name, code, out, c.code, c.says)
		}
	}
	// open development viewers: anyone may say they are ines
	p.s.DevViewersOpen = true
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), map[string]string{InstallerHeader: ""}); code != http.StatusForbidden || !says(out, "a development viewer is no one") {
		t.Errorf("an open development viewer: %d %v", code, out)
	}
	p.s.DevViewersOpen = false
	p.s.FarmBook, p.s.FarmBookReader = nil, nil
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), nil); code != http.StatusNotImplemented || !says(out, "no farm book") {
		t.Errorf("no farm book: %d %v", code, out)
	}
}

// For a membership the book is the state: a write the book does not take
// changes nothing.
func TestPartyMembershipNeedsTheBook(t *testing.T) {
	p := newPartyRig(t)
	p.people(t)
	p.org(t, "kestrel", "noor", "noor")
	p.s.FarmBook = &farmbook.Writer{Store: p.store, Pointer: failingPointer{p.ptr}}
	code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "noor", map[string]any{"person": "dana", "purposes": []string{"member"}}, nil)
	if code != http.StatusServiceUnavailable || !says(out, "nothing changed") {
		t.Fatalf("add dana, the book refusing: %d %v", code, out)
	}
	if _, ok := members(p.party(t, "kestrel", "ines"))["dana"]; ok {
		t.Error("dana is a member though the book did not take it")
	}
}

// failingPointer is a farm book whose State takes no write.
type failingPointer struct{ *farmbook.MemoryPointer }

func (failingPointer) Write(context.Context, farmbook.State, farmbook.State) error {
	return errors.New("the system store is down")
}

// A party: wait is the c4:party: group's while the farm keeps no Party of
// that name; once it does, only a member holding member decides it, by
// their Party or by a group, and the record says under which membership.
func TestPartyWaitsTakeMembers(t *testing.T) {
	p := newPartyRig(t)
	rec := p.s.m.byKey["show-a/sh040-light-v007"]
	since := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.Waits = []c4v1.WorkWait{{Unit: 9, Name: "registry", Server: "party:kestrel", Since: &since, Blocking: true}}
	if err := p.s.Client.Create(context.Background(), rec.job.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	may := func(as string) error {
		r, v := p.viewer(as)
		return MayControl("decide", "party:kestrel", p.s.decider(r, v))
	}
	if may("kim") != nil || may("dana") == nil {
		t.Fatalf("before the Party, the group decides: kim %v, dana %v", may("kim"), may("dana"))
	}
	p.people(t)
	p.org(t, "kestrel", "noor", "noor")
	add := func(method string, body map[string]any) {
		t.Helper()
		if code, out := p.call(t, method, "/api/v1/parties/kestrel/members", "noor", body, nil); code != http.StatusOK {
			t.Fatalf("%v: %d %v", body, code, out)
		}
	}
	add(http.MethodPost, map[string]any{"person": "dana", "purposes": []string{"approve-spend"}})
	if err := may("dana"); err == nil || !strings.Contains(err.Error(), "waits for a member of kestrel") {
		t.Errorf("a member without member decides: %v", err)
	}
	if may("maya") == nil || may("sam") == nil {
		t.Error("someone with no membership decides")
	}
	add(http.MethodPut, map[string]any{"person": "dana", "purposes": []string{"member", "approve-spend"}})
	add(http.MethodPost, map[string]any{"group": "lighting-sups", "purposes": []string{"member"}})
	if may("dana") != nil || may("sam") != nil || may("kim") != nil {
		t.Errorf("members do not decide: dana %v, sam %v, kim %v", may("dana"), may("sam"), may("kim"))
	}
	// the decision's record names the membership it was made under
	code, out := p.call(t, http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/decide", "dana", map[string]any{"unit": "registry", "do": "approve"}, nil)
	if code != http.StatusOK {
		t.Fatalf("dana decides: %d %v", code, out)
	}
	pred := p.g.predicate(t, "show-a", out["record"].(string))
	if !strings.Contains(pred.Authorization, "a member of kestrel holding member (record ") {
		t.Errorf("the decision's record: %q", pred.Authorization)
	}
	// and the wait is on sam's page of what needs him, and not on maya's
	needs := func(as string) bool {
		t.Helper()
		r, v := p.viewer(as)
		snap := &api.Snapshot{Jobs: []api.Job{{Namespace: "show-a", Name: "sh040-light-v007", Project: "show-a",
			Waits: []api.Wait{{Unit: 9, Name: "registry", WaitsFor: "party:kestrel"}}}}}
		for _, it := range p.s.needsMe(r, v, snap).Items {
			if it.ID == "decision:show-a/sh040-light-v007/9" {
				return true
			}
		}
		return false
	}
	if !needs("sam") || needs("maya") {
		t.Errorf("kestrel's wait needs sam %v (want true), maya %v (want false)", needs("sam"), needs("maya"))
	}
	// an agent and a unit still never decide for the party
	r, v := p.viewer("dana")
	d := p.s.decider(r, v)
	d.Agent = "ra"
	if MayControl("decide", "party:kestrel", d) == nil {
		t.Error("an agent decides for its person's party")
	}
}

// A person makes their own Party (R2): signed in with the name it lists,
// each provider subject proved by a fresh ID token for it; one sign-in is
// one person's; the person adds only their own sign-ins and removes any,
// a farm admin only removes; a sign-in of theirs decides their waits.
// Sign-ins go only to the person and to farm admins.
func TestPartySignIns(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	p.secret()
	mk := func(as, name string, signIns []api.SignIn, token string) (int, map[string]any) {
		body := map[string]any{"name": name, "kind": "person", "displayName": name, "signIns": signIns}
		if token != "" {
			body["idToken"] = token
		}
		return p.call(t, http.MethodPost, "/api/v1/parties", as, body, nil)
	}
	if code, out := mk("ines", "noor", ins("noor@studio.example"), ""); code != http.StatusForbidden || !says(out, "made by the person") {
		t.Errorf("a farm admin makes noor's Party: %d %v", code, out)
	}
	if code, out := mk("noor-sso", "noor", ins("noor@studio.example", sso+" "+peopletest.NoorSub), ""); code != http.StatusForbidden || !says(out, "fresh ID token") {
		t.Errorf("a provider subject with no token: %d %v", code, out)
	}
	if code, out := mk("noor-sso", "noor", ins("noor@studio.example", sso+" "+peopletest.NoorSub), p.token(peopletest.DanaSub)); code != http.StatusForbidden || !says(out, "proves") {
		t.Errorf("a provider subject on another's token: %d %v", code, out)
	}
	if code, out := mk("noor-sso", "noor", ins("noor@studio.example", "noor"), ""); code != http.StatusForbidden || !says(out, "added only by someone signed in as it") {
		t.Errorf("a HUD name noor isn't signed in as: %d %v", code, out)
	}
	if code, out := mk("noor-sso", "noor", ins("noor@studio.example", sso+" "+peopletest.NoorSub), p.token(peopletest.NoorSub)); code != http.StatusOK {
		t.Fatalf("noor makes her Party: %d %v", code, out)
	}
	p.uid(t, "noor")
	if code, out := mk("lee", "lee", ins("lee", sso+" "+peopletest.NoorSub), p.token(peopletest.NoorSub)); code != http.StatusConflict || !says(out, "one sign-in is one person's") {
		t.Errorf("lee takes noor's subject: %d %v", code, out)
	}
	if code, _ := p.call(t, http.MethodPost, "/api/v1/parties", "ines", map[string]any{"name": "saltmarsh", "kind": "organisation", "displayName": "Saltmarsh", "signIns": ins("x")}, nil); code != http.StatusBadRequest {
		t.Errorf("an organisation with sign-ins: %d", code)
	}
	r, noorSSO := p.viewer("noor-sso")
	d := p.s.decider(r, noorSSO)
	if MayControl("decide", "person:noor", d) != nil || MayControl("decide", "party:noor", d) != nil {
		t.Errorf("noor's sign-in does not decide noor's waits: %+v", d)
	}
	r, maya := p.viewer("maya")
	if MayControl("decide", "person:noor", p.s.decider(r, maya)) == nil {
		t.Error("maya decides noor's wait")
	}
	if got := p.party(t, "noor", "maya").SignIns; got != nil {
		t.Errorf("maya sees noor's sign-ins: %v", got)
	}
	if got := p.party(t, "noor", "noor-sso").SignIns; len(got) != 2 {
		t.Errorf("noor does not see her sign-ins: %v", got)
	}
	signIns := func(as string, token string, list ...string) (int, map[string]any) {
		body := map[string]any{"signIns": ins(list...)}
		if token != "" {
			body["idToken"] = token
		}
		return p.call(t, http.MethodPut, "/api/v1/parties/noor/sign-ins", as, body, nil)
	}
	if code, out := signIns("noor-sso", "", "noor@studio.example", sso+" "+peopletest.NoorSub, "maya"); code != http.StatusForbidden || !says(out, "added only by someone signed in as it") {
		t.Errorf("noor adds maya's name: %d %v", code, out)
	}
	if code, out := signIns("ines", "", "noor@studio.example", sso+" "+peopletest.NoorSub, "noor-laptop"); code != http.StatusForbidden || !says(out, "the person adds their own") {
		t.Errorf("a farm admin adds to noor's sign-ins: %d %v", code, out)
	}
	if code, out := signIns("maya", "", "noor@studio.example"); code != http.StatusForbidden {
		t.Errorf("maya removes noor's sign-in: %d %v", code, out)
	}
	if code, out := signIns("ines", "", "noor@studio.example"); code != http.StatusOK {
		t.Errorf("a farm admin removes noor's provider subject: %d %v", code, out)
	}
	if code, out := signIns("noor-sso", p.token(peopletest.NoorSub), "noor@studio.example", "HTTPS://SSO.Studio.Example/ "+peopletest.NoorSub); code != http.StatusOK {
		t.Errorf("noor adds her subject back, proved: %d %v", code, out)
	}
	pt, err := p.s.parties(ctx)
	if err != nil || !slices.Equal(findParty(pt, "noor").Spec.SignIns, []c4v1.PartySignIn{{Subject: "noor@studio.example"}, {Issuer: sso, Subject: peopletest.NoorSub}}) {
		t.Errorf("noor's sign-ins: %+v %v", findParty(pt, "noor"), err)
	}
	// a sign-in two Parties hold, written around the HUD, is no one's
	if err := p.s.Client.Create(ctx, &c4v1.Party{ObjectMeta: metav1.ObjectMeta{Name: "lee"}, Spec: c4v1.PartySpec{Kind: c4v1.PartyPerson,
		DisplayName: "Lee", SignIns: []c4v1.PartySignIn{{Subject: "noor@studio.example"}}, Made: c4v1.PartyAct{By: "ines", At: metav1.Now()}}}); err != nil {
		t.Fatal(err)
	}
	if d := p.s.decider(r, noorSSO); MayControl("decide", "party:noor", d) == nil || MayControl("decide", "party:lee", d) == nil {
		t.Errorf("a sign-in in two people's Parties decides for one: %+v", d)
	}
	p.seal(t)
}

// P1 of the O1 review: a user name is no hidden sign-in. A viewer signed
// in as dana, though dana's Party lists only dana@studio.example and her
// provider subject, is not dana: holds none of her purposes, and can't
// rewrite her sign-ins or drop her provider subject (R1).
func TestPartyNameIsNoSignIn(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	p.secret()
	p.own(t, "dana-sso", "dana", sso+" "+peopletest.DanaSub)
	_, v := p.viewer("dana")
	if st := p.s.standingOf(ctx, v); st.person != "" || st.holds("dana", PurposeAdmin) {
		t.Errorf("viewer dana is Party dana: %q", st.person)
	}
	code, out := p.call(t, http.MethodPut, "/api/v1/parties/dana/sign-ins", "dana", map[string]any{"signIns": ins("dana")}, nil)
	if code != http.StatusForbidden {
		t.Errorf("viewer dana rewrites dana's sign-ins: %d %v", code, out)
	}
	ps, _ := p.s.parties(ctx)
	if got := findParty(ps, "dana").Spec.SignIns; len(got) != 2 {
		t.Errorf("dana's sign-ins now: %+v", got)
	}
}

// P2 of the O1 review: an admin can't grant herself purposes under a
// second name. A membership names a Party, never a bare sign-in name, so
// her sign-in's name is refused as a member; and a purpose she holds only
// by a group is never copied into her named membership (R3).
func TestPartySelfGrantByAliasRefused(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	p.secret()
	p.own(t, "noor-sso", "noor", sso+" "+peopletest.NoorSub)
	p.people(t, "ines", "lee", "sam")
	p.org(t, "kestrel", "noor", "noor-sso")
	code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "noor-sso", map[string]any{"person": "noor@studio.example", "purposes": []string{"member", "approve-spend", "admin"}}, nil)
	if code == http.StatusOK {
		t.Errorf("noor adds a member under her sign-in name: %d %v", code, out)
	}
	_, alias := p.viewer("noor-sso")
	if p.s.holdsFor(ctx, alias, "kestrel", PurposeApproveSpend) {
		t.Error("noor holds approve-spend under a second name")
	}
	// a purpose held only by a group: lighting-sups holds approve-spend, and
	// sam, in it, an admin by name, can't copy it into his own membership
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "noor-sso", map[string]any{"person": "sam", "purposes": []string{"member", "admin"}}, nil); code != http.StatusOK {
		t.Fatalf("sam an admin: %d %v", code, out)
	}
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "noor-sso", map[string]any{"group": "lighting-sups", "purposes": []string{"member", "approve-spend"}}, nil); code != http.StatusOK {
		t.Fatalf("lighting-sups: %d %v", code, out)
	}
	if code, out := p.call(t, http.MethodPut, "/api/v1/parties/kestrel/members", "sam", map[string]any{"person": "sam", "purposes": []string{"member", "admin", "approve-spend"}}, nil); code != http.StatusForbidden || !says(out, "never grants for themselves") {
		t.Errorf("sam copies his group's approve-spend into his name: %d %v", code, out)
	}
	if code, out := p.call(t, http.MethodPut, "/api/v1/parties/kestrel/members", "sam", map[string]any{"group": "lighting-sups", "purposes": []string{"member", "approve-spend", "approve-egress"}}, nil); code != http.StatusForbidden || !says(out, "a group they are in") {
		t.Errorf("sam grows his own group: %d %v", code, out)
	}
}

// barrierStore makes the first two Gets wait for each other once armed.
type barrierStore struct {
	farmbook.Store
	armed   atomic.Bool
	n       atomic.Int32
	release chan struct{}
	once    sync.Once
}

func (b *barrierStore) Get(ctx context.Context, id string) ([]byte, error) {
	if b.armed.Load() {
		if b.n.Add(1) >= 2 {
			b.once.Do(func() { close(b.release) })
		}
		select {
		case <-b.release:
		case <-time.After(3 * time.Second):
		}
	}
	return b.Store.Get(ctx, id)
}

// P3 of the O1 review: two admins ending each other at once leave one.
// The last-admin check runs inside the book's append, on the book as it
// stands, so the second is judged after the first and refused (R5).
func TestPartyLastAdminRaceRefused(t *testing.T) {
	p := newPartyRig(t)
	p.people(t)
	p.org(t, "kestrel", "noor", "noor")
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "noor", map[string]any{"person": "lee", "purposes": []string{"member", "admin"}}, nil); code != http.StatusOK {
		t.Fatalf("add lee admin: %d %v", code, out)
	}
	bs := &barrierStore{Store: p.store, release: make(chan struct{})}
	p.s.FarmBook = &farmbook.Writer{Store: bs, Pointer: p.ptr}
	bs.armed.Store(true)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, c := range []struct{ as, whom string }{{"noor", "lee"}, {"lee", "noor"}} {
		wg.Add(1)
		go func(i int, as, whom string) {
			defer wg.Done()
			codes[i], _ = p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members/end", as, map[string]any{"person": whom}, nil)
		}(i, c.as, c.whom)
	}
	wg.Wait()
	bs.armed.Store(false)
	pv := p.party(t, "kestrel", "maya")
	admins := 0
	for _, m := range pv.Members {
		if slices.Contains(m.Purposes, PurposeAdmin) {
			admins++
		}
	}
	slices.Sort(codes)
	if admins != 1 || codes[0] != http.StatusOK || codes[1] == http.StatusOK {
		t.Errorf("two admins end each other: codes %v, %d admins left, members %+v", codes, admins, members(pv))
	}
	p.seal(t)
}

// P4 of the O1 review: a Party person's own signed act is taken when it
// is sent by a sign-in their Party lists, not only by their Party's name
// (R7); nobody else's is.
func TestPartyPersonsOwnActTaken(t *testing.T) {
	p := newPartyRig(t)
	p.secret()
	p.own(t, "noor-sso", "noor", sso+" "+peopletest.NoorSub)
	on := p.party(t, "noor", "noor-sso").ActOn
	body := map[string]any{"signIns": ins()}
	act := p.g.act(t, p.f.Person(t, "dana", true), on, "set-sign-ins", body, true)
	if code, _ := p.call(t, http.MethodPut, "/api/v1/parties/noor/sign-ins", "noor-sso", body, map[string]string{ActHeader: act}); code != http.StatusForbidden {
		t.Errorf("dana's act sent as noor: %d", code)
	}
	act = p.g.act(t, p.f.Person(t, "noor", true), on, "set-sign-ins", body, true)
	if code, out := p.call(t, http.MethodPut, "/api/v1/parties/noor/sign-ins", "noor-sso", body, map[string]string{ActHeader: act}); code != http.StatusOK {
		t.Errorf("noor's own act, sent as noor@studio.example: %d %v", code, out)
	}
}

// P5 of the O1 review, the HUD's half: a person who signs in by the
// c4-sign-ins Secret makes their own Party only carrying one of its
// subjects, proved, since the Secret's lines for them are ignored once it
// exists: so their Party never locks them out of the people's issuer
// (R2, R6).
func TestPartyLeavingTheSecretKeepsTheSubject(t *testing.T) {
	p := newPartyRig(t)
	p.s.StandInSignIns = people.StaticSignIns{"noor": {{Issuer: sso, Subject: peopletest.NoorSub}}}
	body := map[string]any{"name": "noor", "kind": "person", "displayName": "Noor", "signIns": ins("noor")}
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "noor", body, nil); code != http.StatusForbidden || !says(out, "signs in by the c4-sign-ins Secret") {
		t.Errorf("noor leaves the Secret with no provider subject: %d %v", code, out)
	}
	body["signIns"], body["idToken"] = ins("noor", sso+" "+peopletest.NoorSub), p.token(peopletest.NoorSub)
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "noor", body, nil); code != http.StatusOK {
		t.Errorf("noor leaves the Secret with her subject, proved: %d %v", code, out)
	}
}

// One subject stays one person's across the Parties and the c4-sign-ins
// Secret: a provider subject the Secret gives someone with no person Party
// of their own is refused on a Party's write, while the same person's own
// Party, or a subject of someone who has a Party now, is not.
func TestPartySignInsAgainstTheSecret(t *testing.T) {
	p := newPartyRig(t)
	p.s.StandInSignIns = people.StaticSignIns{"dana": {{Issuer: sso, Subject: peopletest.DanaSub}}, "kai": {{Issuer: sso, Subject: peopletest.NoorSub}}}
	mk := func(as, name, sub string) (int, map[string]any) {
		body := map[string]any{"name": name, "kind": "person", "displayName": name, "signIns": ins(p.s.DevViewers[as].User, sso+" "+sub), "idToken": p.token(sub)}
		return p.call(t, http.MethodPost, "/api/v1/parties", as, body, nil)
	}
	if code, out := mk("noor", "noor", peopletest.NoorSub); code != http.StatusConflict || !says(out, "is kai's in the c4-sign-ins Secret") {
		t.Errorf("noor takes a subject the Secret gives kai: %d %v", code, out)
	}
	if code, out := mk("dana", "dana", peopletest.DanaSub); code != http.StatusOK {
		t.Errorf("dana's own Party takes her Secret subject: %d %v", code, out)
	}
	// nor added to a Party later
	p.own(t, "lee", "lee")
	if code, out := p.call(t, http.MethodPut, "/api/v1/parties/lee/sign-ins", "lee", map[string]any{"signIns": ins("lee", sso+" "+peopletest.NoorSub), "idToken": p.token(peopletest.NoorSub)}, nil); code != http.StatusConflict || !says(out, "is kai's in the c4-sign-ins Secret") {
		t.Errorf("lee adds a subject the Secret gives kai: %d %v", code, out)
	}
	// kai has a Party now, so the Secret's line for kai signs in no one; kai
	// leaves the Secret by proving another subject of theirs
	p.s.DevViewers["kai"] = &api.Viewer{User: "kai", Role: "artist", All: true}
	p.s.StandInSignIns = people.StaticSignIns{"dana": {{Issuer: sso, Subject: peopletest.DanaSub}}, "kai": {{Issuer: sso, Subject: peopletest.NoorSub}, {Issuer: sso, Subject: "00u-kai"}}}
	p.own(t, "kai", "kai", sso+" 00u-kai")
	if code, out := mk("noor", "noor", peopletest.NoorSub); code != http.StatusOK {
		t.Errorf("noor takes a subject only kai's ignored Secret line names: %d %v", code, out)
	}
}

// A named membership whose name is no Party, as one booked before
// memberships named Parties, lapses visibly: shown, holding nothing (R3).
func TestPartyMembershipByNameLapses(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	p.people(t)
	p.org(t, "kestrel", "noor", "noor")
	// a membership of "kai", booked as an earlier HUD would have
	rec, err := provenance.SignStatement(ctx, p.s.Signer, provenance.PredicateControl,
		[]provenance.Subject{{Name: farmbook.MemberKey("kestrel", "kai"), Digest: map[string]string{"sha256": "00"}}},
		controlPredicate{Actor: "noor", Action: "add-member", Target: farmbook.MemberKey("kestrel", "kai"),
			Change: map[string]any{"party": "kestrel", "member": "kai", "purposes": []string{"member", "admin"}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id := c4.Identify(bytes.NewReader(rec)).String()
	if err := p.store.Put(ctx, id, rec); err != nil {
		t.Fatal(err)
	}
	if err := p.s.bookChecked(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	var kai *api.Member
	for _, m := range p.party(t, "kestrel", "maya").Members {
		if m.Person == "kai" {
			kai = &m
		}
	}
	if kai == nil || kai.Lapsed == "" || len(kai.InEffect) != 0 {
		t.Fatalf("kai's membership by a bare name: %+v", kai)
	}
	// whoever signs in as kai holds nothing by it
	p.s.DevViewers["kai"] = &api.Viewer{User: "kai", Role: "artist", All: true}
	_, v := p.viewer("kai")
	if p.s.holdsFor(ctx, v, "kestrel", PurposeMember) {
		t.Error("a bare name's membership holds for the viewer of that name")
	}
	// the last-admin check does not count a lapsed admin
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members/end", "noor", map[string]any{"person": "noor"}, nil); code != http.StatusConflict {
		t.Errorf("noor leaves with only a lapsed admin left: %d %v", code, out)
	}
}

// A party write may carry its signed act, and its record then cites it:
// the act on the party, of the write's own verb, with its body.
func TestPartyActsCited(t *testing.T) {
	p := newPartyRig(t)
	p.people(t)
	p.org(t, "kestrel", "noor", "noor")
	on := p.party(t, "kestrel", "noor").ActOn
	noor := p.f.Person(t, "noor", true)
	body := map[string]any{"person": "dana", "purposes": []string{"member"}}
	send := func(verb string, confirmed bool) (int, map[string]any) {
		act := p.g.act(t, noor, on, verb, body, confirmed)
		return p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "noor", body, map[string]string{ActHeader: act})
	}
	if code, out := send("end-member", true); code != http.StatusForbidden {
		t.Errorf("an act of another verb: %d %v", code, out)
	}
	code, out := send("add-member", true)
	rec, _ := out["record"].(string)
	if code != http.StatusOK || rec == "" {
		t.Fatalf("add dana with its act: %d %v", code, out)
	}
	pred := p.g.predicate(t, "show-a", rec)
	cites(t, "add-member", code, out, pred, on, CustodyConfirmed)
	p.seal(t)
}

// A tracker login the people map names as a party decides that party's
// wait while the farm keeps no Party of the name; once it does, the login
// names no member, and is refused.
func TestPartyRelayNeedsAMember(t *testing.T) {
	s := farm(t)
	ctx := context.Background()
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	s.Stores = func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.DevViewers[c4v1.SyncDevViewer] = &api.Viewer{User: c4v1.SyncDevViewer, Projects: []string{"show-a"}}
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Labels = map[string]string{c4v1.LabelTracker: "github"}
	since := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.Waits = []c4v1.WorkWait{{Unit: 8, Name: "registry", Server: "party:ml", Since: &since, Blocking: true}}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	tr := &c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "github"},
		Spec: c4v1.TrackerSpec{Kind: "github", People: map[string]string{"mlflow-registry": "party:ml"}}}
	ml := &c4v1.Party{ObjectMeta: metav1.ObjectMeta{Name: "ml"}, Spec: c4v1.PartySpec{Kind: c4v1.PartyOrganisation, DisplayName: "ML",
		Made: c4v1.PartyAct{By: "ines", At: metav1.Now()}}}
	post := func() (int, map[string]any) {
		raw, _ := json.Marshal(map[string]any{"unit": "registry", "do": "approve",
			"via": map[string]any{"tracker": "github", "source": "models:/widgets@champion", "by": "mlflow-registry", "person": "party:ml"}})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/decide?as="+c4v1.SyncDevViewer, bytes.NewReader(raw)))
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy(), tr).Build()
	if code, out := post(); code != http.StatusOK {
		t.Fatalf("relayed for a party the farm doesn't keep: %d %v", code, out)
	}
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy(), tr, ml).Build()
	if code, out := post(); code != http.StatusForbidden || !says(out, "a Party on this farm") {
		t.Errorf("relayed for a kept Party: %d %v", code, out)
	}
}

// The act needs of a Party's verbs: making a party, accepting as its
// first admin, adding a member and changing purposes or sign-ins give
// authority, so need a touch; ending a membership and changing a party's
// names are a person's, silent.
func TestPartyActNeeds(t *testing.T) {
	for verb, want := range map[string]provenance.ActNeed{
		"make-party": {Person: true, Touch: true}, "accept-admin": {Person: true, Touch: true}, "add-member": {Person: true, Touch: true},
		"set-purposes": {Person: true, Touch: true}, "set-sign-ins": {Person: true, Touch: true},
		"end-member": {Person: true}, "change-party": {Person: true},
	} {
		if got := provenance.ActNeeds(verb, nil, time.Now()); got != want {
			t.Errorf("%s needs %+v, want %+v", verb, got, want)
		}
		if !farmbook.IsControlAct(verb) {
			t.Errorf("the farm book does not take %s", verb)
		}
	}
}

// A membership is read from the record that opened it, checked: an entry
// citing a record the HUD didn't sign, or a record of another act, gives
// no one anything, though the entry waits pending where any writer of the
// system store could put it.
func TestPartyMembershipRecordsChecked(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	p.people(t)
	p.org(t, "kestrel", "noor", "noor")
	_, maya := p.viewer("maya")
	key := farmbook.MemberKey("kestrel", "maya")
	forge := func(signer *provenance.Signer) {
		t.Helper()
		pred := controlPredicate{Actor: "ines", Action: "add-member", Target: key,
			Change: map[string]any{"party": "kestrel", "member": "maya", "purposes": []string{"member", "admin"}}}
		rec, err := provenance.SignStatement(ctx, signer, provenance.PredicateControl, []provenance.Subject{{Name: key, Digest: map[string]string{"sha256": "00"}}}, pred, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		id := c4.Identify(bytes.NewReader(rec)).String()
		if err := p.store.Put(ctx, id, rec); err != nil {
			t.Fatal(err)
		}
		e, err := farmbook.ControlEntry(rec, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.s.FarmBook.Append(ctx, farmbook.Submission{Entry: e}); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := provenance.NewSigner(ctx, provenance.LocalIssuer(p.f.CA, provenance.ComponentIdentity("queue"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	forge(queue)
	if p.s.holdsFor(ctx, maya, "kestrel", PurposeAdmin) {
		t.Error("a pending entry citing the queue's record makes maya an admin")
	}
	// The HUD's own accept-admin record, cited by an entry that says it
	// adds maya: the record is the HUD's, of another act.
	acc := p.party(t, "kestrel", "maya").FirstAdmin.Record
	raw, err := p.store.Get(ctx, acc)
	if err != nil {
		t.Fatal(err)
	}
	e, err := farmbook.ControlEntry(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.Act, e.Subject, e.Person = "add-member", key, "maya"
	if _, err := p.s.FarmBook.Append(ctx, farmbook.Submission{Entry: e}); err != nil {
		t.Fatal(err)
	}
	if p.s.holdsFor(ctx, maya, "kestrel", PurposeAdmin) {
		t.Error("an entry citing noor's accept-admin record as maya's add-member makes her an admin")
	}
}

// When the farm's Parties cannot be read, every party may be one the farm
// keeps, so the c4:party: group decides for none.
func TestPartyWaitsWhenPartiesUnreadable(t *testing.T) {
	p := newPartyRig(t)
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	p.s.Client = fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*c4v1.PartyList); ok {
				return errors.New("the API server is away")
			}
			return c.List(ctx, list, opts...)
		}}).Build()
	r, kim := p.viewer("kim")
	if err := MayControl("decide", "party:kestrel", p.s.decider(r, kim)); err == nil {
		t.Error("the c4:party: group decides while the farm's Parties can't be read")
	}
}

// One spelling for each thing: a party's name has one case, and so does a
// wait's party; an issuer URL is kept and compared in its one spelling.
func TestPartySpelledOneWay(t *testing.T) {
	p := newPartyRig(t)
	upper := kestrel()
	upper["name"] = "Kestrel"
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", upper, nil); code != http.StatusBadRequest {
		t.Errorf("a party named Kestrel: %d %v", code, out)
	}
	if err := MayControl("decide", "party:Kestrel", Decider{User: "kim", Parties: []string{"Kestrel"}}); err == nil {
		t.Error("a party:Kestrel wait is decided")
	}
	got, err := validSignIns(ins("HTTPS://SSO.Studio.Example/ 00u-x"))
	if err != nil || len(got) != 1 || got[0].Issuer != "https://sso.studio.example" {
		t.Errorf("an issuer kept as written: %v %v", got, err)
	}
	if _, err := validSignIns(ins("ftp://sso.studio.example 00u-x")); err == nil {
		t.Error("an issuer that is no http(s) URL")
	}
	// two spellings of one subject are one person's
	p.own(t, "noor", "noor", sso+" "+peopletest.NoorSub)
	body := map[string]any{"name": "lee", "kind": "person", "displayName": "Lee", "signIns": ins("lee", "HTTPS://SSO.Studio.Example/ "+peopletest.NoorSub), "idToken": p.token(peopletest.NoorSub)}
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "lee", body, nil); code != http.StatusConflict {
		t.Errorf("noor's subject under another spelling: %d %v", code, out)
	}
}

// A farm without a farm book has no Parties: GET and HEAD /parties and GET
// /parties/{name} answer 501, as the writes do, so the HUD shows no tab;
// with a book they answer.
func TestPartyReadsNeedTheBook(t *testing.T) {
	p := newPartyRig(t)
	p.own(t, "maya", "maya")
	read := func(method, path string) int {
		r := httptest.NewRequest(method, path+"?as=maya", nil)
		r.Header.Set(InstallerHeader, testInstallerToken)
		w := httptest.NewRecorder()
		p.s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct{ method, path string }{{http.MethodGet, "/api/v1/parties"}, {http.MethodHead, "/api/v1/parties"}, {http.MethodGet, "/api/v1/parties/maya"}} {
		if code := read(c.method, c.path); code != http.StatusOK {
			t.Errorf("%s %s with a book: %d", c.method, c.path, code)
		}
	}
	p.s.FarmBook, p.s.FarmBookReader = nil, nil
	for _, c := range []struct{ method, path string }{{http.MethodGet, "/api/v1/parties"}, {http.MethodHead, "/api/v1/parties"}, {http.MethodGet, "/api/v1/parties/maya"}} {
		if code := read(c.method, c.path); code != http.StatusNotImplemented {
			t.Errorf("%s %s without a book: %d, want 501", c.method, c.path, code)
		}
	}
}

// The viewer says what it may do about the party list, by the rules the
// routes take: makeParties is exactly whether POST /parties takes an
// organisation from it; makeOwnParty that it has no Party yet, and makes
// one; bindingOwner names the farm binding's owner, through the owner's
// Party once it exists.
func TestPartyViewerRights(t *testing.T) {
	p := newPartyRig(t)
	p.bind(t, "noor", "")
	p.own(t, "dana", "dana")
	p.secret()
	viewer := func(as string, headers map[string]string) api.Viewer {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?as="+as, nil)
		r.Header.Set(InstallerHeader, testInstallerToken)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		p.s.Handler().ServeHTTP(w, r)
		var snap api.Snapshot
		if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil || w.Code != http.StatusOK || snap.Viewer == nil {
			t.Fatalf("snapshot as %s: %d %s", as, w.Code, w.Body)
		}
		return *snap.Viewer
	}
	for i, c := range []struct {
		as      string
		headers map[string]string
	}{{"ines", nil}, {"maya", nil}, {"noor", nil}, {"ines", map[string]string{"X-C4-Client": "mcp"}}} {
		v := viewer(c.as, c.headers)
		code, _ := p.call(t, http.MethodPost, "/api/v1/parties", c.as, map[string]any{"name": "probe-" + strconv.Itoa(i), "kind": "organisation", "displayName": "Probe", "firstAdmin": "dana"}, c.headers)
		if v.MakeParties != (code == http.StatusOK) {
			t.Errorf("%s %v: makeParties %v, and POST /parties answers %d", c.as, c.headers, v.MakeParties, code)
		}
	}
	// before the owner's Party, no one is its person, by name or otherwise
	if v := viewer("noor", nil); v.MakeParties || v.BindingOwner || !v.MakeOwnParty {
		t.Errorf("viewer noor, the binding's owner's name, before her Party: %+v", v)
	}
	if v := viewer("dana", nil); v.MakeOwnParty || v.BindingOwner {
		t.Errorf("dana, with her Party: %+v", v)
	}
	if v := viewer("maya", nil); v.MakeParties || v.BindingOwner || !v.MakeOwnParty {
		t.Errorf("maya: %+v", v)
	}
	// the owner's Party is made by its person signed in otherwise:
	// bindingOwner now follows its sign-ins, not the user name
	p.own(t, "noor-sso", "noor", sso+" "+peopletest.NoorSub)
	if v := viewer("noor", nil); v.BindingOwner {
		t.Errorf("viewer noor, not among the owner's Party's sign-ins: %+v", v)
	}
	if v := viewer("noor-sso", nil); !v.BindingOwner || v.MakeOwnParty {
		t.Errorf("noor@studio.example, the owner's Party's person: %+v", v)
	}
	p.s.FarmBook, p.s.FarmBookReader = nil, nil
	if v := viewer("ines", nil); v.MakeParties || v.MakeOwnParty {
		t.Errorf("rights on a farm with no book: %+v", v)
	}
}

// Two admins changing one membership at once: the second is judged on the
// book after the first, and refused, since the membership it was signed
// to follow is no longer the one in force (R5).
func TestPartyMembershipChangedWhileActing(t *testing.T) {
	p := newPartyRig(t)
	p.people(t)
	p.org(t, "kestrel", "noor", "noor")
	for _, body := range []map[string]any{{"person": "lee", "purposes": []string{"member", "admin"}}, {"person": "dana", "purposes": []string{"member"}}} {
		if code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "noor", body, nil); code != http.StatusOK {
			t.Fatalf("%v: %d %v", body, code, out)
		}
	}
	bs := &barrierStore{Store: p.store, release: make(chan struct{})}
	p.s.FarmBook = &farmbook.Writer{Store: bs, Pointer: p.ptr}
	bs.armed.Store(true)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, c := range []struct {
		as       string
		purposes []string
	}{{"noor", []string{"member", "view-spend"}}, {"lee", []string{"member", "approve-spend"}}} {
		wg.Add(1)
		go func(i int, as string, purposes []string) {
			defer wg.Done()
			codes[i], _ = p.call(t, http.MethodPut, "/api/v1/parties/kestrel/members", as, map[string]any{"person": "dana", "purposes": purposes}, nil)
		}(i, c.as, c.purposes)
	}
	wg.Wait()
	bs.armed.Store(false)
	slices.Sort(codes)
	if codes[0] != http.StatusOK || codes[1] != http.StatusConflict {
		t.Errorf("two changes of dana's membership at once: %v, want one 200 and one 409", codes)
	}
	p.seal(t)
}

// What is in force is kept per view of the book, never across views: a
// membership read as none at one view is read again at the next.
func TestPartyMemoFollowsTheBook(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	key := farmbook.MemberKey("kestrel", "noor")
	before, err := p.s.partyBook(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// the first view this HUD reads anything at
	if p.s.inForce(before, farmbook.ForceMembership, key) != nil {
		t.Fatal("noor is a member before kestrel is made")
	}
	p.people(t, "ines", "noor")
	p.org(t, "kestrel", "noor", "noor")
	after, err := p.s.partyBook(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.s.inForce(after, farmbook.ForceMembership, key) == nil {
		t.Error("noor's acceptance is not read at the next view of the book")
	}
}

// A person Party is made only by its person, under their own name: the
// one they sign in as, or the one the c4-sign-ins Secret gives a subject
// they prove. So mallory makes no Party named dana, nobody makes one for
// someone else, and while the Secret names dana, a Party named dana takes
// a subject the Secret gives her, proved, from whoever makes it.
func TestPartyIsMadeByItsPerson(t *testing.T) {
	p := newPartyRig(t)
	mk := func(as, name string, signIns []api.SignIn, token string) (int, map[string]any) {
		body := map[string]any{"name": name, "kind": "person", "displayName": name, "signIns": signIns}
		if token != "" {
			body["idToken"] = token
		}
		return p.call(t, http.MethodPost, "/api/v1/parties", as, body, nil)
	}
	if code, out := mk("mallory", "dana", ins("mallory"), ""); code != http.StatusForbidden || !says(out, "takes the name they sign in as") {
		t.Errorf("mallory makes dana: %d %v", code, out)
	}
	if code, out := mk("ines", "dana", ins("dana"), ""); code != http.StatusForbidden {
		t.Errorf("a farm admin makes dana's Party: %d %v", code, out)
	}
	p.secret()
	// dana's HUD name, under a subject the Secret doesn't give her
	if code, out := mk("dana", "dana", ins("dana", sso+" 00u-other"), p.token("00u-other")); code != http.StatusForbidden || !says(out, "signs in by the c4-sign-ins Secret") {
		t.Errorf("dana's Party on a subject the Secret doesn't give her: %d %v", code, out)
	}
	if code, out := mk("mallory", "dana", ins("mallory", sso+" 00u-mallory"), p.token("00u-mallory")); code != http.StatusForbidden {
		t.Errorf("mallory makes dana, proving her own subject: %d %v", code, out)
	}
	if code, out := mk("dana-sso", "dana", ins("dana@studio.example", sso+" "+peopletest.DanaSub), p.token(peopletest.DanaSub)); code != http.StatusOK {
		t.Errorf("dana makes her Party, proving the subject the Secret gives her: %d %v", code, out)
	}
	// once, and one subject is one person's
	if code, out := mk("dana", "dana2", ins("dana", sso+" "+peopletest.DanaSub), p.token(peopletest.DanaSub)); code != http.StatusConflict {
		t.Errorf("a second Party on dana's subject: %d %v", code, out)
	}
}

// On a farm its owner runs, a farm admin may name themselves an
// organisation's first admin, a small studio's one admin; they count only
// once they accept with their own touched act, booked, and the Party says
// who named whom and when they accepted. On a managed farm they may not
// (TestPartiesAndMembers).
func TestPartySelfNamedOnASelfRunFarm(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	p.bind(t, "kestrel", "")
	p.people(t, "ines", "noor")
	body := kestrel()
	body["firstAdmin"] = "ines"
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", body, nil); code != http.StatusOK {
		t.Fatalf("ines names herself on a self-run farm: %d %v", code, out)
	}
	p.uid(t, "kestrel")
	_, ines := p.viewer("ines")
	if p.s.holdsFor(ctx, ines, "kestrel", PurposeAdmin) {
		t.Fatal("ines is kestrel's admin before she accepts")
	}
	if fa := p.party(t, "kestrel", "noor").FirstAdmin; fa == nil || !fa.SelfNamed || fa.Accepted {
		t.Errorf("kestrel's first admin before she accepts: %+v", fa)
	}
	if code, out := p.accept(t, "kestrel", "ines", "ines", true); code != http.StatusOK {
		t.Fatalf("ines accepts: %d %v", code, out)
	}
	fa := p.party(t, "kestrel", "noor").FirstAdmin
	if fa == nil || !fa.SelfNamed || !fa.Accepted || fa.At.IsZero() || fa.Record == "" {
		t.Errorf("kestrel's first admin after she accepts: %+v", fa)
	}
	if !p.s.holdsFor(ctx, ines, "kestrel", PurposeAdmin) {
		t.Error("ines is no admin after accepting")
	}
	p.seal(t)
}
