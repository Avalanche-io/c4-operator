package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Avalanche-io/c4"
	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/imageref"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/schema"
)

const busybox = "busybox@sha256:5e1c0aa1c6dc0f7b8e1cf0b6a6e1b9fbd0b2f2e1a7f43d1a9e6c2b7f0d8a1c3e"

// toolFarm is the submit farm with Tool and WorkJob status, Projects to
// grant in (show-b reads only granted trees), and a farm admin.
func toolFarm(t *testing.T) *Server {
	t.Helper()
	s := submitFarm(t)
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	s.m.projects[1].Spec.Scope = c4v1.ScopeGrants
	objs := []runtime.Object{}
	for i := range s.m.projects {
		objs = append(objs, s.m.projects[i].DeepCopy())
	}
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).
		WithStatusSubresource(&c4v1.Tool{}, &c4v1.WorkJob{}).Build()
	s.DevViewers["admin"] = &api.Viewer{User: "ops", Role: "farm-admin", Projects: []string{"show-a", "show-b"}, All: true}
	return s
}

// putTree stores files as a view in a project's store.
func putTree(t *testing.T, s *Server, project string, files map[string]string) c4.ID {
	t.Helper()
	ctx := context.Background()
	st, _ := s.Stores(ctx, project)
	root := c4view.NewNode("")
	for p, body := range files {
		id, err := st.PutBytes(ctx, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		dir, name := c4view.SplitPath(p)
		root.Dir(dir).Files[name] = c4view.FileEntry(name, int64(len(body)), id, false)
	}
	v := c4view.FromNode(root)
	if err := storeView(ctx, st, v); err != nil {
		t.Fatal(err)
	}
	return v.ID
}

func notesTool(cmd string) map[string]string {
	return map[string]string{
		"tool.yaml": `c4: tool/1
name: notes
title: Notes
summary: Writes a note about the plates.
kind: once
runs:
  image: ` + busybox + `
  command: [sh, -c, "` + cmd + `"]
params:
- name: width
  type: INT
  default: 960
  minValue: 64
test:
  tree: test/
  expect: [out.txt]
`,
		"test/plates/a.exr": "plate",
	}
}

func publish(t *testing.T, s *Server, as, project string, body api.ToolPublishRequest) (int, api.ToolPublishResponse) {
	t.Helper()
	var out api.ToolPublishResponse
	code := call(t, s, http.MethodPost, "/api/v1/tools/"+project+"?as="+as, body, &out)
	return code, out
}

// finishTest plays the queue: the test job ends in phase, with an output
// holding files.
func finishTest(t *testing.T, s *Server, job, phase string, files map[string]string) {
	t.Helper()
	ctx := context.Background()
	ns, name, _ := strings.Cut(job, "/")
	var j c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &j); err != nil {
		t.Fatal(err)
	}
	j.Status.Phase = phase
	if files != nil {
		j.Status.Output = &c4v1.WorkOutput{View: putTree(t, s, s.m.nsProject[ns], files).String()}
	}
	now := metav1.Now()
	j.Status.Finished = &now
	if err := s.Client.Status().Update(ctx, &j); err != nil {
		t.Fatal(err)
	}
	s.settleToolTests(ctx, s.current())
}

func getTool(t *testing.T, s *Server, ns, name string) c4v1.Tool {
	t.Helper()
	var tool c4v1.Tool
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &tool); err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestPublishToolDryRun(t *testing.T) {
	s := toolFarm(t)
	view := putTree(t, s, "show-a", notesTool("echo hi > out.txt"))
	code, out := publish(t, s, "artist", "show-a", api.ToolPublishRequest{View: view.String(), DryRun: true})
	if code != http.StatusOK || !out.OK || out.Tool != "notes" || out.Version != "" || !strings.Contains(out.Image, "@sha256:") {
		t.Fatalf("dry run: %d %+v", code, out)
	}
	lock, lerr := schema.ParseLock(out.Lock)
	if len(lerr) > 0 || lock.Images[busybox] != "docker.io/library/"+busybox {
		t.Fatalf("lock %s %v", out.Lock, lerr)
	}
	var tools c4v1.ToolList
	if err := s.Client.List(context.Background(), &tools); err != nil || len(tools.Items) != 0 {
		t.Fatalf("a dry run wrote %d tools %v", len(tools.Items), err)
	}
	// a folder with no tool.yaml
	bare := putTree(t, s, "show-a", map[string]string{"run.sh": "echo"})
	if code, out := publish(t, s, "artist", "show-a", api.ToolPublishRequest{View: bare.String()}); code != http.StatusUnprocessableEntity ||
		len(out.Errors) != 1 || !strings.Contains(out.Errors[0].Message, "no tool.yaml") {
		t.Fatalf("no tool.yaml: %d %+v", code, out)
	}
}

// Publishing records a version and tests it; only a version whose test
// passed is offered, and a later failing one leaves it in use.
func TestPublishToolOffersOnlyWhatPassed(t *testing.T) {
	s := toolFarm(t)
	view := putTree(t, s, "show-a", notesTool("echo hi > out.txt"))
	code, out := publish(t, s, "artist", "show-a", api.ToolPublishRequest{View: view.String()})
	if code != http.StatusCreated || out.Version != "v1" || out.Test == nil || out.Test.State != c4v1.ToolTestRunning || out.Test.Job == "" {
		t.Fatalf("publish: %d %+v", code, out)
	}
	tool := getTool(t, s, "show-a", "notes")
	if tool.Spec.View != "" || tool.Status.Version != "" || len(tool.Status.Versions) != 1 {
		t.Fatalf("offered before its test: %+v", tool)
	}
	var list api.ToolListResponse
	if get(t, s, "/api/v1/tools/show-a?as=artist", &list); len(list.Tools) != 0 {
		t.Fatalf("listed before its test: %+v", list)
	}
	finishTest(t, s, out.Test.Job, "Succeeded", map[string]string{"out.txt": "hi"})
	tool = getTool(t, s, "show-a", "notes")
	if tool.Spec.View != out.View || tool.Status.Version != "v1" || tool.Status.Kind != "once" || tool.Status.Versions[0].Test.State != c4v1.ToolTestOK {
		t.Fatalf("after its test passed: %+v", tool)
	}

	// v2 writes nothing its test expects: it fails, and v1 stays in use.
	view2 := putTree(t, s, "show-a", notesTool("true"))
	code, out2 := publish(t, s, "artist", "show-a", api.ToolPublishRequest{View: view2.String()})
	if code != http.StatusCreated || out2.Version != "v2" {
		t.Fatalf("v2: %d %+v", code, out2)
	}
	finishTest(t, s, out2.Test.Job, "Succeeded", map[string]string{"other.txt": "x"})
	tool = getTool(t, s, "show-a", "notes")
	if tool.Spec.View != out.View || tool.Status.Version != "v1" || tool.Status.Versions[1].Test.State != c4v1.ToolTestFailed ||
		!strings.Contains(tool.Status.Versions[1].Test.Reason, "out.txt") {
		t.Fatalf("after v2's test failed: %+v", tool.Status)
	}
	var d api.ToolDetail
	if code := get(t, s, "/api/v1/tools/show-a/notes?as=artist", &d); code != 200 || d.Version != "v1" || len(d.Versions) != 2 ||
		d.ParamSchema["properties"].(map[string]any)["width"].(map[string]any)["type"] != "integer" {
		t.Fatalf("detail: %d %+v", code, d)
	}
	if code := get(t, s, "/api/v1/tools/show-a/notes?as=artist&version=v2", &d); code != 200 || d.Test.State != c4v1.ToolTestFailed {
		t.Fatalf("v2's detail: %d %+v", code, d)
	}
	// a failed test job fails the version
	view3 := putTree(t, s, "show-a", notesTool("exit 1"))
	_, out3 := publish(t, s, "artist", "show-a", api.ToolPublishRequest{View: view3.String()})
	finishTest(t, s, out3.Test.Job, "Failed", nil)
	if tool = getTool(t, s, "show-a", "notes"); tool.Status.Versions[2].Test.State != c4v1.ToolTestFailed || tool.Status.Version != "v1" {
		t.Fatalf("after v3's job failed: %+v", tool.Status)
	}
}

// A farm-wide tool is the farm admin's to publish; its tree is copied into
// every project's store and granted where a project reads only granted
// trees. A project's own tool of the same name shadows it.
func TestFarmTools(t *testing.T) {
	s := toolFarm(t)
	files := notesTool("echo hi > out.txt")
	delete(files, "test/plates/a.exr")
	files["tool.yaml"] = strings.Replace(files["tool.yaml"], "test:\n  tree: test/\n  expect: [out.txt]\n", "", 1)
	view := putTree(t, s, "show-a", files)
	if code, _ := publish(t, s, "artist", "show-a", api.ToolPublishRequest{View: view.String(), Farm: true}); code != http.StatusForbidden {
		t.Fatalf("an artist published farm-wide: %d", code)
	}
	code, out := publish(t, s, "admin", "show-a", api.ToolPublishRequest{View: view.String(), Farm: true})
	if code != http.StatusCreated || out.Scope != "farm" || out.Test.State != c4v1.ToolTestOK {
		t.Fatalf("farm publish: %d %+v", code, out)
	}
	if tool := getTool(t, s, s.Namespace, "notes"); tool.Spec.View != out.View {
		t.Fatalf("a tool with no test is offered at once: %+v", tool)
	}
	ctx := context.Background()
	stB, _ := s.Stores(ctx, "show-b")
	vid, _ := c4.Parse(out.View)
	if _, err := c4view.Load(ctx, vid, c4view.StoreRecords(stB), 4); err != nil {
		t.Fatalf("the tree is not in show-b's store: %v", err)
	}
	var b c4v1.Project
	if err := s.Client.Get(ctx, types.NamespacedName{Name: "show-b"}, &b); err != nil || len(b.Spec.Grants) != 1 || b.Spec.Grants[0].Tree != out.View {
		t.Fatalf("show-b's grants %+v %v", b.Spec.Grants, err)
	}
	var list api.ToolListResponse
	if get(t, s, "/api/v1/tools/show-b?as=wrangler", &list); len(list.Tools) != 1 || list.Tools[0].Scope != "farm" {
		t.Fatalf("show-b's tools: %+v", list)
	}
	// show-a's own notes shadows the farm's
	own := putTree(t, s, "show-a", files)
	if code, _ := publish(t, s, "artist", "show-a", api.ToolPublishRequest{View: own.String()}); code != http.StatusCreated {
		t.Fatalf("own publish: %d", code)
	}
	if get(t, s, "/api/v1/tools/show-a?as=artist", &list); len(list.Tools) != 1 || list.Tools[0].Scope != "project" {
		t.Fatalf("show-a's tools: %+v", list)
	}
	_ = stB
}

func TestSchemaEndpoints(t *testing.T) {
	s := toolFarm(t)
	for format, want := range map[string][]byte{"submit": schema.SubmitSchema(), "tool": schema.ToolSchema()} {
		w := httptestGet(s, "/api/v1/schema/"+format+"?as=artist")
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) {
			t.Fatalf("%s: %d", format, w.Code)
		}
	}
	if w := httptestGet(s, "/api/v1/schema/plan?as=artist"); w.Code != http.StatusNotFound {
		t.Fatalf("plan: %d", w.Code)
	}
}

// c4/<name> is the farm's own image: from its registry, or loaded onto
// its nodes, pinned by the digest a node reports.
func TestResolveAndPinFarmImages(t *testing.T) {
	s := &Server{ImageRegistry: "ghcr.io/avalanche-io", ImageTag: "v0.1.0"}
	for in, want := range map[string]string{
		"c4/runtime-oiio":     "ghcr.io/avalanche-io/c4-runtime-oiio:v0.1.0",
		"c4/job-hydra:v0.2.0": "ghcr.io/avalanche-io/c4-job-hydra:v0.2.0",
		"studio/nuke:16.0":    "studio/nuke:16.0",
	} {
		if got := s.resolveImage(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	local := &Server{ImageTag: "dev"}
	m := &model{nodes: []corev1.Node{{Status: corev1.NodeStatus{Images: []corev1.ContainerImage{{Names: []string{
		"docker.io/library/c4-job-hydra@sha256:" + strings.Repeat("ab", 32), "docker.io/library/c4-job-hydra:dev"}}}}}}}
	if p, err := local.pinImage(context.Background(), m, "c4/job-hydra"); err != nil ||
		p != "docker.io/library/c4-job-hydra@sha256:"+strings.Repeat("ab", 32) {
		t.Fatalf("a loaded image: %q %v", p, err)
	}
	// no node reports it and no registry has it: there is no digest to pin
	local.Images = &imageref.Resolver{Client: &http.Client{Transport: failing{}}}
	if p, err := local.pinImage(context.Background(), &model{}, "c4/runtime-oiio"); err == nil || !strings.Contains(err.Error(), "as c4-runtime-oiio:dev") {
		t.Fatalf("an image no node reports: %q %v", p, err)
	}
}

var _ = storeurl.Spec{}

// failing is a transport that reaches no registry.
type failing struct{}

func (failing) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("offline") }

func httptestGet(s *Server, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// A release's own tool, published by c4 farm up, was tested in its
// release: the farm runs no test, and only the farm admin may say so.
func TestPublishToolTestedInRelease(t *testing.T) {
	s := toolFarm(t)
	view := putTree(t, s, "show-a", notesTool("echo hi > out.txt"))
	if code, _ := publish(t, s, "artist", "show-a", api.ToolPublishRequest{View: view.String(), TestedIn: "v0.1.0"}); code != http.StatusForbidden {
		t.Fatalf("an artist skipped the test: %d", code)
	}
	code, out := publish(t, s, "admin", "show-a", api.ToolPublishRequest{View: view.String(), Farm: true, TestedIn: "v0.1.0"})
	if code != http.StatusCreated || out.Test == nil || out.Test.State != c4v1.ToolTestOK || out.Test.TestedIn != "v0.1.0" || out.Test.Job != "" {
		t.Fatalf("release publish: %d %+v", code, out)
	}
	if tool := getTool(t, s, s.Namespace, "notes"); tool.Spec.View != out.View || tool.Status.Versions[0].Test.TestedIn != "v0.1.0" {
		t.Fatalf("release tool: %+v", tool)
	}
}
