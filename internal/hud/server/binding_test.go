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
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// bindFarm is a loopback HUD that signs, with no binding yet.
func bindFarm(t *testing.T) (*Server, *x509.CertPool) {
	t.Helper()
	s := farm(t)
	s.Loopback = true
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = provenance.NewSigner(context.Background(), provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).Build()
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	return s, roots
}

func bindCall(s *Server, method, as, body string, headers map[string]string) (int, map[string]any) {
	r := httptest.NewRequest(method, "/api/v1/farm/binding?as="+as, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// The installer binds every deployment of the farm to its owner, once:
// the binding is a signed control record (bind), graded claimed on a
// loopback HUD; binding again to the same owner changes nothing; another
// owner is refused, pointing to an owner change.
func TestFarmBinding(t *testing.T) {
	s, roots := bindFarm(t)
	noor := `{"owner": "noor@petrelfinch.example", "to": "laptop-1/*"}`
	if code, out := bindCall(s, http.MethodGet, "wrangler", "", nil); code != http.StatusNotFound {
		t.Fatalf("before any binding: %d %v", code, out)
	}
	code, out := bindCall(s, http.MethodPost, "wrangler", noor, map[string]string{"X-C4-Client": "c4-farm"})
	if code != http.StatusOK {
		t.Fatalf("bind: %d %v", code, out)
	}
	var b c4v1.DeploymentBinding
	if err := s.Client.Get(context.Background(), types.NamespacedName{Name: c4v1.FarmBindingName}, &b); err != nil {
		t.Fatal(err)
	}
	if b.Spec.Owner != "noor@petrelfinch.example" || b.Spec.To != "laptop-1/*" || b.Spec.Record != out["record"] || len(b.Spec.For) == 0 {
		t.Fatalf("binding %+v, response %v", b.Spec, out)
	}
	v, err := provenance.VerifyBundle(b.Spec.Signed, roots)
	if err != nil {
		t.Fatal(err)
	}
	var p controlPredicate
	json.Unmarshal(v.Predicate, &p)
	if p.Action != "bind" || p.Custody != CustodyClaimed || p.Via != "c4-farm" || !strings.Contains(p.Authenticated, "development viewer") {
		t.Errorf("the bind record: %+v", p)
	}

	code, again := bindCall(s, http.MethodPost, "wrangler", noor, nil)
	if code != http.StatusOK || again["existing"] != true || again["record"] != out["record"] {
		t.Errorf("binding again to the same owner: %d %v", code, again)
	}
	if code, got := bindCall(s, http.MethodGet, "artist", "", nil); code != http.StatusOK || got["binding"].(map[string]any)["owner"] != "noor@petrelfinch.example" {
		t.Errorf("GET: %d %v", code, got)
	}

	for _, c := range []struct {
		name, as, body string
		headers        map[string]string
		code           int
		says           string
	}{
		{"another owner", "wrangler", `{"owner": "mallory", "to": "laptop-1/*"}`, nil, http.StatusConflict, "a new binding that follows this one"},
		{"the same owner, run for someone", "wrangler", `{"owner": "noor@petrelfinch.example", "to": "laptop-1/*", "runFor": "studio-x"}`, nil, http.StatusConflict, "owner change"},
		{"an artist", "artist", noor, nil, http.StatusForbidden, "may not create deploymentbindings"},
		{"an agent", "wrangler", noor, map[string]string{"X-C4-Client": "mcp", "X-C4-Agent": "bot"}, http.StatusForbidden, "an agent cannot bind"},
		{"no owner", "wrangler", `{"to": "laptop-1/*"}`, nil, http.StatusBadRequest, "owner:"},
		{"not every deployment", "wrangler", `{"owner": "noor", "to": "laptop-1/node/w1"}`, nil, http.StatusBadRequest, "to: <farm>/*"},
	} {
		code, out := bindCall(s, http.MethodPost, c.as, c.body, c.headers)
		if msg, _ := out["error"].(string); code != c.code || !strings.Contains(msg, c.says) {
			t.Errorf("%s: %d %v, want %d %q", c.name, code, out, c.code, c.says)
		}
	}
}

// A development viewer binds nothing on a HUD the network reaches (it is
// no one there), and a unit never binds a farm.
func TestFarmBindingNeedsTheInstaller(t *testing.T) {
	s, _ := bindFarm(t)
	s.Loopback = false
	if code, out := bindCall(s, http.MethodPost, "wrangler", `{"owner": "noor", "to": "laptop-1/*"}`, nil); code != http.StatusForbidden ||
		!strings.Contains(out["error"].(string), "a development viewer is no one") {
		t.Errorf("a network HUD's development viewer: %d %v", code, out)
	}
	s.Loopback = true
	s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/sh040-light-v007/3", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "sh040-light-v007", Unit: "3", Grant: "project"}}
	if code, out := bindCall(s, http.MethodPost, "unit", `{"owner": "noor", "to": "laptop-1/*"}`, nil); code != http.StatusForbidden ||
		!strings.Contains(out["error"].(string), "is a unit") {
		t.Errorf("a unit: %d %v", code, out)
	}
}

// Who holds every role comes from the farm binding where there is one: its
// owner on a farm they run themselves, no one on a managed farm. Only a
// farm with no binding falls back to --single-person-farm.
func TestSinglePersonFromTheBinding(t *testing.T) {
	s, _ := bindFarm(t)
	s.SinglePerson = "from-the-flag"
	ctx := context.Background()
	if got := s.singlePerson(ctx); got != "from-the-flag" {
		t.Errorf("no binding: %q", got)
	}
	b := &c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: c4v1.FarmBindingName},
		Spec: c4v1.DeploymentBindingSpec{Owner: "noor", To: "laptop-1/*", For: boundKinds, Start: metav1.Now(), By: "noor"}}
	if err := s.Client.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got := s.singlePerson(ctx); got != "noor" {
		t.Errorf("bound to noor: %q", got)
	}
	b.Spec.RunFor = "studio-x"
	if err := s.Client.Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got := s.singlePerson(ctx); got != "" {
		t.Errorf("a managed farm: %q, want no single person", got)
	}
}
