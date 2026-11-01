package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/schema"
)

// actRig makes a HUD trust a scratch farm's people's devices, sign its own
// records and keep them, so a test can send a request with its act and
// read the record that answers it.
type actRig struct {
	s *Server
	f *provtest.Farm
}

func newActRig(t *testing.T, s *Server) *actRig {
	t.Helper()
	return newActRigOn(t, s, provtest.NewFarm(t, time.Now()))
}

// newActRigOn is newActRig on a scratch farm made already (a peopletest
// farm's).
func newActRigOn(t *testing.T, s *Server, f *provtest.Farm) *actRig {
	t.Helper()
	now := time.Now()
	s.trusted, s.keysUntil = trustSet{roots: f.Roots, legacy: map[string]ed25519.PublicKey{}, farm: f.ID}, now.Add(time.Hour)
	if s.Stores == nil {
		st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
		if err != nil {
			t.Fatal(err)
		}
		s.Stores = func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
	}
	if s.Signer == nil {
		ca, err := provenance.NewCA(now)
		if err != nil {
			t.Fatal(err)
		}
		if s.Signer, err = provenance.NewSigner(context.Background(), provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// the API server gives every object a UID; the fakes don't
	if s.m != nil {
		for i := range s.m.projects {
			if s.m.projects[i].UID == "" {
				s.m.projects[i].UID = k8stypes.UID(fmt.Sprintf("5a1e%04d-0000-4000-8000-000000000000", i))
			}
		}
	}
	return &actRig{s: s, f: f}
}

// withUID gives a stored object the UID the API server would have.
func withUID(t *testing.T, s *Server, obj client.Object, uid string) {
	t.Helper()
	if err := s.Client.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	if obj.GetUID() != "" {
		return
	}
	obj.SetUID(k8stypes.UID(uid))
	if err := s.Client.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
}

// act is k's act on on, of verb, with params, as the base64 X-C4-Act
// carries it; confirmed signs with the person's touch key too.
func (g *actRig) act(t *testing.T, k *provtest.Key, on *api.ActOn, verb string, params any, confirmed bool) string {
	t.Helper()
	if on == nil {
		t.Fatalf("%s: the HUD serves no subject to sign it on", verb)
	}
	return base64.StdEncoding.EncodeToString(g.f.SignAct(t, k, provtest.Act{Subject: provenance.Subject{Name: on.Name, Digest: on.Digest},
		Act: verb, Params: params, SignedAt: time.Now(), Confirmed: confirmed}))
}

// actRecord is a stored control record: its predicate and its subject.
type actRecord struct {
	controlPredicate
	subject provenance.Subject
}

// send makes the request, with the act when there is one, and reads the
// control record it answers with from the project's store.
func (g *actRig) send(t *testing.T, method, path string, body any, act, project string, headers map[string]string) (int, map[string]any, *actRecord) {
	t.Helper()
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rd)
	if act != "" {
		r.Header.Set(ActHeader, act)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.s.Handler().ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	id, _ := out["record"].(string)
	if id == "" {
		return w.Code, out, nil
	}
	return w.Code, out, g.predicate(t, project, id)
}

// predicate is a stored control record's predicate.
func (g *actRig) predicate(t *testing.T, project, id string) *actRecord {
	t.Helper()
	cid, err := c4.Parse(id)
	if err != nil {
		t.Fatalf("record %q: %v", id, err)
	}
	st, err := g.s.Stores(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	data, err := c4view.ReadObject(st, cid)
	if err != nil {
		t.Fatal(err)
	}
	var bundle provenance.Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	payload, _ := base64.StdEncoding.DecodeString(bundle.DSSEEnvelope.Payload)
	var st2 struct {
		Subject   []provenance.Subject `json:"subject"`
		Predicate controlPredicate     `json:"predicate"`
	}
	if err := json.Unmarshal(payload, &st2); err != nil {
		t.Fatal(err)
	}
	rec := &actRecord{controlPredicate: st2.Predicate}
	if len(st2.Subject) == 1 {
		rec.subject = st2.Subject[0]
	}
	return rec
}

// cites checks a record cites the act and is on its subject.
func cites(t *testing.T, what string, code int, out map[string]any, p *actRecord, on *api.ActOn, grade string) {
	t.Helper()
	switch {
	case code != http.StatusOK && code != http.StatusCreated:
		t.Errorf("%s with its act: %d %v", what, code, out)
	case p == nil || p.Act == nil || p.Act.Payload == "":
		t.Errorf("%s: the record cites no act: %+v", what, p)
	case p.subject.Name != on.Name || p.subject.Digest["sha256"] != on.Digest["sha256"] || p.subject.Digest["c4"] != on.Digest["c4"]:
		t.Errorf("%s: the record is on %+v, the act on %+v", what, p.subject, on)
	case p.Custody != grade:
		t.Errorf("%s: custody %q, want %q", what, p.Custody, grade)
	}
}

// refused checks each act is refused, 403, before anything is written.
func refused(t *testing.T, what string, send func(act string) int, acts map[string]string) {
	t.Helper()
	for name, a := range acts {
		if code := send(a); code != http.StatusForbidden {
			t.Errorf("%s, %s: %d, want 403", what, name, code)
		}
	}
}

func otherDigest(on *api.ActOn) *api.ActOn {
	o := &api.ActOn{Name: on.Name, Digest: map[string]string{}}
	for k := range on.Digest {
		o.Digest[k] = "0000000000000000000000000000000000000000000000000000000000000000"
	}
	return o
}

func snapshotOf(t *testing.T, s *Server, as string) api.Snapshot {
	t.Helper()
	var snap api.Snapshot
	if code := call(t, s, http.MethodGet, "/api/v1/snapshot?as="+as, nil, &snap); code != http.StatusOK {
		t.Fatalf("snapshot: %d", code)
	}
	return snap
}

func projectActOn(t *testing.T, s *Server, as, name string) *api.ActOn {
	t.Helper()
	for _, p := range snapshotOf(t, s, as).Projects {
		if p.Name == name {
			return p.ActOn
		}
	}
	t.Fatalf("no project %s in the snapshot", name)
	return nil
}

// A grant is an act on the project, as the snapshot names it; revoking it
// an act on the grant, as the grant list names it.
func TestGrantsTakeTheirActs(t *testing.T) {
	s := grantFarm(t)
	g := newActRig(t, s)
	// a grant is on the touch list; its revocation is a person's, silent
	maya := g.f.Person(t, "maya", true)
	on := projectActOn(t, s, "artist", "show-a")
	req := api.GrantRequest{Project: "show-a", Tracker: "github", Pools: []string{"cpu"}, Until: "7d"}
	send := func(a string) int {
		code, _, _ := g.send(t, http.MethodPost, "/api/v1/grants?as=artist", req, a, "show-a", nil)
		return code
	}
	refused(t, "grant", send, map[string]string{
		"without the touch": g.act(t, maya, on, "grant", req, false),
		"on another digest": g.act(t, maya, otherDigest(on), "grant", req, true),
		"another verb":      g.act(t, maya, on, "revoke", req, true),
		"other params":      g.act(t, maya, on, "grant", api.GrantRequest{Project: "show-a", Tracker: "github", Until: "90d"}, true),
	})
	if gs := grants(t, s, "?as=artist"); len(gs) != 0 {
		t.Fatalf("a refused act granted: %+v", gs)
	}
	code, out, p := g.send(t, http.MethodPost, "/api/v1/grants?as=artist", req, g.act(t, maya, on, "grant", req, true), "show-a", nil)
	cites(t, "grant", code, out, p, on, CustodyConfirmed)
	if code, _, p := g.send(t, http.MethodPost, "/api/v1/grants?as=artist", req, "", "show-a", nil); code != http.StatusOK || p == nil || p.Act != nil {
		t.Errorf("a grant with no act: %d %+v", code, p)
	}

	gr := grants(t, s, "?as=artist")[0]
	withUID(t, s, &c4v1.SubmitGrant{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: gr.Name}}, "9a7e0000-0001")
	gr = grants(t, s, "?as=artist")[0]
	path := "/api/v1/grants/show-a/" + gr.Name + "?as=artist"
	revoke := func(a string) int {
		code, _, _ := g.send(t, http.MethodDelete, path, nil, a, "show-a", nil)
		return code
	}
	refused(t, "revoke", revoke, map[string]string{
		"on the project": g.act(t, maya, on, "revoke", map[string]any{}, false),
		"another verb":   g.act(t, maya, gr.ActOn, "grant", map[string]any{}, false),
	})
	code, out, p = g.send(t, http.MethodDelete, path, nil, g.act(t, maya, gr.ActOn, "revoke", map[string]any{}, false), "show-a", nil)
	cites(t, "revoke", code, out, p, gr.ActOn, CustodySigned)
	if after := grants(t, s, "?as=artist"); len(after) != 1 || after[0].ActOn != nil {
		t.Errorf("a revoked grant still offers an act: %+v", after)
	}
}

// Confirming a login is an act on the Tracker's offers; revoking the
// confirmation an act on it. Confirming needs the person's touch.
func TestPeopleMapTakesItsActs(t *testing.T) {
	f := newPeopleFarm(t)
	g := newActRig(t, f.s)
	dana := g.f.Person(t, "dana", true)
	withUID(t, f.s, &c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "github"}}, "7c0e0000-0002")
	var es []api.PeopleEntry
	for _, e := range f.entries(t, "wrangler", "&mine=1") {
		es = append(es, e)
	}
	if len(es) != 1 || es[0].ConfirmOn == nil || es[0].RevokeOn != nil {
		t.Fatalf("dana's offer: %+v", es)
	}
	on := es[0].ConfirmOn
	confirm := api.PeopleConfirm{Project: "show-a", Tracker: "github", Login: "dana"}
	send := func(a string) int {
		code, _, _ := g.send(t, http.MethodPost, "/api/v1/people?as=wrangler", confirm, a, "show-a", nil)
		return code
	}
	refused(t, "people-confirm", send, map[string]string{
		"without the touch": g.act(t, dana, on, "people-confirm", confirm, false),
		"on another digest": g.act(t, dana, otherDigest(on), "people-confirm", confirm, true),
		"another verb":      g.act(t, dana, on, "people-revoke", confirm, true),
		"another login":     g.act(t, dana, on, "people-confirm", api.PeopleConfirm{Project: "show-a", Tracker: "github", Login: "lee"}, true),
	})
	if es := f.entries(t, "wrangler", "&mine=1"); es[0].State != "offered" {
		t.Fatalf("a refused act confirmed: %+v", es)
	}
	code, out, p := g.send(t, http.MethodPost, "/api/v1/people?as=wrangler", confirm, g.act(t, dana, on, "people-confirm", confirm, true), "show-a", nil)
	cites(t, "people-confirm", code, out, p, on, CustodyConfirmed)

	e := f.entries(t, "wrangler", "&mine=1")[0]
	withUID(t, f.s, &c4v1.TrackerPerson{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: e.Name}}, "7c0e0000-0003")
	e = f.entries(t, "wrangler", "&mine=1")[0]
	if e.RevokeOn == nil || e.ConfirmOn == nil {
		t.Fatalf("a confirmed entry: %+v", e)
	}
	path := "/api/v1/people/show-a/" + e.Name + "?as=wrangler"
	revoke := func(a string) int {
		code, _, _ := g.send(t, http.MethodDelete, path, nil, a, "show-a", nil)
		return code
	}
	refused(t, "people-revoke", revoke, map[string]string{
		"on the offers": g.act(t, dana, on, "people-revoke", map[string]any{}, false),
	})
	code, out, p = g.send(t, http.MethodDelete, path, nil, g.act(t, dana, e.RevokeOn, "people-revoke", map[string]any{}, false), "show-a", nil)
	cites(t, "people-revoke", code, out, p, e.RevokeOn, CustodySigned)
}

// The binding is an act on the farm, as the snapshot names it, and needs
// the person's touch.
func TestBindingTakesItsAct(t *testing.T) {
	s, _ := bindFarm(t)
	g := newActRig(t, s)
	dana := g.f.Person(t, "dana", true)
	headers := map[string]string{InstallerHeader: testInstallerToken}
	code, snap, _ := g.send(t, http.MethodGet, "/api/v1/snapshot?as=wrangler", nil, "", "show-a", headers)
	var on *api.ActOn
	if f, _ := snap["farm"].(map[string]any); code == http.StatusOK && f["actOn"] != nil {
		b, _ := json.Marshal(f["actOn"])
		json.Unmarshal(b, &on)
	}
	if on == nil || on.Name != "farm:"+g.f.ID || on.Digest["c4"] != g.f.ID {
		t.Fatalf("the farm's subject: %+v", on)
	}
	body := map[string]any{"owner": "dana", "to": "laptop-1/*"}
	send := func(a string) int {
		code, _, _ := g.send(t, http.MethodPost, "/api/v1/farm/binding?as=wrangler", body, a, "show-a", headers)
		return code
	}
	refused(t, "bind", send, map[string]string{
		"without the touch": g.act(t, dana, on, "bind", body, false),
		"another farm":      g.act(t, dana, &api.ActOn{Name: "farm:" + provtest.ID("another"), Digest: map[string]string{"c4": provtest.ID("another")}}, "bind", body, true),
		"another owner":     g.act(t, dana, on, "bind", map[string]any{"owner": "lee", "to": "laptop-1/*"}, true),
	})
	var b c4v1.DeploymentBinding
	if err := s.Client.Get(context.Background(), k8stypes.NamespacedName{Name: c4v1.FarmBindingName}, &b); err == nil {
		t.Fatal("a refused act bound the farm")
	}
	code, out, p := g.send(t, http.MethodPost, "/api/v1/farm/binding?as=wrangler", body, g.act(t, dana, on, "bind", body, true), "show-a", headers)
	cites(t, "bind", code, out, p, on, CustodyConfirmed)
}

// Enrolling a machine is an act on its offer, by the code it shows, and
// needs the holder's touch; a refused act leaves the offer standing.
func TestEnrollTakesItsAct(t *testing.T) {
	s := hostFarm(t, hostNode("laptop-1"))
	s.DevViewers["dana"].Projects = []string{"show-a"}
	g := newActRig(t, s)
	dana := g.f.Person(t, "dana", true)
	_, offer := hostCall(t, s, http.MethodPost, "/api/v1/nodes/laptop-1/enroll-offer", "host1", map[string]string{"holder": "dana"})
	c, _ := offer["code"].(string)
	on := actOn(enrollSubject("laptop-1", c))
	body := map[string]string{"code": c}
	send := func(a string) int {
		code, _, _ := g.send(t, http.MethodPost, "/api/v1/nodes/laptop-1/enroll?as=dana", body, a, "show-a", nil)
		return code
	}
	refused(t, "enroll", send, map[string]string{
		"without the touch": g.act(t, dana, on, "enroll", body, false),
		"another machine":   g.act(t, dana, actOn(enrollSubject("laptop-2", c)), "enroll", body, true),
		"another code":      g.act(t, dana, actOn(enrollSubject("laptop-1", "AAAA-AAAA")), "enroll", body, true),
	})
	code, out, p := g.send(t, http.MethodPost, "/api/v1/nodes/laptop-1/enroll?as=dana", body, g.act(t, dana, on, "enroll", body, true), "show-a", nil)
	cites(t, "enroll", code, out, p, on, CustodyConfirmed)
}

// Approving a burst is an act on its capacity request, as the pool's
// burst names it, and needs the person's touch.
func TestApproveTakesItsAct(t *testing.T) {
	s, _ := decideFarm(t)
	g := newActRig(t, s)
	dana := g.f.Person(t, "dana", true)
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	cr := &c4v1.CapacityRequest{ObjectMeta: metav1.ObjectMeta{Name: "burst-a-approve-1", UID: "7a1c0000-b0a5"}, Spec: c4v1.CapacityRequestSpec{Pool: "burst-a", Slots: 4,
		Jobs: []string{"show-a/sh040-light-v007"}, EstimatedCost: "12.00"}}
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr.DeepCopy()).Build()
	s.m.crs = []c4v1.CapacityRequest{*cr}
	s.m.pools = append(s.m.pools, c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "burst-a"}, Spec: c4v1.PoolSpec{Burst: &c4v1.PoolBurst{MaxNodes: 1}}})
	var on *api.ActOn
	for _, p := range snapshotOf(t, s, "wrangler").Pools {
		if p.Name == "burst-a" && p.Burst != nil {
			on = p.Burst.Approve
		}
	}
	if on == nil || on.Name != "capacityrequest:burst-a-approve-1@7a1c0000-b0a5" {
		t.Fatalf("the waiting request: %+v", on)
	}
	send := func(a string) int {
		code, _, _ := g.send(t, http.MethodPost, "/api/v1/capacity/burst-a/approve?as=wrangler", nil, a, "show-a", nil)
		return code
	}
	refused(t, "approve-capacity", send, map[string]string{
		"without the touch": g.act(t, dana, on, "approve-capacity", map[string]any{}, false),
		"on another digest": g.act(t, dana, otherDigest(on), "approve-capacity", map[string]any{}, true),
	})
	var got c4v1.CapacityRequest
	if err := s.Client.Get(context.Background(), k8stypes.NamespacedName{Name: cr.Name}, &got); err != nil || got.Spec.Approved {
		t.Fatalf("a refused act approved it: %v", err)
	}
	code, out, p := g.send(t, http.MethodPost, "/api/v1/capacity/burst-a/approve?as=wrangler", nil, g.act(t, dana, on, "approve-capacity", map[string]any{}, true), "show-a", nil)
	cites(t, "approve-capacity", code, out, p, on, CustodyConfirmed)
}

// A held tracker write's confirmation is an act on the write, as the held
// list names it.
func TestHeldWriteTakesItsAct(t *testing.T) {
	s := grantFarm(t)
	scheme := runtime.NewScheme()
	c4v1.AddToScheme(scheme)
	at := metav1.NewTime(time.Date(2026, 10, 20, 9, 50, 0, 0, time.UTC))
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&c4v1.TrackerLink{}).WithObjects(
		&c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "gh"}, Spec: c4v1.TrackerSpec{Kind: "github"}},
		&c4v1.TrackerLink{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "gh-issue-7", UID: "3e9f0000-11a7"},
			Spec:   c4v1.TrackerLinkSpec{Tracker: "gh", Ref: c4v1.TrackerRef{Type: "Issue", ID: "7"}, Target: c4v1.TrackerTarget{WorkJob: "sh040-light-v007"}},
			Status: c4v1.TrackerLinkStatus{Written: map[string]c4v1.TrackerWritten{"comment": {Value: "running#", Ref: "991", At: at, State: "unconfirmed"}}}},
	).Build()
	g := newActRig(t, s)
	dana := g.f.Person(t, "dana", false)
	var held api.HeldList
	if code := call(t, s, http.MethodGet, "/api/v1/held?as=wrangler", nil, &held); code != http.StatusOK || len(held.Held) != 1 || held.Held[0].ActOn == nil {
		t.Fatalf("held: %d %+v", code, held)
	}
	on := held.Held[0].ActOn
	yes := true
	body := api.ConfirmRequest{Happened: &yes}
	path := "/api/v1/held/show-a/gh-issue-7/comment?as=wrangler"
	send := func(a string) int {
		code, _, _ := g.send(t, http.MethodPost, path, body, a, "show-a", nil)
		return code
	}
	no := false
	refused(t, "confirm-write", send, map[string]string{
		"another verb":     g.act(t, dana, on, "deny-write", body, false),
		"the other answer": g.act(t, dana, on, "confirm-write", api.ConfirmRequest{Happened: &no}, false),
		"another field":    g.act(t, dana, &api.ActOn{Name: on.Name[:len(on.Name)-len("comment")] + "check", Digest: on.Digest}, "confirm-write", body, false),
	})
	code, out, p := g.send(t, http.MethodPost, path, body, g.act(t, dana, on, "confirm-write", body, false), "show-a", nil)
	cites(t, "confirm-write", code, out, p, on, CustodySigned)
}

// A job submitted through POST /jobs is an act on the project: its record
// cites it, signed once the job is made.
func TestPostJobsTakesItsAct(t *testing.T) {
	s := submitFarm(t)
	g := newActRig(t, s)
	maya := g.f.Person(t, "maya", false)
	on := projectActOn(t, s, "artist", "show-a")
	spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-2"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	body := api.SubmitRequest{Project: "show-a", Shot: "sh040", Stage: "light", Spec: spec}
	send := func(a string) int {
		code, _, _ := g.send(t, http.MethodPost, "/api/v1/jobs?as=artist", body, a, "show-a", nil)
		return code
	}
	other := body
	other.Shot = "sh050"
	refused(t, "submit", send, map[string]string{
		"another verb":      g.act(t, maya, on, "grant", body, false),
		"other params":      g.act(t, maya, on, "submit", other, false),
		"on another digest": g.act(t, maya, otherDigest(on), "submit", body, false),
	})
	code, out, p := g.send(t, http.MethodPost, "/api/v1/jobs?as=artist", body, g.act(t, maya, on, "submit", body, false), "show-a", nil)
	cites(t, "submit", code, out, p, on, CustodySigned)
	if code, out, p := g.send(t, http.MethodPost, "/api/v1/jobs?as=artist", body, "", "show-a", nil); code != http.StatusCreated || p != nil {
		t.Errorf("a submit with no act: %d %v %+v", code, out, p)
	}
}

// A submission is an act on its project: checked once before any attempt,
// cited by the submission's record; a plan (dry run) takes none.
func TestSubmissionTakesItsAct(t *testing.T) {
	s := submissionFarm(t)
	g := newActRig(t, s)
	maya := g.f.Person(t, "maya", false)
	on := projectActOn(t, s, "artist", "show-a")
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	body := api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree)}
	send := func(a string) int {
		code, _, _ := g.send(t, http.MethodPost, "/api/v1/submissions?as=artist", body, a, "show-a", nil)
		return code
	}
	other := api.SubmissionRequest{Submission: body.Submission, Key: "another"}
	refused(t, "submit", send, map[string]string{
		"another verb":      g.act(t, maya, on, "grant", body, false),
		"other params":      g.act(t, maya, on, "submit", other, false),
		"on another digest": g.act(t, maya, otherDigest(on), "submit", body, false),
	})
	var list c4v1.WorkJobList
	if err := s.Client.List(context.Background(), &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("a refused act made %d jobs", len(list.Items))
	}
	code, out, p := g.send(t, http.MethodPost, "/api/v1/submissions?as=artist", body, g.act(t, maya, on, "submit", body, false), "show-a", nil)
	cites(t, "submit", code, out, p, on, CustodySigned)
	plan := api.SubmissionRequest{Submission: body.Submission, DryRun: true}
	if code, _, _ := g.send(t, http.MethodPost, "/api/v1/submissions?as=artist", plan, g.act(t, maya, on, "submit", plan, false), "show-a", nil); code != http.StatusOK {
		t.Errorf("a plan carrying an act: %d", code)
	}
}

// A person's act is taken only from a device in force in the people's
// index at its current position, a confirmed one's touch key that
// device's: a certificate with no device record, another device's touch
// key and a removed device are refused, though their certificates live on.
func TestActsAreCheckedAgainstThePeoplesIndex(t *testing.T) {
	o := peopletest.OnePerson(t)
	s := grantFarm(t)
	s.DevViewers["noor"] = &api.Viewer{User: "noor", Projects: []string{"show-a"}}
	g := newActRigOn(t, s, o.Scratch)
	s.PeopleIndex = o.Index
	on := projectActOn(t, s, "noor", "show-a")
	req := api.GrantRequest{Project: "show-a", Tracker: "github", Pools: []string{"cpu"}, Until: "7d"}
	sign := func(k *provtest.Key) string {
		return base64.StdEncoding.EncodeToString(o.Scratch.SignAct(t, k, provtest.Act{Subject: provenance.Subject{Name: on.Name, Digest: on.Digest},
			Act: "grant", Params: req, SignedAt: time.Now(), Confirmed: true,
			Chain: &schema.ActChain{By: schema.Actor{Person: "noor", Grade: schema.GradeConfirmed}, For: schema.For{Party: "noor", Grade: schema.GradeConfirmed}}}))
	}
	owner := o.Owner.Key(t)
	owner.Touch = o.Touch.Key(t)
	stranger := o.Scratch.Person(t, "noor", true) // certified, with no device record behind it
	wrongTouch := o.Owner.Key(t)
	wrongTouch.Touch = stranger.Touch
	send := func(a string) int {
		code, _, _ := g.send(t, http.MethodPost, "/api/v1/grants?as=noor", req, a, "show-a", nil)
		return code
	}
	refused(t, "grant", send, map[string]string{
		"a device with no record":    sign(stranger),
		"another device's touch key": sign(wrongTouch),
	})
	code, out, p := g.send(t, http.MethodPost, "/api/v1/grants?as=noor", req, sign(owner), "show-a", nil)
	cites(t, "grant from noor's own device", code, out, p, on, CustodyConfirmed)

	// noor removes the device, signing with it; its certificate lives on
	remove := o.Scratch.SignAct(t, o.Owner.Key(t), provtest.Act{Subject: provenance.Subject{Name: "device:" + o.Owner.ID, Digest: map[string]string{"c4": o.Owner.ID}},
		Act: "remove-device", Params: map[string]any{}, SignedAt: time.Now(),
		Chain: &schema.ActChain{By: schema.Actor{Person: "noor", Grade: schema.GradeSigned}, For: schema.For{Party: "noor", Grade: schema.GradeSigned}}})
	resp, err := http.Post(o.URL+"/v1/people/acts", "application/json", bytes.NewReader(remove))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("removing the device: %d", resp.StatusCode)
	}
	if code, out, _ := g.send(t, http.MethodPost, "/api/v1/grants?as=noor", req, sign(owner), "show-a", nil); code != http.StatusForbidden ||
		!strings.Contains(fmt.Sprint(out["error"]), "no device record") {
		t.Errorf("a removed device's act: %d %v, want 403 for its record", code, out)
	}
	// a farm without a people's index takes it, as before
	s.PeopleIndex = nil
	if code := send(sign(owner)); code != http.StatusOK {
		t.Errorf("without the index: %d", code)
	}
}

// An act refused only for want of the person's touch says so, for a
// client to ask for it and sign again; any other refusal doesn't.
func TestNeedsTouchRefusalSaysSo(t *testing.T) {
	s, _ := bindFarm(t)
	g := newActRig(t, s)
	dana := g.f.Person(t, "dana", true)
	headers := map[string]string{InstallerHeader: testInstallerToken}
	on := actOn(provenance.Subject{Name: "farm:" + g.f.ID, Digest: map[string]string{"c4": g.f.ID}})
	body := map[string]any{"owner": "dana", "to": "laptop-1/*"}
	code, out, _ := g.send(t, http.MethodPost, "/api/v1/farm/binding?as=wrangler", body, g.act(t, dana, on, "bind", body, false), "show-a", headers)
	if code != http.StatusForbidden || out["needs"] != "confirmed" {
		t.Errorf("a bind without the touch: %d %v", code, out)
	}
	code, out, _ = g.send(t, http.MethodPost, "/api/v1/farm/binding?as=wrangler", body, g.act(t, dana, otherDigest(on), "bind", body, true), "show-a", headers)
	if code != http.StatusForbidden || out["needs"] != nil {
		t.Errorf("another refusal: %d %v", code, out)
	}
}

// The product's list of what each act needs is served, so no client
// keeps its own.
func TestActNeedsAreServed(t *testing.T) {
	s := farm(t)
	var out api.ActNeeds
	if code := call(t, s, http.MethodGet, "/api/v1/acts/needs?as=artist", nil, &out); code != http.StatusOK {
		t.Fatalf("needs: %d", code)
	}
	if n := out.Acts["grant"]; !n.Person || !n.Touch {
		t.Errorf("grant: %+v", n)
	}
	if n := out.Acts["revoke"]; !n.Person || n.Touch {
		t.Errorf("revoke: %+v", n)
	}
	if _, ok := out.Acts["submit"]; ok || out.Params["decide"] == "" || out.Params["remove-device"] == "" {
		t.Errorf("acts %v, params %v", out.Acts, out.Params)
	}
}
