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

// bindFarm is a HUD that signs, its development viewers guarded by an
// installer token, with no binding yet.
func bindFarm(t *testing.T) (*Server, *x509.CertPool) {
	t.Helper()
	s := farm(t)
	s.DevViewersOpen, s.InstallerToken = false, []byte(testInstallerToken)
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

// bindCall calls the binding route as a development viewer, with the
// installer token unless headers set InstallerHeader (empty: none).
func bindCall(s *Server, method, as, body string, headers map[string]string) (int, map[string]any) {
	r := httptest.NewRequest(method, "/api/v1/farm/binding?as="+as, strings.NewReader(body))
	r.Header.Set(InstallerHeader, testInstallerToken)
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
// the binding is a signed control record (bind), graded claimed on the
// installer's secret; binding again to the same owner changes nothing; another
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
	if ch, _ := p.Change.(map[string]any); ch == nil || ch["onePerson"] != false || b.Spec.OnePerson {
		t.Errorf("a binding that does not say one person: change %v, spec %+v", p.Change, b.Spec)
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
		{"the same owner, now one person", "wrangler", `{"owner": "noor@petrelfinch.example", "to": "laptop-1/*", "onePerson": true}`, nil, http.StatusConflict, "owner change"},
		{"one person, run for someone", "wrangler", `{"owner": "noor", "to": "laptop-1/*", "runFor": "studio-x", "onePerson": true}`, nil, http.StatusBadRequest, "is not one person's"},
		{"an artist", "artist", noor, nil, http.StatusForbidden, "may not create deploymentbindings"},
		{"an agent", "wrangler", noor, map[string]string{"X-C4-Client": "mcp", "X-C4-Agent": "bot"}, http.StatusForbidden, "an agent cannot bind"},
		{"an agent named from c4 farm", "wrangler", noor, map[string]string{"X-C4-Client": "c4-farm", "X-C4-Agent": "bot"}, http.StatusForbidden, "an agent cannot bind"},
		{"no owner", "wrangler", `{"to": "laptop-1/*"}`, nil, http.StatusBadRequest, "owner:"},
		{"not every deployment", "wrangler", `{"owner": "noor", "to": "laptop-1/node/w1"}`, nil, http.StatusBadRequest, "to: <farm>/*"},
	} {
		code, out := bindCall(s, http.MethodPost, c.as, c.body, c.headers)
		if msg, _ := out["error"].(string); code != c.code || !strings.Contains(msg, c.says) {
			t.Errorf("%s: %d %v, want %d %q", c.name, code, out, c.code, c.says)
		}
	}
}

// A development viewer without the installer's secret binds nothing (it
// is no one), and a unit never binds a farm.
func TestFarmBindingNeedsTheInstaller(t *testing.T) {
	s, _ := bindFarm(t)
	s.DevViewersOpen, s.InstallerToken = true, nil
	if code, out := bindCall(s, http.MethodPost, "wrangler", `{"owner": "noor", "to": "laptop-1/*"}`, map[string]string{InstallerHeader: ""}); code != http.StatusForbidden ||
		!strings.Contains(out["error"].(string), "a development viewer is no one") {
		t.Errorf("an open development viewer: %d %v", code, out)
	}
	s.DevViewersOpen, s.InstallerToken = false, []byte(testInstallerToken)
	s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/sh040-light-v007/3", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "sh040-light-v007", Unit: "3", Grant: "project"}}
	if code, out := bindCall(s, http.MethodPost, "unit", `{"owner": "noor", "to": "laptop-1/*"}`, nil); code != http.StatusForbidden ||
		!strings.Contains(out["error"].(string), "is a unit") {
		t.Errorf("a unit: %d %v", code, out)
	}
}

// Who holds every role comes from the farm binding where there is one:
// its owner only where the installer bound the farm as one person's
// (onePerson), no one on a farm merely bound to someone or run for
// someone else. Only a farm with no binding falls back to
// --single-person-farm.
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
	if got, owner := s.singlePerson(ctx), b.Spec.SingleOwner(); got != "" || owner != "" {
		t.Errorf("bound to noor, not as one person: %q, SingleOwner %q, want no single person", got, owner)
	}
	b.Spec.OnePerson = true
	if err := s.Client.Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got, owner := s.singlePerson(ctx), b.Spec.SingleOwner(); got != "noor" || owner != "noor" {
		t.Errorf("bound to noor as one person: %q, SingleOwner %q", got, owner)
	}
	b.Spec.RunFor = "studio-x"
	if err := s.Client.Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got, owner := s.singlePerson(ctx), b.Spec.SingleOwner(); got != "" || owner != "" {
		t.Errorf("a managed farm: %q, SingleOwner %q, want no single person", got, owner)
	}
}

// A shared farm an admin bound to lee is not lee's alone: lee decides
// neither a role's wait nor another person's, and stand-in stays the
// farm's policy. Bound as one person's, lee holds every role, so decides
// a role's wait for that role, and their own; another person's wait is
// still that person's.
func TestBoundIsNotOnePerson(t *testing.T) {
	s, _ := bindFarm(t)
	ctx := context.Background()
	lee := Decider{User: "lee", Role: "farm-admin"}
	b := &c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: c4v1.FarmBindingName},
		Spec: c4v1.DeploymentBindingSpec{Owner: "lee", To: "studio-1/*", For: boundKinds, Start: metav1.Now(), By: "ivo"}}
	if err := s.Client.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, server := range []string{"person:dana", "person:wrangler"} {
		if as, err := s.mayDecide("show-a", server, lee); err == nil || !strings.Contains(err.Error(), "decide.standIn: deny") {
			t.Errorf("a shared farm bound to lee: lee decides %s (as %q, %v)", server, as, err)
		}
	}
	if as, err := s.mayDecide("show-a", "person:lee", lee); err != nil || as != "" {
		t.Errorf("lee's own wait: %q %v", as, err)
	}

	b.Spec.OnePerson = true
	if err := s.Client.Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	if as, err := s.mayDecide("show-a", "person:wrangler", lee); err != nil || as != "role:wrangler" {
		t.Errorf("one person, a role's wait: %q %v", as, err)
	}
	if as, err := s.mayDecide("show-a", "person:lee", lee); err != nil || as != "" {
		t.Errorf("one person, their own wait: %q %v", as, err)
	}
	if as, err := s.mayDecide("show-a", "person:dana", lee); err == nil {
		t.Errorf("one person decides dana's own wait, as %q", as)
	}
	if as, err := s.mayDecide("show-a", "person:wrangler", Decider{User: "dana", Role: "farm-admin"}); err == nil {
		t.Errorf("someone else takes the one person's roles, as %q", as)
	}
}

// The installer binding a farm as one person's says so in the signed
// record, and the binding carries it.
func TestOnePersonBinding(t *testing.T) {
	s, roots := bindFarm(t)
	code, out := bindCall(s, http.MethodPost, "wrangler", `{"owner": "noor", "to": "c4-local/*", "onePerson": true}`, nil)
	if code != http.StatusOK || out["binding"].(map[string]any)["onePerson"] != true {
		t.Fatalf("bind as one person: %d %v", code, out)
	}
	var b c4v1.DeploymentBinding
	if err := s.Client.Get(context.Background(), types.NamespacedName{Name: c4v1.FarmBindingName}, &b); err != nil {
		t.Fatal(err)
	}
	v, err := provenance.VerifyBundle(b.Spec.Signed, roots)
	if err != nil {
		t.Fatal(err)
	}
	var p controlPredicate
	json.Unmarshal(v.Predicate, &p)
	if ch, _ := p.Change.(map[string]any); !b.Spec.OnePerson || ch == nil || ch["onePerson"] != true {
		t.Fatalf("the one-person binding: change %v, spec %+v", p.Change, b.Spec)
	}
	if got := s.singlePerson(context.Background()); got != "noor" {
		t.Errorf("single person: %q", got)
	}
	if code, again := bindCall(s, http.MethodPost, "wrangler", `{"owner": "noor", "to": "c4-local/*", "onePerson": true}`, nil); code != http.StatusOK || again["existing"] != true {
		t.Errorf("binding again as one person: %d %v", code, again)
	}
	if code, shared := bindCall(s, http.MethodPost, "wrangler", `{"owner": "noor", "to": "c4-local/*"}`, nil); code != http.StatusConflict {
		t.Errorf("rebinding without one person changed nothing: %d %v", code, shared)
	}
}
