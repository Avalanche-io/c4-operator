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
	"slices"
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
	ownersMay(s) // what this tests is not the owner's right
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

// ownersMay makes s's API server allow every owner every write.
func ownersMay(s *Server) {
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "subjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	s.Kube = kube
}

// A unit submits in its owner's name, so the owner must be allowed to
// create jobs where it submits, in its parent's namespace as anywhere: the
// API server is asked about the owner with the groups recorded when the
// parent was made, or by name alone, which the refusal says. On a
// single-person farm its one person always may. The child keeps the
// parent's groups.
func TestAUnitsJobNeedsItsOwnersRight(t *testing.T) {
	s := submitFarm(t)
	ctx := context.Background()
	m := s.current()
	m.projects[0].Spec.Namespaces = append(m.projects[0].Spec.Namespaces, "show-a-fx")
	m.nsProject["show-a-fx"] = "show-a"
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "subjectaccessreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		sar := a.(ktesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		ns := sar.Spec.ResourceAttributes.Namespace
		allowed := sar.Spec.User == "lee" || // lee by name, everywhere
			sar.Spec.User == "rin" && ns == "show-a-fx" && slices.Contains(sar.Spec.Groups, "fx-artists") // rin through a group
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	})
	s.Kube = kube
	spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-2"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	parent := func(name, owner, groups string) {
		ann := map[string]string{c4v1.AnnotationOwner: owner}
		if groups != "" {
			ann[c4v1.AnnotationOwnerGroups] = groups
		}
		p := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name, UID: types.UID("uid-" + name), Annotations: ann},
			Spec: c4v1.WorkJobSpec{Class: "agent", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "x"}}}}}}
		if err := s.Client.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		s.DevViewers["unit-"+name] = &api.Viewer{User: "unit:show-a/" + name + "/0", Projects: []string{"show-a"},
			Unit: &api.UnitViewer{Namespace: "show-a", Job: name, JobUID: string(p.UID), Unit: "0", Grant: "project"}}
	}
	parent("port-lee", "lee", "")
	parent("port-mallory", "mallory", "")
	parent("port-rin", "rin", `["fx-artists"]`)
	parent("port-rin-unknown", "rin", "")
	submit := func(parent, ns, name string) (int, string) {
		var out map[string]any
		code := call(t, s, http.MethodPost, "/api/v1/jobs?as=unit-"+parent, api.SubmitRequest{Project: "show-a", Namespace: ns, Name: name, Spec: spec}, &out)
		msg, _ := out["error"].(string)
		return code, msg
	}
	for _, c := range []struct {
		parent, ns, name string
		code             int
		says             string
	}{
		{"port-lee", "show-a", "here-lee", http.StatusCreated, ""},
		{"port-lee", "show-a-fx", "there-lee", http.StatusCreated, ""},
		{"port-mallory", "show-a", "here-mallory", http.StatusForbidden,
			"mallory may not submit in show-a, so a unit of mallory's job may not either (mallory's groups aren't known for a unit's submission"},
		{"port-rin", "show-a-fx", "there-rin", http.StatusCreated, ""},
		{"port-rin", "show-a", "here-rin", http.StatusForbidden, "rin may not submit in show-a, so a unit of rin's job may not either"},
		{"port-rin-unknown", "show-a-fx", "there-rin-unknown", http.StatusForbidden, "rin's groups aren't known for a unit's submission"},
	} {
		code, msg := submit(c.parent, c.ns, c.name)
		if code != c.code || !strings.Contains(msg, c.says) {
			t.Errorf("%s in %s: %d %q, want %d %q", c.parent, c.ns, code, msg, c.code, c.says)
		}
		if c.says != "" && c.name == "here-rin" && strings.Contains(msg, "aren't known") {
			t.Errorf("rin's groups were recorded, yet the refusal says they aren't known: %q", msg)
		}
	}
	var child c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a-fx", Name: "there-rin"}, &child); err != nil {
		t.Fatal(err)
	}
	if child.Annotations[c4v1.AnnotationOwnerGroups] != `["fx-artists"]` {
		t.Errorf("the child's recorded groups: %q", child.Annotations[c4v1.AnnotationOwnerGroups])
	}
	s.SinglePerson = "mallory"
	if code, msg := submit("port-mallory", "show-a", "here-mallory"); code != http.StatusCreated {
		t.Errorf("the one person of a single-person farm: %d %q", code, msg)
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
	ownersMay(s) // what this tests is not the owner's right
	ctx := context.Background()
	g := &c4v1.SubmitGrant{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: c4v1.SubmitGrantName("github", "dana")},
		Spec: c4v1.SubmitGrantSpec{Person: "dana", Tracker: "github", Pools: []string{"cpu"}, Classes: []string{"render"},
			Until: metav1.NewTime(time.Now().Add(24 * time.Hour)), Record: "c4-grant-renewed"}}
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
	m := s.current()
	m.projects[0].Spec.Namespaces = append(m.projects[0].Spec.Namespaces, "show-a-fx")
	m.nsProject["show-a-fx"] = "show-a"
	submitIn := func(ns, name, class string, pools ...string) (int, string) {
		spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: class, Pools: pools, Params: map[string]string{"frames": "1-2"},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
		var out map[string]any
		code := call(t, s, http.MethodPost, "/api/v1/jobs?as=unit", api.SubmitRequest{Project: "show-a", Namespace: ns, Name: name, Spec: spec}, &out)
		msg, _ := out["error"].(string)
		return code, msg
	}
	submit := func(name, class string, pools ...string) (int, string) {
		return submitIn("show-a", name, class, pools...)
	}
	// the grant holds only where the parent is: elsewhere the child could
	// never run
	if code, msg := submitIn("show-a-fx", "elsewhere", "render", "cpu"); code != http.StatusForbidden ||
		!strings.Contains(msg, "a unit of a job made under dana's grant to tracker github submits in show-a-fx; the grant holds only in show-a") {
		t.Errorf("a granted parent's child in another namespace: %d %q", code, msg)
	}
	if code, msg := submit("in-grant", "render", "cpu"); code != http.StatusCreated {
		t.Fatalf("a child the grant covers: %d %s", code, msg)
	}
	var child c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "in-grant"}, &child); err != nil {
		t.Fatal(err)
	}
	// the grant as it is now (renewed since the parent), not the parent's copy
	if child.Annotations[c4v1.AnnotationGrant] != "c4-grant-renewed" || child.Annotations[c4v1.AnnotationGrantTracker] != "github" || c4v1.GrantTracker(&child) != "github" {
		t.Errorf("the child carries grant %q under tracker %q", child.Annotations[c4v1.AnnotationGrant], child.Annotations[c4v1.AnnotationGrantTracker])
	}
	// It is not the tracker's work: no tracker label, so the tracker's
	// sync may not relay decisions to it.
	if _, ok := child.Labels[c4v1.LabelTracker]; ok {
		t.Errorf("the child carries the tracker's label %q", child.Labels[c4v1.LabelTracker])
	}
	sync := &api.Viewer{User: c4v1.SyncAccount}
	wait := &c4v1.WorkWait{Unit: 0, Name: "review", Server: "person:dana"}
	via := &api.Relay{Tracker: "github", Source: "https://github.com/o/r/pull/41", By: "dana", Person: "dana"}
	if _, _, _, err := s.relayed(ctx, sync, &jobRec{job: child}, wait, via); err == nil || !strings.Contains(err.Error(), "is not tracker github's work") {
		t.Errorf("the tracker's sync relayed a decision to a unit's job: %v", err)
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

// A job the HUD makes for a signed-in person records the groups they
// signed in with, for what its units later submit in their name; a
// development viewer's job records none, and so names none.
func TestHUDJobsRecordTheirOwnersGroups(t *testing.T) {
	s := submitFarm(t)
	ownersMay(s)
	spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-2"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", nil)
	for _, c := range []struct {
		name   string
		groups []string
		want   string
	}{{"signed-in", []string{"fx-artists", "system:authenticated"}, `["fx-artists","system:authenticated"]`}, {"dev", nil, ""}} {
		v := &api.Viewer{User: "rin", Projects: []string{"show-a"}, Groups: c.groups}
		if _, code, err := s.createJob(r, v, s.current(), api.SubmitRequest{Project: "show-a", Namespace: "show-a", Name: c.name, Spec: spec}, createOpts{}); code != http.StatusCreated {
			t.Fatalf("%s: %d %v", c.name, code, err)
		}
		var j c4v1.WorkJob
		if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: c.name}, &j); err != nil {
			t.Fatal(err)
		}
		if got := j.Annotations[c4v1.AnnotationOwnerGroups]; got != c.want {
			t.Errorf("%s: recorded groups %q, want %q", c.name, got, c.want)
		}
	}
}

// A unit's job that names a pool running only other people's work is
// refused when submitted, in the farm's words, rather than wait unplaced.
func TestAUnitsJobNamesItsOwnersPools(t *testing.T) {
	s := submitFarm(t)
	ownersMay(s)
	ctx := context.Background()
	m := s.current()
	m.pools = append(m.pools, c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "ivo-desk"}, Spec: c4v1.PoolSpec{Owners: []string{"ivo"}}})
	parent := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "port", UID: "uid-port", Annotations: map[string]string{c4v1.AnnotationOwner: "lee"}},
		Spec: c4v1.WorkJobSpec{Class: "agent", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "x"}}}}}}
	if err := s.Client.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/port/0", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "port", JobUID: "uid-port", Unit: "0", Grant: "project"}}
	submit := func(name string, pools ...string) (int, string) {
		spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Pools: pools, Params: map[string]string{"frames": "1-2"},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
		var out map[string]any
		code := call(t, s, http.MethodPost, "/api/v1/jobs?as=unit", api.SubmitRequest{Project: "show-a", Namespace: "show-a", Name: name, Spec: spec}, &out)
		msg, _ := out["error"].(string)
		return code, msg
	}
	if code, msg := submit("others", "ivo-desk"); code != http.StatusForbidden || !strings.Contains(msg, "pool ivo-desk runs only the jobs of ivo, so a unit of lee's job may not use it") {
		t.Errorf("a pool of ivo's: %d %q", code, msg)
	}
	if code, msg := submit("anyones", "cpu"); code != http.StatusCreated {
		t.Errorf("a pool that runs anyone's: %d %q", code, msg)
	}
}

// On a farm bound as its owner's alone (onePerson) behind the installer's
// token, that owner is the farm's one person, so a unit of their job may
// submit for them without a SubjectAccessReview, whatever
// --single-person-farm says. Bound any other way, or on a HUD the token
// does not guard, the owner is asked about like anyone.
func TestAUnitsJobOnABoundFarm(t *testing.T) {
	s := submitFarm(t)
	ctx := context.Background()
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "subjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{}, nil // no one may, by RBAC
	})
	s.Kube = kube
	parent := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "port", UID: "uid-port", Annotations: map[string]string{c4v1.AnnotationOwner: "noor"}},
		Spec: c4v1.WorkJobSpec{Class: "agent", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "x"}}}}}}
	b := &c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: c4v1.FarmBindingName},
		Spec: c4v1.DeploymentBindingSpec{Owner: "noor", To: "laptop-1/*", OnePerson: true, For: []string{"task/v1"}, Start: metav1.Now(), By: "noor"}}
	for _, o := range []client.Object{parent, b} {
		if err := s.Client.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	s.SinglePerson = "someone-else"
	s.DevViewersOpen, s.InstallerToken = false, []byte(testInstallerToken)
	s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/port/0", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "port", JobUID: "uid-port", Unit: "0", Grant: "project"}}
	spec, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-2"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	submit := func(name string) (int, string) {
		raw, _ := json.Marshal(api.SubmitRequest{Project: "show-a", Namespace: "show-a", Name: name, Spec: spec})
		r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs?as=unit", bytes.NewReader(raw))
		if !s.DevViewersOpen {
			r.Header.Set(InstallerHeader, testInstallerToken)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, out := submit("child"); code != http.StatusCreated {
		t.Errorf("the bound owner's unit: %d %s", code, out)
	}

	// bound to noor, but not as one person's farm
	b.Spec.OnePerson = false
	if err := s.Client.Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	if code, out := submit("child-shared"); code != http.StatusForbidden {
		t.Errorf("a farm bound to noor, not one person's: %d %s", code, out)
	}

	// one person's, but the HUD's development viewers are open to anyone
	b.Spec.OnePerson = true
	if err := s.Client.Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	s.DevViewersOpen, s.InstallerToken = true, nil
	if code, out := submit("child-open"); code != http.StatusForbidden {
		t.Errorf("a one-person binding on an unguarded HUD: %d %s", code, out)
	}
}
