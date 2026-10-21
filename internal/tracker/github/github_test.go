package github_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/github"
	"github.com/Avalanche-io/c4-operator/internal/tracker/github/githubtest"
)

func newAdapter(t *testing.T, gh *githubtest.Server, secret []byte) *github.Adapter {
	t.Helper()
	c := tracker.Config{
		Endpoint: gh.URL,
		Scope:    map[string]string{"repo": gh.Owner + "/" + gh.Repo},
		Token:    func(context.Context) (string, error) { return "tok", nil },
		Self:     gh.App.Login,
	}
	if secret != nil {
		c.WebhookSecret = func(context.Context) ([]byte, error) { return secret, nil }
	}
	a, err := github.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func fake(t *testing.T) *githubtest.Server {
	gh := githubtest.New("acme", "widgets")
	gh.Token = "tok"
	t.Cleanup(gh.Close)
	gh.AddUser("dana", "Dana Reyes", "dana@example.com")
	gh.AddUser("lee", "Lee Park", "")
	return gh
}

func TestScopeRequired(t *testing.T) {
	for _, repo := range []string{"", "acme", "acme/", "/w", "a/b/c"} {
		if _, err := github.New(tracker.Config{Scope: map[string]string{"repo": repo}}); err == nil {
			t.Errorf("repo %q: want an error", repo)
		}
	}
}

func TestRegistered(t *testing.T) {
	a, err := tracker.New("github", tracker.Config{Scope: map[string]string{"repo": "a/b"}})
	if err != nil || a.Kind() != "github" {
		t.Fatalf("registry: %v %v", a, err)
	}
}

func TestBadCredentialsAreAnError(t *testing.T) {
	gh := fake(t)
	a, _ := github.New(tracker.Config{Endpoint: gh.URL, Scope: map[string]string{"repo": "acme/widgets"},
		Token: func(context.Context) (string, error) { return "wrong", nil }})
	if _, err := a.Get(context.Background(), tracker.Ref{Type: "Issue", ID: "1"}); err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("want Bad credentials, got %v", err)
	}
}

func TestIssueWithMilestoneDue(t *testing.T) {
	gh := fake(t)
	due := time.Date(2026, 11, 2, 17, 0, 0, 0, time.UTC)
	gh.AddIssue(7, "Port the tier fan-in test", []string{"c4:agent"}, &due, "dana")
	a := newAdapter(t, gh, nil)
	it, err := a.Get(context.Background(), tracker.Ref{Type: "Issue", ID: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if it.Due == nil || !it.Due.At.Equal(due) || it.Due.DateOnly {
		t.Fatalf("due: %+v", it.Due)
	}
	if it.Fields["milestone.due_on"] != "2026-11-02T17:00:00Z" || it.Ref.URL == "" || it.Status.Category != "ready" {
		t.Fatalf("item: %+v", it)
	}
	if _, err := a.Get(context.Background(), tracker.Ref{Type: "Issue", ID: "99"}); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("missing issue: %v", err)
	}
}

func TestListPagesAndSkipsPullRequests(t *testing.T) {
	gh := fake(t)
	for i := 1; i <= 230; i++ {
		labels := []string{"c4:agent"}
		if i%2 == 0 {
			labels = []string{"other"}
		}
		gh.AddIssue(i, "issue", labels, nil, "dana")
	}
	gh.AddPull(500, "a PR", "Closes #3", "sha500", "lee")
	a := newAdapter(t, gh, nil)
	var all []tracker.Item
	page, pages := "", 0
	for {
		items, next, err := a.List(context.Background(), tracker.Query{Type: "Issue", Labels: []string{"c4:agent"}}, page)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, items...)
		pages++
		if next == "" {
			break
		}
		page = next
	}
	if len(all) != 115 || pages != 2 {
		t.Fatalf("got %d items in %d pages, want 115 in 2", len(all), pages)
	}
	prs, _, err := a.List(context.Background(), tracker.Query{Type: "PullRequest", Text: "#3"}, "")
	if err != nil || len(prs) != 1 || prs[0].Fields["head_sha"] != "sha500" {
		t.Fatalf("prs: %+v %v", prs, err)
	}
}

func TestChangesCursor(t *testing.T) {
	gh := fake(t)
	t0 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	now := t0
	gh.SetClock(func() time.Time { return now })
	gh.AddIssue(1, "a", nil, nil, "dana")
	now = t0.Add(time.Hour)
	gh.AddIssue(2, "b", nil, nil, "dana")
	a := newAdapter(t, gh, nil)
	refs, next, err := a.Changes(context.Background(), "")
	if err != nil || len(refs) != 2 || next != "2026-10-19T13:00:00Z" {
		t.Fatalf("first: %v %q %v", refs, next, err)
	}
	// The entity at the cursor was seen by the last call.
	refs, next2, err := a.Changes(context.Background(), next)
	if err != nil || len(refs) != 0 || next2 != next {
		t.Fatalf("since: %v %q %v", refs, next2, err)
	}
	now = t0.Add(2 * time.Hour)
	gh.AddIssue(3, "c", nil, nil, "dana")
	refs, next3, err := a.Changes(context.Background(), next)
	if err != nil || len(refs) != 1 || refs[0].ID != "3" || next3 != "2026-10-19T14:00:00Z" {
		t.Fatalf("after a change: %v %q %v", refs, next3, err)
	}
}

func TestDecisionsFoldInlineComments(t *testing.T) {
	gh := fake(t)
	gh.AddPull(12, "Port", "C4-Job: work/gh-7", "abc", "lee")
	t0 := time.Now().Add(-time.Hour)
	gh.SetClock(func() time.Time { return t0 })
	gh.AddReview(12, "dana", "COMMENTED", "looks close")
	gh.SetClock(func() time.Time { return t0.Add(10 * time.Minute) })
	gh.AddReview(12, "dana", "CHANGES_REQUESTED", "Two things.",
		githubtest.ReviewComment{Path: "tier/fanin_test.go", Line: 42, Body: "assert the order, not just the count"},
		githubtest.ReviewComment{Path: "tier/fanin.go", Body: "name the constant"})
	a := newAdapter(t, gh, nil)
	ds, err := a.Decisions(context.Background(), tracker.Ref{Type: "PullRequest", ID: "12"}, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("want the one review after since, got %d", len(ds))
	}
	d := ds[0]
	want := "Two things.\ntier/fanin_test.go:42: assert the order, not just the count\ntier/fanin.go: name the constant"
	if d.Native != "CHANGES_REQUESTED" || d.By.ID != "dana" || d.Body != want || !strings.HasPrefix(d.Source.ID, "12#") {
		t.Fatalf("decision: %+v", d)
	}
	if !bytes.Contains(d.Evidence, []byte(`"CHANGES_REQUESTED"`)) || !bytes.Contains(d.Evidence, []byte("assert the order")) {
		t.Fatalf("evidence: %s", d.Evidence)
	}
}

func TestReceiveVerifiesSignature(t *testing.T) {
	gh := fake(t)
	gh.AddPull(12, "Port", "", "abc", "lee")
	r := gh.AddReview(12, "dana", "APPROVED", "ship it")
	secret := []byte("s3cret")
	a := newAdapter(t, gh, secret)
	var got []tracker.Delivery
	var errs []error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		d, err := a.Receive(req)
		got, errs = append(got, d), append(errs, err)
	}))
	defer srv.Close()
	payload := gh.ReviewPayload(12, r)
	if _, _, err := githubtest.Deliver(srv.URL, "pull_request_review", payload, secret); err != nil {
		t.Fatal(err)
	}
	if _, _, err := githubtest.Deliver(srv.URL, "pull_request_review", payload, []byte("wrong")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := githubtest.Deliver(srv.URL, "pull_request_review", payload, nil); err != nil {
		t.Fatal(err)
	}
	if errs[0] != nil || len(got[0].Refs) != 1 || got[0].Refs[0] != (tracker.Ref{Type: "PullRequest", ID: "12", URL: got[0].Refs[0].URL}) || got[0].ID == "" {
		t.Fatalf("good delivery: %+v %v", got[0], errs[0])
	}
	for i := 1; i < 3; i++ {
		if !errors.Is(errs[i], tracker.ErrBadSignature) {
			t.Fatalf("delivery %d: want a bad signature, got %v", i, errs[i])
		}
	}
}

func TestReceiveCheckAction(t *testing.T) {
	gh := fake(t)
	secret := []byte("hook")
	a := newAdapter(t, gh, secret)
	ref, err := a.Apply(context.Background(), tracker.Write{Kind: "check", Fields: map[string]string{
		"name": "C4", "head_sha": "abc", "status": "in_progress", "title": "Waiting for review", "summary": "s",
		"actions": "approve=Approve;rework=Rework", "external_id": "work/gh-7"}})
	if err != nil {
		t.Fatal(err)
	}
	var got tracker.Delivery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { got, _ = a.Receive(req) }))
	defer srv.Close()
	id := int64(0)
	for _, c := range gh.CheckRuns("abc") {
		id = c.ID
	}
	if _, _, err := githubtest.Deliver(srv.URL, "check_run", gh.CheckActionPayload(id, "rework", "dana"), secret); err != nil {
		t.Fatal(err)
	}
	if len(got.Decisions) != 1 || got.Decisions[0].Native != "action:rework" || got.Decisions[0].On.ID != ref.ID || got.Decisions[0].By.ID != "dana" {
		t.Fatalf("action: %+v", got)
	}

	// Without a webhook secret nothing proves who clicked: the delivery is
	// no decision, so no one who can reach the endpoint approves as dana.
	open := newAdapter(t, gh, nil)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { got, _ = open.Receive(req) }))
	defer srv2.Close()
	if _, _, err := githubtest.Deliver(srv2.URL, "check_run", gh.CheckActionPayload(id, "approve", "dana"), nil); err != nil {
		t.Fatal(err)
	}
	if len(got.Decisions) != 0 {
		t.Fatalf("an unsigned delivery decided: %+v", got.Decisions)
	}
}

func TestReceiveEcho(t *testing.T) {
	gh := fake(t)
	gh.AddIssue(7, "x", nil, nil, "dana")
	a := newAdapter(t, gh, nil)
	var got tracker.Delivery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { got, _ = a.Receive(req) }))
	defer srv.Close()
	if _, _, err := githubtest.Deliver(srv.URL, "issues", gh.IssuePayload(7, "edited"), nil); err != nil {
		t.Fatal(err)
	}
	if !got.Self {
		t.Fatal("a delivery sent by the app itself is an echo")
	}
}

func TestCommentProbeFindsEarlierWrite(t *testing.T) {
	gh := fake(t)
	gh.AddIssue(7, "x", nil, nil, "dana")
	a := newAdapter(t, gh, nil)
	ctx := context.Background()
	w := tracker.Write{On: tracker.Ref{Type: "Issue", ID: "7"}, Kind: "comment", Note: &tracker.Note{Body: "hello"}, Key: "k1"}
	if done, _, err := a.Probe(ctx, w); err != nil || done {
		t.Fatalf("probe before: %v %v", done, err)
	}
	ref, err := a.Apply(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	done, found, err := a.Probe(ctx, w)
	if err != nil || !done || found.ID != ref.ID {
		t.Fatalf("probe after: %v %v %v", done, found, err)
	}
	// An edit in place keeps one comment.
	if _, err := a.Apply(ctx, tracker.Write{Kind: "comment", Note: &tracker.Note{Body: "edited", Replace: &ref}, Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	cs := gh.Comments(7)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Body, "edited") {
		t.Fatalf("comments: %+v", cs)
	}
}

func TestCheckRunValidationAndClipping(t *testing.T) {
	gh := fake(t)
	a := newAdapter(t, gh, nil)
	ctx := context.Background()
	long := strings.Repeat("é", 300)
	ref, err := a.Apply(ctx, tracker.Write{Kind: "check", Fields: map[string]string{
		"name": "C4", "head_sha": "abc", "status": "in_progress", "title": long, "summary": "s",
		"actions": "approve=An overly long label here;rework=Rework;accept=Accept;fail=Fail"}})
	if err != nil {
		t.Fatalf("clipped fields should pass GitHub's validation: %v", err)
	}
	cr := gh.CheckRuns("abc")[0]
	if len(cr.Output.Title) > 255 || len(cr.Actions) != 3 || len(cr.Actions[0].Label) > 20 {
		t.Fatalf("not clipped: %d %+v", len(cr.Output.Title), cr.Actions)
	}
	// action_required needs details_url, as GitHub requires.
	if _, err := a.Apply(ctx, tracker.Write{Kind: "check", Fields: map[string]string{"id": ref.ID, "conclusion": "action_required"}}); err == nil {
		t.Fatal("action_required without details_url should be refused")
	}
	if _, err := a.Apply(ctx, tracker.Write{Kind: "check", Fields: map[string]string{"id": ref.ID, "conclusion": "action_required", "details_url": "https://hud/x"}}); err != nil {
		t.Fatal(err)
	}
	if c, _ := gh.CheckRun(mustInt(ref.ID)); c.Status != "completed" || *c.Conclusion != "action_required" {
		t.Fatalf("check: %+v", c)
	}
}

func TestStatusProbe(t *testing.T) {
	gh := fake(t)
	a := newAdapter(t, gh, nil)
	ctx := context.Background()
	w := tracker.Write{Kind: "status", Fields: map[string]string{"sha": "abc", "state": "pending", "context": "c4/x", "description": "Running"}}
	if done, _, _ := a.Probe(ctx, w); done {
		t.Fatal("no status yet")
	}
	if _, err := a.Apply(ctx, w); err != nil {
		t.Fatal(err)
	}
	if done, _, _ := a.Probe(ctx, w); !done {
		t.Fatal("the same status is already there")
	}
	w.Fields["state"] = "success"
	if done, _, _ := a.Probe(ctx, w); done {
		t.Fatal("a different state is not")
	}
}

func TestRateLimited(t *testing.T) {
	gh := fake(t)
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	gh.SetRateRemaining(0, reset)
	a := newAdapter(t, gh, nil)
	_, err := a.Get(context.Background(), tracker.Ref{Type: "Issue", ID: "1"})
	var rl *tracker.RateLimited
	if !errors.As(err, &rl) || !rl.Until.Equal(reset) {
		t.Fatalf("want rate limited until %v, got %v", reset, err)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
	}))
	defer srv.Close()
	a, _ := github.New(tracker.Config{Endpoint: srv.URL, Scope: map[string]string{"repo": "a/b"}, Now: func() time.Time { return now }})
	_, err := a.Get(context.Background(), tracker.Ref{Type: "Issue", ID: "1"})
	var rl *tracker.RateLimited
	if !errors.As(err, &rl) || !rl.Until.Equal(now.Add(30*time.Second)) {
		t.Fatalf("secondary limit: %v", err)
	}
}

func TestLinkOffHostRefused(t *testing.T) {
	gh := fake(t)
	a := newAdapter(t, gh, nil)
	if _, _, err := a.List(context.Background(), tracker.Query{Type: "Issue"}, "https://evil.example/repos/acme/widgets/issues?page=2"); err == nil {
		t.Fatal("a next link to another host must be refused: it would carry the token")
	}
}

func TestPublishOnce(t *testing.T) {
	gh := fake(t)
	gh.AddPull(12, "Port", "", "abc", "lee")
	a := newAdapter(t, gh, nil)
	out := tracker.Output{Name: "work/gh-7", View: "c4abc", Record: "c4rec", Custody: "https://hud/x/provenance"}
	r1, err := a.Publish(context.Background(), tracker.Ref{Type: "PullRequest", ID: "12"}, out, "pub1")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Publish(context.Background(), tracker.Ref{Type: "PullRequest", ID: "12"}, out, "pub1")
	if err != nil || r2.ID != r1.ID || len(gh.Comments(12)) != 1 {
		t.Fatalf("publish twice: %v %v %d", r1, r2, len(gh.Comments(12)))
	}
	if b := gh.Comments(12)[0].Body; !strings.Contains(b, "`c4abc`") || !strings.Contains(b, "https://hud/x/provenance") {
		t.Fatalf("body: %s", b)
	}
}

func mustInt(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}
