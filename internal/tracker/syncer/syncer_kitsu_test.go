package syncer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm/farmtest"
	_ "github.com/Avalanche-io/c4-operator/internal/tracker/kitsu"
	"github.com/Avalanche-io/c4-operator/internal/tracker/kitsu/kitsutest"
	"github.com/Avalanche-io/c4-operator/internal/tracker/syncer"
)

type kitsuEnv struct {
	t          *testing.T
	site       *kitsutest.Server
	farm       *farmtest.Farm
	k8s        client.Client
	sync       *syncer.Syncer
	clock      *clock
	bot, dana  *kitsutest.Person
	artist     *kitsutest.Person
	shot, task string
	due        time.Time
	firstJob   string
}

// setupKitsu is a Kitsu production with shot sh040 in sq010 and its
// lighting task, ready, due four days out and bid two days, and a Tracker
// that turns ready lighting tasks into render jobs due at dailies: 14:00
// Los Angeles time on the date. The Tracker names no statuses and no
// verdicts: Kitsu's flags supply both.
func setupKitsu(t *testing.T, opts ...func(*fake.ClientBuilder)) *kitsuEnv {
	t.Helper()
	ck := &clock{t: time.Now().UTC().Truncate(time.Second)}
	site := kitsutest.New()
	t.Cleanup(site.Close)
	site.SetClock(ck.now)
	bot := site.AddPerson("C4", "Bot", "c4-bot@example.com", "manager", true)
	dana := site.AddPerson("Dana", "Reyes", "dana@example.com", "supervisor", false)
	artist := site.AddPerson("Mo", "Okafor", "mo@example.com", "user", false)
	seq := site.AddEntity("Sequence", "sq010", "")
	shot := site.AddEntity("Shot", "sh040", seq)
	due := ck.now().AddDate(0, 0, 4)
	task := site.AddTask(shot, "Lighting", "ready", due.Format("2006-01-02"), 960, artist.ID)

	sch := runtime.NewScheme()
	if err := c4v1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	tr := &c4v1.Tracker{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kitsu", Generation: 1},
		Spec: c4v1.TrackerSpec{
			Kind: "kitsu", Endpoint: site.URL,
			Scope:      map[string]string{"project": "Saltmarsh", "eta_field": "c4_eta", "link_field": "c4_link"},
			Credential: c4v1.TrackerCredential{Secret: c4v1.TrackerSecretKey{Name: "kitsu-bot", Key: "token"}},
			Events:     c4v1.TrackerEvents{Mode: "eventlog"},
			Rate:       c4v1.TrackerRate{PerSecond: "1000", Burst: 1000},
			Intake: []c4v1.TrackerIntake{{
				Match:    c4v1.TrackerMatch{Type: "Task", Fields: map[string]string{"task_type": "Lighting", "task_status": "ready"}},
				As:       "job",
				Job:      &c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1001-1240"}},
				Deadline: c4v1.TrackerDeadline{DateOnlyAt: "14:00", TimeZone: "America/Los_Angeles"},
				Review:   &c4v1.TrackerReview{Wait: "dailies"},
				Rework:   "nextVersion",
			}},
			People: map[string]string{"dana@example.com": "dana"},
		},
	}
	b := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&c4v1.Tracker{}, &c4v1.TrackerLink{}).
		WithObjects(tr, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kitsu-bot"}, Data: map[string][]byte{"token": []byte(bot.Token)}})
	for _, o := range opts {
		o(b)
	}
	k8s := b.Build()
	fm := farmtest.New()
	fm.Now = ck.now
	s := &syncer.Syncer{Client: k8s, Farm: fm, Now: ck.now}
	return &kitsuEnv{t: t, site: site, farm: fm, k8s: k8s, sync: s, clock: ck, bot: bot, dana: dana, artist: artist,
		shot: shot, task: task, due: due, firstJob: syncer.JobName("kitsu", tracker.Item{Ref: kref(task), Name: "sh040 Lighting"})}
}

func (e *kitsuEnv) tracker() *c4v1.Tracker {
	e.t.Helper()
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "kitsu"}, &tr); err != nil {
		e.t.Fatal(err)
	}
	return &tr
}

func (e *kitsuEnv) update(fn func(*c4v1.Tracker)) {
	e.t.Helper()
	tr := e.tracker()
	fn(tr)
	if err := e.k8s.Update(context.Background(), tr); err != nil {
		e.t.Fatal(err)
	}
}

func (e *kitsuEnv) pass() {
	e.t.Helper()
	if err := e.sync.Sync(context.Background(), e.tracker()); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

func (e *kitsuEnv) link() *c4v1.TrackerLink {
	e.t.Helper()
	var l c4v1.TrackerLink
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: syncer.LinkName("kitsu", kref(e.task))}, &l); err != nil {
		e.t.Fatal(err)
	}
	return &l
}

func (e *kitsuEnv) job(name string, fn func(*api.Job)) { e.farm.Update(ns, name, fn) }

// botComments are the Task's comments by c4-sync's bot, oldest first.
func (e *kitsuEnv) botComments() []kitsutest.Comment {
	var out []kitsutest.Comment
	for _, c := range e.site.Comments(e.task) {
		if c.PersonID == e.bot.ID {
			out = append(out, c)
		}
	}
	return out
}

// botStatuses are the statuses the bot's comments moved the Task to.
func (e *kitsuEnv) botStatuses() []string {
	var out []string
	prev := ""
	for _, c := range e.site.Comments(e.task) {
		st := e.site.ShortOf(c.StatusID)
		if c.PersonID == e.bot.ID && st != prev {
			out = append(out, st)
		}
		prev = st
	}
	return out
}

func kref(id string) tracker.Ref { return tracker.Ref{Type: "Task", ID: id} }

// The dailies loop against a fake Kitsu, end to end: a Task's due date
// becomes the render's deadline; work states go to the Task as comments
// carrying statuses Kitsu's flags choose; the render publishes a comment
// in review with its C4 ID and a preview revision; the supervisor's
// retake in Kitsu comes back as a rework that makes v002, with her notes
// as its input; her done serves the wait. c4-sync never sets done.
func TestKitsuDailiesLoop(t *testing.T) {
	e := setupKitsu(t)
	first := e.firstJob

	// 1. The ready lighting task becomes a render due 14:00 Los Angeles
	// time on its date.
	e.pass()
	if len(e.farm.Submitted) != 1 {
		t.Fatalf("submitted %d jobs", len(e.farm.Submitted))
	}
	sub := e.farm.Submitted[0]
	loc, _ := time.LoadLocation("America/Los_Angeles")
	y, m, d := e.due.Date()
	want := time.Date(y, m, d, 14, 0, 0, 0, loc).UTC()
	if sub.Name != first || sub.Spec.DeadlineMode != "Tracking" || !sub.Spec.Deadline.Time.Equal(want) ||
		sub.TrackerRef != "kitsu/Task/"+e.task {
		t.Fatalf("submitted: %+v (want deadline %s)", sub, want)
	}

	// 2. It runs: one comment moves the Task to work in progress (the
	// is_wip status), and the ETA and the job link go to its data. An
	// idle pass writes nothing.
	eta := e.clock.now().Add(5 * time.Hour)
	e.job(first, func(j *api.Job) { j.Phase = "Running"; j.Estimate.EtaP80 = &eta })
	e.pass()
	tk := e.site.Task(e.task)
	if e.site.StatusOf(e.task) != "wip" || tk.Data["c4_eta"] != eta.UTC().Format(time.RFC3339) ||
		!strings.Contains(tk.Data["c4_link"].(string), "#/job/work/"+first) {
		t.Fatalf("task while running: %s %v", e.site.StatusOf(e.task), tk.Data)
	}
	if cs := e.botComments(); len(cs) != 1 || !strings.Contains(cs[0].Text, "#/job/work/"+first) {
		t.Fatalf("running: %+v", cs)
	}
	w := len(e.site.Writes())
	e.clock.add(time.Minute)
	e.pass()
	if got := len(e.site.Writes()); got != w {
		t.Fatalf("an idle pass wrote %d times", got-w)
	}

	// 3. Frames fail and a person is needed: one comment with the cause,
	// edited in place when it changes. No status: no flag means it.
	e.job(first, func(j *api.Job) {
		j.NeedsPerson = &api.NeedsPerson{Reason: "failed", Message: "12 frames failed on node-3: GPU driver"}
	})
	e.pass()
	e.job(first, func(j *api.Job) {
		j.NeedsPerson = &api.NeedsPerson{Reason: "failed", Message: "14 frames failed on node-3: GPU driver"}
	})
	e.pass()
	var causes []kitsutest.Comment
	for _, c := range e.botComments() {
		if strings.Contains(c.Text, "frames failed on node-3") {
			causes = append(causes, c)
		}
	}
	if len(causes) != 1 || !strings.Contains(causes[0].Text, "14 frames failed") || e.site.StatusOf(e.task) != "wip" {
		t.Fatalf("cause comments: %+v, status %s", causes, e.site.StatusOf(e.task))
	}
	e.job(first, func(j *api.Job) { j.NeedsPerson = nil })

	// 4. The render is done and its dailies wait opens: the output is
	// published in review with its C4 ID and a preview, revision 1. The
	// in-review status is the publish's: no second comment says it.
	e.farm.Previews = map[string][]byte{ns + "/" + first: []byte("\x89PNG\r\n\x1a\nv1")}
	e.job(first, func(j *api.Job) { j.Output = &api.Output{View: "c4OutputV001"} })
	e.farm.OpenWait(ns, first, "dailies", "person:dana", "approve", "rework", "accept")
	before := len(e.botComments())
	e.pass()
	pub := e.botComments()
	if len(pub) != before+1 || !strings.Contains(pub[len(pub)-1].Text, "Output view: c4OutputV001") || e.site.StatusOf(e.task) != "wfa" {
		t.Fatalf("publish: %d comments (was %d), status %s", len(pub), before, e.site.StatusOf(e.task))
	}
	ps := e.site.Previews(e.task)
	if len(ps) != 1 || ps[0].Revision != 1 || ps[0].Status != "ready" || ps[0].OriginalName != first {
		t.Fatalf("preview: %+v", ps)
	}
	if r := e.link().Status.Review; r == nil || r.Type != "Task" || r.ID != e.task {
		t.Fatalf("review: %+v", r)
	}
	e.pass()
	if len(e.botComments()) != before+1 || len(e.site.Previews(e.task)) != 1 {
		t.Fatal("published twice")
	}

	// 5. Dana, in Kitsu, writes a note, then retakes with another, and a
	// reply. The event log says where to look.
	e.clock.add(time.Minute)
	e.site.Comment(e.dana, e.task, "", "Fill light too hot, 1040 to 1060.")
	e.clock.add(time.Minute)
	rc := e.site.Comment(e.dana, e.task, "retake", "Keep the rim as is.")
	e.site.ReplyTo(e.dana, rc, "And a touch warmer.")
	e.pass()
	if len(e.farm.Relays) != 1 {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
	r := e.farm.Relays[0]
	if r.Do != "rework" || r.Unit != "dailies" || r.Person != "dana" || r.By != "@dana@example.com" || r.Tracker != "kitsu" ||
		!strings.Contains(r.Note, "Fill light too hot, 1040 to 1060.") || !strings.Contains(r.Note, "Keep the rim as is.") ||
		!strings.Contains(r.Note, "And a touch warmer.") || !strings.Contains(r.Source, "Comment/"+rc) || len(r.Evidence) == 0 {
		t.Fatalf("relay: %+v", r)
	}

	// 6. The rework makes v002, which follows v001, reads the notes, and
	// keeps the tracker's deadline. The link moves to it.
	if len(e.farm.Submitted) != 2 {
		t.Fatalf("submitted: %d", len(e.farm.Submitted))
	}
	second := e.farm.Submitted[1]
	if second.Name != first+"-v002" || second.Follows != first || len(second.Inputs) != 1 ||
		!second.Spec.Deadline.Time.Equal(want) || second.Spec.DeadlineMode != "Tracking" {
		t.Fatalf("v002 job: %+v", second)
	}
	if note := string(e.farm.Views[second.Inputs[0].View]["note.md"]); !strings.Contains(note, "Fill light too hot") {
		t.Fatalf("note view: %q", note)
	}
	if l := e.link(); l.Spec.Target.WorkJob != second.Name || len(l.Status.Versions) != 1 || l.Status.Review != nil {
		t.Fatalf("link after rework: %+v %+v", l.Spec.Target, l.Status)
	}

	// 7. v002 runs (back to wip from the retake) and is published as
	// revision 2.
	e.job(second.Name, func(j *api.Job) { j.Phase = "Running" })
	e.pass()
	if e.site.StatusOf(e.task) != "wip" {
		t.Fatalf("v002 running: %s", e.site.StatusOf(e.task))
	}
	e.farm.Previews[ns+"/"+second.Name] = []byte("\x89PNG\r\n\x1a\nv2")
	e.job(second.Name, func(j *api.Job) { j.Output = &api.Output{View: "c4OutputV002"} })
	e.farm.OpenWait(ns, second.Name, "dailies", "person:dana", "approve", "rework", "accept")
	e.pass()
	ps = e.site.Previews(e.task)
	if len(ps) != 2 || ps[1].Revision != 2 || e.site.StatusOf(e.task) != "wfa" {
		t.Fatalf("v002 preview: %+v, status %s", ps, e.site.StatusOf(e.task))
	}
	if last := e.botComments(); !strings.Contains(last[len(last)-1].Text, "C4 output, version 2, follows "+ns+"/"+first+".") {
		t.Fatalf("v002 says what it follows: %q", last[len(last)-1].Text)
	}

	// 8. Dana sets done: the flag approves, and the wait passes. Done
	// writes nothing more; c4-sync never moved the Task to done.
	e.clock.add(time.Minute)
	e.site.Comment(e.dana, e.task, "done", "Approved.")
	e.pass()
	if len(e.farm.Relays) != 2 || e.farm.Relays[1].Do != "approve" {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
	e.job(second.Name, func(j *api.Job) { j.Phase = "Succeeded" })
	w = len(e.site.Writes())
	e.pass()
	e.pass()
	if got := len(e.site.Writes()); got != w {
		t.Fatalf("done wrote %d times: done is not C4's to write", got-w)
	}
	for _, st := range e.botStatuses() {
		if st == "done" || st == "retake" {
			t.Fatalf("c4-sync set %s: %v", st, e.botStatuses())
		}
	}
	if e.site.StatusOf(e.task) != "done" {
		t.Fatal("the supervisor's status must stand")
	}
}

// A studio's own status wins over its flags when the Tracker names it: a
// "could be better" that counts as done is an accept with the note.
func TestKitsuVerdictsInNamesWin(t *testing.T) {
	e := setupKitsu(t)
	e.site.AddStatus(kitsutest.Status{Name: "Could Be Better", ShortName: "cbb", IsDone: true})
	e.update(func(tr *c4v1.Tracker) { tr.Spec.VerdictsIn = map[string]string{"status:cbb": "accept"} })
	e.pass()
	e.job(e.firstJob, func(j *api.Job) { j.Phase = "Running"; j.Output = &api.Output{View: "c4v"} })
	e.farm.OpenWait(ns, e.firstJob, "dailies", "person:dana", "approve", "rework", "accept")
	e.pass()
	e.clock.add(time.Minute)
	e.site.Comment(e.dana, e.task, "cbb", "Fine for now; the rim could be softer.")
	e.pass()
	if len(e.farm.Relays) != 1 || e.farm.Relays[0].Do != "accept" || !strings.Contains(e.farm.Relays[0].Note, "rim could be softer") {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
}

// Someone other than the wait's person, here the assigned artist moving
// the Task to done, cannot serve it; the refusal is on the link.
func TestKitsuOnlyTheReviewerDecides(t *testing.T) {
	e := setupKitsu(t)
	e.pass()
	e.job(e.firstJob, func(j *api.Job) { j.Phase = "Running"; j.Output = &api.Output{View: "c4v"} })
	e.farm.OpenWait(ns, e.firstJob, "dailies", "person:dana", "approve", "rework")
	e.pass()
	e.clock.add(time.Minute)
	e.site.Comment(e.artist, e.task, "done", "looks fine to me")
	e.pass()
	if len(e.farm.Relays) != 0 {
		t.Fatal("an unmapped user's done was relayed")
	}
	if ds := e.link().Status.Decisions; len(ds) != 1 || !strings.Contains(ds[0].Error, "not mapped") {
		t.Fatalf("decisions: %+v", ds)
	}
}

// A status comment whose answer is lost (Kitsu made it; c4-sync never
// heard) is found by its key on the next pass, not posted again.
func TestKitsuLostAnswerIsProbedNotRepeated(t *testing.T) {
	e := setupKitsu(t)
	e.pass()
	e.job(e.firstJob, func(j *api.Job) { j.Phase = "Running" })
	e.site.LoseAnswers(1, "/comment")
	if err := e.sync.Sync(context.Background(), e.tracker()); err == nil {
		t.Fatal("the lost answer should fail the pass")
	}
	if w := e.link().Status.Written["status"]; w.State != "sending" {
		t.Fatalf("journal: %+v", w)
	}
	e.clock.add(time.Minute)
	e.pass()
	if cs := e.botComments(); len(cs) != 1 || e.site.StatusOf(e.task) != "wip" {
		t.Fatalf("comments after the probe: %d", len(cs))
	}
	if w := e.link().Status.Written["status"]; w.State != "" || w.Ref == "" {
		t.Fatalf("journal after: %+v", w)
	}
}

// A due date moved in Kitsu moves the job's tracking deadline, after the
// Task left the status intake matches.
func TestKitsuDueDateMoves(t *testing.T) {
	e := setupKitsu(t)
	e.pass()
	e.job(e.firstJob, func(j *api.Job) { j.Phase = "Running" })
	e.pass() // the Task is wip now: intake no longer lists it
	e.clock.add(time.Minute)
	newDue := e.due.AddDate(0, 0, 2)
	e.site.SetTask(e.dana, e.task, map[string]any{"due": newDue.Format("2006-01-02")})
	e.pass()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	y, m, d := newDue.Date()
	if want := time.Date(y, m, d, 14, 0, 0, 0, loc).UTC(); len(e.farm.Deadlines) != 1 || !e.farm.Deadlines[0].Equal(want) {
		t.Fatalf("deadlines %v, want %s", e.farm.Deadlines, want)
	}
}

// A Task taken as a milestone covers every job of its shot.
func TestKitsuMilestoneCoversTheShot(t *testing.T) {
	proj := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show"}, Spec: c4v1.ProjectSpec{Namespaces: []string{ns},
		Work: &c4v1.ProjectWork{DueTime: &c4v1.DueTime{At: "18:00", TimeZone: "America/Los_Angeles"}}}}
	e := setupKitsu(t, func(b *fake.ClientBuilder) { b.WithObjects(proj) })
	e.update(func(tr *c4v1.Tracker) {
		in := &tr.Spec.Intake[0]
		in.As, in.Job, in.Deadline = "milestone", nil, c4v1.TrackerDeadline{}
		in.Covers = map[string]string{c4v1.LabelShot: "entity.name", "c4.avalanche.io/step": "task_type"}
	})
	e.pass()
	var ms c4v1.Milestone
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: syncer.LinkName("kitsu", kref(e.task))}, &ms); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/Los_Angeles")
	y, m, d := e.due.Date()
	if !ms.Spec.Due.Time.Equal(time.Date(y, m, d, 18, 0, 0, 0, loc)) || ms.Spec.Selector.MatchLabels[c4v1.LabelShot] != "sh040" ||
		ms.Spec.Selector.MatchLabels["c4.avalanche.io/step"] != "Lighting" || len(e.farm.Submitted) != 0 {
		t.Fatalf("milestone: %+v", ms.Spec)
	}
}

// Units assigned to a person in C4 become a comment that mentions them in
// Kitsu, so Kitsu notifies them; once.
func TestKitsuAssignmentMentions(t *testing.T) {
	e := setupKitsu(t)
	e.pass()
	at := e.clock.now()
	e.job(e.firstJob, func(j *api.Job) {
		j.Phase = "Running"
		j.Acks = []api.Ack{{Kind: "assign", Units: "1001-1004", Actor: "pat", Assignee: "dana", Note: "relight the key", At: at}}
	})
	e.pass()
	e.clock.add(time.Minute)
	e.pass()
	var found []kitsutest.Comment
	for _, c := range e.botComments() {
		if strings.Contains(c.Text, "units 1001-1004") {
			found = append(found, c)
		}
	}
	if len(found) != 1 || len(found[0].Mentions) != 1 || found[0].Mentions[0] != e.dana.ID || !strings.HasPrefix(found[0].Text, "@Dana Reyes C4:") {
		t.Fatalf("mentions: %+v", found)
	}
}

// A cause is one comment on the Task, edited in place as it grows.
func TestKitsuCauseComment(t *testing.T) {
	e := setupKitsu(t)
	e.pass()
	cause := func(n int) {
		e.job(e.firstJob, func(j *api.Job) {
			j.Phase = "Running"
			j.Causes = []api.Cause{{Kind: "exit", Key: "exit:137", Title: "exit 137 OOMKilled", Units: "1001-1003", Count: n,
				Nodes: []string{"node-7"}, Did: []string{"retried", "failed"}}}
		})
	}
	cause(2)
	e.pass()
	cause(5)
	e.pass()
	var found []kitsutest.Comment
	for _, c := range e.botComments() {
		if strings.Contains(c.Text, "exit 137 OOMKilled") {
			found = append(found, c)
		}
	}
	if len(found) != 1 || !strings.Contains(found[0].Text, "5 attempts") {
		t.Fatalf("cause comments: %+v", found)
	}
}

// A Task not yet ready is a forecast sized by its estimation (minutes);
// nothing is written to Kitsu for it. The bid moves the forecast; ready,
// the real job replaces it.
func TestKitsuForecastFollowsTheEstimation(t *testing.T) {
	e := setupKitsu(t)
	e.site.Comment(e.dana, e.task, "todo", "")
	e.update(func(tr *c4v1.Tracker) {
		real := tr.Spec.Intake[0]
		fc := *real.DeepCopy()
		fc.As, fc.Review, fc.Rework = "forecast", nil, ""
		fc.Match.Fields = map[string]string{"task_type": "Lighting", "task_status": "todo"}
		fc.Forecast = &c4v1.TrackerForecast{Field: "estimation", PerUnit: "0.05", UnitHours: "1"}
		tr.Spec.Intake = []c4v1.TrackerIntake{fc, real}
	})
	e.pass()
	if len(e.farm.Submitted) != 1 {
		t.Fatalf("submitted %d", len(e.farm.Submitted))
	}
	f := e.farm.Submitted[0]
	if !strings.HasPrefix(f.Name, "kitsu-") || f.Name == e.firstJob || f.Spec.Forecast == nil || f.Spec.Forecast.SlotHours != "48" {
		t.Fatalf("the forecast: %+v", f)
	}
	if len(e.botComments()) != 0 || len(e.site.Task(e.task).Data) != 0 {
		t.Fatal("Kitsu was written for a forecast")
	}
	e.clock.add(time.Minute)
	e.site.SetTask(e.dana, e.task, map[string]any{"estimation": 1440.0})
	e.pass()
	e.pass()
	if len(e.farm.Resized) != 1 || *e.farm.Resized[0].SlotHours != "72" {
		t.Fatalf("resized: %+v", e.farm.Resized)
	}
	e.clock.add(time.Minute)
	e.site.Comment(e.dana, e.task, "ready", "turned over")
	e.pass()
	if len(e.farm.Submitted) != 2 || e.farm.Submitted[1].Replaces != f.Name || e.farm.Submitted[1].Name != e.firstJob {
		t.Fatalf("the real job: %+v", e.farm.Submitted)
	}
	if _, err := e.farm.Job(context.Background(), farm.JobRef{Namespace: ns, Name: e.firstJob}); err != nil && !errors.Is(err, farm.ErrNotFound) {
		t.Fatal(err)
	}
}

// inReview takes the first job to its review with an output and a still.
// The Task's data carries what the job has spent, as one number on the
// fields' throttle, and a finished job leaves no stale ETA.
func TestKitsuSpendAndNoStaleETA(t *testing.T) {
	e := setupKitsu(t)
	e.update(func(tr *c4v1.Tracker) { tr.Spec.Scope["spend_field"] = "c4_spend" })
	first := e.firstJob
	e.pass()
	eta := e.clock.now().Add(5 * time.Hour)
	e.job(first, func(j *api.Job) {
		j.Phase, j.Estimate.EtaP80 = "Running", &eta
		j.Spend = &api.Spend{ModelSlotCost: 1.25, ProceduralSlotCost: 10, TokenCost: 1.5}
	})
	e.pass()
	if d := e.site.Task(e.task).Data; d["c4_spend"] != 12.75 || d["c4_eta"] != eta.UTC().Format(time.RFC3339) {
		t.Fatalf("while running: %#v", d)
	}
	e.job(first, func(j *api.Job) { j.Phase, j.Estimate.EtaP80 = "Succeeded", nil })
	e.pass()
	e.clock.add(time.Hour)
	e.pass()
	if v, ok := e.site.Task(e.task).Data["c4_eta"]; !ok || v != nil {
		t.Fatalf("the finished Task's ETA: %#v", e.site.Task(e.task).Data)
	}
}

func (e *kitsuEnv) inReview() {
	e.t.Helper()
	e.pass()
	e.farm.Previews = map[string][]byte{ns + "/" + e.firstJob: []byte("\x89PNG\r\n\x1a\nv1")}
	e.job(e.firstJob, func(j *api.Job) { j.Phase = "Running"; j.Output = &api.Output{View: "c4OutputV001"} })
	e.farm.OpenWait(ns, e.firstJob, "dailies", "person:dana", "approve", "rework", "accept")
}

func (e *kitsuEnv) published() int {
	n := 0
	for _, c := range e.botComments() {
		if strings.Contains(c.Text, "Output view: c4OutputV001") {
			n++
		}
	}
	return n
}

// A still the farm fails to give (a replica that is not the leader) puts
// the publish off to the next pass, not off for good; past the patience,
// the output is published without it.
func TestKitsuPublishWaitsForTheStill(t *testing.T) {
	e := setupKitsu(t)
	e.inReview()
	e.farm.PreviewFails = 1
	before := len(e.botComments())
	e.pass() // waiting is no failure of the pass
	if e.published() != 0 || len(e.botComments()) != before {
		t.Fatalf("wrote while waiting for the still: %d published, %d comments (was %d)", e.published(), len(e.botComments()), before)
	}
	if c := meta.FindStatusCondition(e.tracker().Status.Conditions, syncer.CondReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("the Tracker while it waits: %+v", c)
	}
	e.clock.add(time.Minute)
	e.pass()
	if ps := e.site.Previews(e.task); e.published() != 1 || len(ps) != 1 || ps[0].Status != "ready" {
		t.Fatalf("published %d, previews %+v", e.published(), ps)
	}

	f := setupKitsu(t)
	f.inReview()
	f.farm.PreviewFails = 1000
	for range 3 {
		_ = f.sync.Sync(context.Background(), f.tracker())
		f.clock.add(2 * time.Minute)
	}
	f.pass()
	if f.published() != 1 || len(f.site.Previews(f.task)) != 0 {
		t.Fatalf("past the patience: published %d, previews %d", f.published(), len(f.site.Previews(f.task)))
	}
}

// The upload made but its answer lost: the next pass finds the comment and
// its uploaded preview, and adds no revision.
func TestKitsuLostUploadAnswerMakesOneRevision(t *testing.T) {
	e := setupKitsu(t)
	e.inReview()
	e.site.LoseAnswers(1, "/pictures/preview-files/")
	if err := e.sync.Sync(context.Background(), e.tracker()); err == nil {
		t.Fatal("the lost answer should fail the pass")
	}
	e.clock.add(time.Minute)
	e.pass()
	e.pass()
	ps := e.site.Previews(e.task)
	if e.published() != 1 || len(ps) != 1 || ps[0].Revision != 1 || ps[0].Status != "ready" {
		t.Fatalf("published %d, previews %+v", e.published(), ps)
	}
	if r := e.link().Status.Review; r == nil || r.ID != e.task {
		t.Fatalf("review: %+v", r)
	}
}

// Two lighting tasks on one shot share a handle; their jobs are told apart
// by the tasks' refs.
func TestKitsuSameHandleTwoJobs(t *testing.T) {
	e := setupKitsu(t)
	second := e.site.AddTask(e.shot, "Lighting", "ready", e.due.Format("2006-01-02"), 480)
	e.pass()
	if len(e.farm.Submitted) != 2 {
		t.Fatalf("submitted %d", len(e.farm.Submitted))
	}
	a, b := e.farm.Submitted[0].Name, e.farm.Submitted[1].Name
	for _, n := range []string{a, b} {
		if !strings.HasPrefix(n, "kitsu-sh040-lighting-") || len(n) != len("kitsu-sh040-lighting-")+6 {
			t.Fatalf("job name %q", n)
		}
	}
	if a == b {
		t.Fatalf("two tasks, one job name %q", a)
	}
	if want := syncer.JobName("kitsu", tracker.Item{Ref: kref(second), Name: "sh040 Lighting"}); a != want && b != want {
		t.Fatalf("the second task's job is neither %s nor %s, want %s", a, b, want)
	}
}
