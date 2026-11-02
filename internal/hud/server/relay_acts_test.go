package server

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A relay signs each decision it carries (PEOPLE-KEYS-2 6.1): an act/v1
// by the tracker user, for the person the people map names, under their
// confirmation of the login, signed by the sync its Tracker names
// (spec.sync). The HUD takes it only with the relayed decision, and the
// control record cites it, graded relayed; an unconfirmed login's is
// cited and graded unattributed. An act signed by anyone else, or saying
// anything else about who decided, is refused, and so is one taken
// already.
func TestRelaySignsTheDecisionItCarries(t *testing.T) {
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
	now := time.Now()
	f := provtest.NewFarm(t, now)
	s.trusted, s.keysUntil = trustSet{roots: f.Roots, legacy: map[string]ed25519.PublicKey{}}, now.Add(time.Hour)
	s.DevViewers[c4v1.SyncDevViewer] = &api.Viewer{User: c4v1.SyncDevViewer, Projects: []string{"show-a"}}

	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.UID = k8stypes.UID(provtest.JobUID)
	rec.job.Labels = map[string]string{c4v1.LabelTracker: "flow"}
	since := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.Waits = []c4v1.WorkWait{{Unit: 7, Name: "review", Server: "person:dana", Choices: []string{"approve", "rework"}, Since: &since, Blocking: true}}
	spec := queue.SpecDigest(&rec.job)
	on := provenance.ActSubject("workjob", "show-a", "sh040-light-v007", provtest.JobUID, spec)

	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	tracker := func(sync *c4v1.TrackerSync) *c4v1.Tracker {
		return &c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "flow"},
			Spec: c4v1.TrackerSpec{Kind: "flow", People: map[string]string{"dana": "dana"}, Sync: sync}}
	}
	confirmation := provtest.ID("dana confirms @dana on flow")
	confirmed := &c4v1.TrackerPerson{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: c4v1.TrackerPersonName("flow", "dana", "dana")},
		Spec: c4v1.TrackerPersonSpec{Tracker: "flow", Login: "dana", Person: "dana",
			Accepted: c4v1.TrackerPersonAct{By: "dana", At: metav1.Now(), Record: confirmation}}}
	syncAccount := &c4v1.TrackerSync{Namespace: "c4-system", ServiceAccount: "c4-sync"}
	objects := func(objs ...runtime.Object) {
		b := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(rec.job.DeepCopy())
		s.Client = b.WithRuntimeObjects(objs...).Build()
	}

	evidence := c4.Identify(strings.NewReader(`{"sg_status_list":"fix"}`)).String()
	body := map[string]any{"unit": "review", "do": "rework", "note": "fill down a stop",
		"via": map[string]any{"tracker": "flow", "source": "Version/88120", "by": "dana", "person": "dana", "evidence": evidence}}
	chain := func(edit func(c *schema.ActChain)) *schema.ActChain {
		c := schema.ActChain{By: schema.Actor{Tracker: "flow", User: "dana", Grade: schema.GradeRelayed},
			For:   schema.For{Party: "dana", Grade: schema.GradeRelayed},
			Under: []schema.Delegation{{Kind: "people-map", Record: confirmation}}}
		if edit != nil {
			edit(&c)
		}
		return &c
	}
	relay := f.Relay(t, "c4-system", "c4-sync", "flow", "dana")
	sign := func(k *provtest.Key, c *schema.ActChain) string {
		return base64.StdEncoding.EncodeToString(f.SignAct(t, k, provtest.Act{Subject: on, Act: "decide", Params: body, Chain: c, SignedAt: time.Now()}))
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	post := func(act string) (int, controlPredicate, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/decide?as="+c4v1.SyncDevViewer, strings.NewReader(string(raw)))
		if act != "" {
			r.Header.Set(ActHeader, act)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		msg, _ := out["error"].(string)
		if w.Code != http.StatusOK {
			return w.Code, controlPredicate{}, msg
		}
		id, _ := out["record"].(string)
		cid, err := c4.Parse(id)
		if err != nil {
			t.Fatalf("record %q: %v", id, err)
		}
		data, err := c4view.ReadObject(st, cid)
		if err != nil {
			t.Fatal(err)
		}
		rv, err := provenance.VerifyBundle(data, roots)
		if err != nil {
			t.Fatal(err)
		}
		var p controlPredicate
		if err := json.Unmarshal(rv.Predicate, &p); err != nil {
			t.Fatal(err)
		}
		return w.Code, p, msg
	}

	// a Tracker that names no sync: its relay signs for no one
	objects(tracker(nil), confirmed)
	if code, _, msg := post(sign(relay, chain(nil))); code != http.StatusForbidden || !strings.Contains(msg, "names no sync") {
		t.Fatalf("a Tracker with no sync: %d %q", code, msg)
	}

	objects(tracker(syncAccount), confirmed)
	signed := sign(relay, chain(nil))
	code, p, msg := post(signed)
	if code != http.StatusOK {
		t.Fatalf("a relay's signed decision: %d %q", code, msg)
	}
	if p.Act == nil || !strings.HasPrefix(p.Act.Record, "c4") || p.Custody != CustodyRelayed || p.Chain == nil || !p.Chain.Accountable {
		t.Fatalf("the record cites %+v, custody %q, chain %+v", p.Act, p.Custody, p.Chain)
	}

	other := f.Relay(t, "c4-system", "another-sync", "flow", "dana")
	for name, c := range map[string]struct {
		act, says string
	}{
		"the same act again":         {signed, "taken"},
		"another account signed it":  {sign(other, chain(nil)), "flow's sync is"},
		"by another tracker user":    {sign(relay, chain(func(c *schema.ActChain) { c.By.User = "mallory" })), "is by @mallory"},
		"for another person":         {sign(relay, chain(func(c *schema.ActChain) { c.For.Party = "noor" })), "is for noor"},
		"under another confirmation": {sign(relay, chain(func(c *schema.ActChain) { c.Under[0].Record = provtest.ID("another") })), "names the confirmation"},
	} {
		if code, _, msg := post(c.act); code != http.StatusForbidden || !strings.Contains(msg, c.says) {
			t.Errorf("%s: %d %q, want 403 %q", name, code, msg, c.says)
		}
	}

	// a login no person has confirmed: the act is cited, and the decision
	// is no one's the farm knows
	objects(tracker(syncAccount))
	code, p, msg = post(sign(relay, chain(func(c *schema.ActChain) { c.Under = nil })))
	if code != http.StatusOK || p.Act == nil || p.Custody != CustodyUnattributed {
		t.Fatalf("an unconfirmed login's signed decision: %d %q, act %+v, custody %q", code, msg, p.Act, p.Custody)
	}
	// and one with no act is taken as before
	if code, p, msg := post(""); code != http.StatusOK || p.Act != nil {
		t.Fatalf("a relayed decision with no act: %d %q, act %+v", code, msg, p.Act)
	}
}

// A relay's act is the caller's own: c4-sync's request carrying an act
// another service account signed is refused, even where that account is
// the Tracker's sync.
func TestRelayActIsTheCallers(t *testing.T) {
	sync := provenance.ServiceAccountIdentity("c4-system", "c4-sync")
	c := &relayCheck{tracker: "flow", user: "dana", person: "dana", signer: sync}
	ch := schema.ActChain{By: schema.Actor{Tracker: "flow", User: "dana"}, For: schema.For{Party: "dana"}}
	c.caller = c4v1.SyncAccount
	if err := c.check(sync, ch); err != nil {
		t.Fatalf("the Tracker's sync, sending its own act: %v", err)
	}
	c.caller = "system:serviceaccount:show-a:importer"
	if err := c.check(sync, ch); err == nil || !strings.Contains(err.Error(), "sent it") {
		t.Fatalf("another account sending the sync's act: %v", err)
	}
}
