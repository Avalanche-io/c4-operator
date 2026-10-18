package server

import (
	"bytes"
	"context"
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
	if args := job.Spec.Template.Spec.Containers[0].Args[0]; !strings.Contains(args, "usdrecord --renderer Cycles") ||
		!strings.Contains(args, "/show/sm0140/sm0140.usda /show/render/sm0140.####.exr") {
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
