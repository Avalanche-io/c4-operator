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

// A person's wait is decided by the person it names, a party's by a member
// of the party, an agent's only by that agent. A farm admin does not stand
// in by default; only a farm at allow under a project at allow lets them,
// and then the signed record names both. Units never decide a person's
// wait or supply another unit's question, and an agent never decides a
// wait that does not name it.
func TestWhoMayDecide(t *testing.T) {
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
	projects := []string{"show-a"}
	s.DevViewers["admin"] = &api.Viewer{User: "ines", Role: "farm-admin", Projects: projects}
	s.DevViewers["ml"] = &api.Viewer{User: "kim", Parties: []string{"ml"}, Projects: projects}
	s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/sh040-light-v007/3", Projects: projects,
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "sh040-light-v007", Unit: "3", Grant: "job"}}

	rec := s.m.byKey["show-a/sh040-light-v007"]
	since := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-6", "failed": "7", "waits": "8-9"}
	rec.job.Status.Units = c4v1.UnitCounts{Total: 10, Succeeded: 7, Failed: 1}
	rec.job.Status.Needs = map[string]c4v1.WorkNeeds{"7": {Text: "the delivery frame rate", At: since}}
	rec.job.Status.Waits = []c4v1.WorkWait{
		{Unit: 8, Name: "ruling", Server: "person:dana", Since: &since, Blocking: true},
		{Unit: 9, Name: "registry", Server: "party:ml", Since: &since, Blocking: true},
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	reset := func() {
		s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy()).Build()
	}
	reset()
	var project *c4v1.Project
	for i := range s.m.projects {
		if s.m.projects[i].Name == "show-a" {
			project = &s.m.projects[i]
		}
	}
	if project == nil {
		t.Fatal("no project show-a")
	}
	setPolicy := func(farm, proj string) {
		s.StandIn = farm
		project.Spec.Work = &c4v1.ProjectWork{}
		if proj != "" {
			project.Spec.Work.Policy = &c4v1.ProjectPolicy{Values: map[string]string{KeyStandIn: proj}}
		}
	}
	post := func(as, path, body string, agent bool) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/"+path+"?as="+as, strings.NewReader(body))
		if agent {
			r.Header.Set("X-C4-Client", "mcp")
			r.Header.Set("X-C4-Agent", "wrangler-bot")
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	ruling := `{"unit": "ruling", "do": "approve"}`
	registry := `{"unit": "registry", "do": "approve"}`
	refused := []struct {
		name, farm, project, as, path, body string
		agent                               bool
		says                                string
	}{
		{"another person", "", "", "artist", "decide", ruling, false, "waits for dana's decision; maya may not decide it"},
		{"an admin, by default", "", "", "admin", "decide", ruling, false, "decide.standIn: deny, from the farm"},
		{"an admin, the project allowing and the farm denying", "deny", "allow", "admin", "decide", ruling, false, "decide.standIn: deny, from the farm"},
		{"an admin, the farm allowing and the project silent", "allow", "", "admin", "decide", ruling, false, "decide.standIn: deny, from the project show-a"},
		{"someone else, stand-in allowed", "allow", "allow", "artist", "decide", ruling, false, "only role:farm-admin may stand in"},
		{"not a member of the party", "", "", "wrangler", "decide", registry, false, "waits for a member of ml"},
		{"a unit", "allow", "allow", "unit", "decide", ruling, false, "is a unit; only a person may decide"},
		{"a unit supplying a sibling", "", "", "unit", "supply", `{"unit": 7, "text": "24 fps"}`, false, "is a unit; only a person may supply"},
		{"an agent, for the person", "", "", "wrangler", "decide", ruling, true, "agent wrangler-bot may not decide a wait that does not name it"},
	}
	for _, c := range refused {
		setPolicy(c.farm, c.project)
		code, out := post(c.as, c.path, c.body, c.agent)
		if msg, _ := out["error"].(string); code != http.StatusForbidden || !strings.Contains(msg, c.says) {
			t.Errorf("%s: %d %v, want 403 %q", c.name, code, out, c.says)
		}
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	record := func(out map[string]any) controlPredicate {
		t.Helper()
		cid, err := c4.Parse(out["record"].(string))
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
		return p
	}

	// The person decides their own wait, directly (unattributed: a
	// development viewer on a HUD the network reaches).
	setPolicy("", "")
	if code, out := post("wrangler", "decide", ruling, false); code != http.StatusOK {
		t.Fatalf("dana decides: %d %v", code, out)
	} else if p := record(out); p.Custody != CustodyUnattributed || p.For != "" || p.Actor != "dana" {
		t.Fatalf("dana's record: %+v", p)
	}
	// A member of the party decides its wait.
	reset()
	if code, out := post("ml", "decide", registry, false); code != http.StatusOK {
		t.Fatalf("a member of ml decides: %d %v", code, out)
	}
	// With the farm and the project allowing, an admin stands in, and the
	// record says for whom and by whom.
	reset()
	setPolicy("allow", "allow")
	code, out := post("admin", "decide", ruling, false)
	if code != http.StatusOK {
		t.Fatalf("an admin stands in: %d %v", code, out)
	}
	if p := record(out); p.Custody != CustodyStandIn || p.For != "person:dana" || p.Actor != "ines" {
		t.Fatalf("stand-in record: %+v", p)
	}

	// On a single-person farm the installer plays every role: they decide
	// a wait addressed to one of them, recorded for that role, by them.
	// On a shared farm the same decision is refused.
	rec.job.Status.Waits = append(rec.job.Status.Waits, c4v1.WorkWait{Unit: 6, Name: "triage", Server: "person:wrangler", Since: &since, Blocking: true})
	reset()
	setPolicy("", "")
	triage := `{"unit": "triage", "do": "approve"}`
	if code, out := post("artist", "decide", triage, false); code != http.StatusForbidden {
		t.Fatalf("maya decides the wrangler's wait on a shared farm: %d %v", code, out)
	}
	s.SinglePerson = "maya"
	code, out = post("artist", "decide", triage, false)
	if code != http.StatusOK {
		t.Fatalf("the installer decides on a local farm: %d %v", code, out)
	}
	if p := record(out); p.Custody != CustodyStandIn || p.For != "role:wrangler" || p.Actor != "maya" {
		t.Fatalf("local farm record: %+v", p)
	}
	if code, out := post("wrangler", "decide", `{"unit": "ruling", "do": "approve"}`, false); code != http.StatusOK {
		t.Fatalf("dana still decides her own: %d %v", code, out)
	}
	s.SinglePerson = ""
	reset()
	setPolicy("allow", "allow")
	if code, out := post("admin", "decide", ruling, false); code != http.StatusOK {
		t.Fatalf("stand-in again: %d %v", code, out)
	}

	// Each request names its record, and the record its request's token,
	// which is what the queue checks before carrying it out.
	if code, out := post("wrangler", "ack", `{"units": "7", "kind": "acknowledge"}`, false); code != http.StatusOK {
		t.Fatalf("ack: %d %v", code, out)
	}
	var live c4v1.WorkJob
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "sh040-light-v007"}, &live); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{c4v1.AnnotationDecide, c4v1.AnnotationAcks} {
		var reqs []struct{ Token, Record string }
		if err := json.Unmarshal([]byte(live.Annotations[key]), &reqs); err != nil || len(reqs) != 1 || reqs[0].Record == "" {
			t.Fatalf("%s: %v %s", key, err, live.Annotations[key])
		}
		p := record(map[string]any{"record": reqs[0].Record})
		if req, _ := p.Change.(map[string]any)["request"].(map[string]any); req["token"] != reqs[0].Token {
			t.Errorf("%s: the record names token %v, the request %s", key, req["token"], reqs[0].Token)
		}
	}
}

// The rule itself, for the waits a relay and an agent bring.
func TestMayControl(t *testing.T) {
	cases := []struct {
		control, server string
		who             Decider
		ok              bool
	}{
		{"decide", "person:dana", Decider{User: "dana"}, true},
		{"decide", "person:dana", Decider{User: "ines", Role: "farm-admin"}, false},
		{"decide", "person:", Decider{User: "maya"}, true}, // a review that named no reviewer
		{"decide", "person:", Decider{User: "maya", Agent: "bot"}, false},
		{"decide", "party:ml", Decider{User: "party:ml", Parties: []string{"ml"}}, true},
		{"decide", "party:ml", Decider{User: "kim", Parties: []string{"vfx"}}, false},
		{"decide", "agent:bot", Decider{User: "dana", Agent: "bot"}, true},
		{"decide", "agent:bot", Decider{User: "dana", Agent: "other"}, false},
		{"decide", "person:dana", Decider{User: "dana", Unit: true}, false},
		{"supply", "", Decider{User: "dana"}, true},
		{"supply", "", Decider{User: "dana", Agent: "bot"}, true},
		{"supply", "", Decider{User: "unit:a/b/1", Unit: true}, false},
		{"withdraw", "", Decider{User: "unit:a/b/1", Unit: true}, false},
	}
	for _, c := range cases {
		if err := MayControl(c.control, c.server, c.who); (err == nil) != c.ok {
			t.Errorf("%s %s by %+v: %v, want ok %v", c.control, c.server, c.who, err, c.ok)
		}
	}
}
