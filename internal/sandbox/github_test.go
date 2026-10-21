package sandbox

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker/github/githubtest"
)

// fakeHUD answers the two questions the page asks: a job, and whether a
// person has granted the Tracker.
type fakeHUD struct {
	mu      sync.Mutex
	jobs    map[string]api.Job
	granted map[string]bool
}

func (f *fakeHUD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/jobs/saltmarsh/"):
		j, ok := f.jobs[strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/saltmarsh/")]
		if !ok || r.URL.Query().Get("as") != "wrangler" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(j)
	case r.URL.Path == "/api/v1/trackers":
		t := api.Tracker{Namespace: "saltmarsh", Name: "sandbox", Project: "saltmarsh", Kind: "github", Ready: true}
		if f.granted[r.URL.Query().Get("as")] {
			t.Mine = &api.Grant{Person: r.URL.Query().Get("as"), Active: true}
		}
		_ = json.NewEncoder(w).Encode(api.TrackerList{Trackers: []api.Tracker{t}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeHUD) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// hooks receives deliveries and checks their signatures.
type hooks struct {
	got chan string
}

func newHooks() *hooks { return &hooks{got: make(chan string, 32)} }

func (h *hooks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	signed := r.Header.Get("X-Hub-Signature-256") == githubtest.Sign(body, []byte("hook-secret"))
	var p struct {
		Action string `json:"action"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	_ = json.Unmarshal(body, &p)
	if !signed {
		p.Action += " (badly signed)"
	}
	h.got <- r.Header.Get("X-GitHub-Event") + " " + p.Action + " by " + p.Sender.Login
}

// wait waits for n deliveries, with a deadline.
func (h *hooks) wait(t *testing.T, n int) []string {
	t.Helper()
	var got []string
	deadline := time.After(5 * time.Second)
	for len(got) < n {
		select {
		case d := <-h.got:
			got = append(got, d)
		case <-deadline:
			t.Fatalf("deliveries %q, want %d", got, n)
		}
	}
	return got
}

type env struct {
	t    *testing.T
	hud  *fakeHUD
	hook *hooks
	srv  *httptest.Server
}

func setup(t *testing.T) *env {
	hud := &fakeHUD{jobs: map[string]api.Job{}, granted: map[string]bool{}}
	hs := httptest.NewServer(hud)
	t.Cleanup(hs.Close)
	hk := newHooks()
	ks := httptest.NewServer(hk)
	t.Cleanup(ks.Close)
	g := New(Config{Owner: "petrel-finch", Repo: "saltmarsh", Token: "tok", URL: "http://sandbox", Hook: ks.URL, HookSecret: "hook-secret",
		HUD: hs.URL, Project: "saltmarsh", Namespace: "saltmarsh", Tracker: "sandbox",
		People: []Person{{Login: "artist", Person: "artist"}, {Login: "supervisor", Person: "supervisor"}}})
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, hud: hud, hook: hk, srv: srv}
}

func (e *env) post(path string, in any) done {
	e.t.Helper()
	b, _ := json.Marshal(in)
	resp, err := http.Post(e.srv.URL+Prefix+path, "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var d struct {
		done
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil || d.Error != "" {
		e.t.Fatalf("%s: %v %s", path, err, d.Error)
	}
	return d.done
}

func (e *env) page(as string) string {
	e.t.Helper()
	resp, err := http.Get(e.srv.URL + Prefix + "?live=1&as=" + as)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func says(t *testing.T, page string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(page, w) {
			t.Fatalf("the page does not say %q:\n%s", w, page)
		}
	}
}

// The walk: an issue before the assignee's grant says how to give it; after
// it, that it will become a job; then the job, the agent's pull request
// naming it, and reviews that say whether they serve the job's review.
func TestTheWalk(t *testing.T) {
	e := setup(t)
	d := e.post("issues", map[string]any{"as": "supervisor", "title": "Light sm0150", "assignee": "artist", "labels": []string{"c4"}})
	if d.Number != 1 || d.Message != "Issue #1 will become a job in saltmarsh for artist; watch it in the HUD." {
		t.Fatalf("new issue: %+v", d)
	}
	says(t, e.page("supervisor"), "c4 hasn&#39;t been allowed to submit work for artist yet: allow it on your page in the HUD",
		`href="/?as=artist#/me"`)

	e.hud.set(func() { e.hud.granted["artist"] = true })
	says(t, e.page("supervisor"), "Issue #1 will become a job in saltmarsh for artist;")

	e.hud.set(func() { e.hud.jobs["sandbox-1"] = api.Job{Namespace: "saltmarsh", Name: "sandbox-1", Phase: "Running"} })
	says(t, e.page("supervisor"), "Issue #1 is job sandbox-1 in saltmarsh for artist: running.",
		`href="/?as=supervisor#/job/saltmarsh/sandbox-1"`, "Open the pull request")

	d = e.post("pulls", map[string]any{"as": "supervisor", "issue": 1})
	if d.Number != 2 {
		t.Fatalf("pull: %+v", d)
	}
	page := e.page("supervisor")
	says(t, page, "C4-Job: saltmarsh/sandbox-1", "Closes #1.", "The job&#39;s review opens once sandbox-1&#39;s work is done")
	if strings.Contains(page, `data-act=approve`) {
		t.Fatal("review buttons before the review is open")
	}

	e.hud.set(func() {
		e.hud.jobs["sandbox-1"] = api.Job{Namespace: "saltmarsh", Name: "sandbox-1", Phase: "Running",
			Waits: []api.Wait{{Name: "@review", WaitsFor: "person:supervisor"}}}
	})
	says(t, e.page("artist"), "up for review by supervisor", "You are acting as artist, so your review would not serve it; act as supervisor.")
	d = e.post("reviews", map[string]any{"as": "artist", "pull": 2, "state": "approve"})
	says(t, d.Message, "Approved as artist, but the review on sandbox-1 waits for supervisor, so this does not serve it.")
	d = e.post("reviews", map[string]any{"as": "supervisor", "pull": 2, "state": "approve"})
	says(t, d.Message, "Approved. This serves the review wait on sandbox-1")

	got := e.hook.wait(t, 4)
	for _, w := range []string{"issues opened by supervisor", "pull_request opened by c4-agent", "pull_request_review submitted by artist", "pull_request_review submitted by supervisor"} {
		if !strings.Contains(strings.Join(got, "\n"), w) {
			t.Fatalf("no %q in %q", w, got)
		}
	}
}

// A rework makes the next version; the page follows the issue to it.
func TestFollowsTheNextVersion(t *testing.T) {
	e := setup(t)
	e.post("issues", map[string]any{"as": "artist", "title": "x", "assignee": "artist", "labels": []string{"c4"}})
	e.hud.set(func() {
		e.hud.jobs["sandbox-1"] = api.Job{Namespace: "saltmarsh", Name: "sandbox-1", Phase: "Succeeded"}
		e.hud.jobs["sandbox-1-v002"] = api.Job{Namespace: "saltmarsh", Name: "sandbox-1-v002", Phase: "Pending"}
	})
	says(t, e.page("artist"), "Issue #1 is job sandbox-1-v002 in saltmarsh for artist: queued.")
}

// Unlabeled, an issue is left alone, and the page says so.
func TestUnlabeled(t *testing.T) {
	e := setup(t)
	d := e.post("issues", map[string]any{"as": "artist", "title": "x", "assignee": "artist"})
	says(t, d.Message, "Issue #1 is not labeled c4, so c4 leaves it alone.")
	says(t, e.page("artist"), "Not labeled c4, so c4 leaves it alone.")
}

// The actions take JSON only, from someone the sandbox knows; the API
// takes c4-sync's token only.
func TestRefusals(t *testing.T) {
	e := setup(t)
	resp, err := http.Post(e.srv.URL+Prefix+"issues", "application/x-www-form-urlencoded", strings.NewReader("as=artist&title=x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("a form post: %d", resp.StatusCode)
	}
	b, _ := json.Marshal(map[string]any{"as": "mallory", "title": "x"})
	resp, err = http.Post(e.srv.URL+Prefix+"issues", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a stranger: %d", resp.StatusCode)
	}
	resp, err = http.Get(e.srv.URL + "/repos/petrel-finch/saltmarsh/issues")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the API without the token: %d", resp.StatusCode)
	}
	if d := e.post("issues", map[string]any{"as": "artist", "title": "x", "assignee": "artist", "labels": []string{"c4"}}); d.Number != 1 {
		t.Fatalf("%+v", d)
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/repos/petrel-finch/saltmarsh/issues", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var is []githubtest.Issue
	if err := json.NewDecoder(resp.Body).Decode(&is); err != nil || len(is) != 1 || is[0].HTMLURL != "/sandbox/issues/1" || is[0].Assignees[0].Login != "artist" {
		t.Fatalf("issues %+v %v", is, err)
	}
}
