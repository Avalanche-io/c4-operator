package kitsu_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/effect"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/kitsu"
	"github.com/Avalanche-io/c4-operator/internal/tracker/kitsu/kitsutest"
)

// show is a site with a bot, a supervisor and an artist, sequence sq010,
// shot sh040 and its lighting task, ready, due 23 October, bid two days.
type show struct {
	site              *kitsutest.Server
	bot, dana, artist *kitsutest.Person
	shot, task        string
}

func newShow(t *testing.T) *show {
	s := kitsutest.New()
	t.Cleanup(s.Close)
	sh := &show{site: s}
	sh.bot = s.AddPerson("C4", "Bot", "c4-bot@example.com", "manager", true)
	sh.dana = s.AddPerson("Dana", "Reyes", "Dana@example.com", "supervisor", false)
	sh.artist = s.AddPerson("Mo", "Okafor", "mo@example.com", "user", false)
	seq := s.AddEntity("Sequence", "sq010", "")
	sh.shot = s.AddEntity("Shot", "sh040", seq)
	sh.task = s.AddTask(sh.shot, "Lighting", "ready", "2026-10-23", 960, sh.artist.ID)
	return sh
}

func (sh *show) adapter(t *testing.T, scope map[string]string) *kitsu.Adapter {
	t.Helper()
	sc := map[string]string{"project": "Saltmarsh"}
	for k, v := range scope {
		sc[k] = v
	}
	tok := sh.bot.Token
	a, err := kitsu.New(tracker.Config{Endpoint: sh.site.URL, Scope: sc,
		Token: func(context.Context) (string, error) { return tok, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestConfigRequired(t *testing.T) {
	for _, c := range []tracker.Config{
		{Scope: map[string]string{"project": "p"}},
		{Endpoint: "https://kitsu.example"},
	} {
		if _, err := kitsu.New(c); err == nil {
			t.Errorf("%+v: want an error", c)
		}
	}
	a, err := tracker.New("kitsu", tracker.Config{Endpoint: "https://kitsu.example/api/", Scope: map[string]string{"project": "p"}})
	if err != nil || a.Kind() != "kitsu" {
		t.Fatalf("registry: %v", err)
	}
	if u := a.(*kitsu.Adapter).TaskURL("P", "Shot", "T"); u != "https://kitsu.example/productions/P/shots/tasks/T" {
		t.Fatalf("an endpoint ending in /api is the site: %s", u)
	}
}

// A revoked token (a bot's, regenerated) and a string that is no token are
// refusals that say the credential is the problem.
func TestTokenRefused(t *testing.T) {
	sh := newShow(t)
	a := sh.adapter(t, nil)
	sh.site.Regenerate(sh.bot)
	_, err := a.Get(context.Background(), tracker.Ref{Type: "Task", ID: sh.task})
	if !errors.Is(err, tracker.ErrRefused) || !strings.Contains(err.Error(), "Token has been revoked") {
		t.Fatalf("revoked: %v", err)
	}
	b, _ := kitsu.New(tracker.Config{Endpoint: sh.site.URL, Scope: map[string]string{"project": "Saltmarsh"},
		Token: func(context.Context) (string, error) { return "nonsense", nil }})
	if _, err := b.Get(context.Background(), tracker.Ref{Type: "Task", ID: sh.task}); !errors.Is(err, tracker.ErrRefused) ||
		!strings.Contains(err.Error(), "token was refused") {
		t.Fatalf("not a token: %v", err)
	}
}

func TestTaskIsWork(t *testing.T) {
	sh := newShow(t)
	a := sh.adapter(t, nil)
	it, err := a.Get(context.Background(), tracker.Ref{Type: "Task", ID: sh.task})
	if err != nil {
		t.Fatal(err)
	}
	if it.Due == nil || !it.Due.DateOnly || it.Due.At.Format("2006-01-02") != "2026-10-23" || it.Title != "sh040 Lighting" ||
		it.Status.Native != "ready" || it.Status.Category != "ready" || it.Parent == nil || it.Parent.Type != "Shot" || it.Name != "sh040 Lighting" || it.NameUnique {
		t.Fatalf("item: %+v due %+v", it, it.Due)
	}
	f := it.Fields
	if f["entity.name"] != "sh040" || f["sequence"] != "sq010" || f["task_type"] != "Lighting" || f["estimation"] != "960" {
		t.Fatalf("fields: %v", f)
	}
	if len(it.Assignees) != 1 || it.Assignees[0].ID != "mo@example.com" || it.Assignees[0].Name != "Mo Okafor" {
		t.Fatalf("assignees: %+v", it.Assignees)
	}
	if !strings.HasSuffix(it.Ref.URL, "/shots/tasks/"+sh.task) {
		t.Fatalf("url: %s", it.Ref.URL)
	}
	if _, err := a.Get(context.Background(), tracker.Ref{Type: "Task", ID: "00000000-0000-0000-0000-000000000000"}); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestListMatchesKitsuWords(t *testing.T) {
	sh := newShow(t)
	s := sh.site
	seq2 := s.AddEntity("Sequence", "sq020", "")
	s.AddTask(s.AddEntity("Shot", "sh100", seq2), "Lighting", "ready", "", 0)
	s.AddTask(sh.shot, "Compositing", "ready", "", 0)
	s.AddTask(sh.shot, "Lighting", "wip", "", 0) // a second lighting task, in progress
	s.AddTask(s.AddEntity("Prop", "jar", ""), "Lighting", "ready", "", 0)
	a := sh.adapter(t, nil)
	list := func(f map[string]string) []tracker.Item {
		t.Helper()
		items, next, err := a.List(context.Background(), tracker.Query{Type: "Task", Fields: f}, "")
		if err != nil || next != "" {
			t.Fatalf("%v: %v %q", f, err, next)
		}
		return items
	}
	if got := list(map[string]string{"task_type": "lighting", "task_status": "ready"}); len(got) != 3 {
		t.Fatalf("ready lighting: %d", len(got))
	}
	got := list(map[string]string{"task_type": "Lighting", "task_status": "ready", "entity.type": "Shot", "sequence": "sq010"})
	if len(got) != 1 || got[0].Ref.ID != sh.task {
		t.Fatalf("sq010 shots: %+v", got)
	}
	if got := list(map[string]string{"task_type": "Lighting", "entity.type": "Prop"}); len(got) != 1 || !strings.Contains(got[0].Ref.URL, "/assets/tasks/") {
		t.Fatalf("props: %+v", got)
	}
	if got := list(map[string]string{"assignees": sh.artist.ID}); len(got) != 1 {
		t.Fatalf("by assignee column: %d", len(got))
	}
	if _, _, err := a.List(context.Background(), tracker.Query{Type: "Task", Fields: map[string]string{"task_type": "Lightning"}}, ""); err == nil {
		t.Fatal("a task type that does not exist is an error, not an empty answer")
	}
	if _, _, err := a.List(context.Background(), tracker.Query{Type: "Task", Labels: []string{"x"}}, ""); err == nil {
		t.Fatal("labels are not a Kitsu concept")
	}
}

func TestChangesFromTheEventLog(t *testing.T) {
	sh := newShow(t)
	ck := time.Date(2026, 10, 20, 10, 0, 0, 250e6, time.UTC)
	sh.site.SetClock(func() time.Time { return ck })
	a := sh.adapter(t, nil)
	ctx := context.Background()
	refs, cur, err := a.Changes(ctx, "")
	if err != nil || len(refs) != 0 || cur != "1970-01-01T00:00:00" {
		t.Fatalf("an empty log: %v %q %v", refs, cur, err)
	}
	sh.site.Comment(sh.dana, sh.task, "wip", "go")
	refs, cur, err = a.Changes(ctx, "")
	if err != nil || len(refs) != 0 || cur != "2026-10-20T10:00:00" {
		t.Fatalf("start: %v %q %v (history is not replayed)", refs, cur, err)
	}
	ck = ck.Add(500 * time.Millisecond) // the same second, stored finer
	sh.site.SetTask(sh.dana, sh.task, map[string]any{"due": "2026-10-24"})
	refs, cur2, err := a.Changes(ctx, cur)
	if err != nil || len(refs) != 1 || refs[0].ID != sh.task || cur2 != cur {
		t.Fatalf("same second: %v %q %v", refs, cur2, err)
	}
	ck = ck.Add(2 * time.Second)
	other := sh.site.AddTask(sh.shot, "Compositing", "ready", "", 0)
	sh.site.Comment(sh.dana, other, "", "hello")
	refs, cur3, err := a.Changes(ctx, cur2)
	if err != nil || len(refs) != 2 || cur3 != "2026-10-20T10:00:02" {
		t.Fatalf("after: %v %q %v", refs, cur3, err)
	}
	if _, _, err := a.Changes(ctx, "yesterday"); err == nil {
		t.Fatal("a bad cursor")
	}
}

// More than a thousand events since the cursor are read in pages back.
func TestChangesPagesBack(t *testing.T) {
	sh := newShow(t)
	ck := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	sh.site.SetClock(func() time.Time { return ck })
	a := sh.adapter(t, nil)
	tasks := []string{sh.task}
	for i := 0; i < 3; i++ {
		tasks = append(tasks, sh.site.AddTask(sh.shot, "Compositing", "ready", "", 0))
	}
	for i := 0; i < 700; i++ {
		ck = ck.Add(time.Second)
		sh.site.Comment(sh.dana, tasks[i%len(tasks)], "", "note")
	}
	refs, next, err := a.Changes(context.Background(), "2026-10-20T10:00:00")
	if err != nil || len(refs) != 4 || next != ck.Format("2006-01-02T15:04:05") {
		t.Fatalf("%d refs to %q: %v", len(refs), next, err)
	}
	n := 0
	for _, c := range sh.site.Calls() {
		if strings.HasPrefix(c.Path, "/api/data/events/last") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d event reads for 1400 events, want 2", n)
	}
}

// A status write is a comment carrying the status; none when the Task is
// in it already. Its key is on it, and Probe finds it.
func TestStatusIsAComment(t *testing.T) {
	sh := newShow(t)
	a := sh.adapter(t, nil)
	ctx := context.Background()
	on := tracker.Ref{Type: "Task", ID: sh.task}
	w := tracker.Write{On: on, Kind: "status", Fields: map[string]string{"flag": "is_wip"}, Key: "k1",
		Note: &tracker.Note{Body: "C4: running #lighting, see https://hud/#/job/x"}}
	if a.Guard(w) != effect.Probed {
		t.Fatalf("guard: %v", a.Guard(w))
	}
	if done, _, err := a.Probe(ctx, w); err != nil || done {
		t.Fatalf("before: %v %v", done, err)
	}
	ref, err := a.Apply(ctx, w)
	if err != nil || ref.Type != "Comment" {
		t.Fatal(ref, err)
	}
	if sh.site.StatusOf(sh.task) != "wip" {
		t.Fatalf("status: %s", sh.site.StatusOf(sh.task))
	}
	cs := sh.site.Comments(sh.task)
	if len(cs) != 1 || !strings.Contains(cs[0].Text, "[c4-sync k1]") || strings.Contains(cs[0].Text, "#lighting") ||
		!strings.Contains(cs[0].Text, "https://hud/#/job/x") {
		t.Fatalf("comment: %+v", cs)
	}
	for _, task := range []string{sh.task} {
		if n := len(sh.site.Comments(task)); n != 1 {
			t.Fatalf("a hashtag in c4-sync's text was posted on other tasks: %d", n)
		}
	}
	if done, got, err := a.Probe(ctx, w); err != nil || !done || got.ID != ref.ID {
		t.Fatalf("after: %v %v %v", done, got, err)
	}
	again, err := a.Apply(ctx, tracker.Write{On: on, Kind: "status", Fields: map[string]string{"task_status": "wip"}, Key: "k2"})
	if err != nil || again.ID != "" || len(sh.site.Comments(sh.task)) != 1 {
		t.Fatalf("a status the Task has already wrote a comment: %v %v", again, err)
	}
	if _, err := a.Apply(ctx, tracker.Write{On: on, Kind: "status", Fields: map[string]string{"task_status": "nope"}}); err == nil {
		t.Fatal("an unknown status")
	}
}

func TestDataUpdateMerges(t *testing.T) {
	sh := newShow(t)
	a := sh.adapter(t, nil)
	ctx := context.Background()
	on := tracker.Ref{Type: "Task", ID: sh.task}
	if _, err := a.Apply(ctx, tracker.Write{On: on, Kind: "update", Fields: map[string]string{"c4_eta": "2026-10-22T01:00:00Z", "c4_link": "https://hud"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(ctx, tracker.Write{On: on, Kind: "update", Fields: map[string]string{"c4_eta": ""}}); err != nil {
		t.Fatal(err)
	}
	d := sh.site.Task(sh.task).Data
	if d["c4_eta"] != "2026-10-22T01:00:00Z" || d["c4_link"] != "https://hud" {
		t.Fatalf("an empty write cleared the data: %v", d)
	}
	if a.Guard(tracker.Write{Kind: "update"}) != effect.Idempotent {
		t.Fatal("guard")
	}
}

// A mention names the person as Kitsu reads it: "@First Last" and a space.
// An edit keeps the comment and is idempotent.
func TestMentionAndEdit(t *testing.T) {
	sh := newShow(t)
	a := sh.adapter(t, nil)
	ctx := context.Background()
	on := tracker.Ref{Type: "Task", ID: sh.task}
	ref, err := a.Apply(ctx, tracker.Write{On: on, Kind: "mention", Fields: map[string]string{"user": "Dana@example.com"},
		Note: &tracker.Note{Body: "C4: units 1001-1004 failed and are assigned to you."}, Key: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	cs := sh.site.Comments(sh.task)
	if len(cs) != 1 || len(cs[0].Mentions) != 1 || cs[0].Mentions[0] != sh.dana.ID || sh.site.StatusOf(sh.task) != "ready" {
		t.Fatalf("mention: %+v", cs)
	}
	w := tracker.Write{On: on, Kind: "comment", Note: &tracker.Note{Body: "edited", Replace: &ref}, Key: "m1"}
	if a.Guard(w) != effect.Idempotent {
		t.Fatal("an edit is idempotent")
	}
	if _, err := a.Apply(ctx, w); err != nil {
		t.Fatal(err)
	}
	if cs := sh.site.Comments(sh.task); len(cs) != 1 || !strings.HasPrefix(cs[0].Text, "edited") {
		t.Fatalf("edit: %+v", cs)
	}
	if _, err := a.Apply(ctx, tracker.Write{On: on, Kind: "mention", Fields: map[string]string{"user": "nobody@example.com"},
		Note: &tracker.Note{Body: "x"}}); err == nil {
		t.Fatal("a mention of nobody")
	}
}

func still(name string) *tracker.Still {
	return &tracker.Still{Name: name, Data: []byte("\x89PNG\r\n\x1a\nfake")}
}

// Publishing comments in review with the C4 IDs and attaches the still as
// a new revision; again, it finds what it made. A crash between the
// comment and the upload is finished, not repeated.
func TestPublish(t *testing.T) {
	sh := newShow(t)
	a := sh.adapter(t, nil)
	ctx := context.Background()
	on := tracker.Ref{Type: "Task", ID: sh.task}
	out := tracker.Output{Name: "work/kitsu-1", Version: 1, View: "c4view1", JobURL: "https://hud/#/job/work/kitsu-1", Custody: "https://hud/p",
		Preview: still("sh040.1120.png")}
	ref, err := a.Publish(ctx, on, out, "pk")
	if err != nil || ref.Type != "Task" || ref.ID != sh.task || ref.URL == "" {
		t.Fatal(ref, err)
	}
	if _, err := a.Publish(ctx, on, out, "pk"); err != nil {
		t.Fatal(err)
	}
	cs, ps := sh.site.Comments(sh.task), sh.site.Previews(sh.task)
	if len(cs) != 1 || len(ps) != 1 || sh.site.StatusOf(sh.task) != "wfa" {
		t.Fatalf("publish is not once: %d comments, %d previews, %s", len(cs), len(ps), sh.site.StatusOf(sh.task))
	}
	if !strings.Contains(cs[0].Text, "Output view: c4view1") || !strings.Contains(cs[0].Text, "C4 output, version 1") ||
		ps[0].Revision != 1 || ps[0].Status != "ready" || ps[0].OriginalName != "sh040.1120" {
		t.Fatalf("comment %q, preview %+v", cs[0].Text, ps[0])
	}

	// v2: the answer to the upload is lost after Kitsu made the preview;
	// the next publish uploads to it, and makes no third revision.
	out2 := out
	out2.Version, out2.View, out2.Preview = 2, "c4view2", still("sh040.1121.png")
	sh.site.LoseAnswers(1, "/add-preview")
	if _, err := a.Publish(ctx, on, out2, "pk2"); err == nil {
		t.Fatal("the lost answer")
	}
	if ps := sh.site.Previews(sh.task); len(ps) != 2 || ps[1].Status != "processing" {
		t.Fatalf("after the lost answer: %+v", ps)
	}
	if _, err := a.Publish(ctx, on, out2, "pk2"); err != nil {
		t.Fatal(err)
	}
	ps = sh.site.Previews(sh.task)
	if len(ps) != 2 || ps[1].Revision != 2 || ps[1].Status != "ready" || len(sh.site.Comments(sh.task)) != 2 {
		t.Fatalf("finished: %+v", ps)
	}
}

// Decisions are people's comments that move the Task out of review, with
// the notes since; the bot's own comments are not decisions, and the
// flags give the default verdicts.
func TestDecisions(t *testing.T) {
	sh := newShow(t)
	sh.site.AddStatus(kitsutest.Status{Name: "Could Be Better", ShortName: "cbb", IsDone: true})
	ck := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	sh.site.SetClock(func() time.Time { return ck })
	a := sh.adapter(t, map[string]string{"review_status": "wfa"})
	ctx := context.Background()
	on := tracker.Ref{Type: "Task", ID: sh.task}
	if _, err := a.Publish(ctx, on, tracker.Output{Version: 1, View: "v1"}, "k"); err != nil {
		t.Fatal(err)
	}
	since := ck
	ck = ck.Add(time.Minute)
	sh.site.Comment(sh.dana, sh.task, "", "Fill light too hot, 1040 to 1060.")
	ck = ck.Add(time.Minute)
	c := sh.site.Comment(sh.dana, sh.task, "retake", "Keep the rim as is.")
	sh.site.ReplyTo(sh.dana, c, "And warmer.")
	ds, err := a.Decisions(ctx, on, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Native != "status:retake" || ds[0].By.ID != "dana@example.com" || ds[0].Source.ID != c ||
		ds[0].Body != "Fill light too hot, 1040 to 1060.\nKeep the rim as is.\n> And warmer." || len(ds[0].Evidence) == 0 {
		t.Fatalf("decisions: %+v", ds)
	}
	if a.Verdict("status:retake") != "rework" || a.Verdict("status:done") != "approve" || a.Verdict("status:cbb") != "approve" || a.Verdict("status:wip") != "" || a.Verdict("x") != "" {
		t.Fatal("verdicts by flag")
	}
	if ds, _ := a.Decisions(ctx, on, ck); len(ds) != 0 {
		t.Fatalf("since: %+v", ds)
	}
}

// A done Task is the supervisor's approval: no status write moves it.
func TestDoneIsNeverMoved(t *testing.T) {
	sh := newShow(t)
	a := sh.adapter(t, nil)
	sh.site.Comment(sh.dana, sh.task, "done", "Approved.")
	ref, err := a.Apply(context.Background(), tracker.Write{On: tracker.Ref{Type: "Task", ID: sh.task}, Kind: "status",
		Fields: map[string]string{"flag": "is_wip"}, Key: "k", Note: &tracker.Note{Body: "C4: running"}})
	if err != nil || ref.ID != "" || sh.site.StatusOf(sh.task) != "done" || len(sh.site.Comments(sh.task)) != 1 {
		t.Fatalf("moved a done task: %v %v %s", ref, err, sh.site.StatusOf(sh.task))
	}
}
