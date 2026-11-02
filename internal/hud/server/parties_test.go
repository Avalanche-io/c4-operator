package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// partyRig is a HUD guarded by an installer token that signs under a
// scratch farm's CA, keeps a farm book the queue seals, and trusts the
// farm's people's keys.
type partyRig struct {
	s      *Server
	f      *provtest.Farm
	g      *actRig
	store  *farmbook.MemoryStore
	ptr    *farmbook.MemoryPointer
	sealer *farmbook.Sealer
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
	for name, v := range map[string]*api.Viewer{
		"ines": {User: "ines", Role: "farm-admin", All: true},
		"dana": {User: "dana", Role: "artist", All: true},
		"noor": {User: "noor", Role: "artist", All: true},
		"maya": {User: "maya", Role: "artist", All: true},
		// the studio's c4:party:kestrel group, as a sign-in reports it
		"kim": {User: "kim", Role: "artist", All: true, Parties: []string{"kestrel"}, Groups: []string{"c4:party:kestrel"}},
		"sam": {User: "sam", Role: "artist", All: true, Groups: []string{"lighting-sups"}},
		// noor, by the studio's sign-in
		"noor-sso": {User: "noor@studio.example", Role: "artist", All: true},
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

// A farm admin makes an organisation and is its first admin; its admins
// add, change and end members, by name and by group, each a signed
// record the queue books; an admin never grants for themselves, the last
// admin stays, a name is never reused, and view-people waits for the
// organisation's policy.
func TestPartiesAndMembers(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "maya", kestrel(), nil); code != http.StatusForbidden || !says(out, "a farm admin keeps the party list") {
		t.Fatalf("an artist makes a party: %d %v", code, out)
	}
	code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), nil)
	if code != http.StatusOK {
		t.Fatalf("make kestrel: %d %v", code, out)
	}
	if st, _ := p.ptr.Read(ctx); len(st.Pending) != 1 {
		t.Fatalf("the make-party is not booked: %v", st.Pending)
	}
	pv := p.party(t, "kestrel", "maya")
	if !pv.Booked || pv.DisplayName != "Kestrel Productions" || pv.Made.By != "ines" ||
		!slices.Equal(members(pv)["ines"], []string{PurposeMember, PurposeAdmin}) {
		t.Fatalf("kestrel: %+v", pv)
	}
	if !slices.Equal(members(pv)["group:c4:party:kestrel"], []string{PurposeMember}) {
		t.Errorf("the studio's c4:party:kestrel group is not shown holding member: %+v", pv.Groups)
	}
	p.seal(t)

	// a name is never reused; the same make again is an answer
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), nil); code != http.StatusOK || out["existing"] != true {
		t.Errorf("make kestrel again: %d %v", code, out)
	}
	other := kestrel()
	other["displayName"] = "Kestrel Films"
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", other, nil); code != http.StatusConflict || !says(out, "never reused") {
		t.Errorf("reuse kestrel: %d %v", code, out)
	}

	members := func(method, path, as string, body map[string]any) (int, map[string]any) {
		return p.call(t, method, "/api/v1/parties/kestrel/members"+path, as, body, nil)
	}
	if code, out := members(http.MethodPost, "", "dana", map[string]any{"person": "maya", "purposes": []string{"member"}}); code != http.StatusForbidden || !says(out, "not an admin") {
		t.Errorf("a non-member adds one: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "ines", map[string]any{"person": "dana", "purposes": []string{"member", "approve-spend"}}); code != http.StatusOK {
		t.Fatalf("add dana: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "ines", map[string]any{"person": "dana", "purposes": []string{"member"}}); code != http.StatusConflict {
		t.Errorf("add dana twice: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "dana", map[string]any{"person": "maya", "purposes": []string{"member"}}); code != http.StatusForbidden {
		t.Errorf("a member who is no admin adds one: %d %v", code, out)
	}
	// Admins revoke, never grant for themselves; the last admin stays.
	if code, out := members(http.MethodPut, "", "ines", map[string]any{"person": "ines", "purposes": []string{"member", "admin", "approve-spend"}}); code != http.StatusForbidden || !says(out, "never grants for themselves") {
		t.Errorf("ines grants herself: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "/end", "ines", map[string]any{"person": "ines"}); code != http.StatusConflict || !says(out, "last admin") {
		t.Errorf("the last admin leaves: %d %v", code, out)
	}
	if code, out := members(http.MethodPut, "", "ines", map[string]any{"person": "ines", "purposes": []string{"member"}}); code != http.StatusConflict || !says(out, "last admin") {
		t.Errorf("the last admin drops admin: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "ines", map[string]any{"person": "noor", "purposes": []string{"member", "admin"}}); code != http.StatusOK {
		t.Fatalf("add noor as an admin: %d %v", code, out)
	}
	if code, out := members(http.MethodPut, "", "noor", map[string]any{"person": "ines", "purposes": []string{"member", "admin", "approve-spend"}}); code != http.StatusOK {
		t.Errorf("another admin grants ines: %d %v", code, out)
	}
	// a member leaves on their own; an admin may then leave, another staying
	if code, out := members(http.MethodPost, "/end", "dana", map[string]any{"person": "dana"}); code != http.StatusOK {
		t.Errorf("dana leaves: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "/end", "dana", map[string]any{"person": "dana"}); code != http.StatusNotFound {
		t.Errorf("dana leaves twice: %d %v", code, out)
	}
	// by the studio's own groups
	if code, out := members(http.MethodPost, "", "ines", map[string]any{"group": "c4:party:kestrel", "purposes": []string{"member", "admin"}}); code != http.StatusBadRequest || !says(out, "member and nothing more") {
		t.Errorf("more for the c4:party: group: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "ines", map[string]any{"group": "lighting-sups", "purposes": []string{"member", "approve-spend"}}); code != http.StatusOK {
		t.Fatalf("lighting-sups: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "ines", map[string]any{"person": "maya", "group": "lighting-sups", "purposes": []string{"member"}}); code != http.StatusBadRequest {
		t.Errorf("a person and a group at once: %d %v", code, out)
	}
	if code, out := members(http.MethodPost, "", "ines", map[string]any{"person": "maya", "purposes": []string{"own"}}); code != http.StatusBadRequest {
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
	want := map[string][]string{"ines": {"member", "admin", "approve-spend"}, "noor": {"member", "admin"},
		"group:lighting-sups": {"member", "approve-spend"}, "group:c4:party:kestrel": {"member"}}
	if len(got) != len(want) {
		t.Errorf("kestrel's members: %v, want %v", got, want)
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
	if code, out := members(http.MethodPost, "", "ines", map[string]any{"person": "maya", "purposes": []string{"view-spend", "view-people"}}); code != http.StatusOK {
		t.Fatalf("maya views: %d %v", code, out)
	}
	_, maya := p.viewer("maya")
	if !p.s.holdsFor(ctx, maya, "kestrel", PurposeViewSpend) || p.s.holdsFor(ctx, maya, "kestrel", PurposeViewPeople) {
		t.Error("view-people holds before the organisation's policy turns per-person views on")
	}
	if code, out := p.call(t, http.MethodPatch, "/api/v1/parties/kestrel", "maya", map[string]any{"perPersonViews": true}, nil); code != http.StatusForbidden {
		t.Errorf("a non-admin sets kestrel's policy: %d %v", code, out)
	}
	if code, out := p.call(t, http.MethodPatch, "/api/v1/parties/kestrel", "noor", map[string]any{"perPersonViews": true}, nil); code != http.StatusOK {
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

// Recommendations 1 and 3 each flip at one switch: with MembersByGroup off
// no group holds a purpose, the c4:party: group included; with
// ViewPeopleNeedsPolicy off, view-people holds once granted.
func TestPartyRecommendationsFlip(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), nil); code != http.StatusOK {
		t.Fatalf("make kestrel: %d %v", code, out)
	}
	add := func(body map[string]any) {
		t.Helper()
		if code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "ines", body, nil); code != http.StatusOK {
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
	if code, _ := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "ines", map[string]any{"group": "comp-sups", "purposes": []string{"member"}}, nil); code != http.StatusBadRequest {
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
	p.s.FarmBook, p.s.FarmBookReader = nil, nil
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), nil); code != http.StatusNotImplemented || !says(out, "no farm book") {
		t.Errorf("no farm book: %d %v", code, out)
	}
}

// For a membership the book is the state: a write the book does not take
// changes nothing.
func TestPartyMembershipNeedsTheBook(t *testing.T) {
	p := newPartyRig(t)
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), nil); code != http.StatusOK {
		t.Fatalf("make kestrel: %d %v", code, out)
	}
	p.s.FarmBook = &farmbook.Writer{Store: p.store, Pointer: failingPointer{p.ptr}}
	code, out := p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "ines", map[string]any{"person": "dana", "purposes": []string{"member"}}, nil)
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
// name or by a group, and the record says under which membership.
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
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), nil); code != http.StatusOK {
		t.Fatalf("make kestrel: %d %v", code, out)
	}
	add := func(method string, body map[string]any) {
		t.Helper()
		if code, out := p.call(t, method, "/api/v1/parties/kestrel/members", "ines", body, nil); code != http.StatusOK {
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
	// and the wait is on her page of what needs her, and not on maya's
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

// A person's party ties their sign-ins together: a farm admin sets them,
// one sign-in is one person's, the person adds only the one they use,
// and a sign-in of theirs decides their waits. Sign-ins go only to the
// person and to farm admins.
func TestPartySignIns(t *testing.T) {
	p := newPartyRig(t)
	ctx := context.Background()
	noor := map[string]any{"name": "noor", "kind": "person", "displayName": "Noor", "signIns": []string{"noor@studio.example", "noor"}}
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", noor, nil); code != http.StatusOK {
		t.Fatalf("make noor: %d %v", code, out)
	}
	lee := map[string]any{"name": "lee", "kind": "person", "displayName": "Lee", "signIns": []string{"noor@studio.example"}}
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", lee, nil); code != http.StatusConflict || !says(out, "one sign-in is one person's") {
		t.Errorf("lee takes noor's sign-in: %d %v", code, out)
	}
	org := map[string]any{"name": "saltmarsh", "kind": "organisation", "displayName": "Saltmarsh", "signIns": []string{"x"}}
	if code, _ := p.call(t, http.MethodPost, "/api/v1/parties", "ines", org, nil); code != http.StatusBadRequest {
		t.Errorf("an organisation with sign-ins: %d", code)
	}
	r, sso := p.viewer("noor-sso")
	d := p.s.decider(r, sso)
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
	signIns := func(as string, list ...string) (int, map[string]any) {
		return p.call(t, http.MethodPut, "/api/v1/parties/noor/sign-ins", as, map[string]any{"signIns": list}, nil)
	}
	if code, out := signIns("noor-sso", "noor@studio.example", "noor", "maya"); code != http.StatusForbidden || !says(out, "adds only the sign-in they are using") {
		t.Errorf("noor adds maya's sign-in: %d %v", code, out)
	}
	if code, out := signIns("maya", "noor@studio.example"); code != http.StatusForbidden {
		t.Errorf("maya changes noor's sign-ins: %d %v", code, out)
	}
	if code, out := signIns("noor-sso", "noor@studio.example"); code != http.StatusOK {
		t.Errorf("noor drops a sign-in: %d %v", code, out)
	}
	if code, out := signIns("ines", "noor@studio.example", "noor-laptop"); code != http.StatusOK {
		t.Errorf("a farm admin sets noor's sign-ins: %d %v", code, out)
	}
	pt, err := p.s.parties(ctx)
	if err != nil || !slices.Equal(findParty(pt, "noor").Spec.SignIns, []string{"noor-laptop", "noor@studio.example"}) || findParty(pt, "noor").Spec.Changed == nil {
		t.Errorf("noor's sign-ins: %+v %v", findParty(pt, "noor"), err)
	}
	p.seal(t)
}

// A party write may carry its signed act, and its record then cites it:
// the act on the party, of the write's own verb, with its body.
func TestPartyActsCited(t *testing.T) {
	p := newPartyRig(t)
	if code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), nil); code != http.StatusOK {
		t.Fatalf("make kestrel: %d %v", code, out)
	}
	withUID(t, p.s, &c4v1.Party{ObjectMeta: metav1.ObjectMeta{Name: "kestrel"}}, "9b1e0000-0000-4000-8000-000000000001")
	on := p.party(t, "kestrel", "ines").ActOn
	ines := p.f.Person(t, "ines", true)
	body := map[string]any{"person": "dana", "purposes": []string{"member"}}
	send := func(verb string, confirmed bool) (int, map[string]any) {
		act := p.g.act(t, ines, on, verb, body, confirmed)
		return p.call(t, http.MethodPost, "/api/v1/parties/kestrel/members", "ines", body, map[string]string{ActHeader: act})
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

// The act needs of a Party's verbs: making a party, adding a member and
// changing purposes or sign-ins give authority, so need a touch; ending a
// membership and changing a party's names are a person's, silent.
func TestPartyActNeeds(t *testing.T) {
	for verb, want := range map[string]provenance.ActNeed{
		"make-party": {Person: true, Touch: true}, "add-member": {Person: true, Touch: true},
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
	code, out := p.call(t, http.MethodPost, "/api/v1/parties", "ines", kestrel(), nil)
	if code != http.StatusOK {
		t.Fatalf("make kestrel: %d %v", code, out)
	}
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
	// The HUD's own make-party record, cited by an entry that says it adds
	// maya: the record is the HUD's, of another act.
	made, _ := out["record"].(string)
	raw, err := p.store.Get(ctx, made)
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
		t.Error("an entry citing the make-party record as maya's add-member makes her an admin")
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
