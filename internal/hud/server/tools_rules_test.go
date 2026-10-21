package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Avalanche-io/c4-operator/internal/imageref"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"
	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// untested is notesTool with no test, running cmd.
func untested(cmd string) map[string]string {
	files := notesTool(cmd)
	delete(files, "test/plates/a.exr")
	files["tool.yaml"] = strings.Replace(files["tool.yaml"], "test:\n  tree: test/\n  expect: [out.txt]\n", "", 1)
	return files
}

// publishAgent publishes as an agent acting for a viewer, through MCP.
func publishAgent(t *testing.T, s *Server, as, project string, body api.ToolPublishRequest) (int, api.ToolPublishResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tools/"+project+"?as="+as, bytes.NewReader(raw))
	r.Header.Set("X-C4-Client", "mcp")
	r.Header.Set("X-C4-Agent", "td-bot")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var out api.ToolPublishResponse
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// toolEvents are the book's entries about a tool: its Events' reasons
// and messages.
func toolEvents(t *testing.T, s *Server, ns, name string) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range s.events(context.Background(), ns, "Tool", name) {
		b.WriteString(ev.Reason + ": " + ev.Message + "\n")
	}
	return b.String()
}

// A pipeline TD or the farm admin publishes, or an agent acting for one;
// a unit never does, and to the whole farm only the farm admin.
func TestWhoPublishesTools(t *testing.T) {
	s := toolFarm(t)
	s.DevViewers["unit"] = &api.Viewer{User: "unit", Role: "pipeline-td", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "j", Unit: "0", Grant: "project"}}
	view := putTree(t, s, "show-a", untested("echo hi"))
	body := api.ToolPublishRequest{View: view.String()}
	for as, want := range map[string]string{"artist": "takes the pipeline TD role", "wrangler": "takes the pipeline TD role", "unit": "a unit does not publish"} {
		var out map[string]string
		if code := call(t, s, http.MethodPost, "/api/v1/tools/show-a?as="+as, body, &out); code != http.StatusForbidden || !strings.Contains(out["error"], want) {
			t.Errorf("%s: %d %v", as, code, out)
		}
	}
	if code, _ := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: view.String(), Farm: true}); code != http.StatusForbidden {
		t.Fatalf("a TD published farm-wide: %d", code)
	}
	// an agent's tool must have a test; a person's need not
	if code, out := publishAgent(t, s, "td", "show-a", body); code != http.StatusUnprocessableEntity || len(out.Errors) != 1 || out.Errors[0].Path != "test" {
		t.Fatalf("an agent's untested tool: %d %+v", code, out)
	}
	tested := putTree(t, s, "show-a", notesTool("echo hi > out.txt"))
	if code, out := publishAgent(t, s, "td", "show-a", api.ToolPublishRequest{View: tested.String()}); code != http.StatusCreated || out.Test.State != c4v1.ToolTestRunning {
		t.Fatalf("an agent's tested tool: %d %+v", code, out)
	}
	if v := getTool(t, s, "show-a", "notes").Status.Versions[0]; v.By != "agent/td-bot for lee" {
		t.Fatalf("by %q", v.By)
	}
}

// A person's tool with no test is offered at once, marked untested.
// Offering sets every field of the Tool from the version, so nothing is
// inherited, and a name an agent's configuration holds is refused.
func TestOfferSetsEveryField(t *testing.T) {
	s := toolFarm(t)
	ctx := context.Background()
	code, out := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", untested("echo one")).String()})
	if code != http.StatusCreated || out.Version != "v1" || out.Test.State != c4v1.ToolTestUntested {
		t.Fatalf("untested publish: %d %+v", code, out)
	}
	var list api.ToolListResponse
	if get(t, s, "/api/v1/tools/show-a?as=artist", &list); len(list.Tools) != 1 || !list.Tools[0].Untested || list.Tools[0].Version != "v1" {
		t.Fatalf("listed: %+v", list)
	}
	if _, ref, err := s.toolAt(ctx, s.current(), "show-a", "notes", ""); err != nil || ref.Version != "v1" {
		t.Fatalf("an untested tool runs: %+v %v", ref, err)
	}
	// left over on the object: egress, sidecars and farm access
	tool := getTool(t, s, "show-a", "notes")
	tool.Spec.Egress = []c4v1.ToolEgress{{Host: "example.com"}}
	tool.Spec.Sidecars = []c4v1.ToolSidecar{{Name: "mcp", Image: "x", Port: 8080}}
	tool.Spec.C4 = "project"
	if err := s.Client.Update(ctx, &tool); err != nil {
		t.Fatal(err)
	}
	code, out = publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", untested("echo two")).String()})
	if code != http.StatusCreated || out.Version != "v2" {
		t.Fatalf("v2: %d %+v", code, out)
	}
	tool = getTool(t, s, "show-a", "notes")
	if tool.Spec.View != out.View || tool.Spec.Egress != nil || tool.Spec.Sidecars != nil || tool.Spec.C4 != "" || tool.Status.Version != "v2" {
		t.Fatalf("v2 inherited: %+v", tool.Spec)
	}
	if ev := toolEvents(t, s, "show-a", "notes"); !strings.Contains(ev, "Offered: notes v2 offered") || !strings.Contains(ev, "untested") {
		t.Fatalf("the book: %s", ev)
	}

	// render is an agent's configuration
	cfg := c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "render"}, Spec: c4v1.ToolSpec{View: out.View, C4: "job"}}
	if err := s.Client.Create(ctx, &cfg); err != nil {
		t.Fatal(err)
	}
	files := untested("echo r")
	files["tool.yaml"] = strings.Replace(files["tool.yaml"], "name: notes", "name: render", 1)
	code, out = publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", files).String()})
	if code != http.StatusUnprocessableEntity || len(out.Errors) != 1 ||
		out.Errors[0].Message != "render is an agent configuration on this farm; choose another name" {
		t.Fatalf("an agent configuration's name: %d %+v", code, out)
	}
	if cfg = getTool(t, s, "show-a", "render"); cfg.Spec.C4 != "job" || len(cfg.Status.Versions) != 0 {
		t.Fatalf("the configuration changed: %+v", cfg)
	}
}

// What the farm cannot lower yet is refused at publish, in the words a
// submission's refusal uses; so is a network the farm does not define. A
// declared network the farm defines publishes, and offering it gives the
// Tool that network's rules.
func TestPublishNetworks(t *testing.T) {
	s := toolFarm(t)
	ctx := context.Background()
	files := untested("echo hi")
	files["tool.yaml"] = strings.Replace(files["tool.yaml"], "params:", "needs: {network: [studio-db]}\nparams:", 1)
	code, out := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", files).String()})
	if code != http.StatusUnprocessableEntity || len(out.Errors) != 1 || out.Errors[0].Path != "needs.network" ||
		out.Errors[0].Message != "notes declares network studio-db, which this farm does not define: a farm admin adds it to the c4-networks ConfigMap in c4-system (internal/hud/API.md, Networks)" {
		t.Fatalf("undefined network: %d %+v", code, out)
	}
	// A malformed c4-networks is the admin's to fix, not a "try again".
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: c4v1.NetworksConfigMap},
		Data: map[string]string{"studio-db": "- {cidr: studio-db}"}}
	if err := s.Client.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	code, out = publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", files).String()})
	if code != http.StatusUnprocessableEntity || len(out.Errors) != 1 || out.Errors[0].Path != "needs.network" ||
		!strings.Contains(out.Errors[0].Message, "the c4-networks ConfigMap in c4-system is malformed: network studio-db: cidr") ||
		!strings.Contains(out.Errors[0].Fix, "farm admin") {
		t.Fatalf("malformed networks: %d %+v", code, out)
	}
	cm.Data = map[string]string{"studio-db": "- {host: db.studio.example, ports: [5432]}"}
	if err := s.Client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	code, out = publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", files).String()})
	if code != http.StatusCreated {
		t.Fatalf("defined network: %d %+v", code, out)
	}
	if tool := getTool(t, s, "show-a", "notes"); len(tool.Spec.Egress) != 1 || tool.Spec.Egress[0].Host != "db.studio.example" {
		t.Fatalf("egress %+v", tool.Spec.Egress)
	}
}

// A version is its tree: publishing it again returns it untested again,
// unless its test failed, when it is tested again. Test jobs are named
// by tool, scope and tree, so a project's v1 and the farm's never meet.
func TestVersionsAreTrees(t *testing.T) {
	s := toolFarm(t)
	ctx := context.Background()
	a := putTree(t, s, "show-a", untested("echo a"))
	_, first := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: a.String()})
	code, again := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: a.String()})
	if code != http.StatusOK || again.Version != first.Version || again.View != first.View || len(getTool(t, s, "show-a", "notes").Status.Versions) != 1 {
		t.Fatalf("the same tree again: %d %+v", code, again)
	}

	tree := putTree(t, s, "show-a", notesTool("echo hi > out.txt"))
	_, v2 := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: tree.String()})
	job := v2.Test.Job
	if v2.Version != "v2" || !strings.HasPrefix(job, "show-a/notes-test-") || len(strings.TrimPrefix(job, "show-a/")) >= 50 {
		t.Fatalf("v2: %+v", v2)
	}
	if code, out := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: tree.String()}); code != http.StatusOK || out.Version != "v2" || out.Test.Job != job {
		t.Fatalf("v2 again while testing: %d %+v", code, out)
	}
	finishTest(t, s, job, "Failed", nil)
	code, retest := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: tree.String()})
	if code != http.StatusCreated || retest.Version != "v2" || retest.Test.State != c4v1.ToolTestRunning || retest.Test.Job != job+"-2" {
		t.Fatalf("a failed tree again: %d %+v", code, retest)
	}
	if n := len(getTool(t, s, "show-a", "notes").Status.Versions); n != 2 {
		t.Fatalf("%d versions", n)
	}
	_, farm := publish(t, s, "admin", "show-a", api.ToolPublishRequest{View: tree.String(), Farm: true})
	if farm.Version != "v1" || !strings.HasPrefix(farm.Test.Job, "show-a/notes-farmtest-") {
		t.Fatalf("the farm's v1: %+v", farm)
	}

	// v1 leaves the versions kept in full: it still resolves, by name and
	// by tree
	tool := getTool(t, s, "show-a", "notes")
	tool.Status.Earlier = []c4v1.ToolVersionRef{{Version: "v1", View: first.View, Image: first.Image, State: c4v1.ToolTestUntested}}
	tool.Status.Versions = tool.Status.Versions[1:]
	if err := s.Client.Status().Update(ctx, &tool); err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{"v1", first.View} {
		if _, ref, err := s.toolAt(ctx, s.current(), "show-a", "notes", at); err != nil || ref.Version != "v1" || ref.View != first.View {
			t.Fatalf("@%s: %+v %v", at, ref, err)
		}
	}
	if _, _, err := s.toolAt(ctx, s.current(), "show-a", "notes", "v2"); err == nil {
		t.Fatal("v2, whose test is running, resolved")
	}
	if _, _, err := s.toolAt(ctx, s.current(), "show-a", "other", first.View); err == nil {
		t.Fatal("a tree resolved as another tool")
	}
	// a new version is numbered past the earlier ones
	if _, out := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", untested("echo b")).String()}); out.Version != "v3" {
		t.Fatalf("next: %+v", out)
	}
}

// A farm-wide tool is copied and granted when it is offered, not before
// its test passes; a project made later gets it from the HUD's loop; a
// version no longer offered loses its grants where no job uses it.
func TestFarmToolsSpreadWhenOffered(t *testing.T) {
	s := toolFarm(t)
	ctx := context.Background()
	stores := map[string]*storeurl.Sharded{}
	for _, p := range []string{"show-a", "show-b"} {
		stores[p], _ = s.Stores(ctx, p)
	}
	stC, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	stores["show-c"] = stC
	s.Stores = func(_ context.Context, project string) (*storeurl.Sharded, error) { return stores[project], nil }
	has := func(project, view string) bool {
		id, _ := c4.Parse(view)
		_, err := c4view.Load(ctx, id, c4view.StoreRecords(stores[project]), 4)
		return err == nil
	}
	grants := func(project string) []string {
		var p c4v1.Project
		if err := s.Client.Get(ctx, types.NamespacedName{Name: project}, &p); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range p.Spec.Grants {
			out = append(out, g.Tree)
		}
		return out
	}

	_, v1 := publish(t, s, "admin", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", notesTool("echo hi > out.txt")).String(), Farm: true})
	if has("show-b", v1.View) || len(grants("show-b")) != 0 {
		t.Fatal("copied before its test passed")
	}
	finishTest(t, s, v1.Test.Job, "Succeeded", map[string]string{"out.txt": "hi"})
	if !has("show-b", v1.View) || strings.Join(grants("show-b"), ",") != v1.View {
		t.Fatalf("show-b once offered: %v", grants("show-b"))
	}
	ev := toolEvents(t, s, s.Namespace, "notes")
	for _, want := range []string{"Published: notes v1 published", "TestPassed: notes v1 passed its test", "Offered: notes v1 offered"} {
		if !strings.Contains(ev, want) {
			t.Fatalf("the book has no %q:\n%s", want, ev)
		}
	}

	// show-c, made later, reading only granted trees
	c := c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-c"}, Spec: c4v1.ProjectSpec{Namespaces: []string{"show-c"}, Scope: c4v1.ScopeGrants}}
	if err := s.Client.Create(ctx, &c); err != nil {
		t.Fatal(err)
	}
	s.m.projects = append(s.m.projects, c)
	s.reconcileFarmTools(ctx, s.current())
	if !has("show-c", v1.View) || strings.Join(grants("show-c"), ",") != v1.View {
		t.Fatalf("show-c: %v", grants("show-c"))
	}

	// v2 is offered; v1 keeps its grant only where a job uses it
	for i := range s.m.projects {
		s.m.projects[i].Spec.Grants = nil
		if s.m.projects[i].Name != "show-a" {
			s.m.projects[i].Spec.Grants = []c4v1.Grant{{Tree: v1.View}}
		}
	}
	s.m.jobs = append(s.m.jobs, &jobRec{key: "show-c/uses-v1", project: "show-c", job: c4v1.WorkJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "show-c", Name: "uses-v1"},
		Spec:       c4v1.WorkJobSpec{Inputs: []c4v1.WorkInput{{Materialization: inputMaterialization(v1.View), MountPath: "/etc/c4/tools"}}}}})
	_, v2 := publish(t, s, "admin", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", untested("echo two")).String(), Farm: true})
	if v2.Version != "v2" || !has("show-b", v2.View) {
		t.Fatalf("v2: %+v", v2)
	}
	for i := range s.m.projects {
		var p c4v1.Project
		s.Client.Get(ctx, types.NamespacedName{Name: s.m.projects[i].Name}, &p)
		s.m.projects[i].Spec.Grants = p.Spec.Grants
	}
	s.reconcileFarmTools(ctx, s.current())
	if g := strings.Join(grants("show-b"), ","); g != v2.View {
		t.Fatalf("show-b keeps v1: %s", g)
	}
	if g := grants("show-c"); len(g) != 2 {
		t.Fatalf("show-c, whose job uses v1: %v", g)
	}
	// show-a's own notes shadows the farm's
	if code, _ := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", untested("echo own")).String()}); code != http.StatusCreated {
		t.Fatalf("own publish: %d", code)
	}
	var list api.ToolListResponse
	if get(t, s, "/api/v1/tools/show-a?as=artist", &list); len(list.Tools) != 1 || list.Tools[0].Scope != "project" {
		t.Fatalf("show-a's tools: %+v", list)
	}
}

// A tree the release built is published as it is, its pins kept, and
// booked as released; its test still runs on the farm, and the request
// cannot change either.
func TestReleaseToolsByTree(t *testing.T) {
	s := toolFarm(t)
	files := notesTool("echo hi > out.txt")
	lock := `{"c4":"lock/1","images":{"` + busybox + `":"registry.example/` + busybox + `"}}`
	files["tool.lock"] = lock
	rel := putTree(t, s, "show-a", files)
	s.ReleaseTrees = map[string]string{rel.String(): "v0.2.0"}
	code, out := publish(t, s, "admin", "show-a", api.ToolPublishRequest{View: rel.String(), Farm: true})
	if code != http.StatusCreated || out.View != rel.String() || out.Release != "v0.2.0" || out.Image != "registry.example/"+busybox ||
		out.Test.State != c4v1.ToolTestRunning || out.Test.Job == "" {
		t.Fatalf("a release's tool: %d %+v", code, out)
	}
	if tool := getTool(t, s, s.Namespace, "notes"); tool.Status.Version != "" {
		t.Fatalf("offered before its test: %+v", tool.Status)
	}
	if ev := toolEvents(t, s, s.Namespace, "notes"); !strings.Contains(ev, "released v0.2.0, testing in") || strings.Contains(ev, "tested in") {
		t.Fatalf("the book: %s", ev)
	}
	finishTest(t, s, out.Test.Job, "Succeeded", map[string]string{"out.txt": "hi"})
	if tool := getTool(t, s, s.Namespace, "notes"); tool.Spec.View != rel.String() || tool.Status.Version != "v1" || tool.Status.Versions[0].Release != "v0.2.0" {
		t.Fatalf("after its test: %+v", tool)
	}
	// c4 farm up again: the same version, nothing tested
	if code, again := publish(t, s, "admin", "show-a", api.ToolPublishRequest{View: rel.String(), Farm: true}); code != http.StatusOK || again.Version != "v1" {
		t.Fatalf("the release's tool again: %d %+v", code, again)
	}
	// any other tree is pinned by the farm and has no release
	other := putTree(t, s, "show-a", notesTool("echo other > out.txt"))
	if _, out := publish(t, s, "admin", "show-a", api.ToolPublishRequest{View: other.String(), Farm: true}); out.Release != "" || out.Image != "docker.io/library/"+busybox {
		t.Fatalf("another tree: %+v", out)
	}
}

// Only a tree that was a version of the tool runs by its ID, and never one
// whose test failed, even once it has left the versions kept in full.
func TestToolByTreeOnlyAsAVersion(t *testing.T) {
	s := toolFarm(t)
	ctx := context.Background()
	_, ok := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", untested("echo ok")).String()})
	// a tree with the tool's name, tool.lock and all, never published
	loose := untested("echo loose")
	loose["tool.lock"] = `{"c4":"lock/1","images":{"` + busybox + `":"docker.io/library/` + busybox + `"}}`
	lv := putTree(t, s, "show-a", loose)
	if _, _, err := s.toolAt(ctx, s.current(), "show-a", "notes", lv.String()); err == nil {
		t.Fatal("an unpublished tree ran by its ID")
	}
	// a version whose test failed
	_, bad := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", notesTool("true")).String()})
	finishTest(t, s, bad.Test.Job, "Failed", nil)
	if _, _, err := s.toolAt(ctx, s.current(), "show-a", "notes", bad.View); err == nil {
		t.Fatal("a failed version ran by its ID")
	}
	// ... and once more versions push it out of the kept ones
	key := types.NamespacedName{Namespace: "show-a", Name: "notes"}
	for i := 0; i < c4v1.MaxToolVersions; i++ {
		ver := c4v1.ToolVersion{View: putTree(t, s, "show-a", untested(fmt.Sprintf("echo %d", i))).String(), Test: c4v1.ToolTest{State: c4v1.ToolTestUntested}}
		if _, _, err := s.addVersion(ctx, key, ver); err != nil {
			t.Fatal(err)
		}
	}
	tool := getTool(t, s, "show-a", "notes")
	if len(tool.Status.Earlier) != 2 || tool.Status.Earlier[1].View != bad.View || tool.Status.Earlier[1].State != c4v1.ToolTestFailed {
		t.Fatalf("earlier: %+v", tool.Status.Earlier)
	}
	if _, _, err := s.toolAt(ctx, s.current(), "show-a", "notes", bad.View); err == nil {
		t.Fatal("a failed earlier version ran by its ID")
	}
	if _, ref, err := s.toolAt(ctx, s.current(), "show-a", "notes", ok.View); err != nil || ref.Version != "v1" {
		t.Fatalf("an earlier offered version: %+v %v", ref, err)
	}
}

// A publish refused after the Tool would have been made leaves no Tool.
func TestRefusedPublishLeavesNoTool(t *testing.T) {
	s := toolFarm(t)
	files := notesTool("echo hi > out.txt")
	files["tool.yaml"] = strings.Replace(files["tool.yaml"], "tree: test/", "tree: nowhere/", 1)
	code, out := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", files).String()})
	if code != http.StatusUnprocessableEntity || len(out.Errors) != 1 || out.Errors[0].Path != "test.tree" {
		t.Fatalf("publish: %d %+v", code, out)
	}
	var l c4v1.ToolList
	if err := s.Client.List(context.Background(), &l); err != nil || len(l.Items) != 0 {
		t.Fatalf("%d tools left, %v", len(l.Items), err)
	}
}

// The detail carries the tool.yaml as JSON in written order, and the
// params' schema says what a submission must give.
func TestToolDetailForForms(t *testing.T) {
	s := toolFarm(t)
	files := untested("echo hi")
	files["tool.yaml"] = strings.Replace(files["tool.yaml"], "params:\n", `params:
- name: plates
  type: PATH
  objectType: DIRECTORY
  dataFlow: IN
  from: scene.selection
- name: frames
  type: RANGE_EXPR
  default: "1-10"
  show: advanced
`, 1)
	code, out := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", files).String()})
	if code != http.StatusCreated {
		t.Fatalf("publish: %d %+v", code, out)
	}
	var d api.ToolDetail
	if code := get(t, s, "/api/v1/tools/show-a/notes?as=artist", &d); code != 200 {
		t.Fatalf("detail: %d", code)
	}
	if !bytes.HasPrefix(d.Tool, []byte(`{"c4":"tool/1","name":"notes","title":"Notes"`)) || !d.Untested {
		t.Fatalf("tool: %s", d.Tool)
	}
	props := d.ParamSchema["properties"].(map[string]any)
	plates, frames := props["plates"].(map[string]any), props["frames"].(map[string]any)
	req, _ := json.Marshal(d.ParamSchema["required"])
	if plates["x-from"] != "scene.selection" || frames["x-show"] != "advanced" || frames["pattern"] != rangeExpr || string(req) != `["plates"]` {
		t.Fatalf("schema: %+v required %s", props, req)
	}
}

// Publishing a tool again, identical (the same tree, the same pins), is
// the version it already is: 200, and no test job runs. The tree publish
// wrote, tool.lock and all, is that version too.
func TestRepublishIsIdempotent(t *testing.T) {
	s := toolFarm(t)
	ctx := context.Background()
	tree := putTree(t, s, "show-a", notesTool("echo hi > out.txt"))
	_, v1 := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: tree.String()})
	finishTest(t, s, v1.Test.Job, "Succeeded", map[string]string{"out.txt": "hi"})
	jobs := func() int {
		var l c4v1.WorkJobList
		if err := s.Client.List(ctx, &l); err != nil {
			t.Fatal(err)
		}
		return len(l.Items)
	}
	n := jobs()
	for _, view := range []string{tree.String(), v1.View} {
		code, out := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: view})
		if code != http.StatusOK || out.Version != "v1" || out.View != v1.View || out.Test.State != c4v1.ToolTestOK || !bytes.Equal(out.Lock, v1.Lock) {
			t.Fatalf("again from %s: %d %+v", view, code, out)
		}
	}
	if jobs() != n || len(getTool(t, s, "show-a", "notes").Status.Versions) != 1 {
		t.Fatalf("a republish ran a test or made a version")
	}
	// tool.lock is lock/1 as MarshalIndent writes it, two spaces, a newline
	st, _ := s.Stores(ctx, "show-a")
	vid, _ := c4.Parse(v1.View)
	pv, err := c4view.Load(ctx, vid, c4view.StoreRecords(st), 4)
	if err != nil {
		t.Fatal(err)
	}
	lock, _, _ := readFile(ctx, st, pv, lockFile, 1<<20)
	if want := "{\n  \"c4\": \"lock/1\",\n  \"images\": {\n    \"" + busybox + "\": \"docker.io/library/" + busybox + "\"\n  }\n}\n"; string(lock) != want {
		t.Fatalf("lock bytes %q", lock)
	}
}

// The farm's own tools' names are reserved, in a project and farm-wide.
func TestPublishRefusesFarmToolNames(t *testing.T) {
	s := toolFarm(t)
	files := untested("echo none")
	files["tool.yaml"] = strings.Replace(files["tool.yaml"], "name: notes", "name: none", 1)
	view := putTree(t, s, "show-a", files).String()
	for _, b := range []api.ToolPublishRequest{{View: view}, {View: view, Farm: true}} {
		code, out := publish(t, s, "admin", "show-a", b)
		if code != http.StatusUnprocessableEntity || len(out.Errors) != 1 || out.Errors[0].Message != "none is one of the farm's own tools; choose another name" {
			t.Fatalf("farm %v: %d %+v", b.Farm, code, out)
		}
	}
	var l c4v1.ToolList
	if err := s.Client.List(context.Background(), &l); err != nil || len(l.Items) != 0 {
		t.Fatalf("%d tools, %v", len(l.Items), err)
	}
}

// Publishing a tool whose image runs as root warns that its units, which
// run as 65532, may fail on their first write, and says how to fix it;
// an image with a user publishes without the warning.
func TestPublishWarnsOfARootImage(t *testing.T) {
	s := toolFarm(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifests/sha256:"+strings.Repeat("1", 64)):
			w.Write([]byte(`{"config":{"digest":"sha256:root"}}`))
		case strings.HasSuffix(r.URL.Path, "/manifests/sha256:"+strings.Repeat("2", 64)):
			w.Write([]byte(`{"config":{"digest":"sha256:user"}}`))
		case strings.HasSuffix(r.URL.Path, "/blobs/sha256:root"):
			w.Write([]byte(`{"config":{}}`))
		case strings.HasSuffix(r.URL.Path, "/blobs/sha256:user"):
			w.Write([]byte(`{"config":{"User":"65532"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	s.Images = &imageref.Resolver{Client: srv.Client()}
	for digit, root := range map[string]bool{"1": true, "2": false} {
		image := host + "/studio/notes@sha256:" + strings.Repeat(digit, 64)
		files := untested("echo hi")
		files["tool.yaml"] = strings.Replace(files["tool.yaml"], busybox, image, 1)
		view := putTree(t, s, "show-a", files)
		code, out := publish(t, s, "td", "show-a", api.ToolPublishRequest{View: view.String(), DryRun: true})
		if code != http.StatusOK || !out.OK {
			t.Fatalf("%s: %d %+v", image, code, out)
		}
		warned := len(out.Warnings) == 1 && strings.Contains(out.Warnings[0], "runs as root") && strings.Contains(out.Warnings[0], "FROM c4/runtime-base")
		if warned != root {
			t.Errorf("%s: warnings %v", image, out.Warnings)
		}
	}
}
