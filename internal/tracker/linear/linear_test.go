package linear_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/effect"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/linear"
	"github.com/Avalanche-io/c4-operator/internal/tracker/linear/lineartest"
)

var hook = []byte("linear-hook-secret")

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func ctx() context.Context           { return context.Background() }

func setup(t *testing.T) (*lineartest.Server, *linear.Adapter, *clock) {
	t.Helper()
	ck := &clock{t: time.Now().UTC().Truncate(time.Second)}
	ws := lineartest.New("ENG")
	t.Cleanup(ws.Close)
	ws.SetClock(ck.now)
	a, err := linear.New(tracker.Config{Endpoint: ws.URL + "/graphql", Scope: map[string]string{"team": "ENG"},
		Token:         func(context.Context) (string, error) { return ws.Key, nil },
		WebhookSecret: func(context.Context) ([]byte, error) { return hook, nil },
		Now:           ck.now})
	if err != nil {
		t.Fatal(err)
	}
	return ws, a, ck
}

func TestNewNeedsATeam(t *testing.T) {
	if _, err := linear.New(tracker.Config{}); err == nil {
		t.Fatal("an adapter with no team")
	}
}

// Issues are read by label and state type, with their date-only due date,
// assignee, estimate and state; a closed issue is not open work, and a
// field Linear cannot match is refused by name.
func TestListAndGet(t *testing.T) {
	ws, a, _ := setup(t)
	dana := ws.AddUser("Dana Reyes", "dana@example.com")
	render := ws.AddIssue("Render the trailer", "2026-10-23", dana, "c4", "render")
	ws.Update(render.ID, func(i *lineartest.Issue) { i.Estimate = 8; i.Project = "Trailer" })
	ws.AddIssue("Only one label", "", nil, "c4")
	done := ws.AddIssue("Already done", "", nil, "c4", "render")
	ws.SetState(done.ID, "Done", dana)

	items, next, err := a.List(ctx(), tracker.Query{Type: "Issue", Labels: []string{"c4", "render"}}, "")
	if err != nil || next != "" {
		t.Fatalf("list: %v %q", err, next)
	}
	if len(items) != 1 || items[0].Ref.ID != render.ID {
		t.Fatalf("the open issue with both labels: %+v", items)
	}
	it := items[0]
	if it.Due == nil || !it.Due.DateOnly || it.Due.At.Format("2006-01-02") != "2026-10-23" ||
		len(it.Assignees) != 1 || it.Assignees[0].ID != "dana@example.com" || it.Fields["estimate"] != "8" ||
		it.Fields["identifier"] != "ENG-1" || it.Fields["project"] != "Trailer" || it.Status.Native != "Todo" || it.Status.Category != "ready" {
		t.Fatalf("item: %+v", it)
	}
	closed, _, err := a.List(ctx(), tracker.Query{Type: "Issue", Labels: []string{"render"}, State: "closed"}, "")
	if err != nil || len(closed) != 1 || closed[0].Ref.ID != done.ID || closed[0].Status.Category != "done" {
		t.Fatalf("closed: %+v %v", closed, err)
	}
	byProject, _, err := a.List(ctx(), tracker.Query{Type: "Issue", Fields: map[string]string{"project": "Trailer", "stateType": "unstarted"}}, "")
	if err != nil || len(byProject) != 1 {
		t.Fatalf("by project and state type: %+v %v", byProject, err)
	}
	if _, _, err := a.List(ctx(), tracker.Query{Type: "Issue", Fields: map[string]string{"sg_status_list": "rdy"}}, ""); err == nil ||
		!strings.Contains(err.Error(), "sg_status_list") {
		t.Fatalf("an unknown field: %v", err)
	}
	if _, _, err := a.List(ctx(), tracker.Query{Type: "Task"}, ""); !errors.Is(err, tracker.ErrUnsupported) {
		t.Fatalf("a Task: %v", err)
	}
	got, err := a.Get(ctx(), tracker.Ref{Type: "Issue", ID: render.ID})
	if err != nil || got.Title != "Render the trailer" || got.Ref.URL == "" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := a.Get(ctx(), tracker.Ref{Type: "Issue", ID: "nope"}); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("a missing issue: %v", err)
	}
}

// Changes finds issues updated after the cursor; the first call starts
// now; a page boundary does not lose any.
func TestChanges(t *testing.T) {
	ws, a, ck := setup(t)
	old := ws.AddIssue("Before", "", nil)
	refs, cur, err := a.Changes(ctx(), "")
	if err != nil || len(refs) != 0 || cur == "" {
		t.Fatalf("first: %v %v %q", refs, err, cur)
	}
	ck.add(time.Second)
	var made []string
	for i := 0; i < 60; i++ {
		made = append(made, ws.AddIssue("After", "", nil).ID)
	}
	ws.Update(old.ID, func(i *lineartest.Issue) { i.Title = "Before, renamed" })
	refs, cur2, err := a.Changes(ctx(), cur)
	if err != nil || len(refs) != 61 || !later(cur2, cur) {
		t.Fatalf("changes: %d %v %q", len(refs), err, cur2)
	}
	refs, _, err = a.Changes(ctx(), cur2)
	if err != nil || len(refs) != 0 {
		t.Fatalf("nothing new: %d %v", len(refs), err)
	}
}

func later(a, b string) bool {
	ta, _ := time.Parse(time.RFC3339Nano, a)
	tb, _ := time.Parse(time.RFC3339Nano, b)
	return ta.After(tb)
}

// A person's move of the issue to another state is a decision, with the
// comments they wrote since as its text; c4-sync's own moves, and moves
// before since, are not. A completed state approves by default, whatever
// the team calls it.
func TestDecisions(t *testing.T) {
	ws, a, ck := setup(t)
	dana := ws.AddUser("Dana Reyes", "dana@example.com")
	lee := ws.AddUser("Lee Park", "lee@example.com")
	ws.AddState("Changes Requested", "started", 3.5)
	i := ws.AddIssue("Render", "", nil, "c4")
	ws.SetState(i.ID, "In Progress", dana) // before the review
	ck.add(time.Minute)
	since := ck.now()
	ck.add(time.Minute)
	if _, err := a.Apply(ctx(), tracker.Write{On: tracker.Ref{Type: "Issue", ID: i.ID}, Kind: "status",
		Fields: map[string]string{"state": "In Review"}}); err != nil {
		t.Fatal(err)
	}
	ws.AddComment(i.ID, "The rim light is too hot, 1040 to 1060.", dana)
	ws.AddComment(i.ID, "Looks fine to me.", lee)
	ws.SetState(i.ID, "Changes Requested", dana)
	ds, err := a.Decisions(ctx(), tracker.Ref{Type: "Issue", ID: i.ID}, since)
	if err != nil || len(ds) != 1 {
		t.Fatalf("decisions: %+v %v", ds, err)
	}
	d := ds[0]
	if d.Native != "state:Changes Requested" || d.By.ID != "dana@example.com" || d.Body != "The rim light is too hot, 1040 to 1060." ||
		d.Source.Type != "IssueHistory" || len(d.Evidence) == 0 {
		t.Fatalf("decision: %+v", d)
	}
	ck.add(time.Minute)
	ws.SetState(i.ID, "Done", lee)
	ds, _ = a.Decisions(ctx(), tracker.Ref{Type: "Issue", ID: i.ID}, since)
	if len(ds) != 2 || ds[1].Native != "state:Done" || ds[1].By.ID != "lee@example.com" || ds[1].Body != "Looks fine to me." {
		t.Fatalf("decisions after done: %+v", ds)
	}
	if v := a.Verdict("state:Done"); v != "approve" {
		t.Fatalf("a completed state approves: %q", v)
	}
	if v := a.Verdict("state:Changes Requested"); v != "" {
		t.Fatalf("a started state decides nothing by default: %q", v)
	}
	if v := a.Verdict("APPROVED"); v != "" {
		t.Fatalf("GitHub's words mean nothing here: %q", v)
	}
}

// Each write kind and its guard: a state by name or type, never a
// completed one; an attachment keyed by its URL, updated in place; a new
// comment probed by its key, an edit sent again.
func TestWrites(t *testing.T) {
	ws, a, _ := setup(t)
	i := ws.AddIssue("Render", "", nil, "c4")
	on := tracker.Ref{Type: "Issue", ID: i.ID}

	start := tracker.Write{On: on, Kind: "status", Fields: map[string]string{"stateType": "started"}}
	if _, err := a.Apply(ctx(), start); err != nil {
		t.Fatal(err)
	}
	if _, st := ws.Issue(i.ID); st != "In Progress" {
		t.Fatalf("the first started state: %s", st)
	}
	if _, err := a.Apply(ctx(), tracker.Write{On: on, Kind: "status", Fields: map[string]string{"state": "Done"}}); err == nil ||
		!strings.Contains(err.Error(), "reviewer's") {
		t.Fatalf("c4-sync moved an issue to done: %v", err)
	}
	if _, err := a.Apply(ctx(), tracker.Write{On: on, Kind: "status", Fields: map[string]string{"state": "Rendering"}}); err == nil {
		t.Fatal("a state the team does not have")
	}
	// A reviewer's Done stands: a work state written after it is a no-op.
	dana := ws.AddUser("Dana Reyes", "dana@example.com")
	ws.SetState(i.ID, "Done", dana)
	n := ws.Mutations()
	if _, err := a.Apply(ctx(), start); err != nil {
		t.Fatal(err)
	}
	if _, st := ws.Issue(i.ID); st != "Done" || ws.Mutations() != n {
		t.Fatalf("c4-sync moved a done issue to %s", st)
	}
	ws.SetState(i.ID, "Todo", dana)

	card := tracker.Write{On: on, Kind: "attachment", Fields: map[string]string{"url": "https://hud/#/job/work/eng-1", "title": "C4: Running",
		"subtitle": "Running · ETA 18:00", "metadata": `{"job":"work/eng-1"}`}}
	r1, err := a.Apply(ctx(), card)
	if err != nil {
		t.Fatal(err)
	}
	card.Fields["title"] = "C4: Waiting for review by dana"
	r2, err := a.Apply(ctx(), card)
	if err != nil || r2.ID != r1.ID {
		t.Fatalf("the card again: %v %v %v", r1, r2, err)
	}
	if as := ws.Attachments(i.ID); len(as) != 1 || as[0].Title != "C4: Waiting for review by dana" || as[0].Metadata["job"] != "work/eng-1" {
		t.Fatalf("one card, updated: %+v", as)
	}

	note := tracker.Write{On: on, Kind: "comment", Key: "k1", Note: &tracker.Note{Body: "Twelve frames failed."}}
	if done, _, err := a.Probe(ctx(), note); err != nil || done {
		t.Fatalf("probed before it was written: %v %v", done, err)
	}
	c, err := a.Apply(ctx(), note)
	if err != nil || c.ID == "" {
		t.Fatal(err)
	}
	if done, ref, err := a.Probe(ctx(), note); err != nil || !done || ref.ID != c.ID {
		t.Fatalf("the probe finds it by its key: %v %v %v", done, ref, err)
	}
	edit := tracker.Write{On: on, Kind: "comment", Key: "k1", Note: &tracker.Note{Body: "Fourteen frames failed.", Replace: &c}}
	if _, err := a.Apply(ctx(), edit); err != nil {
		t.Fatal(err)
	}
	if cs := ws.Comments(i.ID); len(cs) != 1 || !strings.HasPrefix(cs[0].Body, "Fourteen") || !strings.Contains(cs[0].Body, "[c4-sync k1]") {
		t.Fatalf("one comment, edited: %+v", cs)
	}

	for _, g := range []struct {
		w    tracker.Write
		want effect.Guard
	}{
		{start, effect.Idempotent}, {card, effect.Idempotent}, {edit, effect.Idempotent}, {note, effect.Probed},
		{tracker.Write{Kind: "comment", Note: &tracker.Note{Body: "x"}}, effect.Unguarded},
		{tracker.Write{Kind: "release"}, effect.Unguarded},
	} {
		if got := a.Guard(g.w); got != g.want {
			t.Errorf("guard of %s %+v: %s, want %s", g.w.Kind, g.w.Note, got, g.want)
		}
	}
	if _, err := a.Apply(ctx(), tracker.Write{On: on, Kind: "release"}); !errors.Is(err, tracker.ErrUnsupported) {
		t.Fatalf("an unknown kind: %v", err)
	}
}

// An output is an attachment at its provenance URL carrying its C4 ID:
// publishing again (a crash) updates the same one.
func TestPublish(t *testing.T) {
	ws, a, _ := setup(t)
	i := ws.AddIssue("Render", "", nil, "c4")
	out := tracker.Output{Name: "work/eng-1-v002", Version: 2, JobURL: "https://hud/#/job/work/eng-1-v002", View: "c4OutV2",
		Record: "c4Rec", Custody: "https://hud/#/job/work/eng-1-v002/provenance", Follows: "work/eng-1"}
	r1, err := a.Publish(ctx(), tracker.Ref{Type: "Issue", ID: i.ID}, out, "k")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Publish(ctx(), tracker.Ref{Type: "Issue", ID: i.ID}, out, "k")
	if err != nil || r2.ID != r1.ID {
		t.Fatalf("again: %v %v", r2, err)
	}
	as := ws.Attachments(i.ID)
	if len(as) != 1 || as[0].URL != out.Custody || as[0].Metadata["c4Id"] != "c4OutV2" || as[0].Title != "C4 output, version 2" ||
		!strings.Contains(as[0].Subtitle, "follows work/eng-1") {
		t.Fatalf("published once: %+v", as)
	}
}

// A delivery is verified by its signature and its timestamp, and names
// its issue; c4-sync's own change is an echo.
func TestReceive(t *testing.T) {
	ws, a, ck := setup(t)
	dana := ws.AddUser("Dana Reyes", "dana@example.com")
	i := ws.AddIssue("Render", "", nil, "c4")
	if _, err := a.Decisions(ctx(), tracker.Ref{Type: "Issue", ID: i.ID}, ck.now()); err != nil {
		t.Fatal(err) // learns its own user
	}
	deliver := func(body, secret []byte) (tracker.Delivery, error) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		r.Header.Set("Linear-Signature", lineartest.Sign(body, secret))
		r.Header.Set("Linear-Delivery", "d-1")
		return a.Receive(r)
	}
	d, err := deliver(ws.Payload("Issue", "update", map[string]any{"id": i.ID}, dana), hook)
	if err != nil || d.ID != "d-1" || len(d.Refs) != 1 || d.Refs[0].ID != i.ID || d.Self {
		t.Fatalf("issue delivery: %+v %v", d, err)
	}
	d, err = deliver(ws.Payload("Comment", "create", map[string]any{"id": "c-1", "issueId": i.ID}, &ws.Viewer), hook)
	if err != nil || len(d.Refs) != 1 || d.Refs[0].ID != i.ID || !d.Self {
		t.Fatalf("our own comment: %+v %v", d, err)
	}
	if _, err := deliver(ws.Payload("Issue", "update", map[string]any{"id": i.ID}, dana), []byte("wrong")); !errors.Is(err, tracker.ErrBadSignature) {
		t.Fatalf("a bad signature: %v", err)
	}
	stale := ws.Payload("Issue", "update", map[string]any{"id": i.ID}, dana)
	ck.add(2 * time.Minute)
	if _, err := deliver(stale, hook); !errors.Is(err, tracker.ErrBadSignature) {
		t.Fatalf("a replayed delivery: %v", err)
	}
}

// A rate limit says until when; a refusal is definite; a lost answer is
// not, so the effect journal holds what nothing guards.
func TestErrors(t *testing.T) {
	ws, a, ck := setup(t)
	i := ws.AddIssue("Render", "", nil, "c4")
	ws.RateLimit(1)
	_, err := a.Get(ctx(), tracker.Ref{Type: "Issue", ID: i.ID})
	var rl *tracker.RateLimited
	if !errors.As(err, &rl) || !rl.Until.After(ck.now()) {
		t.Fatalf("rate limited: %v", err)
	}
	if _, err := a.Get(ctx(), tracker.Ref{Type: "Issue", ID: i.ID}); err != nil {
		t.Fatalf("after the limit: %v", err)
	}
	_, err = a.Apply(ctx(), tracker.Write{On: tracker.Ref{Type: "Issue", ID: i.ID}, Kind: "attachment", Fields: map[string]string{"url": "u"}})
	if err == nil {
		t.Fatal("an attachment with no title")
	}
	ws.LoseAnswer("CommentCreate", 1)
	_, err = a.Apply(ctx(), tracker.Write{On: tracker.Ref{Type: "Issue", ID: i.ID}, Kind: "comment", Key: "k", Note: &tracker.Note{Body: "x"}})
	if err == nil || tracker.Definite(err) {
		t.Fatalf("a lost answer may have followed the write: %v", err)
	}
	if cs := ws.Comments(i.ID); len(cs) != 1 {
		t.Fatalf("the comment was made: %d", len(cs))
	}
	bad, _ := linear.New(tracker.Config{Endpoint: ws.URL + "/graphql", Scope: map[string]string{"team": "ENG"},
		Token: func(context.Context) (string, error) { return "lin_api_wrong", nil }})
	if _, err := bad.Get(ctx(), tracker.Ref{Type: "Issue", ID: i.ID}); !errors.Is(err, tracker.ErrRefused) {
		t.Fatalf("a wrong key is refused: %v", err)
	}
}
