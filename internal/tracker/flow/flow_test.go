package flow_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/flow"
	"github.com/Avalanche-io/c4-operator/internal/tracker/flow/flowtest"
)

func site(t *testing.T) *flowtest.Server {
	s := flowtest.New()
	t.Cleanup(s.Close)
	return s
}

func adapter(t *testing.T, s *flowtest.Server, key string, secret []byte) *flow.Adapter {
	t.Helper()
	c := tracker.Config{Endpoint: s.URL,
		Scope: map[string]string{"project": strconv.Itoa(s.Project().ID), "script": "c4-sync"},
		Token: func(context.Context) (string, error) { return key, nil }}
	if secret != nil {
		c.WebhookSecret = func(context.Context) ([]byte, error) { return secret, nil }
	}
	a, err := flow.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestConfigRequired(t *testing.T) {
	for _, c := range []tracker.Config{
		{Scope: map[string]string{"project": "1", "script": "s"}},
		{Endpoint: "https://x", Scope: map[string]string{"project": "x", "script": "s"}},
		{Endpoint: "https://x", Scope: map[string]string{"project": "1"}},
	} {
		if _, err := flow.New(c); err == nil {
			t.Errorf("%+v: want an error", c)
		}
	}
	if a, err := tracker.New("flow", tracker.Config{Endpoint: "https://x", Scope: map[string]string{"project": "1", "script": "s"}}); err != nil || a.Kind() != "flow" {
		t.Fatalf("registry: %v", err)
	}
}

func TestWrongKey(t *testing.T) {
	s := site(t)
	a := adapter(t, s, "wrong", nil)
	_, err := a.Get(context.Background(), tracker.Ref{Type: "Task", ID: "1"})
	if err == nil || !strings.Contains(err.Error(), "Can't authenticate script") {
		t.Fatalf("want the site's refusal, got %v", err)
	}
}

func TestTaskDueIsADate(t *testing.T) {
	s := site(t)
	u := s.AddUser("pat", "Pat", "")
	task := s.AddTask(s.AddShot("sh040"), "Lighting", "LGT", "rdy", "2026-10-23", u)
	a := adapter(t, s, s.Key, nil)
	it, err := a.Get(context.Background(), tracker.Ref{Type: "Task", ID: strconv.Itoa(task.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if it.Due == nil || !it.Due.DateOnly || it.Due.At.Format("2006-01-02") != "2026-10-23" ||
		it.Fields["entity.name"] != "sh040" || it.Fields["step"] != "LGT" || it.Status.Native != "rdy" || it.Name != "sh040-LGT" || it.NameUnique {
		t.Fatalf("item: %+v due %+v", it, it.Due)
	}
	if _, err := a.Get(context.Background(), tracker.Ref{Type: "Task", ID: "1"}); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestListFiltersAndPages(t *testing.T) {
	s := site(t)
	u := s.AddUser("pat", "Pat", "")
	shot := s.AddShot("sh040")
	for i := 0; i < 1100; i++ {
		step := "LGT"
		if i%2 == 1 {
			step = "CMP"
		}
		s.AddTask(shot, "t", step, "rdy", "", u)
	}
	a := adapter(t, s, s.Key, nil)
	var all []tracker.Item
	page, pages := "", 0
	for {
		items, next, err := a.List(context.Background(), tracker.Query{Type: "Task", Fields: map[string]string{"step.Step.short_name": "LGT"}}, page)
		if err != nil {
			t.Fatal(err)
		}
		all, pages = append(all, items...), pages+1
		if next == "" {
			break
		}
		page = next
	}
	if len(all) != 550 || pages != 2 {
		t.Fatalf("%d tasks in %d pages, want 550 in 2", len(all), pages)
	}
	if _, _, err := a.List(context.Background(), tracker.Query{Type: "Task", Labels: []string{"x"}}, ""); err == nil {
		t.Fatal("labels are not a Flow concept")
	}
}

func TestChangesFromTheEventLog(t *testing.T) {
	s := site(t)
	u := s.AddUser("pat", "Pat", "")
	shot := s.AddShot("sh040")
	task := s.AddTask(shot, "Lighting", "LGT", "rdy", "2026-10-23", u)
	a := adapter(t, s, s.Key, nil)
	refs, cur, err := a.Changes(context.Background(), "")
	if err != nil || len(refs) != 0 || cur != strconv.Itoa(s.LastEvent()) {
		t.Fatalf("start: %v %q %v (history is not replayed)", refs, cur, err)
	}
	s.Set(task, map[string]any{"due_date": "2026-10-24"}, u)
	s.Set(task, map[string]any{"sg_status_list": "ip"}, u)
	refs, cur2, err := a.Changes(context.Background(), cur)
	if err != nil || len(refs) != 1 || refs[0].ID != strconv.Itoa(task.ID) || cur2 != strconv.Itoa(s.LastEvent()) {
		t.Fatalf("after: %v %q %v", refs, cur2, err)
	}
}

func TestPublishAndDecisions(t *testing.T) {
	s := site(t)
	pat := s.AddUser("pat", "Pat", "")
	dana := s.AddUser("dana", "Dana", "dana@example.com")
	task := s.AddTask(s.AddShot("sh040"), "Lighting", "LGT", "ip", "2026-10-23", pat)
	a := adapter(t, s, s.Key, nil)
	ctx := context.Background()
	out := tracker.Output{Name: "work/flow-1", Version: 7, View: "c4view", JobURL: "https://hud/#/job/work/flow-1", Custody: "https://hud/p", Follows: "work/flow-0"}
	on := tracker.Ref{Type: "Task", ID: strconv.Itoa(task.ID)}
	v, err := a.Publish(ctx, on, out, "k")
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.Publish(ctx, on, out, "k")
	if err != nil || again.ID != v.ID || len(s.All("Version")) != 1 {
		t.Fatalf("publish is not idempotent: %v %v %d", v, again, len(s.All("Version")))
	}
	rec := s.All("Version")[0]
	if rec.Fields["code"] != "sh040_lgt_v007" || rec.Fields["sg_status_list"] != "rev" || rec.Fields["sg_c4_id"] != "c4view" {
		t.Fatalf("version: %v", rec.Fields)
	}
	since := time.Now().Add(-time.Hour)
	vid, _ := strconv.Atoi(v.ID)
	vl := flowtest.Link{Type: "Version", ID: vid}
	s.Set(vl, map[string]any{"sg_status_list": "fix"}, dana)
	s.AddNote(vl, "lighting", "Too hot.", dana, "Rim stays.")
	ds, err := a.Decisions(ctx, v, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Native != "status:fix" || ds[0].By.ID != "dana" || ds[0].Body != "lighting: Too hot.\n> Rim stays." || len(ds[0].Evidence) == 0 {
		t.Fatalf("decisions: %+v", ds)
	}
	// The script's own status write (the publish's rev) is not a decision.
	for _, d := range ds {
		if d.Native == "status:rev" {
			t.Fatal("a script's change was read as a decision")
		}
	}
}

func TestNoteProbe(t *testing.T) {
	s := site(t)
	u := s.AddUser("pat", "Pat", "")
	task := s.AddTask(s.AddShot("sh040"), "Lighting", "LGT", "ip", "", u)
	a := adapter(t, s, s.Key, nil)
	ctx := context.Background()
	w := tracker.Write{On: tracker.Ref{Type: "Task", ID: strconv.Itoa(task.ID)}, Kind: "note",
		Fields: map[string]string{"subject": "C4"}, Note: &tracker.Note{Body: "Needs a person."}, Key: "abc"}
	if done, _, err := a.Probe(ctx, w); err != nil || done {
		t.Fatalf("before: %v %v", done, err)
	}
	ref, err := a.Apply(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if done, got, err := a.Probe(ctx, w); err != nil || !done || got.ID != ref.ID {
		t.Fatalf("after: %v %v %v", done, got, err)
	}
}

func TestReceive(t *testing.T) {
	s := site(t)
	u := s.AddUser("pat", "Pat", "")
	task := s.AddTask(s.AddShot("sh040"), "Lighting", "LGT", "rdy", "", u)
	s.Set(task, map[string]any{"sg_status_list": "ip"}, u)
	secret := []byte("tok")
	a := adapter(t, s, s.Key, secret)
	var got tracker.Delivery
	var gerr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got, gerr = a.Receive(r) }))
	defer srv.Close()
	body := s.Payload(s.LastEvent())
	if _, err := flowtest.Deliver(srv.URL, body, secret, "d1"); err != nil {
		t.Fatal(err)
	}
	if gerr != nil || got.ID != "d1" || got.Self || len(got.Refs) != 1 || got.Refs[0].ID != strconv.Itoa(task.ID) {
		t.Fatalf("delivery: %+v %v", got, gerr)
	}
	if _, err := flowtest.Deliver(srv.URL, body, []byte("nope"), "d2"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(gerr, tracker.ErrBadSignature) {
		t.Fatalf("bad signature: %v", gerr)
	}
}

func TestVersionCode(t *testing.T) {
	cases := map[string]string{
		flow.VersionCode("sh040", "LGT", "Lighting", 7): "sh040_lgt_v007",
		flow.VersionCode("sh040", "", "Comp Final", 12): "sh040_comp_final_v012",
		flow.VersionCode("prop_jar", "MDL", "Model", 0): "prop_jar_mdl_v001",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// A Task's assignees are its people, by login; a Group assigned is none.
func TestTaskAssignees(t *testing.T) {
	s := site(t)
	pat, dana := s.AddUser("pat", "Pat", ""), s.AddUser("dana", "Dana Reyes", "dana@example.com")
	task := s.AddTask(s.AddShot("sh040"), "Lighting", "LGT", "rdy", "2026-10-23", pat)
	s.Set(task, map[string]any{"task_assignees": []flowtest.Link{{Type: "Group", ID: 3, Name: "lighting"}, dana}}, pat)
	a := adapter(t, s, s.Key, nil)
	it, err := a.Get(context.Background(), tracker.Ref{Type: "Task", ID: strconv.Itoa(task.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if len(it.Assignees) != 1 || it.Assignees[0].ID != "dana" || it.Assignees[0].Name != "Dana Reyes" {
		t.Fatalf("assignees %+v", it.Assignees)
	}
	items, _, err := a.List(context.Background(), tracker.Query{Type: "Task", Fields: map[string]string{"sg_status_list": "rdy"}}, "")
	if err != nil || len(items) != 1 || len(items[0].Assignees) != 1 || items[0].Assignees[0].ID != "dana" {
		t.Fatalf("listed %+v %v", items, err)
	}
}
