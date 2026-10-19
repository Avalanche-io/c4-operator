package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
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
	return s
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
		job.Labels[c4v1.LabelShot] != "sm0140" || job.Labels[c4v1.LabelJobType] != "render" {
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
