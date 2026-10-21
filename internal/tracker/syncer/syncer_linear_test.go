package syncer_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm/farmtest"
	_ "github.com/Avalanche-io/c4-operator/internal/tracker/linear"
	"github.com/Avalanche-io/c4-operator/internal/tracker/linear/lineartest"
	"github.com/Avalanche-io/c4-operator/internal/tracker/syncer"
)

var linearHook = []byte("linear-hook-secret")

type linearEnv struct {
	t     *testing.T
	ws    *lineartest.Server
	farm  *farmtest.Farm
	k8s   client.Client
	sync  *syncer.Syncer
	clock *clock
	hooks *httptest.Server
	dana  *lineartest.User
	issue *lineartest.Issue
}

// setupLinear is a Linear team (ENG) with one issue labeled c4 and
// render, due in four days, assigned to dana, and a Tracker that turns
// such issues into render jobs with a review dana serves in Linear.
func setupLinear(t *testing.T) *linearEnv {
	t.Helper()
	ck := &clock{t: time.Now().UTC().Truncate(time.Second)}
	ws := lineartest.New("ENG")
	t.Cleanup(ws.Close)
	ws.SetClock(ck.now)
	ws.AddState("Changes Requested", "started", 3.5)
	dana := ws.AddUser("Dana Reyes", "dana@example.com")
	issue := ws.AddIssue("Render the trailer", ck.now().AddDate(0, 0, 4).Format("2006-01-02"), dana, "c4", "render")

	sch := runtime.NewScheme()
	if err := c4v1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	tr := &c4v1.Tracker{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "linear", Generation: 1},
		Spec: c4v1.TrackerSpec{
			Kind: "linear", Endpoint: ws.URL + "/graphql", Scope: map[string]string{"team": "ENG"},
			Credential:    c4v1.TrackerCredential{Secret: c4v1.TrackerSecretKey{Name: "linear-key", Key: "key"}},
			WebhookSecret: &c4v1.TrackerSecretKey{Name: "linear-hook", Key: "secret"},
			Events:        c4v1.TrackerEvents{Mode: "webhook"},
			Rate:          c4v1.TrackerRate{PerSecond: "1000", Burst: 1000},
			Intake: []c4v1.TrackerIntake{{
				Match:    c4v1.TrackerMatch{Type: "Issue", Labels: []string{"c4", "render"}},
				As:       "job",
				Job:      &c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-240"}},
				Deadline: c4v1.TrackerDeadline{DateOnlyAt: "18:00", TimeZone: "America/Los_Angeles"},
				Review:   &c4v1.TrackerReview{Wait: "review"},
				Rework:   "nextVersion",
			}},
			StatusOut:  c4v1.TrackerStatusOut{States: map[string]string{"in-review": "In Review"}},
			VerdictsIn: map[string]string{"state:Changes Requested": "rework", "state:Done": "approve"},
			People:     map[string]string{"dana@example.com": "dana"},
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&c4v1.Tracker{}, &c4v1.TrackerLink{}).
		WithObjects(tr,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "linear-key"}, Data: map[string][]byte{"key": []byte(ws.Key)}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "linear-hook"}, Data: map[string][]byte{"secret": linearHook}},
		).Build()
	fm := farmtest.New()
	fm.Now = ck.now
	s := &syncer.Syncer{Client: k8s, Farm: fm, Now: ck.now}
	hooks := httptest.NewServer(s.Webhooks())
	t.Cleanup(hooks.Close)
	return &linearEnv{t: t, ws: ws, farm: fm, k8s: k8s, sync: s, clock: ck, hooks: hooks, dana: dana, issue: issue}
}

func (e *linearEnv) pass() {
	e.t.Helper()
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "linear"}, &tr); err != nil {
		e.t.Fatal(err)
	}
	if err := e.sync.Sync(context.Background(), &tr); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

func (e *linearEnv) link() *c4v1.TrackerLink {
	var ls c4v1.TrackerLinkList
	if err := e.k8s.List(context.Background(), &ls, client.InNamespace(ns)); err != nil || len(ls.Items) != 1 {
		e.t.Fatalf("links: %d %v", len(ls.Items), err)
	}
	return &ls.Items[0]
}

func (e *linearEnv) state() string { _, s := e.ws.Issue(e.issue.ID); return s }

// The review loop against the fake workspace: an issue labeled for the
// farm becomes a render job, its due date the deadline; work states move
// the issue and update one status card; a person needed is one comment,
// edited; the output is an attachment carrying its C4 ID; dana's move to
// Changes Requested, with her comment, comes back as a rework that makes
// v002; her move to Done approves it. c4-sync never moves the issue to a
// completed state, and an idle pass writes nothing.
func TestLinearReviewLoop(t *testing.T) {
	e := setupLinear(t)
	first := "linear-" + strings.ToLower(e.issue.ID)

	// 1. The issue becomes a render job: 18:00 Los Angeles on its date.
	e.pass()
	if len(e.farm.Submitted) != 1 {
		t.Fatalf("submitted %d", len(e.farm.Submitted))
	}
	sub := e.farm.Submitted[0]
	loc, _ := time.LoadLocation("America/Los_Angeles")
	d, _ := time.Parse("2006-01-02", e.issue.DueDate)
	want := time.Date(d.Year(), d.Month(), d.Day(), 18, 0, 0, 0, loc).UTC()
	if sub.Spec.DeadlineMode != "Tracking" || !sub.Spec.Deadline.Time.Equal(want) || sub.TrackerRef != "linear/Issue/"+e.issue.ID {
		t.Fatalf("submitted: %+v (want %s)", sub, want)
	}
	first = sub.Name

	// 2. It runs: the issue moves to the team's first started state, and
	// one card shows the job. An idle pass writes nothing.
	eta := e.clock.now().Add(5 * time.Hour)
	e.farm.Update(ns, first, func(j *api.Job) { j.Phase = "Running"; j.Estimate.EtaP80 = &eta })
	e.pass()
	if e.state() != "In Progress" {
		t.Fatalf("running: %s", e.state())
	}
	as := e.ws.Attachments(e.issue.ID)
	if len(as) != 1 || !strings.Contains(as[0].URL, "#/job/work/"+first) || !strings.HasPrefix(as[0].Title, "C4: ") {
		t.Fatalf("the card: %+v", as)
	}
	m := e.ws.Mutations()
	e.clock.add(time.Minute)
	e.pass()
	if got := e.ws.Mutations(); got != m {
		t.Fatalf("an idle pass wrote %d times", got-m)
	}

	// 3. A person is needed: one comment with the cause, edited in place.
	e.farm.Update(ns, first, func(j *api.Job) {
		j.NeedsPerson = &api.NeedsPerson{Reason: "failed", Message: "12 frames failed on node-3: GPU driver"}
	})
	e.pass()
	e.farm.Update(ns, first, func(j *api.Job) {
		j.NeedsPerson = &api.NeedsPerson{Reason: "failed", Message: "14 frames failed on node-3: GPU driver"}
	})
	e.pass()
	cs := e.ws.Comments(e.issue.ID)
	if len(cs) != 1 || !strings.Contains(cs[0].Body, "14 frames failed on node-3") {
		t.Fatalf("cause comments: %+v", cs)
	}
	e.farm.Update(ns, first, func(j *api.Job) { j.NeedsPerson = nil })

	// 4. The output, and the review wait: an attachment with the C4 ID,
	// the issue In Review.
	e.farm.Update(ns, first, func(j *api.Job) { j.Output = &api.Output{View: "c4OutputV001"} })
	e.farm.OpenWait(ns, first, "review", "person:dana", "approve", "rework")
	e.pass()
	e.pass()
	var outs []lineartest.Attachment
	for _, a := range e.ws.Attachments(e.issue.ID) {
		if a.Metadata["c4Id"] != nil {
			outs = append(outs, a)
		}
	}
	if len(outs) != 1 || outs[0].Metadata["c4Id"] != "c4OutputV001" || e.state() != "In Review" {
		t.Fatalf("published: %+v, state %s", outs, e.state())
	}

	// 5. Dana asks for changes, with a comment; the webhook is a hint.
	e.clock.add(time.Minute)
	e.ws.AddComment(e.issue.ID, "Rim light too hot on 40 to 60.", e.dana)
	e.ws.SetState(e.issue.ID, "Changes Requested", e.dana)
	body := e.ws.Payload("Issue", "update", map[string]any{"id": e.issue.ID}, e.dana)
	if c, err := lineartest.Deliver(e.hooks.URL+"/webhooks/"+ns+"/linear", body, []byte("wrong"), "d1"); err != nil || c != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d %v", c, err)
	}
	if c, err := lineartest.Deliver(e.hooks.URL+"/webhooks/"+ns+"/linear", body, linearHook, "d1"); err != nil || c != http.StatusAccepted {
		t.Fatalf("delivery: %d %v", c, err)
	}
	e.pass()
	if len(e.farm.Relays) != 1 {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
	r := e.farm.Relays[0]
	if r.Do != "rework" || r.Unit != "review" || r.Person != "dana" || !strings.Contains(r.Note, "Rim light too hot") || len(r.Evidence) == 0 {
		t.Fatalf("relay: %+v", r)
	}

	// 6. v002 follows, with the note; it runs and is up for review.
	if len(e.farm.Submitted) != 2 || e.farm.Submitted[1].Follows != first {
		t.Fatalf("v002: %+v", e.farm.Submitted)
	}
	second := e.farm.Submitted[1].Name
	e.farm.Update(ns, second, func(j *api.Job) { j.Phase = "Running" })
	e.pass()
	if e.state() != "In Progress" {
		t.Fatalf("v002 running: %s", e.state())
	}
	e.farm.Update(ns, second, func(j *api.Job) { j.Output = &api.Output{View: "c4OutputV002"} })
	e.farm.OpenWait(ns, second, "review", "person:dana", "approve", "rework")
	e.pass()
	e.pass()

	// 7. Dana moves the issue to Done: the approval serves the wait.
	e.clock.add(time.Minute)
	e.ws.SetState(e.issue.ID, "Done", e.dana)
	e.pass()
	if len(e.farm.Relays) != 2 || e.farm.Relays[1].Do != "approve" || e.farm.Relays[1].Person != "dana" {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
	e.farm.Update(ns, second, func(j *api.Job) { j.Phase = "Succeeded" })
	m = e.ws.Mutations()
	e.pass()
	e.pass()
	if e.state() != "Done" {
		t.Fatalf("c4-sync moved a done issue: %s", e.state())
	}
	for _, mv := range e.ws.Moves(e.issue.ID) {
		if mv[0] == e.ws.Viewer.ID && (mv[1] == "Done" || mv[2] == "Done" || mv[2] == "Canceled") {
			t.Fatalf("c4-sync made a reviewer's move: %v in %v", mv, e.ws.Moves(e.issue.ID))
		}
	}
	if got := e.ws.Mutations(); got > m+1 {
		t.Fatalf("done wrote %d times (the card's last update at most)", got-m)
	}
}

// Every write survives a lost answer: Linear makes the write, c4-sync
// never hears back. A new comment is found by its key, not posted again;
// the card (an attachment by URL) and the state are sent again, which
// changes nothing.
func TestLinearLostAnswers(t *testing.T) {
	e := setupLinear(t)
	ctx := context.Background()
	e.pass()
	job := e.farm.Submitted[0].Name
	e.farm.Update(ns, job, func(j *api.Job) {
		j.Phase = "Running"
		j.NeedsPerson = &api.NeedsPerson{Reason: "failed", Message: "12 frames failed on node-3"}
	})
	for _, op := range []string{"IssueUpdate", "AttachmentCreate", "CommentCreate"} {
		e.ws.LoseAnswer(op, 1)
	}
	var tr c4v1.Tracker
	for i := 0; i < 3; i++ {
		if err := e.k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "linear"}, &tr); err != nil {
			t.Fatal(err)
		}
		_ = e.sync.Sync(ctx, &tr) // each lost answer fails the pass it is in
	}
	e.pass()
	e.pass()
	if cs := e.ws.Comments(e.issue.ID); len(cs) != 1 {
		t.Fatalf("%d comments after a lost answer, want 1", len(cs))
	}
	if as := e.ws.Attachments(e.issue.ID); len(as) != 1 {
		t.Fatalf("%d cards after a lost answer, want 1", len(as))
	}
	// Running again: the state write, its answer lost, is sent again.
	e.farm.Update(ns, job, func(j *api.Job) { j.NeedsPerson = nil })
	if err := e.k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "linear"}, &tr); err != nil {
		t.Fatal(err)
	}
	_ = e.sync.Sync(ctx, &tr)
	e.pass()
	if e.state() != "In Progress" {
		t.Fatalf("state: %s", e.state())
	}
	if mv := e.ws.Moves(e.issue.ID); len(mv) != 1 || mv[0][2] != "In Progress" {
		t.Fatalf("moves: %v", mv)
	}
	for f, w := range e.link().Status.Written {
		if w.State != "" {
			t.Fatalf("%s left %s", f, w.State)
		}
	}
}

// With no verdictsIn for it, a move to a completed-type state approves,
// whatever the team calls it (the adapter's own default).
func TestLinearDoneApprovesByDefault(t *testing.T) {
	e := setupLinear(t)
	ctx := context.Background()
	var tr c4v1.Tracker
	if err := e.k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "linear"}, &tr); err != nil {
		t.Fatal(err)
	}
	tr.Spec.VerdictsIn = map[string]string{"state:Changes Requested": "rework"}
	if err := e.k8s.Update(ctx, &tr); err != nil {
		t.Fatal(err)
	}
	e.pass()
	job := e.farm.Submitted[0].Name
	e.farm.Update(ns, job, func(j *api.Job) { j.Phase = "Running"; j.Output = &api.Output{View: "c4OutputV001"} })
	e.farm.OpenWait(ns, job, "review", "person:dana", "approve", "rework")
	e.pass()
	e.pass()
	e.clock.add(time.Minute)
	e.ws.SetState(e.issue.ID, "Done", e.dana)
	e.pass()
	if len(e.farm.Relays) != 1 || e.farm.Relays[0].Do != "approve" {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
}
