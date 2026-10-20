package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/schema"
)

// TestFarmCLIAgainstTheHUD drives `c4 farm submit` and `get` against the
// real HUD handler: the tree is hashed on "this machine", only what the
// store lacks is uploaded, the view the farm records has the same ID, the
// job is created owned by the viewer, and the files come back with their
// IDs checked.
func TestFarmCLIAgainstTheHUD(t *testing.T) {
	s := submitFarm(t)
	la, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 10, 23, 14, 10, 0, 0, la)
	s.DryRun = func(_ context.Context, job *c4v1.WorkJob, _ string) (api.SubmitEstimate, error) {
		eta, p90 := now.Add(42*time.Minute), now.Add(54*time.Minute)
		dl := now.Add(3 * time.Hour)
		if job.Spec.Deadline != nil {
			dl = job.Spec.Deadline.Time
		}
		return api.SubmitEstimate{Units: 4, ETA50: &eta, ETA90: &p90, Deadline: &dl, DeadlineSource: "spec",
			SlackSeconds: int64(dl.Sub(eta).Seconds()), Feasible: true}, nil
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := &farmcli.Client{Base: ts.URL, As: "artist", HTTP: ts.Client()}

	root := t.TempDir()
	writeFile(t, root, ".c4project", nil)
	writeFile(t, root, "sm0140/sm0140.usda", []byte("#usda 1.0\n(subLayers = [@lights/key.usda@])\n"))
	writeFile(t, root, "sm0140/lights/key.usda", []byte("#usda 1.0\nintensity = 8\n"))
	writeFile(t, root, "sm0140/tex/heron.exr", bytes.Repeat([]byte("feathers"), 4096))

	var out bytes.Buffer
	o := farmcli.Out{W: &out, Clock: farmcli.Clock{Now: func() time.Time { return now }, Loc: la}}
	args := farmcli.SubmitArgs{Type: "render", Asset: filepath.Join(root, "sm0140", "sm0140.usda"), Project: "show-a",
		Deadline: "18:00", Yes: true, Options: farmcli.Options{Frames: "1001-1004"}}
	resp, err := farmcli.Submit(context.Background(), c, o, args)
	if err != nil {
		t.Fatalf("submit: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"sm0140-light-v001   render · 4 frames · Cycles · pool cpu", "4 files found from sm0140.usda",
		"4 to upload", "ready ~14:52 PDT  (p90 ~15:04 PDT)", "deadline  18:00 PDT today (01:00 UTC) · slack ~3h 8m",
		"uploaded  4 files", "queued    show-a/sm0140-light-v001", "?as=artist#/job/show-a/sm0140-light-v001"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	var job c4v1.WorkJob
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: resp.Name}, &job); err != nil {
		t.Fatal(err)
	}
	want, err := c4view.ScanID(root)
	if err != nil {
		t.Fatal(err)
	}
	var mat c4v1.Materialization
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: job.Spec.Inputs[0].Materialization}, &mat); err != nil {
		t.Fatal(err)
	}
	if mat.Spec.Source.ID != want.String() || job.Annotations[c4v1.AnnotationOwner] != "maya" ||
		job.Spec.Deadline == nil || !job.Spec.Deadline.Time.Equal(time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("job: view %s (want %s), owner %q, deadline %v", mat.Spec.Source.ID, want, job.Annotations[c4v1.AnnotationOwner], job.Spec.Deadline)
	}
	if args := job.Spec.Template.Spec.Containers[0].Args[0]; !strings.Contains(args, "set -- --disableGpu --renderer Cycles") ||
		!strings.Contains(args, `usdrecord "$@" /show/sm0140/sm0140.usda /show/render/sm0140.####.exr`) {
		t.Fatalf("script:\n%s", args)
	}

	// one file changes: only it is uploaded, and the job is the next version
	writeFile(t, root, "sm0140/lights/key.usda", []byte("#usda 1.0\nintensity = 10\n"))
	s.m.jobs = append(s.m.jobs, &jobRec{job: job, key: "show-a/" + job.Name, project: "show-a"})
	out.Reset()
	if _, err := farmcli.Submit(context.Background(), c, o, args); err != nil {
		t.Fatalf("resubmit: %v\n%s", err, out.String())
	}
	if got := out.String(); !strings.Contains(got, "sm0140-light-v002") || !strings.Contains(got, "3 already on the farm · 1 to upload") ||
		!strings.Contains(got, "uploaded  1 file ·") {
		t.Fatalf("resubmit output:\n%s", got)
	}

	// take the tree home by its view ID, then again: nothing is rewritten
	home := t.TempDir()
	out.Reset()
	if err := farmcli.Get(context.Background(), c, o, farmcli.GetArgs{Project: "show-a", Ref: mat.Spec.Source.ID, Paths: []string{"sm0140/tex"}, Dir: home}); err != nil {
		t.Fatalf("get: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, "sm0140", "tex", "heron.exr"))
	if err != nil || !bytes.Equal(b, bytes.Repeat([]byte("feathers"), 4096)) {
		t.Fatalf("got back %d bytes: %v", len(b), err)
	}
	if !strings.Contains(out.String(), "1 file · 32 KB · every ID checked") {
		t.Fatalf("get output: %s", out.String())
	}
	out.Reset()
	farmcli.Get(context.Background(), c, o, farmcli.GetArgs{Project: "show-a", Ref: mat.Spec.Source.ID, Paths: []string{"sm0140/tex"}, Dir: home})
	if !strings.Contains(out.String(), "0 files · 0 B · every ID checked · 1 already here") {
		t.Fatalf("second get: %s", out.String())
	}
}

// An API call the HUD lacks answers 404 in JSON, not the web page, so
// `c4 farm submit TYPE` on a farm without tools falls back to the types.
func TestUnknownAPICallIsJSON404(t *testing.T) {
	ts := httptest.NewServer(submitFarm(t).Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/v1/no-such-call/x?as=artist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	if resp.StatusCode != http.StatusNotFound || json.NewDecoder(resp.Body).Decode(&body) != nil || body["error"] == "" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
}

// `c4 farm tool publish`, `tools` and `tool show` against the real HUD: the
// CLI uploads the folder, the farm runs its test (played here), the CLI
// waits for the result, and the tool is then listed and described.
func TestFarmCLIPublishesATool(t *testing.T) {
	s := toolFarm(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := &farmcli.Client{Base: ts.URL, As: "artist", HTTP: ts.Client()}
	dir := filepath.Join(t.TempDir(), "notes")
	for p, body := range notesTool("echo hi > out.txt") {
		writeFile(t, dir, p, []byte(body))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- farmcli.PublishTool(ctx, c, farmcli.Out{W: &out}, farmcli.PublishArgs{Dir: dir, Project: "show-a", Poll: 20 * time.Millisecond, Wait: 15 * time.Second})
	}()
	// the queue's part: the test job runs and leaves what the test expects
	var job string
	for job == "" {
		select {
		case err := <-done:
			t.Fatalf("publish ended before its test ran: %v\n%s", err, out.String())
		case <-ctx.Done():
			t.Fatal("no test job")
		case <-time.After(20 * time.Millisecond):
		}
		var jobs c4v1.WorkJobList
		if err := s.Client.List(ctx, &jobs); err == nil && len(jobs.Items) > 0 {
			job = jobs.Items[0].Namespace + "/" + jobs.Items[0].Name
		}
	}
	finishTest(t, s, job, "Succeeded", map[string]string{"out.txt": "hi"})
	if err := <-done; err != nil {
		t.Fatalf("publish: %v\n%s", err, out.String())
	}
	for _, want := range []string{"notes v1", "test on the farm   " + job, "ok in", `published to show-a: artists see "Notes" in Submit`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := farmcli.ListTools(ctx, c, farmcli.Out{W: &out}, "show-a"); err != nil || !strings.Contains(out.String(), "notes  once    v1    Notes") {
		t.Errorf("tools: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := farmcli.ShowTool(ctx, c, farmcli.Out{W: &out}, "show-a", "notes"); err != nil || !strings.Contains(out.String(), "--set width=int (default 960)") {
		t.Errorf("tool show: %v\n%s", err, out.String())
	}
}

// c4 farm submit FILE against the real submissions handler: the folder
// beside the file is uploaded and named by its ID, the farm plans the
// jobs in the order written, and they are created with the folder as the
// submission's root.
func TestFarmCLISubmitsAFile(t *testing.T) {
	s := submissionFarm(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := &farmcli.Client{Base: ts.URL, As: "artist", HTTP: ts.Client()}
	dir := t.TempDir()
	b, err := fs.ReadFile(schema.Fixtures(), "submit/pass/proxies-movie.yaml")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "submit.yaml", []byte(strings.Replace(string(b), "project: saltmarsh", "project: show-a", 1)))
	writeFile(t, dir, "plates/sm0140.1001.exr", []byte("plate"))
	ctx := context.Background()
	var out bytes.Buffer
	plan, err := farmcli.SubmitFile(ctx, c, farmcli.Out{W: &out}, farmcli.SubmitFileArgs{Path: filepath.Join(dir, "submit.yaml"), DryRun: true})
	if err != nil || len(plan.Jobs) != 2 || plan.Jobs[0].Name != "sm0140-proxies-v001" || plan.Jobs[1].Name != "sm0140-movie-v001" {
		t.Fatalf("plan %+v %v\n%s", plan, err, out.String())
	}
	if !strings.Contains(out.String(), "uploaded  2 files from .") || !strings.Contains(out.String(), "sm0140-movie-v001     ") || !strings.Contains(out.String(), "note      ") {
		t.Errorf("plan output:\n%s", out.String())
	}
	out.Reset()
	done, err := farmcli.SubmitFile(ctx, c, farmcli.Out{W: &out}, farmcli.SubmitFileArgs{Path: filepath.Join(dir, "submit.yaml"), Yes: true})
	if err != nil || len(done.Jobs) != 2 {
		t.Fatalf("submit %+v %v\n%s", done, err, out.String())
	}
	if !strings.Contains(out.String(), "queued    show-a/sm0140-proxies-v001\nqueued    show-a/sm0140-movie-v001\n") {
		t.Errorf("submit output:\n%s", out.String())
	}
	var movie c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "sm0140-movie-v001"}, &movie); err != nil {
		t.Fatal(err)
	}
	root := ""
	for _, e := range movie.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "C4_SOURCE_ROOTS" {
			root = e.Value
		}
	}
	if !strings.Contains(root, `"path":"`+filepath.ToSlash(dir)+`"`) {
		t.Errorf("C4_SOURCE_ROOTS %q, want the folder %s", root, dir)
	}
}
