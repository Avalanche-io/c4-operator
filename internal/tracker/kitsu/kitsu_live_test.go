package kitsu_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/kitsu"
	"github.com/Avalanche-io/c4-operator/internal/tracker/kitsu/kitsulive"
)

// TestLive runs the adapter against a real Kitsu: a throwaway local
// server, never a studio's. It seeds its own production with the admin's
// sign-in, so each run is separate.
//
//	docker run -d --name c4-kitsu -p 127.0.0.1:8091:80 -e MAIL_CHECK_DELIVERABILITY=false cgwire/cgwire:1.0.73
//	KITSU_LIVE=http://127.0.0.1:8091 KITSU_LIVE_ADMIN=admin@example.com:mysecretpassword go test -run TestLive ./internal/tracker/kitsu/
func TestLive(t *testing.T) {
	site, admin := os.Getenv("KITSU_LIVE"), os.Getenv("KITSU_LIVE_ADMIN")
	if site == "" || admin == "" {
		t.Skip("KITSU_LIVE and KITSU_LIVE_ADMIN (email:password) name a throwaway Kitsu")
	}
	ctx := context.Background()
	email, pw, _ := strings.Cut(admin, ":")
	as, err := kitsulive.Login(ctx, site, email, pw)
	if err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprint(time.Now().Unix())
	sh, err := kitsulive.Seed(ctx, as, "Live "+tag, tag, "2026-10-23", 960)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("production %s (%s), task %s", sh.ProjectName, sh.Project, sh.Task)
	a, err := kitsu.New(tracker.Config{Endpoint: site, Scope: map[string]string{"project": sh.ProjectName, "eta_field": "c4_eta"},
		Token: func(context.Context) (string, error) { return sh.BotToken, nil }})
	if err != nil {
		t.Fatal(err)
	}
	on := tracker.Ref{Type: "Task", ID: sh.Task}

	// Reading.
	it, err := a.Get(ctx, on)
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "sh040 Lighting" || it.Due == nil || !it.Due.DateOnly || it.Due.At.Format("2006-01-02") != "2026-10-23" ||
		it.Fields["sequence"] != "sq010" || it.Fields["estimation"] != "960" || it.Status.Native != "ready" ||
		len(it.Assignees) != 1 || it.Assignees[0].ID != sh.Artist.Email {
		t.Fatalf("item: %+v", it)
	}
	items, _, err := a.List(ctx, tracker.Query{Type: "Task", Fields: map[string]string{"task_type": "Lighting", "task_status": "ready",
		"entity.type": "Shot", "sequence": "sq010"}}, "")
	if err != nil || len(items) != 1 || items[0].Ref.ID != sh.Task {
		t.Fatalf("list: %+v %v", items, err)
	}
	_, cursor, err := a.Changes(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	// A status comment, found again by its key; the event log names it.
	time.Sleep(1100 * time.Millisecond) // past the cursor's second
	w := tracker.Write{On: on, Kind: "status", Fields: map[string]string{"flag": "is_wip"}, Key: "live-" + tag,
		Note: &tracker.Note{Body: "C4: running, see https://hud.example/#/job/work/x #lighting"}}
	ref, err := a.Apply(ctx, w)
	if err != nil || ref.ID == "" {
		t.Fatal(ref, err)
	}
	if done, got, err := a.Probe(ctx, w); err != nil || !done || got.ID != ref.ID {
		t.Fatalf("probe: %v %v %v", done, got, err)
	}
	refs, next, err := a.Changes(ctx, cursor)
	if err != nil || len(refs) == 0 || refs[0].ID != sh.Task || next <= cursor {
		t.Fatalf("changes: %v %q %v", refs, next, err)
	}
	if _, err := a.Apply(ctx, tracker.Write{On: on, Kind: "update", Fields: map[string]string{"c4_eta": "2026-10-22T01:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	mref, err := a.Apply(ctx, tracker.Write{On: on, Kind: "mention", Fields: map[string]string{"user": sh.Supervisor.Email},
		Note: &tracker.Note{Body: "C4: units 1001-1004 failed and are assigned to you."}, Key: "m-" + tag})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(ctx, tracker.Write{On: on, Kind: "comment", Note: &tracker.Note{Body: "C4: edited in place.", Replace: &mref}}); err != nil {
		t.Fatal(err)
	}
	var cs []struct {
		ID       string   `json:"id"`
		Text     string   `json:"text"`
		Mentions []string `json:"mentions"`
	}
	if err := as.Do(ctx, "GET", "/data/tasks/"+sh.Task+"/comments", nil, &cs); err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if strings.Contains(c.Text, "#lighting") {
			t.Fatalf("a hashtag went through: %q", c.Text)
		}
	}
	var tk struct {
		Data map[string]any `json:"data"`
	}
	if err := as.Do(ctx, "GET", "/data/tasks/"+sh.Task, nil, &tk); err != nil || tk.Data["c4_eta"] != "2026-10-22T01:00:00Z" {
		t.Fatalf("data: %v %v", tk.Data, err)
	}

	// Publishing with a real PNG: a revision, ready; again, nothing new.
	img := image.NewRGBA(image.Rect(0, 0, 64, 36))
	for x := range 64 {
		for y := range 36 {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 7), 160, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	out := tracker.Output{Version: 1, View: "c4live" + tag, JobURL: "https://hud.example/#/job/work/x", Custody: "https://hud.example/p",
		Preview: &tracker.Still{Name: "sh040.1120.png", Data: b.Bytes()}}
	if _, err := a.Publish(ctx, on, out, "p-"+tag); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Publish(ctx, on, out, "p-"+tag); err != nil {
		t.Fatal(err)
	}
	var ps []struct {
		Revision int    `json:"revision"`
		Status   string `json:"status"`
	}
	if err := as.Do(ctx, "GET", "/data/preview-files?task_id="+sh.Task, nil, &ps); err != nil || len(ps) != 1 || ps[0].Revision != 1 || ps[0].Status != "ready" {
		t.Fatalf("previews: %+v %v", ps, err)
	}

	// Dana's retake is a decision; her done is never moved.
	since := time.Now().UTC().Add(-2 * time.Second)
	dana, err := kitsulive.Login(ctx, site, sh.Supervisor.Email, sh.Supervisor.Password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kitsulive.Comment(ctx, dana, sh.Task, "retake", "Fill light too hot, 1040 to 1060."); err != nil {
		t.Fatal(err)
	}
	ds, err := a.Decisions(ctx, on, since)
	if err != nil || len(ds) != 1 || ds[0].Native != "status:retake" || a.Verdict(ds[0].Native) != "rework" ||
		ds[0].By.ID != sh.Supervisor.Email || !strings.Contains(ds[0].Body, "Fill light too hot") {
		t.Fatalf("decisions: %+v %v", ds, err)
	}
	if _, err := kitsulive.Comment(ctx, dana, sh.Task, "done", "Approved."); err != nil {
		t.Fatal(err)
	}
	if ref, err := a.Apply(ctx, w); err != nil || ref.ID != "" {
		t.Fatalf("moved a done task: %v %v", ref, err)
	}
}
