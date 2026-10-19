package syncer_test

import (
	"context"
	"encoding/json"
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
	e.farm.OpenWait(ns, "gh-7", "review", "person:dana", "rework")
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
	e.farm.OpenWait(ns, "gh-7", "review", "person:dana", "rework")
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
	if len(pub) != 1 || !strings.Contains(pub[0].Body, "`c4Output1234`") {
		t.Fatalf("published: %+v", pub)
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
	e.farm.OpenWait(ns, "gh-7", "review", "person:dana", "rework")
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

// Intake as a milestone needs the operator's Milestone, which is not
// built: the Tracker says so and nothing else happens.
func TestMilestoneIntakeNotBuilt(t *testing.T) {
	e := setup(t, func(tr *c4v1.Tracker) { tr.Spec.Intake[0].As = "milestone" })
	e.pass()
	c := meta.FindStatusCondition(e.tracker().Status.Conditions, syncer.CondIntake)
	if c == nil || c.Reason != "MilestoneNotBuilt" {
		t.Fatalf("condition: %+v", c)
	}
	if len(e.farm.Submitted) != 0 {
		t.Fatal("milestone intake must not submit jobs")
	}
}

func TestDeadline(t *testing.T) {
	at := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)
	cases := []struct {
		due  *tracker.Due
		rule c4v1.TrackerDeadline
		want string
		why  bool
	}{
		{nil, c4v1.TrackerDeadline{}, "", true},
		{&tracker.Due{At: at}, c4v1.TrackerDeadline{}, "2026-11-02T07:00:00Z", false},
		{&tracker.Due{At: at, DateOnly: true}, c4v1.TrackerDeadline{}, "", true},
		{&tracker.Due{At: at, DateOnly: true}, c4v1.TrackerDeadline{DateOnlyAt: "18:00", TZ: "America/Los_Angeles"}, "2026-11-03T02:00:00Z", false},
		{&tracker.Due{At: at}, c4v1.TrackerDeadline{DateOnlyAt: "14:00", TZ: "America/Los_Angeles"}, "2026-11-02T22:00:00Z", false},
	}
	for i, c := range cases {
		got, why, err := syncer.Deadline(c.due, c.rule)
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
	if _, _, err := syncer.Deadline(&tracker.Due{At: at, DateOnly: true}, c4v1.TrackerDeadline{DateOnlyAt: "6pm"}); err == nil {
		t.Error("a bad dateOnlyAt should be an error")
	}
}

func TestNames(t *testing.T) {
	if n := syncer.LinkName("gh", tracker.Ref{Type: "Issue", ID: "7"}); n != "gh-issue-7" {
		t.Fatal(n)
	}
	long := syncer.JobName(strings.Repeat("tracker", 20), tracker.Ref{ID: strconv.Itoa(123456)})
	if len(long) > 63 || strings.HasSuffix(long, "-") {
		t.Fatalf("job name %q", long)
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
