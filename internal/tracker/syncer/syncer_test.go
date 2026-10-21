package syncer_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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
	_ "github.com/Avalanche-io/c4-operator/internal/tracker/github"
	"github.com/Avalanche-io/c4-operator/internal/tracker/github/githubtest"
	"github.com/Avalanche-io/c4-operator/internal/tracker/syncer"
)

const ns = "work"

var jsonMarshal = json.Marshal

var hookSecret = []byte("s3cret")

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	t     *testing.T
	gh    *githubtest.Server
	farm  *farmtest.Farm
	k8s   client.Client
	sync  *syncer.Syncer
	clock *clock
	hooks *httptest.Server
}

var due = time.Date(2026, 11, 2, 17, 0, 0, 0, time.UTC)

func setup(t *testing.T, edit ...func(*c4v1.Tracker)) *env {
	t.Helper()
	// Real time, not a fixed date: the fake's rate-limit reset and the
	// HTTP stack read the wall clock.
	ck := &clock{t: time.Now().UTC().Truncate(time.Second)}
	gh := githubtest.New("acme", "widgets")
	t.Cleanup(gh.Close)
	gh.Token = "tok"
	gh.SetClock(ck.now)
	gh.AddUser("dana", "Dana Reyes", "dana@example.com")
	gh.AddUser("lee", "Lee Park", "")
	gh.AddUser("mallory", "Mallory", "")
	gh.AddIssue(7, "Port the tier fan-in test", []string{"c4:agent"}, &due, "dana")
	gh.AddIssue(8, "Unrelated", []string{"bug"}, &due, "dana")

	sch := runtime.NewScheme()
	if err := c4v1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	tr := &c4v1.Tracker{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gh", Generation: 1,
			Annotations: map[string]string{syncer.AnnotationSelf: gh.App.Login}},
		Spec: c4v1.TrackerSpec{
			Kind: "github", Endpoint: gh.URL, Scope: map[string]string{"repo": "acme/widgets"},
			Credential:    c4v1.TrackerCredential{Secret: c4v1.TrackerSecretKey{Name: "gh-token", Key: "token"}},
			WebhookSecret: &c4v1.TrackerSecretKey{Name: "gh-hook", Key: "secret"},
			Events:        c4v1.TrackerEvents{Mode: "webhook"},
			Rate:          c4v1.TrackerRate{PerSecond: "1000", Burst: 1000},
			Intake: []c4v1.TrackerIntake{{
				Match:  c4v1.TrackerMatch{Type: "Issue", Labels: []string{"c4:agent"}},
				As:     "job",
				Job:    &c4v1.WorkJobSpec{Class: "agent", Params: map[string]string{"bundle": "port"}},
				Review: &c4v1.TrackerReview{Wait: "review"},
			}},
			StatusOut: c4v1.TrackerStatusOut{CommitStatus: true},
			People:    map[string]string{"dana": "dana"},
		},
	}
	for _, e := range edit {
		e(tr)
	}
	k8s := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&c4v1.Tracker{}, &c4v1.TrackerLink{}).
		WithObjects(tr,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gh-token"}, Data: map[string][]byte{"token": []byte("tok\n")}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gh-hook"}, Data: map[string][]byte{"secret": hookSecret}},
		).Build()
	fm := farmtest.New()
	fm.Now = ck.now
	s := &syncer.Syncer{Client: k8s, Farm: fm, Now: ck.now}
	hooks := httptest.NewServer(s.Webhooks())
	t.Cleanup(hooks.Close)
	return &env{t: t, gh: gh, farm: fm, k8s: k8s, sync: s, clock: ck, hooks: hooks}
}

func (e *env) tracker() *c4v1.Tracker {
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "gh"}, &tr); err != nil {
		e.t.Fatal(err)
	}
	return &tr
}

func (e *env) pass() {
	e.t.Helper()
	if err := e.sync.Sync(context.Background(), e.tracker()); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

func (e *env) link() *c4v1.TrackerLink {
	var l c4v1.TrackerLink
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "gh-issue-7"}, &l); err != nil {
		e.t.Fatal(err)
	}
	return &l
}

func (e *env) job(fn func(*api.Job)) { e.farm.Update(ns, "gh-7", fn) }

func (e *env) writes() int { return len(e.gh.Writes()) }

// lastCheck is the newest check run on sha.
func (e *env) lastCheck(sha string) githubtest.CheckRun {
	cs := e.gh.CheckRuns(sha)
	if len(cs) == 0 {
		e.t.Fatalf("no check run on %s", sha)
	}
	return cs[len(cs)-1]
}

func (e *env) deliver(event string, payload any, secret []byte, id string) int {
	e.t.Helper()
	body, err := jsonMarshal(payload)
	if err != nil {
		e.t.Fatal(err)
	}
	_, code, err := githubtest.DeliverRaw(e.hooks.URL+"/webhooks/"+ns+"/gh", event, body, secret, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return code
}

// The whole loop: an issue with a milestone due date becomes a job; work
// states go out as one comment, a check run and a commit status; a review
// requesting changes comes back as a rework; an approval serves the wait;
// the output is published once.
func TestIssueToJobToReviewLoop(t *testing.T) {
	e := setup(t)

	// 1. The labeled issue becomes a job whose deadline is the milestone's.
	e.pass()
	if len(e.farm.Submitted) != 1 {
		t.Fatalf("submitted %d jobs, want 1 (the unlabeled issue stays out)", len(e.farm.Submitted))
	}
	sub := e.farm.Submitted[0]
	if sub.Name != "gh-7" || sub.Spec.DeadlineMode != "Tracking" || !sub.Spec.Deadline.Time.Equal(due) ||
		sub.TrackerRef != "gh/Issue/7" || sub.Spec.Params["bundle"] != "port" {
		t.Fatalf("submitted: %+v", sub)
	}
	l := e.link()
	if l.Spec.Target.WorkJob != "gh-7" || l.Spec.Ref.URL == "" || l.Status.State != syncer.StateQueued {
		t.Fatalf("link: %+v", l)
	}
	cs := e.gh.Comments(7)
	if len(cs) != 1 || !strings.Contains(cs[0].Body, "Queued") || !strings.Contains(cs[0].Body, "C4-Job: work/gh-7") ||
		!strings.Contains(cs[0].Body, "from the tracker") {
		t.Fatalf("issue comment: %+v", cs)
	}
	if len(e.gh.Comments(8)) != 0 {
		t.Fatal("the unlabeled issue was written to")
	}

	// 2. A second pass with nothing changed writes nothing.
	w := e.writes()
	e.pass()
	if e.writes() != w || e.farm.Count() != 1 {
		t.Fatalf("idle pass wrote %d times; %d jobs", e.writes()-w, e.farm.Count())
	}

	// 3. The milestone moves: tracking moves the job's deadline.
	moved := due.Add(24 * time.Hour)
	e.gh.SetMilestoneDue(moved)
	e.clock.add(time.Minute)
	e.pass()
	if len(e.farm.Deadlines) != 1 || !e.farm.Deadlines[0].Equal(moved) {
		t.Fatalf("deadlines: %v", e.farm.Deadlines)
	}

	// 4. The job runs and the agent opens a pull request: a check run and
	// a commit status appear, and the issue's comment is edited in place.
	eta := e.clock.now().Add(3 * time.Hour)
	e.job(func(j *api.Job) {
		j.Phase = "Running"
		j.Estimate.EtaP80 = &eta
		j.Estimate.SlackSeconds = 3600
	})
	e.gh.AddPull(12, "Port the fan-in test", "Closes #7\n\nWork by the C4 bundle.", "abc", "lee")
	e.pass()
	if r := e.link().Status.Review; r == nil || r.ID != "12" {
		t.Fatalf("review: %+v", r)
	}
	cr := e.lastCheck("abc")
	if cr.Status != "in_progress" || !strings.HasPrefix(cr.Output.Title, "Running · ETA ") || cr.ExternalID != "work/gh-7" ||
		!strings.Contains(cr.DetailsURL, "#/job/work/gh-7") {
		t.Fatalf("check run: %+v", cr)
	}
	if st := e.gh.Statuses("abc"); len(st) != 1 || st[0].State != "pending" || st[0].Context != "c4/gh-7" {
		t.Fatalf("statuses: %+v", st)
	}
	if cs := e.gh.Comments(7); len(cs) != 1 || !strings.Contains(cs[0].Body, "Running") {
		t.Fatalf("comment after running: %+v", cs)
	}

	// 5. The ETA moves: not rewritten within fieldSeconds, then rewritten
	// in place on the same check run.
	later := eta.Add(10 * time.Minute)
	e.job(func(j *api.Job) { j.Estimate.EtaP80 = &later })
	w = e.writes()
	e.clock.add(time.Minute)
	e.pass()
	if e.writes() != w {
		t.Fatal("an ETA move was written before fieldSeconds passed")
	}
	e.clock.add(6 * time.Minute)
	e.pass()
	if n := len(e.gh.CheckRuns("abc")); n != 1 {
		t.Fatalf("%d check runs, want the one updated in place", n)
	}
	if got := e.lastCheck("abc").Output.Title; !strings.Contains(got, later.UTC().Format("15:04")) {
		t.Fatalf("title %q does not show the new ETA", got)
	}

	// 6. The review wait opens: the check run says who it waits for and
	// offers the wait's decisions as buttons.
	e.farm.OpenWait(ns, "gh-7", "review", "person:dana", "approve", "rework")
	e.pass()
	cr = e.lastCheck("abc")
	if cr.Status != "in_progress" || cr.Output.Title != "Waiting for review by dana · ETA "+later.UTC().Format("Mon 15:04 UTC") ||
		len(cr.Actions) != 2 || cr.Actions[1].Identifier != "rework" {
		t.Fatalf("check run in review: %+v", cr)
	}
	if e.link().Status.State != syncer.StateInReview {
		t.Fatalf("state: %s", e.link().Status.State)
	}

	// 7. Someone not mapped to a C4 person reviews: nothing is relayed.
	e.clock.add(time.Minute)
	e.gh.AddReview(12, "mallory", "APPROVED", "lgtm")
	e.pass()
	if len(e.farm.Relays) != 0 {
		t.Fatalf("relayed an unmapped reviewer's decision: %+v", e.farm.Relays)
	}
	if ds := e.link().Status.Decisions; len(ds) != 1 || !strings.Contains(ds[0].Error, "not mapped") {
		t.Fatalf("decisions: %+v", ds)
	}

	// 8. Dana requests changes; the webhook is a hint (verified, deduped),
	// and the pass relays a rework with the review's text.
	e.clock.add(time.Minute)
	rv := e.gh.AddReview(12, "dana", "CHANGES_REQUESTED", "Close, two things.",
		githubtest.ReviewComment{Path: "tier/fanin_test.go", Line: 42, Body: "assert the order, not just the count"})
	payload := e.gh.ReviewPayload(12, rv)
	if c := e.deliver("pull_request_review", payload, []byte("wrong"), ""); c != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", c)
	}
	if c := e.deliver("pull_request_review", payload, hookSecret, "d-1"); c != http.StatusAccepted {
		t.Fatalf("delivery: %d", c)
	}
	if c := e.deliver("pull_request_review", payload, hookSecret, "d-1"); c != http.StatusNoContent {
		t.Fatalf("redelivery: %d", c)
	}
	e.pass()
	if len(e.farm.Relays) != 1 {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
	r := e.farm.Relays[0]
	if r.Do != "rework" || r.Unit != "review" || r.Person != "dana" || r.By != "@dana" || r.Project != "proj" ||
		!strings.Contains(r.Note, "tier/fanin_test.go:42: assert the order") || len(r.Evidence) == 0 {
		t.Fatalf("relay: %+v", r)
	}
	if got := e.lastCheck("abc").Output.Title; !strings.HasPrefix(got, "Reworking") {
		t.Fatalf("after rework the check run reads %q", got)
	}
	e.pass()
	if len(e.farm.Relays) != 1 {
		t.Fatal("a decision was relayed twice")
	}

	// 9. The rework comes back for review; Dana approves; the approval
	// serves the new wait. The old rework is not relayed again.
	e.clock.add(time.Hour)
	e.farm.OpenWait(ns, "gh-7", "review", "person:dana", "approve", "rework")
	e.pass()
	e.clock.add(time.Minute)
	e.gh.AddReview(12, "dana", "APPROVED", "Ship it.")
	e.pass()
	if len(e.farm.Relays) != 2 || e.farm.Relays[1].Do != "approve" {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}

	// 10. Done: the check run completes on the same run, the commit status
	// succeeds, and the output is published once with its C4 IDs.
	e.job(func(j *api.Job) {
		j.Phase = "Succeeded"
		j.NeedsPerson = nil
		j.Output = &api.Output{View: "c4Output1234"}
	})
	e.pass()
	runs := e.gh.CheckRuns("abc")
	cr = runs[len(runs)-1]
	if len(runs) != 1 || cr.Status != "completed" || cr.Conclusion == nil || *cr.Conclusion != "success" {
		t.Fatalf("done check runs: %+v", runs)
	}
	st := e.gh.Statuses("abc")
	if st[len(st)-1].State != "success" {
		t.Fatalf("statuses: %+v", st)
	}
	pub := e.gh.Comments(12)
	if len(pub) != 1 || !strings.Contains(pub[0].Body, "`c4Output1234`") || !strings.Contains(pub[0].Body, "C4 output, version 1.") {
		t.Fatalf("published: %+v", pub)
	}
	// What is published stays true: no state in it (that is the check run's).
	for _, w := range []string{"Done", "Waiting", "Running", "review"} {
		if strings.Contains(pub[0].Body, w) {
			t.Fatalf("the published comment carries state (%q): %s", w, pub[0].Body)
		}
	}
	w = e.writes()
	e.pass()
	if e.writes() != w {
		t.Fatalf("a pass after done wrote %d more times", e.writes()-w)
	}
	// Never a judgment: nothing c4-sync wrote is a review or an approval.
	for _, c := range e.gh.Writes() {
		if strings.Contains(c.Path, "/reviews") || strings.Contains(c.Path, "/merge") {
			t.Fatalf("c4-sync wrote a judgment: %s %s", c.Method, c.Path)
		}
	}
}

// A person needed (failed units) completes the check run as
// action_required; when work resumes a new run takes over, as a re-run
// does on GitHub, rather than reopening a completed one.
func TestNeedsPersonThenResumes(t *testing.T) {
	e := setup(t)
	e.pass()
	e.gh.AddPull(12, "Port", "C4-Job: work/gh-7", "abc", "lee")
	e.job(func(j *api.Job) { j.Phase = "Running" })
	e.pass()
	e.job(func(j *api.Job) {
		j.NeedsPerson = &api.NeedsPerson{Reason: "failed", Message: "3 units failed after their retries: node-3, GPU driver"}
	})
	e.pass()
	cr := e.lastCheck("abc")
	if cr.Conclusion == nil || *cr.Conclusion != "action_required" || !strings.Contains(cr.Output.Summary, "node-3, GPU driver") {
		t.Fatalf("needs person: %+v", cr)
	}
	if c := e.gh.Comments(7); len(c) != 1 || !strings.Contains(c[0].Body, "node-3, GPU driver") {
		t.Fatalf("the cause goes in the one comment: %+v", c)
	}
	e.job(func(j *api.Job) { j.NeedsPerson = nil })
	e.pass()
	runs := e.gh.CheckRuns("abc")
	if len(runs) != 2 || runs[1].Status != "in_progress" || *runs[0].Conclusion != "action_required" {
		t.Fatalf("runs: %+v", runs)
	}
}

// A check run's buttons serve the wait too; a person's wait is served
// only by that person.
func TestCheckRunButtonServesWait(t *testing.T) {
	e := setup(t)
	e.pass()
	e.gh.AddPull(12, "Port", "C4-Job: work/gh-7", "abc", "lee")
	e.job(func(j *api.Job) { j.Phase = "Running" })
	e.farm.OpenWait(ns, "gh-7", "review", "person:dana", "approve", "rework")
	e.pass()
	id := e.lastCheck("abc").ID
	if c := e.deliver("check_run", e.gh.CheckActionPayload(id, "approve", "lee"), hookSecret, "a-1"); c != http.StatusAccepted {
		t.Fatalf("delivery: %d", c)
	}
	e.pass()
	if len(e.farm.Relays) != 0 {
		t.Fatal("lee's click served dana's wait")
	}
	e.clock.add(time.Second)
	if c := e.deliver("check_run", e.gh.CheckActionPayload(id, "approve", "dana"), hookSecret, "a-2"); c != http.StatusAccepted {
		t.Fatalf("delivery: %d", c)
	}
	e.pass()
	if len(e.farm.Relays) != 1 || e.farm.Relays[0].Do != "approve" || e.farm.Relays[0].Person != "dana" {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
}

// A crash between a tracker write and recording it: the next pass finds
// the write by its key and does not repeat it.
func TestNoDuplicateAfterLostRecord(t *testing.T) {
	e := setup(t)
	e.pass()
	l := e.link()
	before := l.DeepCopy()
	l.Status.Written = nil
	if err := e.k8s.Status().Patch(context.Background(), l, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if n := len(e.gh.Comments(7)); n != 1 {
		t.Fatalf("%d comments after a lost record, want 1", n)
	}
	if e.link().Status.Written["comment"].Ref == "" {
		t.Fatal("the probe should recover the comment's id")
	}
}

// A crash after the comment was journaled and sent, before its answer was
// kept: the next pass probes, finds it, and does not post it again. Writes
// the journal vouches for are not probed.
func TestNoDuplicateAfterInterruptedWrite(t *testing.T) {
	e := setup(t)
	e.pass()
	l := e.link()
	before := l.DeepCopy()
	w := l.Status.Written["comment"]
	w.State, w.Ref = "sending", ""
	l.Status.Written["comment"] = w
	if err := e.k8s.Status().Patch(context.Background(), l, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if n := len(e.gh.Comments(7)); n != 1 {
		t.Fatalf("%d comments after an interrupted write, want 1", n)
	}
	if got := e.link().Status.Written["comment"]; got.Ref == "" || got.State != "" {
		t.Fatalf("the probe should recover the comment and mark it written: %+v", got)
	}
}

// Rate limited: the pass stops, the Tracker says until when, and nothing
// is called again before then.
func TestRateLimited(t *testing.T) {
	e := setup(t)
	reset := time.Now().Add(20 * time.Minute)
	e.gh.SetRateRemaining(0, reset)
	e.pass()
	c := meta.FindStatusCondition(e.tracker().Status.Conditions, syncer.CondRateLimited)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("condition: %+v", c)
	}
	n := len(e.gh.Calls())
	e.clock.add(time.Minute)
	e.pass()
	if len(e.gh.Calls()) != n {
		t.Fatal("called GitHub while rate limited")
	}
	e.gh.SetRateRemaining(5000, reset)
	e.clock.add(30 * time.Minute)
	e.pass()
	if len(e.farm.Submitted) != 1 {
		t.Fatal("intake did not resume after the reset")
	}
}

// A person who pins another deadline in the HUD wins over the tracker;
// the link says the two have diverged.
func TestPinnedDeadlineDiverges(t *testing.T) {
	e := setup(t)
	e.pass()
	e.job(func(j *api.Job) { j.Objective.Mode = "Pinned" })
	e.gh.SetMilestoneDue(due.Add(48 * time.Hour))
	e.pass()
	if len(e.farm.Deadlines) != 0 {
		t.Fatal("tracking overwrote a pinned deadline")
	}
	c := meta.FindStatusCondition(e.link().Status.Conditions, syncer.CondDiverged)
	if c == nil || c.Reason != "DeadlineNotTracking" {
		t.Fatalf("condition: %+v", c)
	}
}

func TestDeadline(t *testing.T) {
	at := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)
	la := &c4v1.DueTime{At: "18:00", TimeZone: "America/Los_Angeles"}
	cases := []struct {
		due  *tracker.Due
		rule c4v1.TrackerDeadline
		conv *c4v1.DueTime
		want string
		why  bool
	}{
		{nil, c4v1.TrackerDeadline{}, nil, "", true},
		{&tracker.Due{At: at}, c4v1.TrackerDeadline{}, la, "2026-11-02T07:00:00Z", false},
		// a date alone: the project's due time, else 18:00 UTC, as the farm reads one
		{&tracker.Due{At: at, DateOnly: true}, c4v1.TrackerDeadline{}, nil, "2026-11-02T18:00:00Z", false},
		{&tracker.Due{At: at, DateOnly: true}, c4v1.TrackerDeadline{}, la, "2026-11-03T02:00:00Z", false},
		// the rule's time wins over the project's
		{&tracker.Due{At: at, DateOnly: true}, c4v1.TrackerDeadline{DateOnlyAt: "18:00", TimeZone: "America/Los_Angeles"}, nil, "2026-11-03T02:00:00Z", false},
		{&tracker.Due{At: at}, c4v1.TrackerDeadline{DateOnlyAt: "14:00", TimeZone: "America/Los_Angeles"}, &c4v1.DueTime{At: "09:00"}, "2026-11-02T22:00:00Z", false},
	}
	for i, c := range cases {
		got, why, err := syncer.Deadline(c.due, c.rule, c.conv)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		s := ""
		if got != nil {
			s = got.Format(time.RFC3339)
		}
		if s != c.want || (why != "") != c.why {
			t.Errorf("%d: got %q %q, want %q", i, s, why, c.want)
		}
	}
	if _, _, err := syncer.Deadline(&tracker.Due{At: at, DateOnly: true}, c4v1.TrackerDeadline{DateOnlyAt: "6pm"}, nil); err == nil {
		t.Error("a bad dateOnlyAt should be an error")
	}
}

// The job c4-sync submits carries the issue it was made for, and where a
// person opens it.
func TestSubmitCarriesTheTrackerRef(t *testing.T) {
	e := setup(t)
	e.pass()
	if len(e.farm.Submitted) != 1 || e.farm.Submitted[0].TrackerRef != "gh/Issue/7" || e.farm.Submitted[0].Follows != "" ||
		!strings.HasSuffix(e.farm.Submitted[0].TrackerURL, "/issues/7") {
		t.Fatalf("submitted: %+v", e.farm.Submitted)
	}
}

func TestNames(t *testing.T) {
	if n := syncer.LinkName("gh", tracker.Ref{Type: "Issue", ID: "7"}); n != "gh-issue-7" {
		t.Fatal(n)
	}
	task := tracker.Ref{Type: "Task", ID: "2f0fd47e-f4ce-4aa3-863f-5b1a24188ad3"}
	hex := func(r tracker.Ref) string {
		h := sha256.Sum256([]byte(r.Type + "/" + r.ID))
		return fmt.Sprintf("%x", h[:3])
	}
	for _, c := range []struct {
		tr   string
		it   tracker.Item
		want string
	}{
		// A unique handle is the name: GitHub's issue number.
		{"gh", tracker.Item{Ref: tracker.Ref{Type: "Issue", ID: "7"}, Name: "7", NameUnique: true}, "gh-7"},
		{"linear", tracker.Item{Ref: tracker.Ref{Type: "Issue", ID: "9c1d"}, Name: "ENG-12", NameUnique: true}, "linear-eng-12"},
		// One that may repeat is told apart by its ref.
		{"kitsu", tracker.Item{Ref: task, Name: "sh040 Lighting"}, "kitsu-sh040-lighting-" + hex(task)},
		// A handle that cleans to nothing (a shot named in kanji) is no
		// handle: the ID.
		{"kitsu", tracker.Item{Ref: tracker.Ref{Type: "Task", ID: "7c1e"}, Name: "照明 ライティング"}, "kitsu-7c1e"},
		{"kitsu", tracker.Item{Ref: tracker.Ref{Type: "Task", ID: "7c1e"}, Name: "--"}, "kitsu-7c1e"},
		// No handle: the ID, as before.
		{"ml", tracker.Item{Ref: tracker.Ref{Type: "Run", ID: "0a1b"}}, "ml-0a1b"},
		// A long handle is cut, leaving room for -forecast.
		{"kitsu", tracker.Item{Ref: task, Name: "a very long environment asset name modeling"}, "kitsu-a-very-long-environment-asse-" + hex(task)},
		// A Tracker name that leaves no room falls back to the ID's form.
		{strings.Repeat("tracker", 6), tracker.Item{Ref: task, Name: "sh040"}, ""},
	} {
		got := syncer.JobName(c.tr, c.it)
		if c.want != "" && got != c.want {
			t.Errorf("%s %+v: %q, want %q", c.tr, c.it.Name, got, c.want)
		}
		if len(got) > 41 && c.want != "" || len(got) > 50 || strings.HasSuffix(got, "-") {
			t.Errorf("%q does not leave room for -forecast within 50", got)
		}
	}
	long := syncer.JobName(strings.Repeat("tracker", 20), tracker.Item{Ref: tracker.Ref{ID: strconv.Itoa(123456)}})
	// within a WorkJob name's 50 characters, and distinct
	if len(long) > 50 || strings.HasSuffix(long, "-") {
		t.Fatalf("job name %q", long)
	}
	if other := syncer.JobName(strings.Repeat("tracker", 20), tracker.Item{Ref: tracker.Ref{ID: "123457"}}); other == long {
		t.Fatalf("two entities, one job name %q", long)
	}
}

// A push moves the pull request's head: the change feed reports it, and
// the new commit gets its own check run and status. Between changes the
// pull request is not read again.
func TestPushGetsNewCheckRun(t *testing.T) {
	e := setup(t)
	e.pass()
	e.gh.AddPull(12, "Port", "C4-Job: work/gh-7", "abc", "lee")
	e.job(func(j *api.Job) { j.Phase = "Running" })
	e.pass()
	reads := func() int {
		n := 0
		for _, c := range e.gh.Calls() {
			if c.Method == "GET" && strings.HasSuffix(c.Path, "/pulls/12") {
				n++
			}
		}
		return n
	}
	before := reads()
	e.clock.add(time.Minute)
	e.pass()
	if reads() != before {
		t.Fatal("the pull request was read again with nothing changed")
	}
	e.clock.add(time.Minute)
	e.gh.Push(12, "def")
	e.pass()
	if e.link().Status.ReviewHead != "def" {
		t.Fatalf("head: %q", e.link().Status.ReviewHead)
	}
	if len(e.gh.CheckRuns("def")) != 1 || len(e.gh.Statuses("def")) != 1 {
		t.Fatalf("new head: %d runs, %d statuses", len(e.gh.CheckRuns("def")), len(e.gh.Statuses("def")))
	}
}

// The queue and the HUD take only a wait's choices when it has them
// (internal/work/queue/wait.go): an approval the wait does not offer is
// refused and recorded, not sent to be refused 400 times by the HUD.
func TestApproveOnlyWhenTheWaitOffersIt(t *testing.T) {
	e := setup(t)
	e.pass()
	e.gh.AddPull(12, "Port", "C4-Job: work/gh-7", "abc", "lee")
	e.job(func(j *api.Job) { j.Phase = "Running" })
	e.farm.OpenWait(ns, "gh-7", "review", "person:dana", "rework")
	e.pass()
	e.clock.add(time.Minute)
	e.gh.AddReview(12, "dana", "APPROVED", "ok")
	e.pass()
	if len(e.farm.Relays) != 0 {
		t.Fatal("approve was relayed to a wait whose only choice is rework")
	}
	ds := e.link().Status.Decisions
	if len(ds) != 1 || !strings.Contains(ds[0].Error, `does not take "approve"`) {
		t.Fatalf("decisions: %+v", ds)
	}
	if acts := e.lastCheck("abc").Actions; len(acts) != 1 || acts[0].Identifier != "rework" {
		t.Fatalf("the check run offers only what the wait takes: %+v", acts)
	}
}

// An intake rule whose work is for a person submits only under that
// person's grant: without one the Tracker says so and nothing is
// submitted, never c4-sync's own job instead; with one the job is theirs.
func TestSubmitForAPersonNeedsTheirGrant(t *testing.T) {
	e := setup(t, func(tr *c4v1.Tracker) { tr.Spec.Intake[0].For = &c4v1.TrackerFor{Assignee: true} })
	intake := func() *metav1.Condition {
		return meta.FindStatusCondition(e.tracker().Status.Conditions, syncer.CondIntake)
	}
	e.pass()
	if c := intake(); c == nil || c.Reason != "NoPerson" || len(e.farm.Submitted) != 0 {
		t.Fatalf("no assignee: %+v, %d submitted", c, len(e.farm.Submitted))
	}

	e.gh.Assign(7, "mallory") // not one of the Tracker's people
	e.clock.add(time.Minute)
	e.pass()
	if c := intake(); c == nil || c.Reason != "NoPerson" || len(e.farm.Submitted) != 0 {
		t.Fatalf("unmapped assignee: %+v, %d submitted", c, len(e.farm.Submitted))
	}

	e.gh.Assign(7, "dana")
	e.clock.add(time.Minute)
	e.pass()
	if c := intake(); c == nil || c.Reason != "NoGrant" || !strings.Contains(c.Message, "dana has not let tracker gh") ||
		!strings.Contains(c.Message, "c4 farm grant gh") || len(e.farm.Submitted) != 0 {
		t.Fatalf("no grant: %+v, %d submitted", c, len(e.farm.Submitted))
	}

	e.farm.Grants = map[string]bool{"dana": true}
	e.clock.add(time.Minute)
	e.pass()
	if len(e.farm.Submitted) != 1 || e.farm.Submitted[0].For != "dana" {
		t.Fatalf("granted: %+v", e.farm.Submitted)
	}
	j, err := e.farm.Job(context.Background(), farm.JobRef{Namespace: ns, Name: "gh-7"})
	if err != nil || j.Owner != "dana" {
		t.Fatalf("the job is dana's: %+v %v", j, err)
	}
	if c := intake(); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "Granted" {
		t.Fatalf("condition after the grant: %+v", c)
	}
}

// Failed units assigned to a person in C4 are mirrored where they work:
// one @-mention on the issue, once, for a person the Tracker's people map
// names; a C4 person it gives no tracker user stays assigned in C4 only.
func TestAssignmentMentionsThePerson(t *testing.T) {
	e := setup(t)
	e.pass()
	at := e.clock.now()
	e.job(func(j *api.Job) {
		j.Phase = "Running"
		j.Acks = []api.Ack{{Kind: "assign", Units: "14", Actor: "lee", Assignee: "dana", Note: "the GPU driver on node-3", At: at}}
	})
	e.pass()
	e.clock.add(time.Minute)
	e.pass()
	var mentions []string
	for _, c := range e.gh.Comments(7) {
		if strings.HasPrefix(c.Body, "@dana ") {
			mentions = append(mentions, c.Body)
		}
	}
	if len(mentions) != 1 || !strings.Contains(mentions[0], "units 14 of work/gh-7 failed and are assigned to you by lee: the GPU driver on node-3") {
		t.Fatalf("mentions: %q", mentions)
	}

	e.job(func(j *api.Job) {
		j.Acks = append(j.Acks, api.Ack{Kind: "assign", Units: "15", Actor: "lee", Assignee: "ivo", At: at})
	})
	e.clock.add(time.Minute)
	e.pass()
	c := meta.FindStatusCondition(e.link().Status.Conditions, "Assigned")
	if c == nil || c.Reason != "NoTrackerUser" || !strings.Contains(c.Message, "ivo") {
		t.Fatalf("condition: %+v", c)
	}
	for _, cm := range e.gh.Comments(7) {
		if strings.Contains(cm.Body, "units 15") {
			t.Fatalf("mentioned someone for ivo: %s", cm.Body)
		}
	}
}

// One comment per cause a person must act on, edited in place as the
// cause grows; a cause the farm handled (retried only) is not posted.
func TestCauseCommentsOnePerCause(t *testing.T) {
	e := setup(t)
	e.pass()
	at := e.clock.now()
	oom := api.Cause{Kind: "exit", Key: "exit:137:OOMKilled", Title: "exit 137 OOMKilled", Units: "0-1", Count: 2,
		Nodes: []string{"node-3"}, Did: []string{"retried", "failed"}, First: at, Last: at}
	e.job(func(j *api.Job) {
		j.Phase = "Running"
		j.Causes = []api.Cause{oom, {Kind: "exit", Key: "exit:143:Error", Title: "exit 143", Units: "4", Count: 1, Did: []string{"retried"}, Last: at}}
	})
	e.pass()
	causes := func() []string {
		var out []string
		for _, c := range e.gh.Comments(7) {
			if strings.HasPrefix(c.Body, "C4 cause on") {
				out = append(out, c.Body)
			}
		}
		return out
	}
	got := causes()
	if len(got) != 1 || !strings.Contains(got[0], "C4 cause on work/gh-7: exit 137 OOMKilled. 2 attempts, units 0-1, on node-3. The farm retried, then failed.") {
		t.Fatalf("cause comments: %q", got)
	}
	e.job(func(j *api.Job) {
		j.Causes[0].Units, j.Causes[0].Count, j.Causes[0].Did = "0-2", 3, []string{"retried", "failed", "blacklisted"}
	})
	e.clock.add(time.Minute)
	e.pass()
	if got = causes(); len(got) != 1 || !strings.Contains(got[0], "3 attempts, units 0-2") || !strings.Contains(got[0], "then blacklisted") {
		t.Fatalf("after the cause grew: %q", got)
	}
}

// The farm's log, read from the Tracker's cursor: a new Tracker starts at
// its head (no history is posted); a decision made in C4 on a linked job
// reaches the tracker once, however long c4-sync was away; a decision the
// tracker relayed is not echoed back; a cursor the log no longer reaches
// goes on from where it begins.
func TestFarmLogTellsTheTrackerWhatItMissed(t *testing.T) {
	e := setup(t)
	at := e.clock.now()
	seven := 7
	decided := func(seq int64, actor, do string) api.Transition {
		return api.Transition{Seq: seq, At: at, Project: "proj", Job: ns + "/gh-7", Unit: &seven, Kind: "wait-decided", Actor: actor, Do: do,
			Record: "c4rec" + strconv.FormatInt(seq, 10), Key: fmt.Sprintf("%s/gh-7/wait-decided/7/%d", ns, seq)}
	}
	e.farm.Log = []api.Transition{decided(1, "maya", "approve"), {Seq: 2, Job: ns + "/other", Kind: "phase", Key: "p"}}
	e.pass()
	if c := e.tracker().Status.FarmCursor; c != 2 {
		t.Fatalf("a new Tracker's cursor: %d, want the head", c)
	}
	told := func() []string {
		var out []string
		for _, c := range e.gh.Comments(7) {
			if strings.Contains(c.Body, "in C4, at") {
				out = append(out, c.Body)
			}
		}
		return out
	}
	if len(told()) != 0 {
		t.Fatal("history from before the Tracker was posted")
	}

	// While c4-sync was away: Dana decided in the HUD; another decision
	// came from the tracker itself.
	e.farm.Log = append(e.farm.Log, decided(3, "dana", "rework"), decided(4, "dana via tracker gh (@dana)", "approve"))
	e.clock.add(time.Hour)
	e.pass()
	e.pass()
	got := told()
	if len(got) != 1 || !strings.Contains(got[0], "C4: unit 7 of "+ns+"/gh-7 was decided rework by dana, in C4") || !strings.Contains(got[0], "Record c4rec3.") {
		t.Fatalf("told: %q", got)
	}
	if c := e.tracker().Status.FarmCursor; c != 4 {
		t.Fatalf("cursor %d, want 4", c)
	}

	// The log moved past the cursor (its older segments dropped).
	e.farm.Log = append(e.farm.Log, decided(12, "pat", "approve"))
	e.farm.Oldest = 10
	e.pass()
	if got = told(); len(got) != 2 || !strings.Contains(got[1], "decided approve by pat") {
		t.Fatalf("after the log moved on: %q", got)
	}
}

// A job someone else made under the name c4-sync would give an issue's
// job is never adopted: it would carry this tracker's states and
// decisions. The Tracker says the name is taken, and nothing is linked.
func TestNameTakenIsNotAdopted(t *testing.T) {
	e := setup(t)
	e.farm.Seed(api.Job{Namespace: ns, Name: "gh-7", Owner: "mallory", TrackerRef: "gh/Issue/7", Phase: "Queued"})
	e.pass()
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "gh"}, &tr); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(tr.Status.Conditions, syncer.CondIntake)
	if c == nil || c.Reason != "NameTaken" || !strings.Contains(c.Message, "mallory") {
		t.Fatalf("intake: %+v", c)
	}
	var ls c4v1.TrackerLinkList
	if err := e.k8s.List(context.Background(), &ls, client.InNamespace(ns)); err != nil || len(ls.Items) != 0 {
		t.Fatalf("links: %d %v", len(ls.Items), err)
	}
}

// Only a missing grant is reported as one. A grant that ended, or a HUD
// refusing for another reason (a sync it does not know), says so in its
// own words, never "has not let this tracker submit work".
func TestRefusalsSayWhichTheyAre(t *testing.T) {
	for _, c := range []struct {
		err    *farm.APIError
		reason string
		says   string
	}{
		{&farm.APIError{Status: http.StatusForbidden, Reason: api.RefusedGrantLapsed, Msg: "dana revoked the grant from dana to tracker gh"},
			"GrantLapsed", "dana revoked the grant from dana to tracker gh: they grant it again"},
		{&farm.APIError{Status: http.StatusForbidden, Reason: api.RefusedGrantUncovered, Msg: "the grant from dana does not cover pool gpu (only cpu)"},
			"GrantUncovered", "does not cover pool gpu"},
		{&farm.APIError{Status: http.StatusForbidden, Reason: api.RefusedNotSync, Msg: "only a tracker's sync submits work for another person"},
			"Refused", "the HUD refused to submit work for dana: only a tracker's sync"},
		{&farm.APIError{Status: http.StatusNotImplemented, Reason: api.RefusedNotInstalled, Msg: "this farm has no submit grants installed"},
			"Refused", "no submit grants installed"},
	} {
		e := setup(t, func(tr *c4v1.Tracker) { tr.Spec.Intake[0].For = &c4v1.TrackerFor{Person: "dana"} })
		e.farm.Refusal = c.err
		e.pass()
		var tr c4v1.Tracker
		if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "gh"}, &tr); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(tr.Status.Conditions, syncer.CondIntake)
		if cond == nil || cond.Reason != c.reason || !strings.Contains(cond.Message, c.says) || strings.Contains(cond.Message, "has not let") {
			t.Errorf("%s: %+v", c.err.Reason, cond)
		}
	}
}
