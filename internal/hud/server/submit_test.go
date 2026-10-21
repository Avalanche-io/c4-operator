package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Avalanche-io/c4-operator/internal/imageref"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/schema"
)

// submitFarm is farm() with a store per project and a fake API server.
func submitFarm(t *testing.T) *Server {
	t.Helper()
	s := farm(t)
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	s.Client = fake.NewClientBuilder().WithScheme(scheme).Build()
	stores := map[string]*storeurl.Sharded{}
	for _, p := range []string{"show-a", "show-b"} {
		st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
		if err != nil {
			t.Fatal(err)
		}
		stores[p] = st
	}
	s.Stores = func(_ context.Context, project string) (*storeurl.Sharded, error) { return stores[project], nil }
	s.SpoolDir = t.TempDir()
	// No registry answers here: a test that needs one gives its own.
	s.Images = &imageref.Resolver{Client: &http.Client{Transport: noRegistry{}}}
	return s
}

type noRegistry struct{}

func (noRegistry) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no registry in this test")
}

func call(t *testing.T, s *Server, method, path string, body any, out any) int {
	t.Helper()
	var r *http.Request
	switch b := body.(type) {
	case nil:
		r = httptest.NewRequest(method, path, nil)
	case []byte:
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
	}
	return w.Code
}

func TestUploadChecksTheID(t *testing.T) {
	s := submitFarm(t)
	frame := []byte("sm0140.1001.exr bytes")
	id := c4.Identify(bytes.NewReader(frame))
	other := c4.Identify(strings.NewReader("something else"))

	var miss api.MissingResponse
	if code := call(t, s, http.MethodPost, "/api/v1/objects/show-a/missing?as=artist", api.MissingRequest{IDs: []string{id.String(), other.String()}}, &miss); code != 200 || len(miss.Missing) != 2 {
		t.Fatalf("missing before upload: %d %v", code, miss.Missing)
	}
	if code := call(t, s, http.MethodPut, "/api/v1/objects/show-a/"+other.String()+"?as=artist", frame, nil); code != http.StatusBadRequest {
		t.Fatalf("bytes stored under the wrong ID: %d", code)
	}
	if code := call(t, s, http.MethodPut, "/api/v1/objects/show-a/"+id.String()+"?as=artist", frame, nil); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	call(t, s, http.MethodPost, "/api/v1/objects/show-a/missing?as=artist", api.MissingRequest{IDs: []string{id.String(), other.String()}}, &miss)
	if len(miss.Missing) != 1 || miss.Missing[0] != other.String() {
		t.Fatalf("missing after upload: %v", miss.Missing)
	}
	// an artist of show-a cannot write to show-b's store
	if code := call(t, s, http.MethodPut, "/api/v1/objects/show-b/"+id.String()+"?as=artist", frame, nil); code != http.StatusNotFound {
		t.Fatalf("upload to another project: %d", code)
	}
}

func TestViewFromListing(t *testing.T) {
	s := submitFarm(t)
	files := map[string][]byte{"sm0140.usda": []byte("#usda 1.0"), "tex/heron.exr": []byte("feathers"), "geo/reeds.usdc": []byte("reeds")}
	var list []api.ViewFile
	for p, b := range files {
		list = append(list, api.ViewFile{Path: p, ID: c4.Identify(bytes.NewReader(b)).String(), Size: int64(len(b))})
	}
	var vr api.ViewResponse
	if code := call(t, s, http.MethodPost, "/api/v1/views/show-a?as=artist", api.ViewRequest{Files: list}, &vr); code != http.StatusConflict || len(vr.Missing) != 3 {
		t.Fatalf("a view of unstored objects: %d %+v", code, vr)
	}
	for _, b := range files {
		if code := call(t, s, http.MethodPut, "/api/v1/objects/show-a/"+c4.Identify(bytes.NewReader(b)).String()+"?as=artist", b, nil); code != 200 {
			t.Fatalf("upload: %d", code)
		}
	}
	if code := call(t, s, http.MethodPost, "/api/v1/views/show-a?as=artist", api.ViewRequest{Files: list}, &vr); code != 200 || vr.View == "" {
		t.Fatalf("view: %d %+v", code, vr)
	}
	// the same tree scanned from disk has the same ID
	dir := t.TempDir()
	for p, b := range files {
		writeFile(t, dir, p, b)
	}
	want, err := c4view.ScanID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if vr.View != want.String() {
		t.Fatalf("view %s, scanned %s", vr.View, want)
	}
	var ls api.ViewListing
	if code := call(t, s, http.MethodGet, "/api/v1/views/show-a/"+vr.View+"/files?path=tex&as=artist", nil, &ls); code != 200 || len(ls.Files) != 1 || ls.Files[0].Path != "tex/heron.exr" {
		t.Fatalf("listing: %d %+v", code, ls)
	}
	for _, bad := range []string{"../etc/passwd", "/abs", "a//b", "dir/"} {
		code := call(t, s, http.MethodPost, "/api/v1/views/show-a?as=artist",
			api.ViewRequest{Files: []api.ViewFile{{Path: bad, ID: list[0].ID, Size: 1}}}, nil)
		if code != http.StatusBadRequest {
			t.Errorf("path %q accepted: %d", bad, code)
		}
	}
}

// A tree's empty directories and symbolic links are part of its ID, so
// the HUD records them and lists them back.
func TestViewWithEmptyDirsAndLinks(t *testing.T) {
	s := submitFarm(t)
	b := []byte("# pipeline tools")
	id := c4.Identify(bytes.NewReader(b)).String()
	if code := call(t, s, http.MethodPut, "/api/v1/objects/show-a/"+id+"?as=artist", b, nil); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	dir := t.TempDir()
	writeFile(t, dir, "README.md", b)
	for _, d := range []string{"out", ".git/refs/tags"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("README.md", filepath.Join(dir, "README")); err != nil {
		t.Fatal(err)
	}
	scanned, err := c4view.ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	req := api.ViewRequest{Files: []api.ViewFile{{Path: "README.md", ID: id, Size: int64(len(b))}},
		Dirs: scanned.EmptyDirs(), Links: []api.ViewLink{{Path: "README", Target: "README.md"}}}
	if got := strings.Join(req.Dirs, " "); got != ".git/refs/tags/ out/" {
		t.Fatalf("empty dirs %q", got)
	}
	var vr api.ViewResponse
	if code := call(t, s, http.MethodPost, "/api/v1/views/show-a?as=artist", req, &vr); code != 200 || vr.View != scanned.ID.String() {
		t.Fatalf("view %d %s, scanned %s", code, vr.View, scanned.ID)
	}
	var ls api.ViewListing
	if code := call(t, s, http.MethodGet, "/api/v1/views/show-a/"+vr.View+"/files?as=artist", nil, &ls); code != 200 ||
		len(ls.Files) != 1 || strings.Join(ls.Dirs, " ") != ".git/refs/tags/ out/" || len(ls.Links) != 1 || ls.Links[0] != req.Links[0] {
		t.Fatalf("listing: %d %+v", code, ls)
	}
	for _, bad := range []api.ViewRequest{
		{Links: []api.ViewLink{{Path: "l", Target: ""}}},
		{Links: []api.ViewLink{{Path: "../l", Target: "x"}}},
		{Files: req.Files, Links: []api.ViewLink{{Path: "README.md", Target: "x"}}},
		{Dirs: []string{"/abs/"}},
		{Files: req.Files, Dirs: []string{"README.md/"}},
		{Files: []api.ViewFile{{Path: "a", ID: id, Size: int64(len(b))}, {Path: "a/b", ID: id, Size: int64(len(b))}}},
		{Files: []api.ViewFile{{Path: "a/b", ID: id, Size: int64(len(b))}, {Path: "a", ID: id, Size: int64(len(b))}}},
	} {
		if code := call(t, s, http.MethodPost, "/api/v1/views/show-a?as=artist", bad, nil); code != http.StatusBadRequest {
			t.Errorf("%+v accepted: %d", bad, code)
		}
	}
}

func TestSubmit(t *testing.T) {
	s := submitFarm(t)
	eta := time.Date(2026, 10, 23, 14, 52, 0, 0, time.UTC)
	deadline := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)
	var planned *c4v1.WorkJob
	s.DryRun = func(_ context.Context, job *c4v1.WorkJob, project string) (api.SubmitEstimate, error) {
		planned = job
		if project != "show-a" {
			t.Errorf("planned in project %s", project)
		}
		return api.SubmitEstimate{Units: 48, ETA50: &eta, Deadline: &deadline, DeadlineSource: "spec",
			SlackSeconds: int64(deadline.Sub(eta).Seconds()), Feasible: true}, nil
	}
	spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1001-1048", "order": "binary"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	view := c4.Identify(strings.NewReader("a tree")).String()
	req := api.SubmitRequest{Project: "show-a", Type: "render", Stage: "light", Shot: "sm0140", Spec: spec,
		Inputs: []api.SubmitInput{{View: view, MountPath: "/show"}}, DryRun: true}

	var out api.SubmitResponse
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", req, &out); code != 200 || !out.DryRun || out.Estimate == nil || out.Estimate.Units != 48 {
		t.Fatalf("dry run: %d %+v", code, out)
	}
	if out.Name != "sm0140-light-v001" || out.Namespace != "show-a" {
		t.Fatalf("name %s/%s", out.Namespace, out.Name)
	}
	if planned.Spec.DeadlineMode != "Suggested" || planned.Annotations[c4v1.AnnotationOwner] != "maya" {
		t.Fatalf("planned draft: mode %q owner %q", planned.Spec.DeadlineMode, planned.Annotations[c4v1.AnnotationOwner])
	}
	var list c4v1.WorkJobList
	if err := s.Client.List(context.Background(), &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("a dry run created jobs: %v %d", err, len(list.Items))
	}

	req.DryRun = false
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", req, &out); code != http.StatusCreated {
		t.Fatalf("submit: %d %+v", code, out)
	}
	var job c4v1.WorkJob
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: "sm0140-light-v001"}, &job); err != nil {
		t.Fatal(err)
	}
	if job.Annotations[c4v1.AnnotationOwner] != "maya" || job.Annotations[c4v1.AnnotationSubmittedVia] != "hud" ||
		job.Labels[c4v1.LabelShot] != "sm0140" || job.Labels[c4v1.LabelJobType] != "render" || job.Labels[c4v1.LabelStage] != "light" {
		t.Fatalf("job metadata %v %v", job.Annotations, job.Labels)
	}
	if sc := job.Spec.Template.Spec.SecurityContext; sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatalf("the template was not restricted: %+v", sc)
	}
	if len(job.Spec.Inputs) != 1 || job.Spec.Inputs[0].Materialization != inputMaterialization(view) {
		t.Fatalf("inputs %+v", job.Spec.Inputs)
	}
	var mat c4v1.Materialization
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: inputMaterialization(view)}, &mat); err != nil || mat.Spec.Source.ID != view {
		t.Fatalf("materialization: %v %+v", err, mat.Spec)
	}

	// the same name again conflicts; another project is not visible
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", api.SubmitRequest{Project: "show-a", Name: "sm0140-light-v001", Spec: spec}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate: %d", code)
	}
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", api.SubmitRequest{Project: "show-b", Spec: spec}, nil); code != http.StatusNotFound {
		t.Fatalf("another project: %d", code)
	}
}

func TestSubmitNamesTheNextVersion(t *testing.T) {
	s := submitFarm(t)
	// farm() has show-a/sh040-light-v007
	if got := nextName(s.m, "show-a", "sh040", "light"); got != "sh040-light-v008" {
		t.Fatalf("next %s", got)
	}
	if got := nextName(s.m, "show-b", "sh040", "light"); got != "sh040-light-v001" {
		t.Fatalf("next in another namespace %s", got)
	}
}

func TestSubmitDryRunErrors(t *testing.T) {
	s := submitFarm(t)
	spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-4"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	req := api.SubmitRequest{Project: "show-a", Spec: spec, DryRun: true}
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", req, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("no queue: %d", code)
	}
	s.DryRun = func(context.Context, *c4v1.WorkJob, string) (api.SubmitEstimate, error) {
		return api.SubmitEstimate{}, errors.New("render: frames: bad range")
	}
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", req, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid plan: %d", code)
	}
	// without a dry run the job is still created, with the error noted
	req.DryRun = false
	var out api.SubmitResponse
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", req, &out); code != http.StatusCreated || out.EstimateError == "" {
		t.Fatalf("submit without an estimate: %d %+v", code, out)
	}
}

func writeFile(t *testing.T, dir, rel string, b []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A host runner (c4-hosts/<node>) writes objects as itself, only for
// projects with a unit pod bound to its node.
func TestHostUploadsForItsOwnUnits(t *testing.T) {
	s := submitFarm(t)
	// farm(): show-a has units on w1 and w2; show-b one on w2
	s.DevViewers["host-w1"] = &api.Viewer{User: HostAccountPrefix + "w1", Projects: []string{}}
	s.DevViewers["host-w9"] = &api.Viewer{User: HostAccountPrefix + "w9", Projects: []string{}}
	doc := []byte(`{"done": 3, "of": 5}`)
	id := c4.Identify(bytes.NewReader(doc)).String()
	if code := call(t, s, http.MethodPut, "/api/v1/objects/show-a/"+id+"?as=host-w1", doc, nil); code != 200 {
		t.Fatalf("w1 storing for show-a, whose unit it runs: %d", code)
	}
	var miss api.MissingResponse
	if code := call(t, s, http.MethodPost, "/api/v1/objects/show-a/missing?as=host-w1", api.MissingRequest{IDs: []string{id}}, &miss); code != 200 || len(miss.Missing) != 0 {
		t.Fatalf("missing as the host: %d %v", code, miss.Missing)
	}
	if code := call(t, s, http.MethodPut, "/api/v1/objects/show-b/"+id+"?as=host-w1", doc, nil); code != http.StatusForbidden {
		t.Fatalf("w1 storing for show-b, which runs nothing on it: %d", code)
	}
	if code := call(t, s, http.MethodPut, "/api/v1/objects/show-a/"+id+"?as=host-w9", doc, nil); code != http.StatusForbidden {
		t.Fatalf("a host with no units storing: %d", code)
	}
	spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render"})
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=host-w1", api.SubmitRequest{Project: "show-a", Spec: spec}, nil); code == http.StatusCreated {
		t.Fatal("a host account submitted a job")
	}
}

// A submitter names the tracker entity a job is made for, the version it
// follows, and a date-only due date (DESIGN.md 10.7).
func TestSubmitTrackerRefFollowsDue(t *testing.T) {
	s := submitFarm(t)
	s.DryRun = func(context.Context, *c4v1.WorkJob, string) (api.SubmitEstimate, error) {
		return api.SubmitEstimate{}, nil
	}
	spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-4"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	req := api.SubmitRequest{Project: "show-a", Stage: "light", Shot: "sm0150", Spec: spec}
	var out api.SubmitResponse
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", req, &out); code != http.StatusCreated {
		t.Fatalf("v001: %d %+v", code, out)
	}
	req.Name, req.TrackerRef, req.Follows, req.Due = "sm0150-light-v002", "flow/Task/881", "sm0150-light-v001", "2026-10-23"
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", req, &out); code != http.StatusCreated || out.Name != "sm0150-light-v002" {
		t.Fatalf("v002: %d %+v", code, out)
	}
	var job c4v1.WorkJob
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: "sm0150-light-v002"}, &job); err != nil {
		t.Fatal(err)
	}
	if job.Annotations[c4v1.AnnotationTrackerRef] != "flow/Task/881" || job.Labels[c4v1.LabelTracker] != "flow" || job.Spec.Follows != "sm0150-light-v001" ||
		job.Spec.Deadline == nil || !job.Spec.Deadline.Time.Equal(time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)) || job.Spec.DeadlineMode != "" {
		t.Fatalf("job %v %v follows %q deadline %v mode %q", job.Annotations, job.Labels, job.Spec.Follows, job.Spec.Deadline, job.Spec.DeadlineMode)
	}
	for _, bad := range []api.SubmitRequest{
		{Project: "show-a", Spec: spec, TrackerRef: "flow/881"},
		{Project: "show-a", Spec: spec, Follows: "sm0150-light-v009"},
		{Project: "show-a", Spec: spec, Due: "Friday"},
	} {
		if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", bad, &out); code != http.StatusBadRequest {
			t.Fatalf("%+v: %d", bad, code)
		}
	}
}

// An object list as large as the API allows, 100,000 IDs, is read whole
// (c4 farm sends 50,000 at a time); one over it is refused by count, and
// a body over the list limit with 413.
func TestMissingTakesAFullList(t *testing.T) {
	s := submitFarm(t)
	ids := make([]string, maxListIDs)
	for i := range ids {
		ids[i] = c4.Identify(strings.NewReader(fmt.Sprintf("frame %d", i))).String()
	}
	var miss api.MissingResponse
	if code := call(t, s, http.MethodPost, "/api/v1/objects/show-a/missing?as=artist", api.MissingRequest{IDs: ids}, &miss); code != 200 || len(miss.Missing) != maxListIDs {
		t.Fatalf("%d IDs: %d, %d missing", len(ids), code, len(miss.Missing))
	}
	if code := call(t, s, http.MethodPost, "/api/v1/objects/show-a/missing?as=artist", api.MissingRequest{IDs: append(ids, ids[0])}, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d IDs: %d", len(ids)+1, code)
	}
	big := []byte(`{"ids": ["` + strings.Repeat("x", listBody) + `"]}`)
	if code := call(t, s, http.MethodPost, "/api/v1/objects/show-a/missing?as=artist", big, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over %d bytes: %d", listBody, code)
	}
	// A view of 20,000 files, well past the control limit, is read too.
	var files []api.ViewFile
	for i := 0; i < 20000; i++ {
		files = append(files, api.ViewFile{Path: fmt.Sprintf("frames/sh040.%05d.exr", i), ID: ids[i], Size: 1})
	}
	var vr api.ViewResponse
	if code := call(t, s, http.MethodPost, "/api/v1/views/show-a?as=artist", api.ViewRequest{Files: files}, &vr); code != http.StatusConflict || len(vr.Missing) != 20000 {
		t.Fatalf("a view of 20,000 files: %d, %d missing", code, len(vr.Missing))
	}
}

// A submission is read strictly: a field the request or its spec does not
// have is refused by name, never dropped.
func TestSubmitIsStrict(t *testing.T) {
	s := submitFarm(t)
	for body, want := range map[string]string{
		`{"project": "show-a", "dryRun": true, "spec": {"class": "render", "deadlinMode": "Tracking",
			"template": {"spec": {"containers": [{"name": "unit", "image": "c4-job-hydra"}]}}}}`: `unknown field "deadlinMode"`,
		`{"project": "show-a", "dryRun": true, "spec": {"class": "render",
			"template": {"spec": {"containers": [{"name": "unit", "image": "c4-job-hydra", "imag": "x"}]}}}}`: `unknown field "imag"`,
		`{"project": "show-a", "dryRun": true, "trackRef": "ftrack/task/1", "spec": {"class": "render",
			"template": {"spec": {"containers": [{"name": "unit", "image": "c4-job-hydra"}]}}}}`: `unknown field "trackRef"`,
	} {
		var out struct {
			Error string `json:"error"`
		}
		if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", []byte(body), &out); code != http.StatusBadRequest || !strings.Contains(out.Error, want) {
			t.Errorf("%d %q, want 400 naming %s", code, out.Error, want)
		}
	}
}

// A forecast is submitted with no image (it never runs); a real job
// replaces it, and it is gone. Only a forecast is replaced, and only by a
// real job.
func TestSubmitReplacesAForecast(t *testing.T) {
	s := submitFarm(t)
	s.DryRun = func(context.Context, *c4v1.WorkJob, string) (api.SubmitEstimate, error) {
		return api.SubmitEstimate{}, nil
	}
	fc, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-100"},
		Forecast: &c4v1.WorkForecast{SlotHours: "40"}})
	var out api.SubmitResponse
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", api.SubmitRequest{Project: "show-a", Name: "sh090-forecast", Spec: fc}, &out); code != http.StatusCreated {
		t.Fatalf("forecast: %d %+v", code, out)
	}
	real, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-100"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	bad := []struct {
		req  api.SubmitRequest
		says string
	}{
		{api.SubmitRequest{Project: "show-a", Name: "a", Spec: real, Replaces: "nope"}, "no such job"},
		{api.SubmitRequest{Project: "show-a", Name: "b", Spec: fc, Replaces: "sh090-forecast"}, "a forecast replaces nothing"},
	}
	for _, b := range bad {
		var e map[string]any
		if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", b.req, &e); code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(e["error"]), b.says) {
			t.Errorf("%s: %d %v", b.req.Name, code, e)
		}
	}
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", api.SubmitRequest{Project: "show-a", Name: "sh090", Spec: real, Replaces: "sh090-forecast"}, &out); code != http.StatusCreated {
		t.Fatalf("the real job: %d %+v", code, out)
	}
	var gone c4v1.WorkJob
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: "sh090-forecast"}, &gone); err == nil {
		t.Fatal("the forecast is still there")
	}
	var e map[string]any
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", api.SubmitRequest{Project: "show-a", Name: "c", Spec: real, Replaces: "sh090"}, &e); code != http.StatusBadRequest ||
		!strings.Contains(fmt.Sprint(e["error"]), "real job, not a forecast") {
		t.Fatalf("replacing a real job: %d %v", code, e)
	}
}

// A job a unit submits is its own job's owner's: that owner's runners run
// it and that owner answers for it, and the job names the unit. A unit
// whose job is gone submits nothing.
func TestAUnitsJobIsItsOwners(t *testing.T) {
	s := submitFarm(t)
	ctx := context.Background()
	parent := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "port", UID: "9c1d2e7f-port", Annotations: map[string]string{c4v1.AnnotationOwner: "lee"}},
		Spec: c4v1.WorkJobSpec{Class: "agent", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "x"}}}}}}
	if err := s.Client.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/port/2", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "port", JobUID: string(parent.UID), Unit: "2", Grant: "project"}}
	spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-2"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	var out api.SubmitResponse
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=unit", api.SubmitRequest{Project: "show-a", Name: "child", Spec: spec}, &out); code != http.StatusCreated {
		t.Fatalf("submit: %d %+v", code, out)
	}
	var job c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "child"}, &job); err != nil {
		t.Fatal(err)
	}
	if job.Annotations[c4v1.AnnotationOwner] != "lee" || job.Annotations[c4v1.AnnotationSubmittedByUnit] != schema.UnitRef("show-a", "port", string(parent.UID), "2") ||
		!strings.Contains(job.Annotations[c4v1.AnnotationSubmittedByUnit], "port@") {
		t.Fatalf("annotations %v", job.Annotations)
	}
	s.DevViewers["orphan"] = &api.Viewer{User: "unit:show-a/gone/0", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "gone", Unit: "0", Grant: "project"}}
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=orphan", api.SubmitRequest{Project: "show-a", Name: "child2", Spec: spec}, nil); code != http.StatusForbidden {
		t.Fatalf("a unit of no job submitted: %d", code)
	}
	// its job made again under the same name, by someone else: not its job
	s.DevViewers["stale"] = &api.Viewer{User: "unit:show-a/port/2", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "port", JobUID: "an-earlier-port", Unit: "2", Grant: "project"}}
	var refused map[string]string
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=stale", api.SubmitRequest{Project: "show-a", Name: "child3", Spec: spec}, &refused); code != http.StatusForbidden || !strings.Contains(refused["error"], "was replaced") {
		t.Fatalf("a replaced job's unit submitted: %d %v", code, refused)
	}
	// a pod with no WorkJob controller: said as it is, not as a replacement
	s.DevViewers["loose"] = &api.Viewer{User: "unit:show-a/port/2", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "port", Unit: "2", Grant: "project"}}
	refused = nil
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=loose", api.SubmitRequest{Project: "show-a", Name: "child4", Spec: spec}, &refused); code != http.StatusForbidden || !strings.Contains(refused["error"], "names no WorkJob as its controller") {
		t.Fatalf("a unit pod of no job controller: %d %v", code, refused)
	}
}

// A unit submits in its owner's name, so outside its parent's namespace the
// owner must be allowed to create jobs there; in the parent's namespace the
// owner already was.
func TestAUnitsJobNeedsItsOwnersRight(t *testing.T) {
	s := submitFarm(t)
	ctx := context.Background()
	m := s.current()
	m.projects[0].Spec.Namespaces = append(m.projects[0].Spec.Namespaces, "show-a-fx")
	m.nsProject["show-a-fx"] = "show-a"
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "subjectaccessreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		sar := a.(ktesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		allowed := sar.Spec.User == "lee" && sar.Spec.ResourceAttributes.Namespace == "show-a-fx"
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	})
	s.Kube = kube
	spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-2"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	for _, owner := range []string{"lee", "mallory"} {
		parent := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "port-" + owner, UID: types.UID("uid-" + owner),
			Annotations: map[string]string{c4v1.AnnotationOwner: owner}},
			Spec: c4v1.WorkJobSpec{Class: "agent", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "x"}}}}}}
		if err := s.Client.Create(ctx, parent); err != nil {
			t.Fatal(err)
		}
		s.DevViewers["unit-"+owner] = &api.Viewer{User: "unit:show-a/port-" + owner + "/0", Projects: []string{"show-a"},
			Unit: &api.UnitViewer{Namespace: "show-a", Job: "port-" + owner, JobUID: string(parent.UID), Unit: "0", Grant: "project"}}
		// in the parent's own namespace: no question asked, made
		if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=unit-"+owner, api.SubmitRequest{Project: "show-a", Namespace: "show-a", Name: "here-" + owner, Spec: spec}, nil); code != http.StatusCreated {
			t.Fatalf("%s: a child in its parent's namespace: %d", owner, code)
		}
	}
	// elsewhere in the project: lee may create jobs there, mallory may not
	var out api.SubmitResponse
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=unit-lee", api.SubmitRequest{Project: "show-a", Namespace: "show-a-fx", Name: "there-lee", Spec: spec}, &out); code != http.StatusCreated {
		t.Fatalf("lee's unit in show-a-fx: %d %+v", code, out)
	}
	var refused map[string]string
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=unit-mallory", api.SubmitRequest{Project: "show-a", Namespace: "show-a-fx", Name: "there-mallory", Spec: spec}, &refused); code != http.StatusForbidden ||
		!strings.Contains(refused["error"], "mallory, who may not create jobs in show-a-fx") {
		t.Fatalf("mallory's unit made a job where mallory may not: %d %v", code, refused)
	}
}

// What a job runs under is the farm's to say: POST /jobs naming spec.tool
// is refused, dry run or not, so a client cannot name a tool, or widen its
// networks, by writing the spec itself; only the HUD's lowering of a
// tool's submission sets it.
func TestSubmitRefusesSpecTool(t *testing.T) {
	s := submitFarm(t)
	for _, dry := range []string{"true", "false"} {
		body := `{"project": "show-a", "dryRun": ` + dry + `, "spec": {"class": "render", "tool": {"name": "fetch", "network": ["studio-api"]},
			"template": {"spec": {"containers": [{"name": "unit", "image": "c4-job-hydra"}]}}}}`
		var out struct {
			Error string `json:"error"`
		}
		if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", []byte(body), &out); code != http.StatusBadRequest ||
			!strings.Contains(out.Error, "spec.tool is set by the farm when it lowers a tool's submission") {
			t.Errorf("dryRun %s: %d %q, want 400 refusing spec.tool", dry, code, out.Error)
		}
	}
}

// A job a tracker's sync made for a person, under the person's grant, has
// children bound by the same grant: a unit of it may not ask for a pool or
// a class the grant leaves out, nor submit once the grant is revoked. A
// child it may make carries the grant, so the queue bounds it as it bounds
// the parent.
func TestAUnitsJobKeepsItsParentsGrant(t *testing.T) {
	s := submitFarm(t)
	ctx := context.Background()
	g := &c4v1.SubmitGrant{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: c4v1.SubmitGrantName("github", "dana")},
		Spec: c4v1.SubmitGrantSpec{Person: "dana", Tracker: "github", Pools: []string{"cpu"}, Classes: []string{"render"},
			Until: metav1.NewTime(time.Now().Add(24 * time.Hour)), Record: "c4-grant-record"}}
	parent := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "issue-41", UID: "uid-issue-41",
		Labels:      map[string]string{c4v1.LabelTracker: "github"},
		Annotations: map[string]string{c4v1.AnnotationOwner: "dana", c4v1.AnnotationGrant: "c4-grant-record"}},
		Spec: c4v1.WorkJobSpec{Class: "agent", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "x"}}}}}}
	for _, o := range []client.Object{g, parent} {
		if err := s.Client.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/issue-41/0", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "issue-41", JobUID: "uid-issue-41", Unit: "0", Grant: "project"}}
	submit := func(name, class string, pools ...string) (int, string) {
		spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: class, Pools: pools, Params: map[string]string{"frames": "1-2"},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
		var out map[string]any
		code := call(t, s, http.MethodPost, "/api/v1/jobs?as=unit", api.SubmitRequest{Project: "show-a", Namespace: "show-a", Name: name, Spec: spec}, &out)
		msg, _ := out["error"].(string)
		return code, msg
	}
	if code, msg := submit("in-grant", "render", "cpu"); code != http.StatusCreated {
		t.Fatalf("a child the grant covers: %d %s", code, msg)
	}
	var child c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "in-grant"}, &child); err != nil {
		t.Fatal(err)
	}
	if child.Annotations[c4v1.AnnotationGrant] != "c4-grant-record" || child.Labels[c4v1.LabelTracker] != "github" {
		t.Errorf("the child carries grant %q, tracker %q", child.Annotations[c4v1.AnnotationGrant], child.Labels[c4v1.LabelTracker])
	}
	for _, c := range []struct {
		name, class string
		pools       []string
		says        string
	}{
		{"other-pool", "render", []string{"spot-a"}, "does not cover pool spot-a (only cpu)"},
		{"other-class", "ci", []string{"cpu"}, "does not cover class ci (only render)"},
		{"no-pool", "render", nil, "covers only pools cpu, and the job names none"},
	} {
		code, msg := submit(c.name, c.class, c.pools...)
		if code != http.StatusForbidden || !strings.Contains(msg, "made under dana's grant to tracker github") || !strings.Contains(msg, c.says) {
			t.Errorf("%s: %d %q, want 403 saying %q", c.name, code, msg, c.says)
		}
	}
	g.Spec.Revoked = &c4v1.GrantRevocation{By: "dana", At: metav1.Now()}
	if err := s.Client.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	if code, msg := submit("after-revoke", "render", "cpu"); code != http.StatusForbidden || !strings.Contains(msg, "dana revoked the grant") {
		t.Errorf("after the grant is revoked: %d %q", code, msg)
	}
}
