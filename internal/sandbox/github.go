// Package sandbox is a stand-in GitHub for the local farm (c4 farm up
// --tracker): a repository on this machine that a newcomer walks with
// c4-sync, from an issue to a job to a review, without an account anywhere
// and without anything leaving the computer.
//
// It is the fake GitHub REST API the tests use
// (internal/tracker/github/githubtest), which c4-sync speaks to as it
// speaks to GitHub, and a page at /sandbox/ (served through the HUD's own
// port) where a person does what they would do on GitHub: act as one of
// the local farm's people, open an issue, play the agent that opens the
// pull request, and approve it or request changes. Each step says, in
// C4's words, what it does to the farm; the page asks the HUD.
package sandbox

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/github/githubtest"
	"github.com/Avalanche-io/c4-operator/internal/tracker/syncer"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// Prefix is where the page is served, on the sandbox and through the HUD.
const Prefix = "/sandbox/"

// Label is the label that makes an issue the farm's work.
const Label = "c4"

// Person is a sandbox login and the local farm's person it stands for.
type Person struct {
	Login, Person, Name string
}

// Config shapes the sandbox.
type Config struct {
	// Owner and Repo name the repository.
	Owner, Repo string
	// Token is the only credential the API takes (c4-sync's).
	Token string
	// URL is where c4-sync reaches the API, for the links it reads.
	URL string
	// Hook is where deliveries go (c4-sync's /webhooks/{ns}/{tracker}),
	// signed with HookSecret. Empty: none, and c4-sync polls.
	Hook, HookSecret string
	// HUD is the HUD's API base as the sandbox reaches it, and HUDAs the
	// viewer it reads jobs as (a development farm's ?as=).
	HUD, HUDAs string
	// Project, Namespace and Tracker are where the Tracker is.
	Project, Namespace, Tracker string
	// People are who a person may act as; Agent is the login the agent's
	// part (opening the pull request) is played as.
	People []Person
	Agent  string
}

// GitHub is the sandbox.
type GitHub struct {
	cfg Config
	gh  *githubtest.Server
	hc  *http.Client

	mu   sync.Mutex
	next int
	// pulls maps an issue to the pull request opened for it.
	pulls map[int]int
}

// New makes the sandbox. Its links point at the page, under Prefix.
func New(cfg Config) *GitHub {
	if cfg.Agent == "" {
		cfg.Agent = "c4-agent"
	}
	if cfg.HUDAs == "" {
		cfg.HUDAs = "wrangler"
	}
	gh := githubtest.Unstarted(cfg.Owner, cfg.Repo)
	gh.Token, gh.URL = cfg.Token, cfg.URL
	gh.HTMLBase = strings.TrimSuffix(Prefix, "/")
	gh.MaxCalls = 1000
	for _, p := range cfg.People {
		gh.AddUser(p.Login, p.Name, "")
	}
	gh.AddUser(cfg.Agent, "the agent (you play it here)", "")
	return &GitHub{cfg: cfg, gh: gh, hc: &http.Client{Timeout: 3 * time.Second}, next: 1, pulls: map[int]int{}}
}

// Handler serves the API (/repos/, /users/), the page (Prefix) and
// /healthz.
func (g *GitHub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/repos/", g.gh.Handler())
	mux.Handle("/users/", g.gh.Handler())
	mux.HandleFunc("GET "+Prefix, g.page)
	mux.HandleFunc("POST "+Prefix+"issues", g.newIssue)
	mux.HandleFunc("POST "+Prefix+"pulls", g.openPull)
	mux.HandleFunc("POST "+Prefix+"reviews", g.review)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}

func (g *GitHub) person(login string) (Person, bool) {
	for _, p := range g.cfg.People {
		if p.Login == login {
			return p, true
		}
	}
	return Person{}, false
}

// --- what people do ---

type issueIn struct {
	As       string   `json:"as"`
	Title    string   `json:"title"`
	Assignee string   `json:"assignee"`
	Labels   []string `json:"labels"`
}

// done answers an action: what it did, in a sentence, and what it made.
type done struct {
	Number  int    `json:"number,omitempty"`
	Message string `json:"message"`
}

// Issue opens an issue as a person does: by as, assigned and labeled.
func (g *GitHub) Issue(in issueIn) (done, error) {
	if _, ok := g.person(in.As); !ok {
		return done{}, fmt.Errorf("no one here is %q", in.As)
	}
	if strings.TrimSpace(in.Title) == "" {
		return done{}, errors.New("an issue needs a title")
	}
	if in.Assignee != "" {
		if _, ok := g.person(in.Assignee); !ok {
			return done{}, fmt.Errorf("no one here is %q to assign", in.Assignee)
		}
	}
	g.mu.Lock()
	n := g.next
	g.next++
	g.mu.Unlock()
	g.gh.AddIssue(n, strings.TrimSpace(in.Title), in.Labels, nil, in.As)
	if in.Assignee != "" {
		g.gh.Assign(n, in.Assignee)
	}
	g.deliver("issues", g.payload(n, "opened", in.As))
	return done{Number: n, Message: g.intakeSays(n, in.Assignee, has(in.Labels, Label))}, nil
}

// intakeSays is what opening issue n does on the farm.
func (g *GitHub) intakeSays(n int, assignee string, labeled bool) string {
	switch {
	case !labeled:
		return fmt.Sprintf("Issue #%d is not labeled %s, so c4 leaves it alone.", n, Label)
	case assignee == "":
		return fmt.Sprintf("Issue #%d has no one assigned: c4 submits the work for the person it is assigned to.", n)
	}
	p, _ := g.person(assignee)
	return fmt.Sprintf("Issue #%d will become a job in %s for %s; watch it in the HUD.", n, g.cfg.Project, p.Person)
}

type pullIn struct {
	As    string `json:"as"`
	Issue int    `json:"issue"`
}

// Pull plays the agent: it opens the pull request for an issue's job,
// naming the job (C4-Job) and closing the issue.
func (g *GitHub) Pull(ctx context.Context, in pullIn) (done, error) {
	is, ok := g.issue(in.Issue)
	if !ok {
		return done{}, fmt.Errorf("no issue #%d", in.Issue)
	}
	j := g.job(ctx, in.Issue)
	if j == nil {
		return done{}, fmt.Errorf("issue #%d has no job yet: the agent opens its pull request once there is work to show", in.Issue)
	}
	g.mu.Lock()
	if p, ok := g.pulls[in.Issue]; ok {
		g.mu.Unlock()
		return done{Number: p, Message: fmt.Sprintf("Pull request #%d is already open for issue #%d.", p, in.Issue)}, nil
	}
	n := g.next
	g.next++
	g.pulls[in.Issue] = n
	g.mu.Unlock()
	body := fmt.Sprintf("Closes #%d.\n\nC4-Job: %s/%s", in.Issue, g.cfg.Namespace, j.Name)
	h := sha1.Sum([]byte(fmt.Sprintf("%s/%s#%d %d", g.cfg.Owner, g.cfg.Repo, n, time.Now().UnixNano())))
	sha := hex.EncodeToString(h[:])
	g.gh.AddPull(n, is.Title, body, sha, g.cfg.Agent)
	g.deliver("pull_request", g.pullPayload(n, "opened"))
	return done{Number: n, Message: fmt.Sprintf("The agent opened pull request #%d for %s. Its review is the job's review: approve it, or request changes, once the job asks.", n, j.Name)}, nil
}

type reviewIn struct {
	As    string `json:"as"`
	Pull  int    `json:"pull"`
	State string `json:"state"` // approve or changes
	Body  string `json:"body,omitempty"`
}

// Review reviews a pull request as a person, and says what it does to
// the job's review wait.
func (g *GitHub) Review(ctx context.Context, in reviewIn) (done, error) {
	p, ok := g.person(in.As)
	if !ok {
		return done{}, fmt.Errorf("no one here is %q", in.As)
	}
	issue := g.issueOf(in.Pull)
	if issue == 0 {
		return done{}, fmt.Errorf("no pull request #%d", in.Pull)
	}
	state := map[string]string{"approve": "APPROVED", "changes": "CHANGES_REQUESTED"}[in.State]
	if state == "" {
		return done{}, fmt.Errorf("a review approves or requests changes, not %q", in.State)
	}
	body := in.Body
	if body == "" && state == "CHANGES_REQUESTED" {
		body = "Changes requested."
	}
	rv := g.gh.AddReview(in.Pull, in.As, state, body)
	g.deliver("pull_request_review", g.gh.ReviewPayload(in.Pull, rv))
	j := g.job(ctx, issue)
	w := reviewWait(j)
	verb := map[string]string{"APPROVED": "Approved", "CHANGES_REQUESTED": "Changes requested"}[state]
	switch {
	case j == nil || w == nil:
		return done{Message: fmt.Sprintf("%s. The job's review is not open yet, so c4 does not take it: review again once the job asks.", verb)}, nil
	case w.WaitsFor != "person:"+p.Person:
		return done{Message: fmt.Sprintf("%s as %s, but the review on %s waits for %s, so this does not serve it. Act as %s to review it.",
			verb, p.Person, j.Name, who(w.WaitsFor), who(w.WaitsFor))}, nil
	case state == "APPROVED":
		return done{Message: fmt.Sprintf("Approved. This serves the review wait on %s: the job is done once c4-sync relays it, in a few seconds.", j.Name)}, nil
	}
	return done{Message: fmt.Sprintf("Changes requested. This serves the review wait on %s: c4 makes the next version, with your review as its input.", j.Name)}, nil
}

func who(server string) string {
	_, id, _ := strings.Cut(server, ":")
	return id
}

// --- deliveries ---

// deliver sends a signed delivery, as GitHub would. It is a hint: c4-sync
// polls under it, so one that does not arrive is logged, not fatal.
func (g *GitHub) deliver(event string, payload any) {
	if g.cfg.Hook == "" {
		return
	}
	go func() {
		if _, code, err := githubtest.Deliver(g.cfg.Hook, event, payload, []byte(g.cfg.HookSecret)); err != nil || code >= 300 {
			log.Printf("delivery %s to %s: %d %v (c4-sync polls under it)", event, g.cfg.Hook, code, err)
		}
	}()
}

// payload is an issues delivery sent by the person who acted, not the
// app: c4-sync drops its own deliveries as echoes.
func (g *GitHub) payload(n int, action, login string) map[string]any {
	m := g.gh.IssuePayload(n, action)
	if u, ok := g.gh.User(login); ok {
		m["sender"] = u
	}
	return m
}

func (g *GitHub) pullPayload(n int, action string) map[string]any {
	p, _, _ := g.gh.Pull(n)
	m := g.gh.IssuePayload(n, action)
	delete(m, "issue")
	m["pull_request"], m["sender"], m["number"] = p, p.User, n
	return m
}

// --- reading the farm ---

func (g *GitHub) issue(n int) (githubtest.Issue, bool) {
	for _, is := range g.gh.Issues() {
		if is.Number == n && is.PullRequest == nil {
			return is, true
		}
	}
	return githubtest.Issue{}, false
}

func (g *GitHub) issueOf(pull int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	for is, p := range g.pulls {
		if p == pull {
			return is
		}
	}
	return 0
}

// hud reads the HUD API as viewer as; a 404 is no answer and no error.
func (g *GitHub) hud(ctx context.Context, path, as string, out any) (bool, error) {
	u := strings.TrimSuffix(g.cfg.HUD, "/") + "/api/v1" + path
	if strings.Contains(u, "?") {
		u += "&as=" + url.QueryEscape(as)
	} else {
		u += "?as=" + url.QueryEscape(as)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("the HUD answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return true, json.NewDecoder(resp.Body).Decode(out)
}

// job is the issue's job as it stands: its latest version (a rework makes
// the next, -v002 and on), or nil before there is one.
func (g *GitHub) job(ctx context.Context, issue int) *api.Job {
	base := syncer.JobName(g.cfg.Tracker, tracker.Item{Ref: tracker.Ref{Type: "Issue", ID: strconv.Itoa(issue)},
		Name: strconv.Itoa(issue), NameUnique: true}) // as the GitHub adapter names an issue
	var last *api.Job
	for v := 1; v < 100; v++ {
		name := base
		if v > 1 {
			name = fmt.Sprintf("%s-v%03d", base, v)
		}
		var j api.Job
		ok, err := g.hud(ctx, "/jobs/"+g.cfg.Namespace+"/"+name, g.cfg.HUDAs, &j)
		if err != nil || !ok {
			break
		}
		last = &j
	}
	return last
}

// granted says whether person has let the Tracker submit work for them.
func (g *GitHub) granted(ctx context.Context, person string) (bool, error) {
	var l api.TrackerList
	if _, err := g.hud(ctx, "/trackers", person, &l); err != nil {
		return false, err
	}
	for _, t := range l.Trackers {
		if t.Namespace == g.cfg.Namespace && t.Name == g.cfg.Tracker {
			return t.Mine != nil && t.Mine.Active, nil
		}
	}
	return false, nil
}

// reviewWait is the job's open review wait, if it has one.
func reviewWait(j *api.Job) *api.Wait {
	if j == nil {
		return nil
	}
	for i := range j.Waits {
		if w := &j.Waits[i]; w.Name == class.ReviewName && w.Do == "" {
			return w
		}
	}
	return nil
}

func has(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
