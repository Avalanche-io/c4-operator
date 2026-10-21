// Package huggingface reads the Hugging Face Hub as a source of trees
// (internal/source): a model or dataset repository at a pinned commit.
//
// It speaks the Hub's REST API as huggingface_hub does:
//   - GET /api/{models|datasets}/<repo>/revision/<rev> resolves a revision
//     to its commit, with whether the repository is gated;
//   - GET /api/.../tree/<commit>?recursive=true lists the files with their
//     git blob SHA-1, and a large file's LFS SHA-256 and Xet hash;
//   - /<repo>/resolve/<commit>/<path> serves a file, often by redirect to
//     a CDN, answering X-Repo-Commit (required, and checked against the
//     commit) and, for a large file, X-Linked-Etag (its SHA-256).
//
// The token goes only on requests the client makes to the Hub's own host,
// never on a redirect it follows. Redirects are followed one hop at a
// time, https only, to the hosts the source allows, a few at most. URLs in
// errors carry no query, since a CDN's are signed. Every request reads the
// Hub's RateLimit headers, and the client paces itself before a window
// runs out rather than waiting for a 429.
package huggingface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/source"
)

// Endpoint is the Hub.
const Endpoint = "https://huggingface.co"

// Hosts are the hosts the Hub's downloads redirect to.
var Hosts = []string{"huggingface.co", "*.huggingface.co", "*.hf.co"}

// MaxHops is how many redirects a download may follow.
const MaxHops = 5

// Repository types.
const (
	Model   = "model"
	Dataset = "dataset"
)

// Client reads the Hub.
type Client struct {
	// Endpoint is the Hub's address (default Endpoint). It may carry a
	// path prefix (a mirror at https://host/hub).
	Endpoint string
	// Hosts are the hosts a download may be redirected to: names, or
	// *.suffix for any name under it (default Hosts). The Endpoint's own
	// host is always allowed.
	Hosts []string
	// Token returns the bearer token, or "" to read anonymously. It is
	// read on every request, so a leased token may rotate.
	Token func(context.Context) (string, error)
	// HTTP is the client (default one with no overall timeout: a large file
	// takes as long as it takes, and the context bounds it). Its own
	// redirect policy is never used.
	HTTP *http.Client
	// Share is the part of each rate window this client may use, from 0
	// to 1 (default 1): a farm leaves the rest to people at the same
	// address or account.
	Share float64
	// Now and Sleep are the clock (tests replace them).
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error

	mu      sync.Mutex
	windows map[string]*window
	// inflight counts requests admitted and not yet answered, by bucket:
	// an answer's count of what is left predates them.
	inflight map[string]int
}

// window is what the Hub last said of one rate bucket: how many requests
// are left (less those admitted since), its quota if it said, and when it
// resets.
type window struct {
	quota, left int
	reset       time.Time
	// fresh: the Hub's window has reset and this one counts from it,
	// until an answer says how many are left.
	fresh bool
}

func (c *Client) endpoint() string {
	if c.Endpoint == "" {
		return Endpoint
	}
	return strings.TrimSuffix(c.Endpoint, "/")
}

func (c *Client) base() *url.URL {
	u, err := url.Parse(c.endpoint())
	if err != nil {
		return &url.URL{}
	}
	return u
}

// url is the Hub's address for path p.
func (c *Client) url(p string) string { return c.endpoint() + p }

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// client is the HTTP client, never following a redirect itself.
func (c *Client) client() *http.Client {
	hc := http.Client{}
	if c.HTTP != nil {
		hc = *c.HTTP
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hc
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// apiPath and resolvePath are a repository's API and download roots.
func apiPath(repoType, repo string) (string, error) {
	if !repoName.MatchString(repo) || strings.Contains(repo, "..") {
		return "", fmt.Errorf("huggingface: %q is not owner/name", repo)
	}
	switch repoType {
	case Model, "":
		return "/api/models/" + repo, nil
	case Dataset:
		return "/api/datasets/" + repo, nil
	}
	return "", fmt.Errorf("huggingface: repository type %q: want model or dataset", repoType)
}

func resolvePath(s source.Snapshot, file string) string {
	root := "/" + s.Repo
	if s.RepoType == Dataset {
		root = "/datasets/" + s.Repo
	}
	segs := strings.Split(file, "/")
	for i, p := range segs {
		segs[i] = url.PathEscape(p)
	}
	return root + "/resolve/" + url.PathEscape(s.Commit) + "/" + strings.Join(segs, "/")
}

// redact is u as an error may show it: no query (a CDN's are signed) and
// no user.
func redact(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// plain is err without the URL a url.Error carries, whose query may be a
// signature.
func plain(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// --- requests, paced ---

// bucket is the rate bucket a request counts against: downloads are
// resolvers, the rest the API.
func bucket(u string) string {
	if strings.Contains(u, "/resolve/") {
		return "resolvers"
	}
	return "api"
}

// pace waits, before a request of bucket b, until the window has room in
// this client's share, and counts the request against it, so callers at
// once do not all pass at the edge. Once the Hub's window resets, a fresh
// one admits its quota when the Hub said it, else one request, whose
// answer tells the rest how many are left. A window whose quota the Hub
// did not say keeps one request in hand.
func (c *Client) pace(ctx context.Context, b string) error {
	for {
		c.mu.Lock()
		w := c.windows[b]
		if w == nil {
			c.admit(b)
			c.mu.Unlock()
			return nil
		}
		keep := 1
		if w.quota > 0 {
			share := c.Share
			if share <= 0 || share > 1 {
				share = 1
			}
			// what the share leaves to others, and one request in hand
			keep = w.quota - int(float64(w.quota)*share) + 1
			// but never the whole quota, or nothing is ever sent
			keep = min(keep, w.quota-1)
		}
		now := c.now()
		if !now.Before(w.reset) && !w.fresh {
			w.fresh = true
			if w.quota > 0 {
				w.left = w.quota
			} else {
				w.left = keep + 1
			}
		}
		if w.left > keep {
			w.left--
			c.admit(b)
			c.mu.Unlock()
			return nil
		}
		d := w.reset.Sub(now)
		if w.fresh {
			// spent before the Hub has answered for the fresh window
			d = probeWait
		}
		c.mu.Unlock()
		if err := c.sleep(ctx, d); err != nil {
			return err
		}
		c.mu.Lock()
		if c.windows[b] == w && !w.fresh {
			// waited what the Hub said: its window has reset
			w.reset = time.Time{}
		}
		c.mu.Unlock()
	}
}

// probeWait is how long a caller waits for the answer that tells a fresh
// window's count.
const probeWait = 100 * time.Millisecond

// admit counts a request of bucket b in flight; c.mu is held.
func (c *Client) admit(b string) {
	if c.inflight == nil {
		c.inflight = map[string]int{}
	}
	c.inflight[b]++
}

// answered takes a request of bucket b out of flight.
func (c *Client) answered(b string) {
	c.mu.Lock()
	c.inflight[b]--
	c.mu.Unlock()
}

// RateLimit: "resolvers";r=2998;t=227 and RateLimit-Policy: "fixed
// window";"resolvers";q=3000;w=300 (the IETF draft the Hub follows).
var (
	rlName  = regexp.MustCompile(`"([^"]+)"\s*;`)
	rlParam = regexp.MustCompile(`\b([rtqw])=(\d+)`)
)

func params(h string) map[string]int {
	out := map[string]int{}
	for _, m := range rlParam.FindAllStringSubmatch(h, -1) {
		n, _ := strconv.Atoi(m[2])
		out[m[1]] = n
	}
	return out
}

// observe keeps what a response says of its window.
func (c *Client) observe(h http.Header) {
	rl := h.Get("RateLimit")
	if rl == "" {
		return
	}
	names := rlName.FindAllStringSubmatch(rl, -1)
	if len(names) == 0 {
		return
	}
	p := params(rl)
	b := names[0][1]
	w := &window{left: p["r"], reset: c.now().Add(time.Duration(p["t"]) * time.Second)}
	w.quota = params(h.Get("RateLimit-Policy"))["q"]
	c.mu.Lock()
	if c.windows == nil {
		c.windows = map[string]*window{}
	}
	// the Hub counted what it had seen; requests still in flight are not in
	// its count
	w.left -= c.inflight[b]
	c.windows[b] = w
	c.mu.Unlock()
}

// do sends a request to the Hub, u being on the Hub's own host, with the
// token, paced, following no redirect; a 429 waits out its window and
// tries again.
func (c *Client) do(ctx context.Context, method, u string) (*http.Response, error) {
	ru, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	if b := c.base(); ru.Scheme != b.Scheme || ru.Host != b.Host {
		return nil, fmt.Errorf("huggingface: %s is not the Hub", redact(ru))
	}
	for attempt := 0; ; attempt++ {
		if err := c.pace(ctx, bucket(u)); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return nil, err
		}
		if c.Token != nil {
			tok, err := c.Token(ctx)
			if err != nil {
				return nil, fmt.Errorf("huggingface: the token: %w", err)
			}
			if tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		resp, err := c.client().Do(req)
		c.answered(bucket(u))
		if err != nil {
			return nil, fmt.Errorf("huggingface: %s %s: %w", method, redact(ru), plain(err))
		}
		c.observe(resp.Header)
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= 5 {
			return resp, nil
		}
		resp.Body.Close()
		wait := time.Duration(params(resp.Header.Get("RateLimit"))["t"]) * time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && time.Duration(s)*time.Second > wait {
			wait = time.Duration(s) * time.Second
		}
		if wait <= 0 {
			wait = time.Second << attempt
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// failure is a non-success answer, in the Hub's words where it gives them.
func failure(resp *http.Response, what string) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := resp.Header.Get("X-Error-Message")
	if msg == "" {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		} else {
			msg = strings.TrimSpace(string(b))
		}
	}
	kind := errors.New(resp.Status)
	switch code := resp.Header.Get("X-Error-Code"); {
	case code == "GatedRepo":
		kind = source.ErrGated
	case code == "RepoNotFound" || code == "RevisionNotFound" || code == "EntryNotFound" || resp.StatusCode == http.StatusNotFound:
		kind = source.ErrNotFound
	}
	if msg == "" {
		return fmt.Errorf("huggingface: %s: %w", what, kind)
	}
	return fmt.Errorf("huggingface: %s: %s: %w", what, msg, kind)
}

func (c *Client) getJSON(ctx context.Context, u, what string, out any) (*http.Response, error) {
	resp, err := c.do(ctx, http.MethodGet, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, failure(resp, what)
	}
	return resp, json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

// --- resolving, listing, fetching ---

// Resolve reads repo (owner/name) of repoType at ref (a branch, tag or
// commit) and pins it to the commit ref means now. Every later read is of
// that commit.
func (c *Client) Resolve(ctx context.Context, repoType, repo, ref string) (source.Snapshot, error) {
	api, err := apiPath(repoType, repo)
	if err != nil {
		return source.Snapshot{}, err
	}
	if ref == "" {
		ref = "main"
	}
	if repoType == "" {
		repoType = Model
	}
	var info struct {
		SHA      string          `json:"sha"`
		Gated    json.RawMessage `json:"gated"`
		Private  bool            `json:"private"`
		CardData struct {
			License json.RawMessage `json:"license"`
		} `json:"cardData"`
	}
	if _, err := c.getJSON(ctx, c.url(api+"/revision/"+url.PathEscape(ref)), repo+"@"+ref, &info); err != nil {
		return source.Snapshot{}, err
	}
	if !commitSHA.MatchString(info.SHA) {
		return source.Snapshot{}, fmt.Errorf("huggingface: %s@%s: the Hub gave no commit", repo, ref)
	}
	s := source.Snapshot{Source: "huggingface", Endpoint: c.endpoint(), RepoType: repoType, Repo: repo, Ref: ref,
		Commit: info.SHA, Private: info.Private}
	var gated string
	if json.Unmarshal(info.Gated, &gated) == nil {
		s.Gated = gated
	}
	var lic string
	if json.Unmarshal(info.CardData.License, &lic) == nil {
		s.License = lic
	} else {
		var lics []string
		if json.Unmarshal(info.CardData.License, &lics) == nil {
			s.License = strings.Join(lics, ", ")
		}
	}
	return s, nil
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkPath refuses a listed path that is not a plain relative path: one
// that is empty, absolute, has an empty, . or .. part, a backslash or a
// NUL. Such a path would leave the tree it is written into.
func checkPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("huggingface: the listing names %q, which is not a plain relative path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("huggingface: the listing names %q, which is not a plain relative path", p)
		}
	}
	return nil
}

// List fills s.Files with the files at s.Commit that f takes, by path. A
// listing with a path that is not plain and relative, or a path twice, is
// refused whole.
func (c *Client) List(ctx context.Context, s *source.Snapshot, f source.Filter) error {
	if err := f.Check(); err != nil {
		return err
	}
	api, err := apiPath(s.RepoType, s.Repo)
	if err != nil {
		return err
	}
	type entry struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		OID  string `json:"oid"`
		LFS  *struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"lfs"`
		XetHash string `json:"xetHash"`
	}
	u := c.url(api + "/tree/" + url.PathEscape(s.Commit) + "?recursive=true")
	var files []source.File
	seenPage, seenPath := map[string]bool{}, map[string]bool{}
	for u != "" {
		if seenPage[u] {
			return fmt.Errorf("huggingface: %s@%s: the listing's pages go round", s.Repo, s.Commit)
		}
		seenPage[u] = true
		var page []entry
		resp, err := c.getJSON(ctx, u, s.Repo+"@"+s.Commit+" files", &page)
		if err != nil {
			return err
		}
		for _, e := range page {
			if e.Type != "file" {
				continue
			}
			if err := checkPath(e.Path); err != nil {
				return err
			}
			if seenPath[e.Path] {
				return fmt.Errorf("huggingface: the listing names %q twice", e.Path)
			}
			seenPath[e.Path] = true
			if !f.Takes(e.Path) {
				continue
			}
			file := source.File{Path: e.Path, Size: e.Size, GitBlob: e.OID, XetHash: e.XetHash}
			if e.LFS != nil {
				file.SHA256, file.Size = e.LFS.OID, e.LFS.Size
			}
			files = append(files, file)
		}
		if u, err = c.next(resp.Header.Get("Link")); err != nil {
			return err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	s.Files = files
	return nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

// next is the URL of a listing's next page, which must be on the Hub's
// own host and scheme: the token goes nowhere else.
func (c *Client) next(link string) (string, error) {
	m := linkNext.FindStringSubmatch(link)
	if m == nil {
		return "", nil
	}
	b := c.base()
	u, err := b.Parse(m[1])
	if err != nil {
		return "", err
	}
	if u.Scheme != b.Scheme || u.Host != b.Host {
		return "", fmt.Errorf("huggingface: the next page is at %s, not the Hub", redact(u))
	}
	return u.String(), nil
}

// Access says, before anything is fetched, whether this client may read
// s's files: a gated repository answers source.ErrGated, in the Hub's
// words, to a client without access. It asks for the headers of one file.
func (c *Client) Access(ctx context.Context, s source.Snapshot) error {
	if len(s.Files) == 0 {
		return nil
	}
	small := s.Files[0]
	for _, f := range s.Files {
		if f.Size < small.Size {
			small = f
		}
	}
	resp, err := c.do(ctx, http.MethodHead, c.url(resolvePath(s, small.Path)))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return failure(resp, s.Repo+"@"+s.Commit)
	}
	return nil
}

func redirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// allowed says whether a download may be redirected to u: https (or
// http, only from a Hub reached over http, a local one), to the Hub's own
// host or one of Hosts.
func (c *Client) allowed(u *url.URL) error {
	b := c.base()
	if u.Scheme != "https" && !(u.Scheme == "http" && b.Scheme == "http") {
		return fmt.Errorf("a redirect to %s, which is not https", redact(u))
	}
	if u.Host == b.Host {
		return nil
	}
	hosts := c.Hosts
	if hosts == nil {
		hosts = Hosts
	}
	name := u.Hostname()
	for _, h := range hosts {
		if suffix, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(name, "."+suffix) {
				return nil
			}
		} else if name == h {
			return nil
		}
	}
	return fmt.Errorf("a redirect to %s, which this source does not allow", redact(u))
}

// Fetch reads one file of s and stores it through put, checked against
// its hashes (source.Store), spooling in dir. The Hub must say which
// commit it serves (X-Repo-Commit), and every hop that says one must say
// s.Commit. Redirects are followed one at a time, without the token, to
// allowed hosts only.
func (c *Client) Fetch(ctx context.Context, s source.Snapshot, f source.File, dir string, put source.Put) (source.Fetched, error) {
	what := s.Repo + "@" + s.Commit + ": " + f.Path
	resp, err := c.do(ctx, http.MethodGet, c.url(resolvePath(s, f.Path)))
	if err != nil {
		return source.Fetched{}, err
	}
	defer func() { resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return source.Fetched{}, failure(resp, what)
	}
	got := resp.Header.Get("X-Repo-Commit")
	if got == "" {
		return source.Fetched{}, fmt.Errorf("huggingface: %s: the Hub did not say which commit it served: %w", what, source.ErrMoved)
	}
	if got != s.Commit {
		return source.Fetched{}, fmt.Errorf("huggingface: %s: %s: %w", what, got, source.ErrMoved)
	}
	if etag := strings.Trim(resp.Header.Get("X-Linked-Etag"), `"`); f.SHA256 != "" && etag != "" && !strings.EqualFold(etag, f.SHA256) {
		return source.Fetched{}, fmt.Errorf("huggingface: %s: the Hub's etag %s, its listing %s: %w", what, etag, f.SHA256, source.ErrMismatch)
	}
	for hops := 0; redirect(resp.StatusCode); hops++ {
		if hops >= MaxHops {
			return source.Fetched{}, fmt.Errorf("huggingface: %s: more than %d redirects", what, MaxHops)
		}
		loc, err := resp.Location()
		if err != nil {
			return source.Fetched{}, fmt.Errorf("huggingface: %s: a redirect without a location", what)
		}
		if err := c.allowed(loc); err != nil {
			return source.Fetched{}, fmt.Errorf("huggingface: %s: %w", what, err)
		}
		next, err := c.hop(ctx, loc)
		if err != nil {
			return source.Fetched{}, fmt.Errorf("huggingface: %s: %w", what, err)
		}
		resp.Body.Close()
		resp = next
		if h := resp.Header.Get("X-Repo-Commit"); h != "" && h != s.Commit {
			return source.Fetched{}, fmt.Errorf("huggingface: %s: %s: %w", what, h, source.ErrMoved)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return source.Fetched{}, fmt.Errorf("huggingface: %s: the download answered %s", what, resp.Status)
	}
	return source.Store(ctx, f, resp.Body, dir, put)
}

// hop follows one redirect, without the token.
func (c *Client) hop(ctx context.Context, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", redact(u), plain(err))
	}
	return resp, nil
}

// Whoami is the Hub account the token reads as, "" when anonymous. An
// import compares it with the account its lease names (accountMatchesLease)
// and refuses when they differ; the record never names the account.
func (c *Client) Whoami(ctx context.Context) (string, error) {
	if c.Token == nil {
		return "", nil
	}
	if tok, err := c.Token(ctx); err != nil || tok == "" {
		return "", err
	}
	var me struct {
		Name string `json:"name"`
	}
	if _, err := c.getJSON(ctx, c.url("/api/whoami-v2"), "whoami", &me); err != nil {
		return "", err
	}
	return me.Name, nil
}
