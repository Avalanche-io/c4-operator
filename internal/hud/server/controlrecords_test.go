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

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
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
)

// A development viewer has no RBAC behind it, so its role's rights stand
// in for what a studio would bind: an artist may not approve a burst or
// disable a node, facilities may not touch jobs, and no one acts in a
// project they don't see.
func TestDevViewerRoleRights(t *testing.T) {
	s := farm(t)
	s.DevViewers["producer"] = &api.Viewer{User: "pat", Role: "producer", Projects: []string{"show-a"}}
	s.DevViewers["facilities"] = &api.Viewer{User: "fay", Role: "facilities", Projects: []string{"show-a", "show-b"}, All: true}
	ctx := context.Background()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	for _, c := range []struct {
		as, ns, verb, resource string
		ok                     bool
	}{
		{"artist", "", "patch", "capacityrequests", false},
		{"producer", "", "patch", "capacityrequests", true},
		{"wrangler", "", "patch", "capacityrequests", true},
		{"artist", "", "patch", "nodes", false},
		{"facilities", "", "patch", "nodes", true},
		{"facilities", "show-a", "patch", "workjobs", false},
		{"artist", "show-a", "patch", "workjobs", true},
		{"artist", "show-b", "patch", "workjobs", false},
		{"artist", "show-a", "delete", "submitgrants", false},
		{"producer", "show-a", "delete", "submitgrants", true},
	} {
		v := s.expand(s.DevViewers[c.as])
		err := s.authorize(ctx, r, v, c.ns, c.verb, c.resource, "x")
		if (err == nil) != c.ok {
			t.Errorf("%s %s %s in %q: %v, want allowed %v", c.as, c.verb, c.resource, c.ns, err, c.ok)
		}
	}
}

// An unknown ?as= is refused, naming the viewers there are, instead of
// quietly making the caller a wrangler. No ?as= at all is still the
// wrangler.
func TestUnknownDevViewer(t *testing.T) {
	s := farm(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?as=nobody", nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "artist, wrangler") {
		t.Errorf("?as=nobody: %d %s, want 400 naming artist, wrangler", w.Code, w.Body.String())
	}
	if code := get(t, s, "/api/v1/snapshot", nil); code != http.StatusOK {
		t.Errorf("no ?as=: %d", code)
	}
}

// decideFarm is a farm whose HUD signs control records into a store, with
// dana's wait on unit 8 of sh040-light-v007; decide posts a decision and
// returns the signed record.
func decideFarm(t *testing.T) (*Server, func(r *http.Request) (int, controlPredicate)) {
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
	rec := s.m.byKey["show-a/sh040-light-v007"]
	since := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-7", "waits": "8"}
	rec.job.Status.Waits = []c4v1.WorkWait{{Unit: 8, Name: "ruling", Server: "person:dana", Since: &since, Blocking: true}}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	return s, func(r *http.Request) (int, controlPredicate) {
		t.Helper()
		s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy()).Build()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		id, _ := out["record"].(string)
		if w.Code != http.StatusOK || id == "" {
			return w.Code, controlPredicate{}
		}
		cid, err := c4.Parse(id)
		if err != nil {
			t.Fatal(err)
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
		return w.Code, p
	}
}

func decideRequest(as string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/decide?as="+as,
		strings.NewReader(`{"unit": "ruling", "do": "approve"}`))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// The signed record names the client that carried the act (the web app,
// MCP, c4 farm or c4-sync) and the agent it says acts, graded claimed: the
// headers are the client's word, the actor is not.
func TestControlRecordNamesItsClient(t *testing.T) {
	_, decide := decideFarm(t)
	for _, c := range []struct {
		headers     map[string]string
		via, agent  string
		agentGraded bool
	}{
		{nil, "hud", "", false},
		{map[string]string{"X-C4-Client": "c4-farm"}, "c4-farm", "", false},
		{map[string]string{"X-C4-Client": "c4-sync", "X-C4-Agent": "c4-sync"}, "c4-sync", "c4-sync", true},
		{map[string]string{"X-C4-Client": "mcp", "X-C4-Agent": "wrangler-bot"}, "mcp", "wrangler-bot", true},
		{map[string]string{"X-C4-Client": "curl"}, "unknown client (X-C4-Client: curl)", "", false},
	} {
		code, p := decide(decideRequest("wrangler", c.headers))
		if code != http.StatusOK && c.agent == "wrangler-bot" {
			// an agent may not decide a wait that does not name it
			continue
		}
		if code != http.StatusOK {
			t.Fatalf("%v: %d", c.headers, code)
		}
		if p.Via != c.via || p.Agent != c.agent {
			t.Errorf("%v: via %q, agent %q; want %q, %q", c.headers, p.Via, p.Agent, c.via, c.agent)
		}
		if c.agentGraded && p.AgentGrade != CustodyClaimed {
			t.Errorf("%v: the agent is graded %q, want claimed", c.headers, p.AgentGrade)
		}
	}
}

// A unit's control record says how the unit was let act (its tool's
// grant, checked by the HUD), not a SubjectAccessReview that never ran.
func TestUnitControlRecordIsTruthful(t *testing.T) {
	s := farm(t)
	u := &api.Viewer{User: "unit:show-a/sh040-light-v007/3",
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "sh040-light-v007", Unit: "3", Grant: "job"}}
	authz := s.authorization(u, "patch", "workjobs", "show-a", "sh040-light-v007")
	if strings.HasPrefix(authz, "SubjectAccessReview") || !strings.Contains(authz, "grants c4: job") {
		t.Errorf("a unit's authorization reads %q", authz)
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer "+unitToken("sh040-light-v007-u3"))
	if by := s.authenticatedBy(r, u); !strings.Contains(by, "pod token") {
		t.Errorf("a unit is authenticated by %q", by)
	}
}

// A development viewer on a HUD that listens only on loopback is the
// installer, graded claimed: any local process can open it as any role,
// so it is not direct. On a HUD the network reaches, the viewer is
// unattributed. Both records still say "development viewer".
func TestLoopbackDevViewerIsClaimed(t *testing.T) {
	s, decide := decideFarm(t)
	s.SinglePerson = "joshua"
	for _, c := range []struct {
		loopback bool
		custody  string
		says     string
	}{
		{true, CustodyClaimed, "claimed by the installer, joshua"},
		{false, CustodyUnattributed, "not authenticated"},
	} {
		s.Loopback = c.loopback
		code, p := decide(decideRequest("wrangler", nil))
		if code != http.StatusOK {
			t.Fatalf("loopback %v: %d", c.loopback, code)
		}
		if p.Custody != c.custody || !strings.Contains(p.Authenticated, "development viewer") || !strings.Contains(p.Authenticated, c.says) {
			t.Errorf("loopback %v: custody %q, authenticated %q; want %q and %q", c.loopback, p.Custody, p.Authenticated, c.custody, c.says)
		}
	}
}

// An agent's wait decided by the agent the client names (X-C4-Agent) rests
// on that header alone: the person behind the token is authenticated, the
// agent is the client's word, so the decision is graded claimed.
func TestAgentDecisionIsClaimed(t *testing.T) {
	s, decide := decideFarm(t)
	s.DevViewers = nil
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: "dana"}}}, nil
	})
	kube.PrependReactor("create", "subjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	s.Kube = kube
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.Waits[0].Server = "agent:wrangler-bot"
	r := decideRequest("", map[string]string{"X-C4-Client": "mcp", "X-C4-Agent": "wrangler-bot", "Authorization": "Bearer " + apiToken("dana", "")})
	code, p := decide(r)
	if code != http.StatusOK {
		t.Fatalf("the agent decides its wait: %d", code)
	}
	if p.Custody != CustodyClaimed || p.Agent != "wrangler-bot" || p.AgentGrade != CustodyClaimed {
		t.Errorf("custody %q, agent %q (%q); want claimed, wrangler-bot (claimed)", p.Custody, p.Agent, p.AgentGrade)
	}
}

// A requeue, an allowed node and an acceptance of a validated input are
// written as requests that name their signed record, with the record's own
// token, so the queue can carry each out on its record alone.
func TestRequeueAllowAcceptNameTheirRecords(t *testing.T) {
	s, post := decideFarm(t)
	for _, c := range []struct {
		path, body, key, action string
	}{
		{"requeue", `{"units": [7]}`, c4v1.AnnotationRequeue, "requeue"},
		{"blacklist", `{"node": "w1", "remove": true}`, c4v1.AnnotationAllowNodes, "allow-node"},
		{"accept-validated", `{"accept": true}`, c4v1.AnnotationAcceptValidated, "accept-validated"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/"+c.path+"?as=wrangler", strings.NewReader(c.body))
		code, p := post(r)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", c.path, code)
		}
		var j c4v1.WorkJob
		if err := s.Client.Get(context.Background(), k8stypes.NamespacedName{Namespace: "show-a", Name: "sh040-light-v007"}, &j); err != nil {
			t.Fatal(err)
		}
		var reqs []map[string]any
		if err := json.Unmarshal([]byte(j.Annotations[c.key]), &reqs); err != nil || len(reqs) != 1 {
			t.Fatalf("%s: %s is %q", c.path, c.key, j.Annotations[c.key])
		}
		req := p.Change.(map[string]any)["request"].(map[string]any)
		if p.Action != c.action || reqs[0]["record"] == "" || reqs[0]["token"] != req["token"] || reqs[0]["actor"] != "dana" {
			t.Errorf("%s: request %v, record %s %v", c.path, reqs[0], p.Action, req)
		}
	}
}
