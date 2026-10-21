package server

import (
	"bytes"
	"context"
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
	ktesting "k8s.io/client-go/testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// grantFarm is submitFarm with a signer (so grants carry records), a
// Tracker "github" in show-a, c4-sync as a development viewer, and lee, a
// second artist on show-a.
func grantFarm(t *testing.T) *Server {
	t.Helper()
	s := submitFarm(t)
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = provenance.NewSigner(context.Background(), provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.DevViewers[c4v1.SyncDevViewer] = &api.Viewer{User: c4v1.SyncDevViewer, Projects: []string{"show-a"}}
	s.DevViewers["lee"] = &api.Viewer{User: "lee", Projects: []string{"show-a"}}
	if err := s.Client.Create(context.Background(), &c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "github"},
		Spec: c4v1.TrackerSpec{Kind: "github"}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func grants(t *testing.T, s *Server, query string) []api.Grant {
	t.Helper()
	var out api.GrantList
	if code := call(t, s, http.MethodGet, "/api/v1/grants"+query, nil, &out); code != http.StatusOK {
		t.Fatalf("grants%s: %d", query, code)
	}
	return out.Grants
}

// A person grants a tracker as themselves, sees it, renews it and revokes
// it; each is a signed record. Nobody else revokes it but a project admin,
// and a tracker's sync cannot grant itself.
func TestGrantGiveListRevoke(t *testing.T) {
	s := grantFarm(t)
	kube := kfake.NewSimpleClientset()
	// The fake does not honour generateName as the API server does.
	n := 0
	kube.PrependReactor("create", "events", func(a ktesting.Action) (bool, runtime.Object, error) {
		if ev := a.(ktesting.CreateAction).GetObject().(*corev1.Event); ev.Name == "" {
			n++
			ev.Name = fmt.Sprintf("%s%d", ev.GenerateName, n)
		}
		return false, nil, nil
	})
	s.Kube = kube
	var out api.GrantResponse
	req := api.GrantRequest{Project: "show-a", Tracker: "github", Pools: []string{"cpu"}, Until: "7d"}
	if code := call(t, s, http.MethodPost, "/api/v1/grants?as=artist", req, &out); code != http.StatusOK {
		t.Fatalf("grant: %d %+v", code, out)
	}
	g := out.Grant
	if g.Person != "maya" || g.Tracker != "github" || !g.Active || g.Record == "" || out.Record != g.Record ||
		g.Until.Sub(time.Now()) < 6*24*time.Hour || !strings.Contains(out.Message, "maya lets tracker github submit work for them (in cpu)") {
		t.Fatalf("grant: %+v", out)
	}
	if mine := grants(t, s, "?as=artist"); len(mine) != 1 || mine[0].Name != g.Name {
		t.Fatalf("maya's grants: %+v", mine)
	}
	if none := grants(t, s, "?as=lee"); len(none) != 0 {
		t.Fatalf("lee's own grants: %+v", none)
	}
	if who := grants(t, s, "?as=lee&tracker=show-a/github"); len(who) != 1 || who[0].Person != "maya" {
		t.Fatalf("who granted github: %+v", who)
	}
	if hidden := grants(t, s, "?as=artist&person=maya&tracker=show-b/github"); len(hidden) != 0 {
		t.Fatalf("filtered: %+v", hidden)
	}

	// Granting again renews the same grant, with a new record.
	req.Until = "14d"
	var again api.GrantResponse
	if code := call(t, s, http.MethodPost, "/api/v1/grants?as=artist", req, &again); code != http.StatusOK ||
		again.Grant.Name != g.Name || again.Record == g.Record || again.Grant.Until.Sub(g.Until) < 6*24*time.Hour {
		t.Fatalf("renew: %d %+v", code, again)
	}

	path := "/api/v1/grants/show-a/" + g.Name
	if code := call(t, s, http.MethodDelete, path+"?as=lee", nil, nil); code != http.StatusForbidden {
		t.Fatalf("lee revoking maya's grant: %d", code)
	}
	var rev api.GrantResponse
	if code := call(t, s, http.MethodDelete, path+"?as=artist", nil, &rev); code != http.StatusOK ||
		rev.Grant.Active || rev.Grant.Revoked == nil || rev.Grant.Revoked.By != "maya" || rev.Record == "" || !strings.Contains(rev.Grant.Why, "revoked") {
		t.Fatalf("revoke: %d %+v", code, rev)
	}
	if code := call(t, s, http.MethodDelete, path+"?as=artist", nil, nil); code != http.StatusConflict {
		t.Fatalf("revoking twice: %d", code)
	}
	// A farm-wide viewer stands in for a project admin: revoking, yes.
	if code := call(t, s, http.MethodPost, "/api/v1/grants?as=artist", req, nil); code != http.StatusOK {
		t.Fatalf("grant again: %d", code)
	}
	if code := call(t, s, http.MethodDelete, path+"?as=wrangler", nil, nil); code != http.StatusOK {
		t.Fatalf("an admin revoking: %d", code)
	}
	// The book shows each grant and revocation, by whom, with its record.
	var book api.Book
	if code := call(t, s, http.MethodGet, "/api/v1/book?as=artist", nil, &book); code != http.StatusOK {
		t.Fatalf("book: %d", code)
	}
	kinds := map[string]int{}
	for _, e := range book.Entries {
		if e.Object.Type == "grant" && e.Object.Grant == "show-a/"+g.Name && e.Record != "" {
			kinds[e.Kind]++
		}
	}
	if kinds["grant"] != 3 || kinds["revoke"] != 2 {
		t.Fatalf("book: %v in %+v", kinds, book.Entries)
	}

	bad := []struct {
		as   string
		req  api.GrantRequest
		code int
	}{
		{c4v1.SyncDevViewer, api.GrantRequest{Project: "show-a", Tracker: "github"}, http.StatusForbidden},
		{"artist", api.GrantRequest{Project: "show-a", Tracker: "jira"}, http.StatusNotFound},
		{"artist", api.GrantRequest{Project: "show-a", Tracker: "github", Until: "200d"}, http.StatusBadRequest},
		{"artist", api.GrantRequest{Project: "show-a", Tracker: "github", Until: "2001-01-01T00:00:00Z"}, http.StatusBadRequest},
		{"artist", api.GrantRequest{Project: "show-a", Tracker: "github", Until: "soon"}, http.StatusBadRequest},
		{"artist", api.GrantRequest{Project: "show-b", Tracker: "github"}, http.StatusNotFound},
	}
	for _, b := range bad {
		if code := call(t, s, http.MethodPost, "/api/v1/grants?as="+b.as, b.req, nil); code != b.code {
			t.Errorf("%s %+v: %d, want %d", b.as, b.req, code, b.code)
		}
	}
}

// Work a tracker submits for a person: only its sync, naming the tracker,
// under the person's grant, within its pools and classes; the person owns
// the job and it carries the grant. Never the caller's own job instead.
func TestSubmitForUnderAGrant(t *testing.T) {
	s := grantFarm(t)
	s.DryRun = func(context.Context, *c4v1.WorkJob, string) (api.SubmitEstimate, error) {
		return api.SubmitEstimate{}, nil
	}
	spec := func(pools ...string) json.RawMessage {
		b, _ := json.Marshal(c4v1.WorkJobSpec{Class: "graph", Pools: pools, Params: map[string]string{"root": "plan"},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-block"}}}}})
		return b
	}
	n := 0
	submit := func(as string, edit func(*api.SubmitRequest)) (int, string) {
		n++
		req := api.SubmitRequest{Project: "show-a", Name: "github-7-" + string(rune('a'+n)), Spec: spec("cpu"),
			TrackerRef: "github/Issue/7", For: "maya"}
		edit(&req)
		var out map[string]any
		code := call(t, s, http.MethodPost, "/api/v1/jobs?as="+as, req, &out)
		msg, _ := out["error"].(string)
		if msg == "" {
			msg, _ = out["message"].(string)
		}
		return code, msg
	}
	none := func(*api.SubmitRequest) {}
	if code, msg := submit(c4v1.SyncDevViewer, none); code != http.StatusForbidden || !strings.Contains(msg, "no grant from maya for tracker show-a/github") {
		t.Fatalf("no grant: %d %s", code, msg)
	}
	var g api.GrantResponse
	if code := call(t, s, http.MethodPost, "/api/v1/grants?as=artist",
		api.GrantRequest{Project: "show-a", Tracker: "github", Pools: []string{"cpu"}, Classes: []string{"graph"}}, &g); code != http.StatusOK {
		t.Fatalf("grant: %d", code)
	}
	refused := []struct {
		as   string
		edit func(*api.SubmitRequest)
		code int
		says string
	}{
		{"artist", none, http.StatusForbidden, "only a tracker's sync"},
		{c4v1.SyncDevViewer, func(r *api.SubmitRequest) { r.Spec = spec("gpu") }, http.StatusForbidden, "does not cover pool gpu"},
		{c4v1.SyncDevViewer, func(r *api.SubmitRequest) { r.Spec = spec() }, http.StatusForbidden, "the job names none"},
		{c4v1.SyncDevViewer, func(r *api.SubmitRequest) { r.TrackerRef = "" }, http.StatusBadRequest, "trackerRef"},
		{c4v1.SyncDevViewer, func(r *api.SubmitRequest) { r.TrackerRef = "jira/Issue/7" }, http.StatusForbidden, "no tracker jira"},
		{c4v1.SyncDevViewer, func(r *api.SubmitRequest) { r.For = "lee" }, http.StatusForbidden, "no grant from lee"},
	}
	for _, c := range refused {
		if code, msg := submit(c.as, c.edit); code != c.code || !strings.Contains(msg, c.says) {
			t.Errorf("%s: %d %q, want %d %q", c.as, code, msg, c.code, c.says)
		}
	}

	code, msg := submit(c4v1.SyncDevViewer, func(r *api.SubmitRequest) { r.Name = "github-7" })
	if code != http.StatusCreated || !strings.Contains(msg, "for maya under their grant to tracker github") {
		t.Fatalf("under the grant: %d %s", code, msg)
	}
	var job c4v1.WorkJob
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: "github-7"}, &job); err != nil {
		t.Fatal(err)
	}
	if a := job.Annotations; a[c4v1.AnnotationOwner] != "maya" || a[c4v1.AnnotationSubmittedVia] != "c4-sync" ||
		a[c4v1.AnnotationGrant] != g.Record || a[c4v1.AnnotationTrackerRef] != "github/Issue/7" {
		t.Fatalf("job annotations: %v", a)
	}

	// Revoked: refused again, and nothing is made as c4-sync's own.
	if code := call(t, s, http.MethodDelete, "/api/v1/grants/show-a/"+g.Grant.Name+"?as=artist", nil, nil); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if code, msg := submit(c4v1.SyncDevViewer, func(r *api.SubmitRequest) { r.Name = "github-7-v002" }); code != http.StatusForbidden || !strings.Contains(msg, "revoked") {
		t.Fatalf("after revoking: %d %s", code, msg)
	}
	var list c4v1.WorkJobList
	if err := s.Client.List(context.Background(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("jobs: %d %v", len(list.Items), err)
	}
}

// TestGrantCLI drives `c4 farm grant`, `grants` and `revoke` against the
// real HUD handler.
func TestGrantCLI(t *testing.T) {
	s := grantFarm(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := &farmcli.Client{Base: ts.URL, As: "artist", HTTP: ts.Client()}
	var out bytes.Buffer
	o := farmcli.Out{W: &out, Clock: farmcli.Clock{UTC: true}}
	ctx := context.Background()
	if err := farmcli.Grant(ctx, c, o, "show-a", api.GrantRequest{Tracker: "github", Pools: []string{"cpu"}, Until: "7d"}); err != nil {
		t.Fatalf("grant: %v\n%s", err, out.String())
	}
	name := c4v1.SubmitGrantName("github", "maya")
	if got := out.String(); !strings.Contains(got, "maya lets tracker github submit work for them (in cpu)") ||
		!strings.Contains(got, "c4 farm revoke show-a/"+name) {
		t.Fatalf("grant said:\n%s", got)
	}
	out.Reset()
	if err := farmcli.Grants(ctx, c, o, ""); err != nil || !strings.Contains(out.String(), "maya") || !strings.Contains(out.String(), "(pools cpu)") {
		t.Fatalf("grants: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := farmcli.Revoke(ctx, c, o, "show-a/"+name); err != nil || !strings.Contains(out.String(), "maya revoked the grant") {
		t.Fatalf("revoke: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := farmcli.Grants(ctx, c, o, "show-a/github"); err != nil || !strings.Contains(out.String(), "maya revoked the grant from maya to tracker github") {
		t.Fatalf("grants after revoking: %v\n%s", err, out.String())
	}
	if err := farmcli.Revoke(ctx, c, o, "github"); err == nil {
		t.Fatal("revoke takes namespace/name")
	}
}

// A submission for a person refused answers which refusal it is, beside
// the error, so the sync can tell the person what to do.
func TestSubmitRefusalsSayWhich(t *testing.T) {
	s := grantFarm(t)
	spec := json.RawMessage(`{"class":"render","pools":["cpu"],"template":{"spec":{"containers":[{"name":"unit","image":"c4-job-hydra"}]}}}`)
	submit := func(as string, body api.SubmitRequest) (int, api.SubmitRefused) {
		t.Helper()
		var out api.SubmitRefused
		return call(t, s, http.MethodPost, "/api/v1/jobs?as="+as, body, &out), out
	}
	req := api.SubmitRequest{Project: "show-a", Name: "r1", Spec: spec, For: "maya", TrackerRef: "github/Issue/7"}
	if code, out := submit(c4v1.SyncDevViewer, req); code != http.StatusForbidden || out.Reason != api.RefusedNoGrant {
		t.Fatalf("no grant: %d %+v", code, out)
	}
	if code, out := submit("lee", req); code != http.StatusForbidden || out.Reason != api.RefusedNotSync {
		t.Fatalf("not the sync: %d %+v", code, out)
	}
	req.TrackerRef = "jira/Issue/7"
	if code, out := submit(c4v1.SyncDevViewer, req); code != http.StatusForbidden || out.Reason != api.RefusedNoTracker {
		t.Fatalf("no tracker: %d %+v", code, out)
	}
	req.TrackerRef = "github/Issue/7"
	var g api.GrantResponse
	if code := call(t, s, http.MethodPost, "/api/v1/grants?as=artist", api.GrantRequest{Project: "show-a", Tracker: "github", Pools: []string{"gpu"}}, &g); code != http.StatusOK {
		t.Fatalf("grant: %d", code)
	}
	if code, out := submit(c4v1.SyncDevViewer, req); code != http.StatusForbidden || out.Reason != api.RefusedGrantUncovered {
		t.Fatalf("uncovered: %d %+v", code, out)
	}
	if code := call(t, s, http.MethodDelete, "/api/v1/grants/show-a/"+g.Grant.Name+"?as=artist", nil, nil); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if code, out := submit(c4v1.SyncDevViewer, req); code != http.StatusForbidden || out.Reason != api.RefusedGrantLapsed || !strings.Contains(out.Error, "revoked") {
		t.Fatalf("revoked: %d %+v", code, out)
	}
}
