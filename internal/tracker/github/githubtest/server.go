// Package githubtest is a fake GitHub REST API and webhook sender for
// testing c4-sync offline. It serves the subset c4-sync uses, in the shapes
// GitHub documents (docs.github.com/rest: issues, issue comments, pulls,
// reviews, review comments, check runs, commit statuses, users; and the
// webhook headers and payloads for issues, pull_request,
// pull_request_review and check_run). It keeps everything in memory and
// records every write, so tests can assert that nothing was written twice.
package githubtest

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// User is a GitHub account, as REST returns it (abridged).
type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// Milestone is a repository milestone.
type Milestone struct {
	Number  int        `json:"number"`
	Title   string     `json:"title"`
	State   string     `json:"state"`
	DueOn   *time.Time `json:"due_on"`
	HTMLURL string     `json:"html_url"`
}

// Label is an issue label.
type Label struct {
	Name string `json:"name"`
}

// Issue is an issue or, with PullRequest set, a pull request seen through
// the issues API.
type Issue struct {
	Number      int            `json:"number"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	State       string         `json:"state"`
	User        User           `json:"user"`
	Labels      []Label        `json:"labels"`
	Assignees   []User         `json:"assignees"`
	Milestone   *Milestone     `json:"milestone"`
	HTMLURL     string         `json:"html_url"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	PullRequest map[string]any `json:"pull_request,omitempty"`
}

// Branch is a pull request's head or base.
type Branch struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// Pull is a pull request (abridged).
type Pull struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	User      User      `json:"user"`
	Head      Branch    `json:"head"`
	Base      Branch    `json:"base"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Review is a pull request review. REST gives State in upper case
// (APPROVED, CHANGES_REQUESTED, COMMENTED); webhooks in lower case.
type Review struct {
	ID          int64     `json:"id"`
	User        User      `json:"user"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	HTMLURL     string    `json:"html_url"`
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// ReviewComment is an inline comment of a review.
type ReviewComment struct {
	ID       int64  `json:"id"`
	ReviewID int64  `json:"pull_request_review_id"`
	Body     string `json:"body"`
	Path     string `json:"path"`
	Line     int    `json:"line,omitempty"`
	User     User   `json:"user"`
}

// Comment is an issue comment.
type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CheckOutput is a check run's output.
type CheckOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Text    string `json:"text,omitempty"`
}

// CheckAction is a requested-action button on a check run.
type CheckAction struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Identifier  string `json:"identifier"`
}

// CheckRun is a check run.
type CheckRun struct {
	ID          int64         `json:"id"`
	Name        string        `json:"name"`
	HeadSHA     string        `json:"head_sha"`
	Status      string        `json:"status"`
	Conclusion  *string       `json:"conclusion"`
	DetailsURL  string        `json:"details_url,omitempty"`
	ExternalID  string        `json:"external_id,omitempty"`
	Output      CheckOutput   `json:"output"`
	Actions     []CheckAction `json:"actions,omitempty"`
	HTMLURL     string        `json:"html_url"`
	StartedAt   *time.Time    `json:"started_at,omitempty"`
	CompletedAt *time.Time    `json:"completed_at,omitempty"`
}

// Status is a commit status.
type Status struct {
	ID          int64     `json:"id"`
	State       string    `json:"state"`
	Description string    `json:"description"`
	TargetURL   string    `json:"target_url"`
	Context     string    `json:"context"`
	CreatedAt   time.Time `json:"created_at"`
}

// Call is one request the fake served.
type Call struct {
	Method string
	Path   string
	Body   string
}

// Server is the fake. Its fields are guarded by mu; use the methods.
type Server struct {
	*httptest.Server
	Owner, Repo string
	// Token is the only credential accepted. Empty accepts any.
	Token string
	// App is the identity writes are made as (a GitHub App's bot user).
	App User

	mu        sync.Mutex
	now       func() time.Time
	nextID    int64
	users     map[string]User
	issues    map[int]*Issue
	pulls     map[int]*Pull
	reviews   map[int][]*Review
	rcomments map[int][]*ReviewComment
	comments  map[int][]*Comment
	checks    []*CheckRun
	statuses  map[string][]*Status
	calls     []Call
	// remaining is the primary rate-limit budget; at zero every call is
	// refused as GitHub does (403 with x-ratelimit-remaining: 0).
	remaining int
	limit     int
	reset     time.Time
}

// New starts a fake for owner/repo on a random local port.
func New(owner, repo string) *Server {
	s := Unstarted(owner, repo)
	s.Server.Start()
	return s
}

// Unstarted makes a fake that does not listen: serve Handler() yourself
// (hack/demo/fakegithub runs it as a service), and set URL to the address
// clients reach it at, for the links it writes.
func Unstarted(owner, repo string) *Server {
	s := &Server{
		Owner: owner, Repo: repo,
		App:       User{Login: "c4-sync[bot]", ID: 900, Type: "Bot"},
		now:       time.Now,
		nextID:    1000,
		users:     map[string]User{},
		issues:    map[int]*Issue{},
		pulls:     map[int]*Pull{},
		reviews:   map[int][]*Review{},
		rcomments: map[int][]*ReviewComment{},
		comments:  map[int][]*Comment{},
		statuses:  map[string][]*Status{},
		limit:     5000,
		remaining: 5000,
	}
	s.users[s.App.Login] = s.App
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	return s
}

// Handler is the fake's REST API, to mount beside other routes.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

// AddLabel gives an issue a label, as a person does.
func (s *Server) AddLabel(number int, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is := s.issues[number]; is != nil {
		is.Labels = append(is.Labels, Label{Name: label})
		is.UpdatedAt = s.now()
	}
}

// Issues returns the issues and pull requests, by number.
func (s *Server) Issues() []Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Issue
	for _, n := range sortedKeys(s.issues) {
		out = append(out, *s.issues[n])
	}
	return out
}

// Pull returns a pull request and its reviews.
func (s *Server) Pull(number int) (Pull, []Review, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pulls[number]
	if p == nil {
		return Pull{}, nil, false
	}
	var rs []Review
	for _, r := range s.reviews[number] {
		rs = append(rs, *r)
	}
	return *p, rs, true
}

// SetClock replaces time.Now for timestamps the fake assigns.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

func (s *Server) id() int64 { s.nextID++; return s.nextID }

func (s *Server) html(p string) string {
	return fmt.Sprintf("https://github.com/%s/%s/%s", s.Owner, s.Repo, p)
}

// AddUser registers a user.
func (s *Server) AddUser(login, name, email string) User {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := User{Login: login, ID: s.id(), Type: "User", Name: name, Email: email}
	s.users[login] = u
	return u
}

// AddIssue adds an issue with labels and an optional milestone due date.
func (s *Server) AddIssue(number int, title string, labels []string, due *time.Time, author string) *Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	is := &Issue{Number: number, Title: title, State: "open", User: s.users[author],
		HTMLURL: s.html(fmt.Sprintf("issues/%d", number)), CreatedAt: now, UpdatedAt: now}
	for _, l := range labels {
		is.Labels = append(is.Labels, Label{Name: l})
	}
	if due != nil {
		d := due.UTC()
		is.Milestone = &Milestone{Number: 1, Title: "M1", State: "open", DueOn: &d, HTMLURL: s.html("milestone/1")}
	}
	s.issues[number] = is
	return is
}

// SetMilestoneDue moves the due date of every issue's milestone 1.
func (s *Server) SetMilestoneDue(due time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := due.UTC()
	for _, is := range s.issues {
		if is.Milestone != nil {
			is.Milestone.DueOn = &d
			is.UpdatedAt = s.now()
		}
	}
}

// AddPull opens a pull request. It also appears in the issues API, as on
// GitHub.
func (s *Server) AddPull(number int, title, body, headSHA, author string) *Pull {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	p := &Pull{Number: number, Title: title, Body: body, State: "open", User: s.users[author],
		Head: Branch{Ref: "feature", SHA: headSHA}, Base: Branch{Ref: "main", SHA: "base0"},
		HTMLURL: s.html(fmt.Sprintf("pull/%d", number)), CreatedAt: now, UpdatedAt: now}
	s.pulls[number] = p
	s.issues[number] = &Issue{Number: number, Title: title, Body: body, State: "open", User: p.User,
		HTMLURL: p.HTMLURL, CreatedAt: now, UpdatedAt: now,
		PullRequest: map[string]any{"url": s.URL + fmt.Sprintf("/repos/%s/%s/pulls/%d", s.Owner, s.Repo, number)}}
	return p
}

// Push moves a pull request's head to a new commit, as a push does.
func (s *Server) Push(pull int, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pulls[pull].Head.SHA, s.pulls[pull].UpdatedAt = sha, now
	s.issues[pull].UpdatedAt = now
}

// AddReview submits a review: state is APPROVED, CHANGES_REQUESTED or
// COMMENTED. Inline comments are attached to it.
func (s *Server) AddReview(pull int, login, state, body string, inline ...ReviewComment) *Review {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &Review{ID: s.id(), User: s.users[login], Body: body, State: state,
		HTMLURL:     s.html(fmt.Sprintf("pull/%d#pullrequestreview-%d", pull, s.nextID)),
		SubmittedAt: s.now()}
	if p := s.pulls[pull]; p != nil {
		r.CommitID = p.Head.SHA
		p.UpdatedAt = r.SubmittedAt
		s.issues[pull].UpdatedAt = r.SubmittedAt
	}
	s.reviews[pull] = append(s.reviews[pull], r)
	for _, c := range inline {
		c.ID, c.ReviewID, c.User = s.id(), r.ID, s.users[login]
		cc := c
		s.rcomments[pull] = append(s.rcomments[pull], &cc)
	}
	return r
}

// SetRateRemaining sets the primary rate-limit budget and its reset time.
func (s *Server) SetRateRemaining(n int, reset time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remaining, s.reset = n, reset
}

// Calls returns the requests served, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Writes returns the non-GET requests served.
func (s *Server) Writes() []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.Method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// CheckRuns returns the check runs on a commit, oldest first.
func (s *Server) CheckRuns(sha string) []CheckRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CheckRun
	for _, c := range s.checks {
		if c.HeadSHA == sha {
			out = append(out, *c)
		}
	}
	return out
}

// Statuses returns the commit statuses on a commit, oldest first.
func (s *Server) Statuses(sha string) []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Status
	for _, st := range s.statuses[sha] {
		out = append(out, *st)
	}
	return out
}

// Comments returns an issue's comments.
func (s *Server) Comments(number int) []Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Comment
	for _, c := range s.comments[number] {
		out = append(out, *c)
	}
	return out
}

// CheckRun returns one check run by id.
func (s *Server) CheckRun(id int64) (CheckRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.checks {
		if c.ID == id {
			return *c, true
		}
	}
	return CheckRun{}, false
}

// --- webhooks ---

// Deliver posts a webhook to url as GitHub does: JSON body, X-GitHub-Event,
// X-GitHub-Delivery (a GUID) and, with a secret, X-Hub-Signature-256
// (HMAC-SHA256 of the body, hex, prefixed "sha256="). It returns the
// delivery id and the receiver's status.
func Deliver(url, event string, payload any, secret []byte) (string, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", 0, err
	}
	return DeliverRaw(url, event, body, secret, "")
}

// DeliverRaw posts body as is, with deliveryID ("" makes one up).
func DeliverRaw(url, event string, body, secret []byte, deliveryID string) (string, int, error) {
	if deliveryID == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		deliveryID = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GitHub-Hookshot/fake")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	if secret != nil {
		req.Header.Set("X-Hub-Signature-256", Sign(body, secret))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return deliveryID, 0, err
	}
	resp.Body.Close()
	return deliveryID, resp.StatusCode, nil
}

// Sign is GitHub's X-Hub-Signature-256 value for body.
func Sign(body, secret []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// ReviewPayload is a pull_request_review "submitted" webhook payload. The
// review state is lower case in webhooks.
func (s *Server) ReviewPayload(pull int, r *Review) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	rv := *r
	rv.State = strings.ToLower(rv.State)
	return map[string]any{
		"action":       "submitted",
		"review":       rv,
		"pull_request": s.pulls[pull],
		"repository":   s.repoObj(),
		"sender":       r.User,
	}
}

// CheckActionPayload is a check_run "requested_action" webhook payload.
func (s *Server) CheckActionPayload(checkID int64, identifier, login string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cr *CheckRun
	for _, c := range s.checks {
		if c.ID == checkID {
			cr = c
		}
	}
	return map[string]any{
		"action":           "requested_action",
		"check_run":        cr,
		"requested_action": map[string]string{"identifier": identifier},
		"repository":       s.repoObj(),
		"sender":           s.users[login],
	}
}

// IssuePayload is an issues webhook payload (action labeled, milestoned...).
func (s *Server) IssuePayload(number int, action string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"action": action, "issue": s.issues[number], "repository": s.repoObj(), "sender": s.App}
}

func (s *Server) repoObj() map[string]any {
	return map[string]any{"full_name": s.Owner + "/" + s.Repo, "name": s.Repo, "owner": map[string]string{"login": s.Owner}}
}

// --- the REST API ---

type apiErr struct {
	status int
	msg    string
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.RequestURI(), Body: string(body)})

	if s.Token != "" {
		a := r.Header.Get("Authorization")
		if a != "Bearer "+s.Token && a != "token "+s.Token {
			writeErr(w, apiErr{http.StatusUnauthorized, "Bad credentials"})
			return
		}
	}
	w.Header().Set("X-GitHub-Api-Version-Selected", "2022-11-28")
	if s.remaining <= 0 {
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(s.limit))
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(s.reset.Unix(), 10))
		writeErr(w, apiErr{http.StatusForbidden, "API rate limit exceeded for installation."})
		return
	}
	s.remaining--
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(s.limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(s.remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(s.reset.Unix(), 10))

	out, aerr := s.route(r, body)
	if aerr != nil {
		writeErr(w, *aerr)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	if pg, ok := out.(paged); ok {
		if pg.next != "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, pg.next))
		}
		out = pg.items
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

type paged struct {
	items any
	next  string
}

func writeErr(w http.ResponseWriter, e apiErr) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message":           e.msg,
		"documentation_url": "https://docs.github.com/rest",
	})
}

func (s *Server) route(r *http.Request, body []byte) (any, *apiErr) {
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	notFound := &apiErr{http.StatusNotFound, "Not Found"}
	if len(p) == 2 && p[0] == "users" && r.Method == http.MethodGet {
		u, ok := s.users[p[1]]
		if !ok {
			return nil, notFound
		}
		return u, nil
	}
	if len(p) < 4 || p[0] != "repos" || p[1] != s.Owner || p[2] != s.Repo {
		return nil, notFound
	}
	rest := p[3:]
	m := r.Method
	switch {
	case rest[0] == "issues" && len(rest) == 1 && m == http.MethodGet:
		return s.listIssues(r)
	case rest[0] == "issues" && len(rest) == 2 && m == http.MethodGet:
		is := s.issues[atoi(rest[1])]
		if is == nil {
			return nil, notFound
		}
		return is, nil
	case rest[0] == "issues" && len(rest) == 3 && rest[2] == "comments":
		n := atoi(rest[1])
		if s.issues[n] == nil {
			return nil, notFound
		}
		if m == http.MethodGet {
			return s.page(r, toAny(s.comments[n]))
		}
		if m == http.MethodPost {
			var in struct{ Body string }
			if err := json.Unmarshal(body, &in); err != nil || in.Body == "" {
				return nil, &apiErr{http.StatusUnprocessableEntity, "Validation Failed: body is required"}
			}
			now := s.now()
			c := &Comment{ID: s.id(), Body: in.Body, User: s.App, CreatedAt: now, UpdatedAt: now}
			c.HTMLURL = s.html(fmt.Sprintf("issues/%d#issuecomment-%d", n, c.ID))
			s.comments[n] = append(s.comments[n], c)
			return c, nil
		}
	case rest[0] == "issues" && len(rest) == 3 && rest[1] == "comments" && m == http.MethodPatch:
		id := int64(atoi(rest[2]))
		var in struct{ Body string }
		if err := json.Unmarshal(body, &in); err != nil || in.Body == "" {
			return nil, &apiErr{http.StatusUnprocessableEntity, "Validation Failed: body is required"}
		}
		for _, cs := range s.comments {
			for _, c := range cs {
				if c.ID == id {
					c.Body, c.UpdatedAt = in.Body, s.now()
					return c, nil
				}
			}
		}
		return nil, notFound
	case rest[0] == "pulls" && len(rest) == 1 && m == http.MethodGet:
		var ps []*Pull
		for _, n := range sortedKeys(s.pulls) {
			if pl := s.pulls[n]; r.URL.Query().Get("state") == "all" || pl.State == "open" {
				ps = append(ps, pl)
			}
		}
		return s.page(r, toAny(ps))
	case rest[0] == "pulls" && len(rest) == 2 && m == http.MethodGet:
		pl := s.pulls[atoi(rest[1])]
		if pl == nil {
			return nil, notFound
		}
		return pl, nil
	case rest[0] == "pulls" && len(rest) == 3 && rest[2] == "reviews" && m == http.MethodGet:
		return s.page(r, toAny(s.reviews[atoi(rest[1])]))
	case rest[0] == "pulls" && len(rest) == 5 && rest[2] == "reviews" && rest[4] == "comments" && m == http.MethodGet:
		n, rid := atoi(rest[1]), int64(atoi(rest[3]))
		var cs []*ReviewComment
		for _, c := range s.rcomments[n] {
			if c.ReviewID == rid {
				cs = append(cs, c)
			}
		}
		return s.page(r, toAny(cs))
	case rest[0] == "check-runs" && len(rest) == 1 && m == http.MethodPost:
		return s.createCheck(body)
	case rest[0] == "check-runs" && len(rest) == 2 && m == http.MethodPatch:
		return s.updateCheck(int64(atoi(rest[1])), body)
	case rest[0] == "commits" && len(rest) == 3 && rest[2] == "check-runs" && m == http.MethodGet:
		name := r.URL.Query().Get("check_name")
		var cs []*CheckRun
		for i := len(s.checks) - 1; i >= 0; i-- { // newest first, as GitHub lists them
			c := s.checks[i]
			if c.HeadSHA == rest[1] && (name == "" || c.Name == name) {
				cs = append(cs, c)
			}
		}
		return map[string]any{"total_count": len(cs), "check_runs": cs}, nil
	case rest[0] == "statuses" && len(rest) == 2 && m == http.MethodPost:
		var in Status
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, &apiErr{http.StatusBadRequest, "Problems parsing JSON"}
		}
		switch in.State {
		case "error", "failure", "pending", "success":
		default:
			return nil, &apiErr{http.StatusUnprocessableEntity, "Validation Failed: state is not included in the list"}
		}
		if in.Context == "" {
			in.Context = "default"
		}
		in.ID, in.CreatedAt = s.id(), s.now()
		st := in
		s.statuses[rest[1]] = append(s.statuses[rest[1]], &st)
		return st, nil
	case rest[0] == "commits" && len(rest) == 3 && rest[2] == "statuses" && m == http.MethodGet:
		var out []*Status
		for i := len(s.statuses[rest[1]]) - 1; i >= 0; i-- {
			out = append(out, s.statuses[rest[1]][i])
		}
		return s.page(r, toAny(out))
	}
	return nil, notFound
}

func (s *Server) listIssues(r *http.Request) (any, *apiErr) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	var want []string
	if l := q.Get("labels"); l != "" {
		want = strings.Split(l, ",")
	}
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, &apiErr{http.StatusUnprocessableEntity, "Validation Failed: since"}
		}
		since = t
	}
	var out []*Issue
	for _, n := range sortedKeys(s.issues) {
		is := s.issues[n]
		if state != "all" && is.State != state {
			continue
		}
		if !since.IsZero() && is.UpdatedAt.Before(since) {
			continue
		}
		if !hasLabels(is, want) {
			continue
		}
		out = append(out, is)
	}
	if q.Get("sort") == "updated" {
		sort.SliceStable(out, func(i, j int) bool {
			if q.Get("direction") == "asc" {
				return out[i].UpdatedAt.Before(out[j].UpdatedAt)
			}
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		})
	}
	return s.page(r, toAny(out))
}

func hasLabels(is *Issue, want []string) bool {
	for _, w := range want {
		found := false
		for _, l := range is.Labels {
			if l.Name == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// page applies per_page and page, and links the next page as GitHub does.
func (s *Server) page(r *http.Request, items []any) (any, *apiErr) {
	per := 30
	if v := r.URL.Query().Get("per_page"); v != "" {
		per = atoi(v)
		if per > 100 {
			per = 100
		}
		if per < 1 {
			per = 30
		}
	}
	pg := 1
	if v := r.URL.Query().Get("page"); v != "" {
		pg = atoi(v)
	}
	from := (pg - 1) * per
	if from > len(items) {
		from = len(items)
	}
	to := from + per
	if to > len(items) {
		to = len(items)
	}
	res := paged{items: items[from:to]}
	if to < len(items) {
		u := *r.URL
		qq := u.Query()
		qq.Set("page", strconv.Itoa(pg+1))
		u.RawQuery = qq.Encode()
		res.next = s.URL + u.RequestURI()
	}
	if res.items == nil || len(items[from:to]) == 0 {
		res.items = []any{}
	}
	return res, nil
}

type checkIn struct {
	Name       *string        `json:"name"`
	HeadSHA    *string        `json:"head_sha"`
	Status     *string        `json:"status"`
	Conclusion *string        `json:"conclusion"`
	DetailsURL *string        `json:"details_url"`
	ExternalID *string        `json:"external_id"`
	Output     *CheckOutput   `json:"output"`
	Actions    *[]CheckAction `json:"actions"`
}

func (s *Server) createCheck(body []byte) (any, *apiErr) {
	var in checkIn
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, &apiErr{http.StatusBadRequest, "Problems parsing JSON"}
	}
	if in.Name == nil || *in.Name == "" || in.HeadSHA == nil || *in.HeadSHA == "" {
		return nil, &apiErr{http.StatusUnprocessableEntity, "Validation Failed: name and head_sha are required"}
	}
	c := &CheckRun{ID: s.id(), Name: *in.Name, HeadSHA: *in.HeadSHA, Status: "queued"}
	c.HTMLURL = s.html(fmt.Sprintf("runs/%d", c.ID))
	if e := s.applyCheck(c, in); e != nil {
		return nil, e
	}
	s.checks = append(s.checks, c)
	return c, nil
}

func (s *Server) updateCheck(id int64, body []byte) (any, *apiErr) {
	var in checkIn
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, &apiErr{http.StatusBadRequest, "Problems parsing JSON"}
	}
	for _, c := range s.checks {
		if c.ID == id {
			if e := s.applyCheck(c, in); e != nil {
				return nil, e
			}
			return c, nil
		}
	}
	return nil, &apiErr{http.StatusNotFound, "Not Found"}
}

// applyCheck validates as GitHub documents: statuses, conclusions, a
// conclusion completes the run, output needs a title and summary, at most
// three actions with labels up to 20 characters, descriptions up to 40
// and identifiers up to 20; external_id is kept as given.
func (s *Server) applyCheck(c *CheckRun, in checkIn) *apiErr {
	bad := func(m string) *apiErr { return &apiErr{http.StatusUnprocessableEntity, "Validation Failed: " + m} }
	if in.Status != nil {
		switch *in.Status {
		case "queued", "in_progress", "completed", "waiting", "requested", "pending":
		default:
			return bad("status")
		}
	}
	if in.Conclusion != nil {
		switch *in.Conclusion {
		case "action_required", "cancelled", "failure", "neutral", "success", "skipped", "stale", "timed_out":
		default:
			return bad("conclusion")
		}
	}
	if in.Status != nil && *in.Status == "completed" && in.Conclusion == nil && c.Conclusion == nil {
		return bad("conclusion is required when status is completed")
	}
	if in.Output != nil && (in.Output.Title == "" || in.Output.Summary == "") {
		return bad("output needs title and summary")
	}
	details := c.DetailsURL
	if in.DetailsURL != nil {
		details = *in.DetailsURL
	}
	if in.Conclusion != nil && *in.Conclusion == "action_required" && details == "" {
		return bad("details_url is required for action_required")
	}
	if in.Actions != nil {
		if len(*in.Actions) > 3 {
			return bad("at most 3 actions")
		}
		for _, a := range *in.Actions {
			if len(a.Label) > 20 || len(a.Description) > 40 || len(a.Identifier) > 20 {
				return bad("action label, description or identifier too long")
			}
		}
		c.Actions = *in.Actions
	}
	now := s.now()
	if in.Status != nil {
		c.Status = *in.Status
		if c.Status == "in_progress" && c.StartedAt == nil {
			c.StartedAt = &now
		}
		if c.Status != "completed" {
			c.Conclusion, c.CompletedAt = nil, nil
		}
	}
	if in.Conclusion != nil {
		v := *in.Conclusion
		c.Conclusion, c.Status, c.CompletedAt = &v, "completed", &now
	}
	if in.DetailsURL != nil {
		c.DetailsURL = *in.DetailsURL
	}
	if in.ExternalID != nil {
		c.ExternalID = *in.ExternalID
	}
	if in.Output != nil {
		c.Output = *in.Output
	}
	return nil
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func sortedKeys[V any](m map[int]V) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}

func toAny[T any](xs []T) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
