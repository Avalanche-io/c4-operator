package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
)

// bundleFarm adds an agent pool that runs maya's work, served by a runner
// that has registered research and review.
func bundleFarm(t *testing.T) *Server {
	t.Helper()
	s := submitFarm(t)
	s.m.pools = append(s.m.pools, c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "agent"},
		Spec: c4v1.PoolSpec{NodeSelector: map[string]string{"c4.avalanche.io/pool": "agent"}, Owners: []string{"maya"}}})
	s.m.nodes = append(s.m.nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mac",
		Labels: map[string]string{"c4.avalanche.io/pool": "agent"}, Annotations: map[string]string{nodeMembers: "research, review"}}})
	return s
}

// A person runs their own agents without kubectl: the bundle is theirs
// (so their runner runs it), named for them, and refused in the farm's
// words when it could not run.
func TestBundleEndpoint(t *testing.T) {
	s := bundleFarm(t)
	ctx := context.Background()
	post := func(body string) (int, api.BundleResponse) {
		t.Helper()
		var out api.BundleResponse
		return call(t, s, http.MethodPost, "/api/v1/bundles/show-a?as=artist", []byte(body), &out), out
	}

	// A dry run makes nothing, and warns of a member no runner knows.
	code, out := post(`{"members": ["research", {"name": "drafts", "kind": "task"}], "due": "2026-10-24T18:00:00Z", "dryRun": true}`)
	if code != http.StatusOK || !out.OK || out.Name != "agents" || out.Owner != "maya" || len(out.Members) != 2 || out.Due == nil {
		t.Fatalf("dry run: %d %+v", code, out)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "drafts") || !strings.Contains(out.Warnings[0], "c4 farm agents add drafts") {
		t.Fatalf("a member no runner has is not warned of: %v", out.Warnings)
	}
	var list c4v1.AgentBundleList
	if err := s.Client.List(ctx, &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("a dry run made %d bundles", len(list.Items))
	}

	// Made: owned by the person asking, so their runner runs it.
	code, out = post(`{"members": ["research", "review"], "maxRunning": 1}`)
	if code != http.StatusCreated || !out.OK || out.Name != "agents" || out.Link != "#/job/show-a/agents" {
		t.Fatalf("create: %d %+v", code, out)
	}
	var b c4v1.AgentBundle
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "agents"}, &b); err != nil {
		t.Fatal(err)
	}
	if b.Annotations[c4v1.AnnotationOwner] != "maya" || len(b.Spec.Members) != 2 || b.Spec.MaxRunning != 1 {
		t.Fatalf("bundle %+v %+v", b.Annotations, b.Spec)
	}
	// The next one is numbered.
	if code, out := post(`{"members": ["research"]}`); code != http.StatusCreated || out.Name != "agents-2" {
		t.Fatalf("second: %d %+v", code, out)
	}

	// Listed, read, held and removed.
	var all []api.BundleResponse
	if code := call(t, s, http.MethodGet, "/api/v1/bundles/show-a?as=artist", nil, &all); code != http.StatusOK || len(all) != 2 {
		t.Fatalf("list: %d %+v", code, all)
	}
	var one api.BundleResponse
	if code := call(t, s, http.MethodGet, "/api/v1/bundles/show-a/agents?as=artist", nil, &one); code != http.StatusOK ||
		one.Owner != "maya" || len(one.Members) != 2 || one.Members[0].State != "queued" {
		t.Fatalf("get: %d %+v", code, one)
	}
	if code := call(t, s, http.MethodPost, "/api/v1/bundles/show-a/agents/suspend?as=artist", map[string]bool{"suspend": true}, nil); code != http.StatusOK {
		t.Fatalf("suspend: %d", code)
	}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "agents"}, &b); err != nil || !b.Spec.Suspend {
		t.Fatalf("not held: %v %+v", err, b.Spec)
	}
	if code := call(t, s, http.MethodDelete, "/api/v1/bundles/show-a/agents-2?as=artist", nil, nil); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "agents-2"}, &b); err == nil {
		t.Fatal("a deleted bundle is still there")
	}

	// Refused, each with where and what to do.
	for _, c := range []struct{ body, path, words string }{
		{`{"members": []}`, "members", "at least one agent"},
		{`{"members": ["no spaces please"]}`, "members[0]", "not an agent's name"},
		{`{"members": ["research", "research"]}`, "members[1]", "named twice"},
		{`{"members": ["research"], "pools": ["gpu"]}`, "pools", "no pool runs agents for maya"},
		{`{"members": ["research"], "due": "whenever"}`, "due", "not a time"},
		{`{"members": ["research"], "name": "agents"}`, "name", "already has"},
	} {
		code, out := post(c.body)
		if code < 400 || len(out.Errors) == 0 || out.Errors[0].Path != c.path || !strings.Contains(out.Errors[0].Message, c.words) || out.Errors[0].Fix == "" {
			t.Errorf("%s: %d %+v", c.body, code, out.Errors)
		}
	}
	// Another person's pool does not run maya's agents.
	s.m.pools[len(s.m.pools)-1].Spec.Owners = []string{"noor"}
	if code, out := post(`{"members": ["research"]}`); code != http.StatusUnprocessableEntity || out.Errors[0].Fix != "c4 farm up makes one for this machine" ||
		!strings.Contains(out.Errors[0].Message, "runs only the agents of noor") {
		t.Fatalf("someone else's pool: %d %+v", code, out.Errors)
	}
}

// A bundle starts agents, so a unit is held to agents.submit lead there
// as at /submissions, and holds or removes no person's bundle; a person,
// and a unit where the project says anyone, still start one.
func TestAUnitsBundleNeedsALead(t *testing.T) {
	s := bundleFarm(t)
	ctx := context.Background()
	parent := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "port", UID: "uid-port",
		Annotations: map[string]string{c4v1.AnnotationOwner: "maya"}},
		Spec: c4v1.WorkJobSpec{Class: "graph", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "x"}}}}}}
	if err := s.Client.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/port/0", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "port", JobUID: string(parent.UID), Unit: "0", Grant: "project"}}
	for _, dry := range []string{"true", "false"} {
		var out api.BundleResponse
		code := call(t, s, http.MethodPost, "/api/v1/bundles/show-a?as=unit", []byte(`{"members": ["research"], "dryRun": `+dry+`}`), &out)
		if code != http.StatusForbidden || out.OK || len(out.Errors) != 1 || out.Errors[0].Message != leadOnlyMessage ||
			!strings.Contains(out.Errors[0].Fix, "agents.submit: anyone") {
			t.Fatalf("a unit's bundle (dry run %s): %d %+v", dry, code, out)
		}
	}
	var list c4v1.AgentBundleList
	if err := s.Client.List(ctx, &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("a refused bundle was made: %d", len(list.Items))
	}

	// A person's bundle: the unit may neither hold nor remove it.
	if code := call(t, s, http.MethodPost, "/api/v1/bundles/show-a?as=artist", []byte(`{"members": ["research"]}`), nil); code != http.StatusCreated {
		t.Fatalf("a person's bundle: %d", code)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/bundles/show-a/agents/suspend?as=unit"},
		{http.MethodDelete, "/api/v1/bundles/show-a/agents?as=unit"},
	} {
		var refused map[string]string
		var body any
		if c.method == http.MethodPost {
			body = []byte(`{"suspend": true}`)
		}
		if code := call(t, s, c.method, c.path, body, &refused); code != http.StatusForbidden || refused["error"] != errUnitBundles {
			t.Fatalf("%s %s: %d %v", c.method, c.path, code, refused)
		}
	}
	var b c4v1.AgentBundle
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "agents"}, &b); err != nil || b.Spec.Suspend {
		t.Fatalf("the person's bundle after the unit's tries: %v suspend %v", err, b.Spec.Suspend)
	}

	// agents.submit: anyone lets the unit start one.
	for i := range s.m.projects {
		if s.m.projects[i].Name == "show-a" {
			s.m.projects[i].Spec.Work = &c4v1.ProjectWork{Policy: &c4v1.ProjectPolicy{Values: map[string]string{policy.AgentsSubmit: policy.SubmitAnyone}}}
		}
	}
	var out api.BundleResponse
	if code := call(t, s, http.MethodPost, "/api/v1/bundles/show-a?as=unit", []byte(`{"members": ["research"], "dryRun": true}`), &out); code == http.StatusForbidden {
		t.Fatalf("a unit's bundle with agents.submit anyone: %d %+v", code, out)
	}
}
