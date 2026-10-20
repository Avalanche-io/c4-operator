package server

import (
	"context"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
	"github.com/Avalanche-io/c4-operator/schema"
)

// submissionFarm is the tool farm with the fixtures' proxies, depth and
// transcode published in show-a, and a dailies Milestone there.
func submissionFarm(t *testing.T) *Server {
	t.Helper()
	s := toolFarm(t)
	ctx := context.Background()
	for _, name := range []string{"proxies", "depth", "transcode"} {
		b, err := fs.ReadFile(schema.Fixtures(), path.Join("tool/pass", name, "tool.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		view := putTree(t, s, "show-a", map[string]string{"tool.yaml": string(b)}).String()
		tool := &c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name}, Spec: c4v1.ToolSpec{View: view}}
		if err := s.Client.Create(ctx, tool); err != nil {
			t.Fatal(err)
		}
		image := "registry.example/" + name + "@sha256:" + strings.Repeat("a", 64)
		tool.Status = c4v1.ToolStatus{Version: "v1", Image: image, Versions: []c4v1.ToolVersion{{Version: "v1", View: view, Image: image,
			Test: c4v1.ToolTest{State: c4v1.ToolTestOK}}}}
		if err := s.Client.Status().Update(ctx, tool); err != nil {
			t.Fatal(err)
		}
	}
	due := time.Date(2026, 10, 22, 22, 0, 0, 0, time.UTC)
	ms := &c4v1.Milestone{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "dailies"},
		Spec: c4v1.MilestoneSpec{Due: metav1.NewTime(due), Source: &c4v1.MilestoneSource{Tracker: "flow", Ref: "Task/881"}}}
	if err := s.Client.Create(ctx, ms); err != nil {
		t.Fatal(err)
	}
	eta := time.Date(2026, 10, 22, 20, 0, 0, 0, time.UTC)
	s.DryRun = func(_ context.Context, job *c4v1.WorkJob, _ string) (api.SubmitEstimate, error) {
		e := eta
		if job.Spec.Params["frames"] == "1" {
			e = eta.Add(time.Hour)
		}
		return api.SubmitEstimate{Units: 6, ETA50: &e, ETA90: &e, Deadline: &due, DeadlineSource: "milestone",
			SlackSeconds: int64(due.Sub(e).Seconds()), Feasible: true}, nil
	}
	return s
}

// submission is a fixture submission as JSON, for project show-a, with
// its tree replaced (as the CLI replaces a folder with its upload's ID).
func submission(t *testing.T, name, tree string) []byte {
	t.Helper()
	b, err := fs.ReadFile(schema.Fixtures(), "submit/pass/"+name+".yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(strings.Replace(string(b), "project: saltmarsh", "project: show-a", 1), "tree: .", "tree: "+tree, 1)
	j, err := schema.ToJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func submit(t *testing.T, s *Server, body api.SubmissionRequest) (int, api.SubmissionResponse) {
	t.Helper()
	var out api.SubmissionResponse
	code := call(t, s, http.MethodPost, "/api/v1/submissions?as=artist", body, &out)
	return code, out
}

// proxies-movie, planned, then created: two jobs named for the whole
// submission, the movie on the proxies' output, both joined to dailies
// and carrying the submission's ID; a tree not yet stored plans but does
// not run.
func TestSubmissionProxiesMovie(t *testing.T) {
	s := submissionFarm(t)
	ctx := context.Background()
	tree := putTree(t, s, "show-b", map[string]string{"plates/sm0140.1001.exr": "plate"}).String() // stored elsewhere
	code, out := submit(t, s, api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree), DryRun: true, Explain: true})
	if code != http.StatusOK || !out.OK || !out.DryRun || len(out.Jobs) != 2 || out.Submission == "" {
		t.Fatalf("dry run: %d %+v", code, out)
	}
	px, mv := out.Jobs[0], out.Jobs[1]
	if px.Name != "sm0140-proxies-v001" || mv.Name != "sm0140-movie-v001" || mv.Tool != "transcode" || mv.Version != "v1" ||
		len(mv.After) != 1 || mv.After[0] != px.Name || px.What != "48 frames in 6 units" || mv.Estimate == nil || px.Link == "" {
		t.Fatalf("jobs %+v", out.Jobs)
	}
	if e := out.Estimate; e == nil || e.Jobs != 2 || !e.ETA50.Equal(*mv.Estimate.ETA50) || e.SlackSeconds != mv.Estimate.SlackSeconds || !e.Feasible {
		t.Fatalf("estimate %+v", out.Estimate)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Explain, "WorkJob show-a/sm0140-movie-v001") {
		t.Fatalf("warnings %v explain %s", out.Warnings, out.Explain)
	}
	var list c4v1.WorkJobList
	if err := s.Client.List(ctx, &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("a dry run created %d jobs", len(list.Items))
	}

	// a real submit needs the tree in the project's store
	if code, out := submit(t, s, api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree)}); code != http.StatusUnprocessableEntity ||
		len(out.Errors) != 1 || out.Errors[0].Path != "tree" {
		t.Fatalf("tree not stored: %d %+v", code, out)
	}
	tree = putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	code, out = submit(t, s, api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree)})
	if code != http.StatusCreated || !out.OK || len(out.Jobs) != 2 {
		t.Fatalf("submit: %d %+v", code, out)
	}
	var movie c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "sm0140-movie-v001"}, &movie); err != nil {
		t.Fatal(err)
	}
	if len(movie.Spec.DependsOn) != 1 || movie.Spec.DependsOn[0] != "sm0140-proxies-v001" || movie.Spec.Params["frames"] != "1" ||
		movie.Labels[c4v1.LabelMilestone] != "dailies" || movie.Labels[c4v1.LabelJobType] != "transcode" ||
		movie.Annotations[authoring.AnnotationSubmission] != out.Submission || movie.Annotations["c4.avalanche.io/derivation-class"] != "exact" {
		t.Fatalf("movie %+v %v %v", movie.Spec, movie.Labels, movie.Annotations)
	}
	c := movie.Spec.Template.Spec.Containers[0]
	if c.Image != "registry.example/transcode@sha256:"+strings.Repeat("a", 64) || strings.Join(c.Command, " ") != "c4-unit run" {
		t.Fatalf("container %+v", c)
	}
	var proxies c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "sm0140-proxies-v001"}, &proxies); err != nil {
		t.Fatal(err)
	}
	if len(proxies.Spec.Inputs) != 2 || proxies.Spec.Inputs[1].Materialization != inputMaterialization(tree) || proxies.Spec.Inputs[1].MountPath != "/show" {
		t.Fatalf("proxies inputs %+v", proxies.Spec.Inputs)
	}
}

// depth runs on its pinned image with its GPU and memory limited; a
// folder tree, a check error and a refusal are each answered with the
// farm's errors, and nothing is created.
func TestSubmissionRefusals(t *testing.T) {
	s := submissionFarm(t)
	ctx := context.Background()
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	code, out := submit(t, s, api.SubmissionRequest{Submission: submission(t, "depth", tree)})
	if code != http.StatusCreated || len(out.Jobs) != 1 {
		t.Fatalf("depth: %d %+v", code, out)
	}
	var depth c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "sm0140-depth-v001"}, &depth); err != nil {
		t.Fatal(err)
	}
	res := depth.Spec.Template.Spec.Containers[0].Resources
	if q := res.Limits["nvidia.com/gpu"]; q.String() != "1" || res.Limits.Memory().String() != "16Gi" || depth.Spec.Deadline == nil {
		t.Fatalf("depth resources %+v deadline %v", res, depth.Spec.Deadline)
	}

	for _, c := range []struct {
		name, doc string
		path      string
	}{
		{"folder", string(submission(t, "proxies", ".")), "tree"},
		{"unknown tool", `{"c4":"submit/1","project":"show-a","shot":"sm0140","tree":"` + tree + `","jobs":{"light":{"tool":"proxie","frames":"1-4","with":{"plates":"p.exr"}}}}`, "jobs.light.tool"},
		{"two afters", `{"c4":"submit/1","project":"show-a","shot":"sm0140","tree":"` + tree + `","jobs":{"a":{"tool":"proxies","frames":"1-4","with":{"plates":"p.exr"}},"b":{"tool":"proxies","frames":"1-4","with":{"plates":"p.exr"}},"c":{"tool":"transcode","frames":"1-4","after":["a","b"],"with":{"plates":"p.exr"}}}}`, "jobs.c.after"},
	} {
		code, out := submit(t, s, api.SubmissionRequest{Submission: []byte(c.doc)})
		if code != http.StatusUnprocessableEntity || out.OK || len(out.Errors) != 1 || out.Errors[0].Path != c.path {
			t.Errorf("%s: %d %+v", c.name, code, out)
		}
	}
	var list c4v1.WorkJobList
	if err := s.Client.List(ctx, &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("refusals created jobs: %d", len(list.Items))
	}
	if code, _ := submit(t, s, api.SubmissionRequest{Submission: []byte(strings.Replace(string(submission(t, "proxies", tree)), "show-a", "show-b", 1))}); code != http.StatusNotFound {
		t.Fatalf("another project: %d", code)
	}
}

// GET /projects/{project} is what a submitter writes for: the root, the
// zone and due time, the milestones soonest first.
func TestProjectDetail(t *testing.T) {
	s := submissionFarm(t)
	var d api.ProjectDetail
	if code := get(t, s, "/api/v1/projects/show-a?as=artist", &d); code != 200 || d.Name != "show-a" || d.Root != "/show" ||
		d.TimeZone != "UTC" || d.DueTime != "18:00" || len(d.Milestones) != 1 || d.Milestones[0].Name != "dailies" || d.Milestones[0].Label != "flow/Task/881" {
		t.Fatalf("%d %+v", code, d)
	}
	if code := get(t, s, "/api/v1/projects/show-b?as=artist", &d); code != http.StatusNotFound {
		t.Fatalf("another project: %d", code)
	}
}
