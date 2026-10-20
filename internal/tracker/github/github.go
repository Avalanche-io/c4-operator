// Package github is c4-sync's GitHub adapter (docs/c4-sync.md). It speaks
// GitHub's REST API v3 and verifies webhook deliveries. It writes only work states: check runs,
// commit statuses and one comment per link, edited in place. Approvals
// stay GitHub's; the adapter reads them back as decisions.
//
// Ref types: Issue and PullRequest (ID: the number), CheckRun and
// Comment (ID: GitHub's id), Commit (ID: the SHA), Review (ID:
// "<pull>#<review id>").
package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/effect"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// Kind is the adapter's registry name.
const Kind = "github"

// DefaultEndpoint is GitHub's public API.
const DefaultEndpoint = "https://api.github.com"

// APIVersion is the REST API version requested.
const APIVersion = "2022-11-28"

// maxDelivery is GitHub's cap on webhook payloads (25 MB).
const maxDelivery = 25 << 20

func init() {
	tracker.Register(Kind, func(c tracker.Config) (tracker.Adapter, error) { return New(c) })
}

// Adapter is the GitHub adapter for one repository.
type Adapter struct {
	c           tracker.Config
	base        string
	owner, repo string
	hc          *http.Client
	now         func() time.Time
}

// New builds the adapter. Scope must name repo: owner/name.
func New(c tracker.Config) (*Adapter, error) {
	full := c.Scope["repo"]
	owner, repo, ok := strings.Cut(full, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("github: scope.repo must be owner/name, got %q", full)
	}
	base := strings.TrimSuffix(c.Endpoint, "/")
	if base == "" {
		base = DefaultEndpoint
	}
	a := &Adapter{c: c, base: base, owner: owner, repo: repo, hc: c.HTTPClient, now: c.Now}
	if a.hc == nil {
		a.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

func (a *Adapter) Kind() string { return Kind }

func (a *Adapter) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{
		Webhooks:         true,
		EditableComments: true,
		ReviewHeads:      true,
		Mentions:         true,
		// GitHub takes no idempotency key on writes: Probe stands in.
		// Milestone due_on is a timestamp; whether GitHub keeps a time of
		// day for it is unverified, so an intake rule
		// may still treat it as a date with dateOnlyAt.
	}
}

// --- HTTP ---

type apiError struct {
	Message string `json:"message"`
}

func (a *Adapter) repoPath(p string) string {
	return fmt.Sprintf("/repos/%s/%s%s", url.PathEscape(a.owner), url.PathEscape(a.repo), p)
}

// do calls the API. path is a path under the endpoint or a full URL (a
// Link header's next page, which must be on the same host). It returns
// the response's Link next URL.
func (a *Adapter) do(ctx context.Context, method, path string, in, out any) (string, error) {
	u := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		u = a.base + path
	} else if !strings.HasPrefix(path, a.base+"/") {
		return "", fmt.Errorf("github: refusing to follow a link off %s: %s", a.base, path)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("User-Agent", "c4-sync")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.c.Token != nil {
		tok, err := a.c.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("github: credential: %w", err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		var e apiError
		_ = json.Unmarshal(data, &e)
		return "", a.classify(resp, e.Message)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return "", fmt.Errorf("github: %s %s: decoding: %w", method, path, err)
		}
	}
	return nextLink(resp.Header.Get("Link")), nil
}

// classify turns a refusal into an error the loop acts on. GitHub signals
// the primary limit with 403 or 429 and x-ratelimit-remaining: 0 (wait
// until x-ratelimit-reset), and secondary limits with retry-after; with
// neither header, it asks clients to wait at least a minute.
func (a *Adapter) classify(resp *http.Response, msg string) error {
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", tracker.ErrNotFound, msg)
	case http.StatusForbidden, http.StatusTooManyRequests:
		if s := resp.Header.Get("Retry-After"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				return &tracker.RateLimited{Until: a.now().Add(time.Duration(n) * time.Second), Msg: msg}
			}
		}
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				return &tracker.RateLimited{Until: time.Unix(n, 0), Msg: msg}
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(msg), "rate limit") {
			return &tracker.RateLimited{Until: a.now().Add(time.Minute), Msg: msg}
		}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return fmt.Errorf("github: %s: %s: %w", resp.Status, msg, tracker.ErrRefused)
	}
	return fmt.Errorf("github: %s: %s", resp.Status, msg)
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(h string) string {
	if m := linkNext.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

// --- GitHub shapes (abridged to what the adapter reads) ---

type ghUser struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Type  string `json:"type"`
}

type ghIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	User      ghUser    `json:"user"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []ghUser `json:"assignees"`
	Milestone *struct {
		Number int        `json:"number"`
		Title  string     `json:"title"`
		DueOn  *time.Time `json:"due_on"`
	} `json:"milestone"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type ghPull struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	User      ghUser    `json:"user"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
	Head      struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
}

type ghReview struct {
	ID          int64     `json:"id"`
	User        ghUser    `json:"user"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	HTMLURL     string    `json:"html_url"`
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
}

type ghReviewComment struct {
	Body string `json:"body"`
	Path string `json:"path"`
	Line int    `json:"line"`
}

type ghComment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    ghUser `json:"user"`
}

type ghCheckRun struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	HeadSHA    string  `json:"head_sha"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
	ExternalID string  `json:"external_id"`
	HTMLURL    string  `json:"html_url"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

type ghStatus struct {
	State       string `json:"state"`
	Description string `json:"description"`
	Context     string `json:"context"`
	TargetURL   string `json:"target_url"`
}

func (is ghIssue) item() tracker.Item {
	t := "Issue"
	if len(is.PullRequest) > 0 && string(is.PullRequest) != "null" {
		t = "PullRequest"
	}
	it := tracker.Item{
		Ref:       tracker.Ref{Type: t, ID: strconv.Itoa(is.Number), URL: is.HTMLURL},
		Title:     is.Title,
		Body:      is.Body,
		Status:    tracker.Status{Native: is.State, Category: category(is.State)},
		UpdatedAt: is.UpdatedAt,
		Version:   is.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Fields:    map[string]string{"author": is.User.Login},
	}
	for _, l := range is.Labels {
		it.Labels = append(it.Labels, l.Name)
	}
	for _, u := range is.Assignees {
		it.Assignees = append(it.Assignees, tracker.Person{ID: u.Login, Name: u.Name})
	}
	if m := is.Milestone; m != nil {
		it.Fields["milestone"] = m.Title
		if m.DueOn != nil {
			it.Due = &tracker.Due{At: m.DueOn.UTC()}
			it.Fields["milestone.due_on"] = m.DueOn.UTC().Format(time.RFC3339)
		}
	}
	return it
}

func category(state string) string {
	if state == "closed" {
		return "done"
	}
	return "ready"
}

// --- reading ---

func (a *Adapter) Get(ctx context.Context, ref tracker.Ref) (tracker.Item, error) {
	switch ref.Type {
	case "Issue":
		var is ghIssue
		if _, err := a.do(ctx, http.MethodGet, a.repoPath("/issues/"+ref.ID), nil, &is); err != nil {
			return tracker.Item{}, err
		}
		return is.item(), nil
	case "PullRequest":
		var p ghPull
		if _, err := a.do(ctx, http.MethodGet, a.repoPath("/pulls/"+ref.ID), nil, &p); err != nil {
			return tracker.Item{}, err
		}
		return pullItem(p), nil
	}
	return tracker.Item{}, fmt.Errorf("github: Get %s: %w", ref.Type, tracker.ErrUnsupported)
}

func pullItem(p ghPull) tracker.Item {
	return tracker.Item{
		Ref:       tracker.Ref{Type: "PullRequest", ID: strconv.Itoa(p.Number), URL: p.HTMLURL},
		Title:     p.Title,
		Body:      p.Body,
		Status:    tracker.Status{Native: p.State, Category: category(p.State)},
		UpdatedAt: p.UpdatedAt,
		Version:   p.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Fields:    map[string]string{"head": p.Head.SHA, "head_sha": p.Head.SHA, "author": p.User.Login},
	}
}

// List pages through issues (Type Issue, by labels and state) or pull
// requests (Type PullRequest, by state; Text keeps those whose body holds
// it). page is "" for the first page, then the returned next.
func (a *Adapter) List(ctx context.Context, q tracker.Query, page string) ([]tracker.Item, string, error) {
	state := q.State
	if state == "" {
		state = "open"
	}
	switch q.Type {
	case "Issue":
		path := page
		if path == "" {
			v := url.Values{"state": {state}, "per_page": {"100"}}
			if len(q.Labels) > 0 {
				v.Set("labels", strings.Join(q.Labels, ","))
			}
			path = a.repoPath("/issues?" + v.Encode())
		}
		var raw []ghIssue
		next, err := a.do(ctx, http.MethodGet, path, nil, &raw)
		if err != nil {
			return nil, "", err
		}
		var out []tracker.Item
		for _, is := range raw {
			it := is.item()
			if it.Ref.Type != "Issue" || (q.Text != "" && !strings.Contains(it.Body, q.Text)) {
				continue
			}
			out = append(out, it)
		}
		return out, next, nil
	case "PullRequest":
		path := page
		if path == "" {
			path = a.repoPath("/pulls?" + url.Values{"state": {state}, "per_page": {"100"}}.Encode())
		}
		var raw []ghPull
		next, err := a.do(ctx, http.MethodGet, path, nil, &raw)
		if err != nil {
			return nil, "", err
		}
		var out []tracker.Item
		for _, p := range raw {
			if q.Text != "" && !strings.Contains(p.Body, q.Text) {
				continue
			}
			out = append(out, pullItem(p))
		}
		return out, next, nil
	}
	return nil, "", fmt.Errorf("github: List %s: %w", q.Type, tracker.ErrUnsupported)
}

// Changes lists issues and pull requests updated since the cursor (an RFC
// 3339 time), oldest first, and returns the latest update time seen as
// the next cursor. GitHub's since is inclusive and its times have whole
// seconds, so entities stamped exactly at the cursor were seen by the
// last call and are skipped; one changed again within that same second
// is caught by the loop's resync.
func (a *Adapter) Changes(ctx context.Context, cursor string) ([]tracker.Ref, string, error) {
	v := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if cursor != "" {
		if _, err := time.Parse(time.RFC3339Nano, cursor); err != nil {
			return nil, cursor, fmt.Errorf("github: bad cursor %q", cursor)
		}
		v.Set("since", cursor)
	}
	path := a.repoPath("/issues?" + v.Encode())
	next := cursor
	var refs []tracker.Ref
	for path != "" {
		var raw []ghIssue
		var err error
		path, err = a.do(ctx, http.MethodGet, path, nil, &raw)
		if err != nil {
			return refs, next, err
		}
		for _, is := range raw {
			it := is.item()
			c := is.UpdatedAt.UTC().Format(time.RFC3339)
			if cursor != "" && c == cursor {
				continue
			}
			refs = append(refs, it.Ref)
			if c > next || next == "" {
				next = c
			}
		}
	}
	return refs, next, nil
}

// Decisions reads a pull request's reviews submitted after since, each
// with its inline comments folded into Body.
func (a *Adapter) Decisions(ctx context.Context, on tracker.Ref, since time.Time) ([]tracker.Decision, error) {
	if on.Type != "PullRequest" {
		return nil, fmt.Errorf("github: Decisions on %s: %w", on.Type, tracker.ErrUnsupported)
	}
	path := a.repoPath("/pulls/" + on.ID + "/reviews?per_page=100")
	var out []tracker.Decision
	for path != "" {
		var raw []json.RawMessage
		var err error
		path, err = a.do(ctx, http.MethodGet, path, nil, &raw)
		if err != nil {
			return nil, err
		}
		for _, rm := range raw {
			var r ghReview
			if err := json.Unmarshal(rm, &r); err != nil {
				return nil, err
			}
			if !r.SubmittedAt.After(since) {
				continue
			}
			body, evidence, err := a.reviewBody(ctx, on.ID, r, rm)
			if err != nil {
				return nil, err
			}
			out = append(out, tracker.Decision{
				On:       on,
				Source:   tracker.Ref{Type: "Review", ID: on.ID + "#" + strconv.FormatInt(r.ID, 10), URL: r.HTMLURL},
				By:       tracker.Person{ID: r.User.Login, Name: r.User.Name},
				Native:   strings.ToUpper(r.State),
				Body:     body,
				At:       r.SubmittedAt,
				Evidence: evidence,
			})
		}
	}
	return out, nil
}

// reviewBody is the review's text and its inline comments, and the
// evidence: the review and its comments as GitHub returned them.
func (a *Adapter) reviewBody(ctx context.Context, pull string, r ghReview, raw json.RawMessage) (string, []byte, error) {
	var comments []json.RawMessage
	path := a.repoPath(fmt.Sprintf("/pulls/%s/reviews/%d/comments?per_page=100", pull, r.ID))
	for path != "" {
		var page []json.RawMessage
		var err error
		path, err = a.do(ctx, http.MethodGet, path, nil, &page)
		if err != nil {
			return "", nil, err
		}
		comments = append(comments, page...)
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(r.Body))
	for _, cm := range comments {
		var c ghReviewComment
		if err := json.Unmarshal(cm, &c); err != nil {
			return "", nil, err
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if c.Line > 0 {
			fmt.Fprintf(&b, "%s:%d: %s", c.Path, c.Line, strings.TrimSpace(c.Body))
		} else {
			fmt.Fprintf(&b, "%s: %s", c.Path, strings.TrimSpace(c.Body))
		}
	}
	ev, err := json.Marshal(map[string]any{"review": raw, "comments": comments})
	return b.String(), ev, err
}

// --- webhooks ---

// Receive verifies X-Hub-Signature-256 (when a secret is configured, a
// delivery without one is refused) and parses the events the loop uses.
// A check run's requested action carries its decision in the delivery
// alone, so it is returned as a Decision.
func (a *Adapter) Receive(r *http.Request) (tracker.Delivery, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDelivery+1))
	if err != nil {
		return tracker.Delivery{}, err
	}
	if len(body) > maxDelivery {
		return tracker.Delivery{}, errors.New("github: delivery over 25 MB")
	}
	if a.c.WebhookSecret != nil {
		secret, err := a.c.WebhookSecret(r.Context())
		if err != nil {
			return tracker.Delivery{}, fmt.Errorf("github: webhook secret: %w", err)
		}
		if len(secret) > 0 && !verify(body, secret, r.Header.Get("X-Hub-Signature-256")) {
			return tracker.Delivery{}, tracker.ErrBadSignature
		}
	}
	d := tracker.Delivery{ID: r.Header.Get("X-GitHub-Delivery")}
	var p struct {
		Action      string          `json:"action"`
		Issue       *ghIssue        `json:"issue"`
		PullRequest *ghPull         `json:"pull_request"`
		Review      json.RawMessage `json:"review"`
		CheckRun    *ghCheckRun     `json:"check_run"`
		Requested   *struct {
			Identifier string `json:"identifier"`
		} `json:"requested_action"`
		Sender ghUser `json:"sender"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return d, fmt.Errorf("github: delivery: %w", err)
	}
	d.Self = a.c.Self != "" && p.Sender.Login == a.c.Self
	switch r.Header.Get("X-GitHub-Event") {
	case "issues", "issue_comment":
		if p.Issue != nil {
			d.Refs = append(d.Refs, p.Issue.item().Ref)
		}
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		if p.PullRequest != nil {
			d.Refs = append(d.Refs, pullItem(*p.PullRequest).Ref)
		}
	case "check_run":
		if p.Action == "requested_action" && p.CheckRun != nil && p.Requested != nil {
			d.Decisions = append(d.Decisions, tracker.Decision{
				On:       tracker.Ref{Type: "CheckRun", ID: strconv.FormatInt(p.CheckRun.ID, 10), URL: p.CheckRun.HTMLURL},
				Source:   tracker.Ref{Type: "CheckAction", ID: d.ID},
				By:       tracker.Person{ID: p.Sender.Login, Name: p.Sender.Name},
				Native:   "action:" + p.Requested.Identifier,
				Body:     "requested from the check run by @" + p.Sender.Login,
				At:       a.now(),
				Evidence: body,
			})
		}
	}
	return d, nil
}

func verify(body, secret []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// --- writing ---

// keyMarker hides the write's key in what it writes, so Probe can find
// a write that happened before a crash.
func keyMarker(key string) string { return "<!-- c4-sync:key=" + key + " -->" }

// Apply makes one write. Kinds:
//   - check: a check run on Fields["head_sha"]; Fields["id"] updates
//     that run in place, else a run is created. Fields: name, status,
//     conclusion, title, summary, text, details_url, external_id, and
//     actions as "identifier=Label;..." (three at most).
//   - status: a commit status on Fields["sha"]: state, description,
//     target_url, context.
//   - comment: a comment on On (an Issue or PullRequest), or with
//     Note.Replace, an edit of that comment.
//   - mention: a comment on On that @-mentions Fields["user"], so GitHub
//     notifies them.
func (a *Adapter) Apply(ctx context.Context, w tracker.Write) (tracker.Ref, error) {
	switch w.Kind {
	case "check":
		return a.applyCheck(ctx, w)
	case "status":
		f := w.Fields
		if f["sha"] == "" {
			return tracker.Ref{}, errors.New("github: status write without sha")
		}
		in := map[string]string{"state": f["state"], "context": f["context"]}
		if v := f["description"]; v != "" {
			in["description"] = clip(v, 140)
		}
		if v := f["target_url"]; v != "" {
			in["target_url"] = v
		}
		var st struct {
			ID int64 `json:"id"`
		}
		if _, err := a.do(ctx, http.MethodPost, a.repoPath("/statuses/"+f["sha"]), in, &st); err != nil {
			return tracker.Ref{}, err
		}
		return tracker.Ref{Type: "Status", ID: strconv.FormatInt(st.ID, 10)}, nil
	case "comment", "mention":
		if w.Note == nil || w.Note.Body == "" {
			return tracker.Ref{}, errors.New("github: comment write without a body")
		}
		body := w.Note.Body
		if w.Kind == "mention" {
			if w.Fields["user"] == "" || w.Note.Replace != nil {
				return tracker.Ref{}, errors.New("github: a mention names its user and is a new comment")
			}
			body = "@" + w.Fields["user"] + " " + body
		}
		if w.Key != "" {
			body += "\n\n" + keyMarker(w.Key)
		}
		var c ghComment
		if w.Note.Replace != nil {
			if _, err := a.do(ctx, http.MethodPatch, a.repoPath("/issues/comments/"+w.Note.Replace.ID), map[string]string{"body": body}, &c); err != nil {
				return tracker.Ref{}, err
			}
		} else {
			if w.On.Type != "Issue" && w.On.Type != "PullRequest" {
				return tracker.Ref{}, fmt.Errorf("github: comment on %s: %w", w.On.Type, tracker.ErrUnsupported)
			}
			if _, err := a.do(ctx, http.MethodPost, a.repoPath("/issues/"+w.On.ID+"/comments"), map[string]string{"body": body}, &c); err != nil {
				return tracker.Ref{}, err
			}
		}
		return tracker.Ref{Type: "Comment", ID: strconv.FormatInt(c.ID, 10), URL: c.HTMLURL}, nil
	}
	return tracker.Ref{}, fmt.Errorf("github: write kind %q: %w", w.Kind, tracker.ErrUnsupported)
}

func (a *Adapter) applyCheck(ctx context.Context, w tracker.Write) (tracker.Ref, error) {
	f := w.Fields
	in := map[string]any{}
	if v := f["status"]; v != "" {
		in["status"] = v
	}
	if v := f["conclusion"]; v != "" {
		in["conclusion"] = v
	}
	for _, k := range []string{"details_url", "external_id"} {
		if v := f[k]; v != "" {
			in[k] = v
		}
	}
	if f["title"] != "" || f["summary"] != "" {
		summary := f["summary"]
		if w.Key != "" {
			summary += "\n\n" + keyMarker(w.Key)
		}
		out := map[string]string{"title": clip(f["title"], 255), "summary": clip(summary, 65535)}
		if v := f["text"]; v != "" {
			out["text"] = clip(v, 65535)
		}
		in["output"] = out
	}
	if v, ok := f["actions"]; ok {
		acts := []map[string]string{}
		for _, part := range strings.Split(v, ";") {
			id, label, ok := strings.Cut(part, "=")
			if !ok || id == "" {
				continue
			}
			acts = append(acts, map[string]string{
				"identifier": clip(id, 20), "label": clip(label, 20), "description": clip(label+" (C4)", 40),
			})
		}
		if len(acts) > 3 {
			acts = acts[:3]
		}
		in["actions"] = acts
	}
	var cr ghCheckRun
	if id := f["id"]; id != "" {
		if _, err := a.do(ctx, http.MethodPatch, a.repoPath("/check-runs/"+id), in, &cr); err != nil {
			return tracker.Ref{}, err
		}
	} else {
		if f["head_sha"] == "" || f["name"] == "" {
			return tracker.Ref{}, errors.New("github: new check run needs name and head_sha")
		}
		in["name"], in["head_sha"] = f["name"], f["head_sha"]
		if _, err := a.do(ctx, http.MethodPost, a.repoPath("/check-runs"), in, &cr); err != nil {
			return tracker.Ref{}, err
		}
	}
	return tracker.Ref{Type: "CheckRun", ID: strconv.FormatInt(cr.ID, 10), URL: cr.HTMLURL}, nil
}

// Publish posts what a finished job made, as a comment on the issue or
// pull request: the output view's C4 ID, the job record's C4 ID and the
// custody link. It probes first, so a retry never posts twice.
func (a *Adapter) Publish(ctx context.Context, on tracker.Ref, out tracker.Output, key string) (tracker.Ref, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "**C4 output: %s**\n\n", out.Name)
	b.WriteString(out.Describe() + ".\n\n")
	fmt.Fprintf(&b, "- output view: `%s`\n", out.View)
	if out.Record != "" {
		fmt.Fprintf(&b, "- job record: `%s`\n", out.Record)
	}
	if out.Custody != "" {
		fmt.Fprintf(&b, "- provenance: %s\n", out.Custody)
	}
	w := tracker.Write{On: on, Kind: "comment", Note: &tracker.Note{Body: b.String()}, Key: key}
	if done, ref, err := a.Probe(ctx, w); err != nil || done {
		return ref, err
	}
	return a.Apply(ctx, w)
}

// Probe looks for a write already made under w.Key: a comment or check
// run carrying the key's marker, or a commit status with the same context,
// state and description as the newest one.
// Guard: an edit, a check-run update and a commit status are the same
// write sent again; a new comment or check run carries the key's marker,
// which Probe finds.
func (a *Adapter) Guard(w tracker.Write) effect.Guard {
	switch w.Kind {
	case "status":
		return effect.Idempotent
	case "check":
		if w.Fields["id"] != "" {
			return effect.Idempotent
		}
	case "comment", "mention":
		if w.Note != nil && w.Note.Replace != nil {
			return effect.Idempotent
		}
	default:
		return effect.Unguarded
	}
	if w.Key == "" {
		return effect.Unguarded
	}
	return effect.Probed
}

func (a *Adapter) Probe(ctx context.Context, w tracker.Write) (bool, tracker.Ref, error) {
	switch w.Kind {
	case "comment", "mention":
		if w.Key == "" || w.Note == nil || w.Note.Replace != nil {
			return false, tracker.Ref{}, nil // an edit is idempotent by itself
		}
		path := a.repoPath("/issues/" + w.On.ID + "/comments?per_page=100")
		for path != "" {
			var cs []ghComment
			var err error
			path, err = a.do(ctx, http.MethodGet, path, nil, &cs)
			if err != nil {
				return false, tracker.Ref{}, err
			}
			for _, c := range cs {
				if strings.Contains(c.Body, keyMarker(w.Key)) {
					return true, tracker.Ref{Type: "Comment", ID: strconv.FormatInt(c.ID, 10), URL: c.HTMLURL}, nil
				}
			}
		}
		return false, tracker.Ref{}, nil
	case "check":
		if w.Fields["id"] != "" || w.Key == "" {
			return false, tracker.Ref{}, nil // an update is idempotent by itself
		}
		v := url.Values{"check_name": {w.Fields["name"]}}
		var res struct {
			CheckRuns []ghCheckRun `json:"check_runs"`
		}
		if _, err := a.do(ctx, http.MethodGet, a.repoPath("/commits/"+w.Fields["head_sha"]+"/check-runs?"+v.Encode()), nil, &res); err != nil {
			return false, tracker.Ref{}, err
		}
		for _, c := range res.CheckRuns {
			if strings.Contains(c.Output.Summary, keyMarker(w.Key)) {
				return true, tracker.Ref{Type: "CheckRun", ID: strconv.FormatInt(c.ID, 10), URL: c.HTMLURL}, nil
			}
		}
		return false, tracker.Ref{}, nil
	case "status":
		var sts []ghStatus
		if _, err := a.do(ctx, http.MethodGet, a.repoPath("/commits/"+w.Fields["sha"]+"/statuses?per_page=100"), nil, &sts); err != nil {
			return false, tracker.Ref{}, err
		}
		for _, s := range sts { // newest first: only the newest per context counts
			if s.Context != w.Fields["context"] {
				continue
			}
			same := s.State == w.Fields["state"] && s.Description == clip(w.Fields["description"], 140)
			return same, tracker.Ref{}, nil
		}
		return false, tracker.Ref{}, nil
	}
	return false, tracker.Ref{}, nil
}

func (a *Adapter) Person(ctx context.Context, id string) (tracker.Person, error) {
	var u ghUser
	if _, err := a.do(ctx, http.MethodGet, "/users/"+url.PathEscape(id), nil, &u); err != nil {
		return tracker.Person{}, err
	}
	return tracker.Person{ID: u.Login, Name: u.Name, Email: u.Email}, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ell = "…" // three bytes
	r := []rune(s)
	for len(string(r)) > n-len(ell) {
		r = r[:len(r)-1]
	}
	return string(r) + ell
}
