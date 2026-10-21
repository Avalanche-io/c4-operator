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
		v := *s.expand(s.DevViewers[c.as])
		v.DevBy = devOpen
		err := s.authorize(ctx, r, &v, c.ns, c.verb, c.resource, "x")
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

// A development viewer on the installer's secret is the installer,
// graded claimed: whoever holds this machine's token may open it as any
// role, so it is not direct. With open development viewers the viewer is
// unattributed. Both records still say "development viewer".
func TestInstallerDevViewerIsClaimed(t *testing.T) {
	s, decide := decideFarm(t)
	s.SinglePerson = "noor"
	s.InstallerToken = []byte(testInstallerToken)
	for _, c := range []struct {
		open    bool
		headers map[string]string
		custody string
		says    string
	}{
		{false, map[string]string{InstallerHeader: testInstallerToken}, CustodyClaimed, "installer's secret on this machine (claimed): noor"},
		{true, nil, CustodyUnattributed, "not authenticated"},
	} {
		s.DevViewersOpen = c.open
		code, p := decide(decideRequest("wrangler", c.headers))
		if code != http.StatusOK {
			t.Fatalf("open %v: %d", c.open, code)
		}
		if p.Custody != c.custody || !strings.Contains(p.Authenticated, "development viewer") || !strings.Contains(p.Authenticated, c.says) {
			t.Errorf("open %v: custody %q, authenticated %q; want %q and %q", c.open, p.Custody, p.Authenticated, c.custody, c.says)
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

// A wait that names an agent is decided by a unit of the same job running
// that agent, known by its unit token, so the decision is direct. A unit
// running another agent, or the same agent in another job, may not.
func TestAgentWaitDecidedByItsUnit(t *testing.T) {
	s, decide := decideFarm(t)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.Waits[0].Server = "agent:render-bot"
	unit := func(job, agent, grant string) *api.Viewer {
		return &api.Viewer{User: "unit:show-a/" + job + "/3", Projects: []string{"show-a"},
			Unit: &api.UnitViewer{Namespace: "show-a", Job: job, Unit: "3", Grant: grant, Agent: agent}}
	}
	s.DevViewers["its-unit"] = unit("sh040-light-v007", "render-bot", "job")
	s.DevViewers["other-agent"] = unit("sh040-light-v007", "comp-bot", "job")
	s.DevViewers["other-job"] = unit("sh050-comp-v001", "render-bot", "project")
	code, p := decide(decideRequest("its-unit", nil))
	if code != http.StatusOK || p.Custody != CustodyDirect || p.Actor != "unit:show-a/sh040-light-v007/3" {
		t.Errorf("the agent's own unit: %d, custody %q, actor %q", code, p.Custody, p.Actor)
	}
	for _, as := range []string{"other-agent", "other-job"} {
		if code, _ := decide(decideRequest(as, nil)); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", as, code)
		}
	}
}

// On a single-person farm the farm's owner holds every role: signed in as
// themselves, they decide a wait addressed to a role, recorded as standing
// in for that role with the farm's owner named, and they have every
// role's rights. On a shared farm the same sign-in is just its own role.
func TestOwnerHoldsEveryRole(t *testing.T) {
	s, decide := decideFarm(t)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.Waits[0].Server = "role:wrangler"
	s.DevViewers["noor"] = &api.Viewer{User: "noor", Role: "artist", Projects: []string{"show-a"}}
	s.DevViewers["wrangler"].Role = "wrangler" // as --hud-dev-viewers gives a viewer named for a role
	may := func(ns, resource string) bool {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		v := *s.DevViewers["noor"]
		v.DevBy = devOpen // as the open test farm resolves ?as=
		return s.authorize(context.Background(), r, &v, ns, "patch", resource, "x") == nil
	}
	approve := func() int {
		if !may("", "capacityrequests") {
			return http.StatusForbidden
		}
		return http.StatusOK
	}
	// every gate a role holds, in order: approving a burst, disabling a
	// node, revoking another's grant, binding the farm, publishing
	gates := func() []bool {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/w1/disable?as=noor", strings.NewReader(`{"disabled": true}`)))
		g := &c4v1.SubmitGrant{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "dana-github"},
			Spec: c4v1.SubmitGrantSpec{Person: "dana", Tracker: "github", Until: metav1.NewTime(time.Now().Add(time.Hour))}}
		s.Client.Delete(context.Background(), g)
		if err := s.Client.Create(context.Background(), g); err != nil {
			t.Fatal(err)
		}
		revoke := httptest.NewRecorder()
		s.Handler().ServeHTTP(revoke, httptest.NewRequest(http.MethodDelete, "/api/v1/grants/show-a/dana-github?as=noor", nil))
		return []bool{may("", "capacityrequests"), w.Code != http.StatusForbidden && may("", "nodes"), revoke.Code != http.StatusForbidden && may("show-a", "submitgrants"),
			may("", "deploymentbindings"), mayPublish(s.DevViewers["noor"], true, s.everyRole(context.Background(), s.DevViewers["noor"])) == ""}
	}

	if code, _ := decide(decideRequest("noor", nil)); code != http.StatusForbidden {
		t.Errorf("an artist decides the wrangler's wait on a shared farm: %d", code)
	}
	for i, ok := range gates() {
		if ok {
			t.Errorf("gate %d is open to an artist on a shared farm", i)
		}
	}
	if code := approve(); code != http.StatusForbidden {
		t.Errorf("an artist approves a burst on a shared farm: %d", code)
	}
	if code, p := decide(decideRequest("wrangler", nil)); code != http.StatusOK || p.For != "" {
		t.Errorf("the wrangler decides a wait addressed to their role: %d, for %q", code, p.For)
	}

	s.SinglePerson = "noor"
	code, p := decide(decideRequest("noor", nil))
	if code != http.StatusOK {
		t.Fatalf("the owner decides on a single-person farm: %d", code)
	}
	if p.Actor != "noor" || p.For != "role:wrangler" || p.Custody != CustodyStandIn || !strings.Contains(p.Authorization, "noor, the farm's owner") {
		t.Errorf("the owner's record: actor %q, for %q, custody %q, authorization %q", p.Actor, p.For, p.Custody, p.Authorization)
	}
	if code := approve(); code == http.StatusForbidden {
		t.Error("the owner may not approve a burst on a single-person farm")
	}
	for i, ok := range gates() {
		if !ok {
			t.Errorf("gate %d is closed to the owner on a single-person farm", i)
		}
	}

	// bound as noor's alone, on a HUD the installer's token does not
	// guard: no one holds every role, whatever --single-person-farm says
	b := &c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: c4v1.FarmBindingName},
		Spec: c4v1.DeploymentBindingSpec{Owner: "noor", To: "c4-local/*", OnePerson: true, For: []string{"task/v1"}, Start: metav1.Now(), By: "noor"}}
	if err := s.Client.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	for i, ok := range gates() {
		if ok {
			t.Errorf("gate %d is open to a one-person binding's owner on an unguarded HUD", i)
		}
	}
}
