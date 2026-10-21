package server

import (
	"bytes"
	"context"
	"crypto/x509"
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
	"k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/schema"
)

type peopleFarm struct {
	s     *Server
	st    *storeurl.Sharded
	roots *x509.CertPool
}

// newPeopleFarm is farm() with a signer, a store, c4-sync and a member of
// party ml as viewers, and a Tracker github in show-a offering three
// logins: @dana as dana, @lee as lee, @mlflow-registry as party:ml.
func newPeopleFarm(t *testing.T) *peopleFarm {
	t.Helper()
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
	s.DevViewers["lee"] = &api.Viewer{User: "lee", Projects: []string{"show-a"}}
	s.DevViewers["ml-lead"] = &api.Viewer{User: "priya", Projects: []string{"show-a"}, Parties: []string{"ml"}}
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Labels = map[string]string{c4v1.LabelTracker: "github"}
	since := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.Waits = []c4v1.WorkWait{
		{Unit: 7, Name: "review", Server: "person:dana", Choices: []string{"approve", "rework"}, Since: &since, Blocking: true},
		{Unit: 9, Name: "ruling", Server: "person:dana", Since: &since},
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	tr := &c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "github", Generation: 3},
		Spec: c4v1.TrackerSpec{Kind: "github", People: map[string]string{"dana": "dana", "lee": "lee", "mlflow-registry": "party:ml"}}}
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy(), tr).Build()
	kube := kfake.NewSimpleClientset()
	n := 0
	kube.PrependReactor("create", "events", func(a ktesting.Action) (bool, runtime.Object, error) {
		if ev := a.(ktesting.CreateAction).GetObject().(*corev1.Event); ev.Name == "" {
			n++
			ev.Name = fmt.Sprintf("%s%d", ev.GenerateName, n)
		}
		return false, nil, nil
	})
	s.Kube = kube
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	return &peopleFarm{s: s, st: st, roots: roots}
}

func (f *peopleFarm) do(t *testing.T, method, path string, body any, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (f *peopleFarm) entries(t *testing.T, as, query string) []api.PeopleEntry {
	t.Helper()
	var out api.PeopleList
	if code := call(t, f.s, http.MethodGet, "/api/v1/people?as="+as+query, nil, &out); code != http.StatusOK {
		t.Fatalf("people as %s%s: %d", as, query, code)
	}
	return out.Entries
}

// record reads and verifies a signed control record.
func (f *peopleFarm) record(t *testing.T, id string) controlPredicate {
	t.Helper()
	cid, err := c4.Parse(id)
	if err != nil {
		t.Fatalf("record %q: %v", id, err)
	}
	data, err := c4view.ReadObject(f.st, cid)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := provenance.VerifyBundle(data, f.roots)
	if err != nil {
		t.Fatal(err)
	}
	var p controlPredicate
	if err := json.Unmarshal(rv.Predicate, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A Tracker's people map is offered, and counts once the person it names
// confirms it, as themselves: never another person, the tracker's sync, a
// unit or an agent. A party's login is confirmed by a member of the party.
// Confirming another person for a login revokes the first; a revoked
// entry is kept, marked; both are signed and booked.
func TestPeopleMapConfirmedByThePerson(t *testing.T) {
	f := newPeopleFarm(t)
	es := f.entries(t, "wrangler", "")
	if len(es) != 3 {
		t.Fatalf("three offers: %+v", es)
	}
	for _, e := range es {
		if e.State != "offered" || e.Lets == "" {
			t.Fatalf("offered, with what it lets: %+v", e)
		}
	}
	if mine := f.entries(t, "wrangler", "&mine=1"); len(mine) != 1 || mine[0].Login != "dana" || !mine[0].Yours {
		t.Fatalf("dana's own offer: %+v", mine)
	}
	confirm := api.PeopleConfirm{Project: "show-a", Tracker: "github", Login: "dana"}
	for _, c := range []struct {
		as   string
		hdr  map[string]string
		body api.PeopleConfirm
		code int
		says string
	}{
		{"lee", nil, confirm, http.StatusForbidden, "only dana confirms it"},
		{c4v1.SyncDevViewer, nil, confirm, http.StatusForbidden, "only the person confirms"},
		{"wrangler", map[string]string{"X-C4-Client": "mcp"}, confirm, http.StatusForbidden, "an agent may not"},
		{"wrangler", map[string]string{"X-C4-Agent": "planner"}, confirm, http.StatusForbidden, "an agent may not"},
		{"wrangler", nil, api.PeopleConfirm{Project: "show-a", Tracker: "github", Login: "eve"}, http.StatusNotFound, "offers no person"},
		{"lee", nil, api.PeopleConfirm{Project: "show-a", Tracker: "github", Login: "mlflow-registry"}, http.StatusForbidden, "a member of ml"},
	} {
		code, out := f.do(t, http.MethodPost, "/api/v1/people?as="+c.as, c.body, c.hdr)
		if msg, _ := out["error"].(string); code != c.code || !strings.Contains(msg, c.says) {
			t.Errorf("%s confirming @%s: %d %v, want %d %q", c.as, c.body.Login, code, out, c.code, c.says)
		}
	}
	code, out := f.do(t, http.MethodPost, "/api/v1/people?as=wrangler", confirm, nil)
	if code != http.StatusOK {
		t.Fatalf("dana confirms: %d %v", code, out)
	}
	id, _ := out["record"].(string)
	p := f.record(t, id)
	ch, _ := p.Change.(map[string]any)
	if p.Action != "people-confirm" || p.Actor != "dana" || ch["login"] != "dana" || ch["person"] != "dana" || ch["offer"] == nil {
		t.Fatalf("offer and acceptance on one record: %+v", p)
	}
	if code, out := f.do(t, http.MethodPost, "/api/v1/people?as=ml-lead", api.PeopleConfirm{Project: "show-a", Tracker: "github", Login: "mlflow-registry"}, nil); code != http.StatusOK {
		t.Fatalf("a member of ml confirms the registry: %d %v", code, out)
	}
	var tp c4v1.TrackerPerson
	name := c4v1.TrackerPersonName("github", "dana", "dana")
	if err := f.s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: name}, &tp); err != nil ||
		tp.Spec.Accepted.Record != id || tp.Spec.Offer.Generation != 3 || tp.Spec.Offer.From != "spec.people" {
		t.Fatalf("the entry: %+v %v", tp.Spec, err)
	}

	// The project now offers @dana as lee: lee's confirmation replaces
	// dana's, which is kept, revoked.
	var tr c4v1.Tracker
	f.s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: "github"}, &tr)
	tr.Spec.People["dana"] = "lee"
	f.s.Client.Update(context.Background(), &tr)
	if code, out := f.do(t, http.MethodPost, "/api/v1/people?as=lee", confirm, nil); code != http.StatusOK {
		t.Fatalf("lee confirms @dana: %d %v", code, out)
	}
	if err := f.s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: name}, &tp); err != nil || tp.Spec.Revoked == nil || tp.Spec.Revoked.By != "lee" {
		t.Fatalf("dana's confirmation is revoked, kept: %+v", tp.Spec)
	}
	all := f.entries(t, "wrangler", "&all=1")
	revoked := 0
	for _, e := range all {
		if e.State == "revoked" {
			revoked++
		}
	}
	if revoked != 1 {
		t.Fatalf("one revoked entry, kept: %+v", all)
	}

	// Lee takes it back; then nobody's confirmation stands for @dana.
	leeName := c4v1.TrackerPersonName("github", "dana", "lee")
	if code, out := f.do(t, http.MethodDelete, "/api/v1/people/show-a/"+leeName+"?as=ml-lead", nil, nil); code != http.StatusForbidden {
		t.Fatalf("someone else revoking: %d %v", code, out)
	}
	if code, out := f.do(t, http.MethodDelete, "/api/v1/people/show-a/"+leeName+"?as=lee", nil, nil); code != http.StatusOK {
		t.Fatalf("lee revokes: %d %v", code, out)
	}
	if code, _ := f.do(t, http.MethodDelete, "/api/v1/people/show-a/"+leeName+"?as=lee", nil, nil); code != http.StatusConflict {
		t.Fatalf("again: %d", code)
	}
	var book api.Book
	if code := call(t, f.s, http.MethodGet, "/api/v1/book?as=wrangler", nil, &book); code != http.StatusOK {
		t.Fatalf("book: %d", code)
	}
	kinds := map[string]int{}
	for _, e := range book.Entries {
		if e.Object.Type == "people" && e.Record != "" {
			kinds[e.Kind]++
		}
	}
	if kinds["people-confirm"] != 3 || kinds["people-revoke"] != 1 {
		t.Fatalf("book: %v", kinds)
	}
}

// A relayed decision's record carries its chain: by the tracker's user,
// for the mapped person, under that person's confirmation of the login;
// before any confirmation it goes through, unaccountable, saying what is
// missing. Both chains are valid chain/1.
func TestRelayNamesTheConfirmedLogin(t *testing.T) {
	f := newPeopleFarm(t)
	relay := func(unit string) controlPredicate {
		t.Helper()
		code, out := f.do(t, http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/decide?as="+c4v1.SyncDevViewer,
			map[string]any{"unit": unit, "do": "approve", "via": map[string]any{"tracker": "github",
				"source": "Review/12#" + unit, "by": "dana", "person": "dana"}}, nil)
		if code != http.StatusOK {
			t.Fatalf("relay %s: %d %v", unit, code, out)
		}
		id, _ := out["record"].(string)
		return f.record(t, id)
	}
	valid := func(c *schema.Chain) {
		t.Helper()
		b, _ := json.Marshal(c)
		if _, errs := schema.ParseChain(b); len(errs) > 0 {
			t.Fatalf("not chain/1: %s: %v", b, errs)
		}
	}

	p := relay("review")
	c := p.Chain
	if c == nil || c.Accountable || !strings.Contains(c.Gap, "dana has not yet confirmed that the login dana") || c.By.User != "dana" ||
		c.By.Tracker != "github" || c.For.Party != "dana" || len(c.Under) != 0 || p.Custody != CustodyRelayed {
		t.Fatalf("an unconfirmed login: %+v %+v", p, c)
	}
	valid(c)

	code, out := f.do(t, http.MethodPost, "/api/v1/people?as=wrangler", api.PeopleConfirm{Project: "show-a", Tracker: "github", Login: "dana"}, nil)
	if code != http.StatusOK {
		t.Fatalf("confirm: %d %v", code, out)
	}
	confirmation, _ := out["record"].(string)
	c = relay("ruling").Chain
	if c == nil || !c.Accountable || c.Gap != "" || len(c.Under) != 1 || c.Under[0].Kind != "people-map" || c.Under[0].Record != confirmation {
		t.Fatalf("a confirmed login: %+v", c)
	}
	valid(c)
}
