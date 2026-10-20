package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// A decision made in a tracker is relayed only by the tracker's sync, for
// that tracker's job, from a tracker user its people map makes the person
// the wait is for; the record signs it as relayed (or, by a party the map
// names, relayed-unattributed), with where it came from. A person's own
// decision in the HUD is direct.
func TestRelayedDecisions(t *testing.T) {
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
	rec.job.Status.Waits = []c4v1.WorkWait{
		{Unit: 7, Name: "review", Server: "person:dana", Choices: []string{"approve", "rework"}, Since: &since, Blocking: true},
		{Unit: 8, Name: "registry", Server: "party:ml", Since: &since, Blocking: true},
		{Unit: 9, Name: "ruling", Server: "person:dana", Since: &since},
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	tr := &c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "github"},
		Spec: c4v1.TrackerSpec{Kind: "github", People: map[string]string{"dana": "dana", "mallory": "mallory", "mlflow-registry": "party:ml"}}}
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy(), tr).Build()

	post := func(as string, body map[string]any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/decide?as="+as, strings.NewReader(string(raw))))
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	evidence := c4.Identify(strings.NewReader(`{"review":{"state":"CHANGES_REQUESTED"}}`)).String()
	via := func(edit func(map[string]any)) map[string]any {
		v := map[string]any{"tracker": "github", "source": "https://github.com/acme/widgets/pull/12#pullrequestreview-5 (Review/12#5)",
			"by": "dana", "person": "dana", "evidence": evidence}
		if edit != nil {
			edit(v)
		}
		return map[string]any{"unit": "review", "do": "rework", "note": "assert the order", "via": v}
	}
	refused := []struct {
		as   string
		body map[string]any
		code int
		says string
	}{
		{"wrangler", via(nil), http.StatusForbidden, "only a tracker's sync relays"},
		{c4v1.SyncDevViewer, via(func(v map[string]any) { v["tracker"] = "jira" }), http.StatusForbidden, "not tracker jira's work"},
		{c4v1.SyncDevViewer, via(func(v map[string]any) { v["by"], v["person"] = "mallory", "mallory" }), http.StatusForbidden, "this waits for dana's decision"},
		{c4v1.SyncDevViewer, via(func(v map[string]any) { v["person"] = "dan" }), http.StatusForbidden, "does not map @dana to dan"},
		{c4v1.SyncDevViewer, via(func(v map[string]any) { v["by"] = "eve" }), http.StatusForbidden, "does not map @eve"},
		{c4v1.SyncDevViewer, via(func(v map[string]any) { v["source"] = "" }), http.StatusBadRequest, "names its tracker, source"},
		{c4v1.SyncDevViewer, via(func(v map[string]any) { v["evidence"] = "nope" }), http.StatusBadRequest, "not a C4 ID"},
	}
	for _, c := range refused {
		code, out := post(c.as, c.body)
		if msg, _ := out["error"].(string); code != c.code || !strings.Contains(msg, c.says) {
			t.Errorf("%s %v: %d %v, want %d %q", c.as, c.body["via"], code, out, c.code, c.says)
		}
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	record := func(out map[string]any) controlPredicate {
		t.Helper()
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
		return p
	}

	code, out := post(c4v1.SyncDevViewer, via(nil))
	if code != http.StatusOK {
		t.Fatalf("relayed: %d %v", code, out)
	}
	p := record(out)
	relay, _ := p.Relay.(map[string]any)
	if p.Custody != CustodyRelayed || p.Actor != "dana via tracker github (@dana)" || !strings.HasPrefix(p.Authenticated, "relayed by c4-sync") ||
		relay["evidence"] != evidence || relay["by"] != "dana" {
		t.Fatalf("relayed record: %+v", p)
	}

	code, out = post(c4v1.SyncDevViewer, map[string]any{"unit": "registry", "do": "approve",
		"via": map[string]any{"tracker": "github", "source": "models:/widgets@champion", "by": "mlflow-registry", "person": "party:ml"}})
	if code != http.StatusOK {
		t.Fatalf("relayed by a party: %d %v", code, out)
	}
	if p := record(out); p.Custody != CustodyRelayedUnattributed || p.Actor != "party:ml via tracker github (@mlflow-registry)" {
		t.Fatalf("unattributed record: %+v", p)
	}

	code, out = post("wrangler", map[string]any{"unit": "ruling", "do": "approve"})
	if code != http.StatusOK {
		t.Fatalf("direct: %d %v", code, out)
	}
	if p := record(out); p.Custody != CustodyDirect || p.Relay != nil || p.Actor != "dana" {
		t.Fatalf("direct record: %+v", p)
	}

	var live c4v1.WorkJob
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "sh040-light-v007"}, &live); err != nil {
		t.Fatal(err)
	}
	var reqs []c4v1.WorkDecideRequest
	json.Unmarshal([]byte(live.Annotations[c4v1.AnnotationDecide]), &reqs)
	if len(reqs) != 3 || reqs[0].Actor != "dana via tracker github (@dana)" || reqs[0].Note != "assert the order" || reqs[2].Actor != "dana" {
		t.Fatalf("requests: %+v", reqs)
	}
}
