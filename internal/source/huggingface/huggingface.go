// Package huggingface reads the Hugging Face Hub as a source of trees
// (internal/source): a model or dataset repository at a pinned commit.
//
// It speaks the Hub's REST API as huggingface_hub does:
//   - GET /api/{models|datasets}/<repo>/revision/<rev> resolves a revision
//     to its commit, with whether the repository is gated;
//   - GET /api/.../tree/<commit>?recursive=true lists the files with their
//     git blob SHA-1, and a large file's LFS SHA-256 and Xet hash;
//   - /<repo>/resolve/<commit>/<path> serves a file, often by redirect to
//     a CDN, answering X-Repo-Commit (checked against the commit) and, for
//     a large file, X-Linked-Etag (its SHA-256).
//
// Every request reads the Hub's RateLimit headers and the client paces
// itself before a window runs out, rather than waiting for a 429. A token,
// when there is one, goes only to the Hub's own host, never to a CDN a
// download is redirected to.
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

// Repository types.
const (
	Model   = "model"
	Dataset = "dataset"
)

// Client reads the Hub.
type Client struct {
	// Endpoint is the Hub's address (default Endpoint).
	Endpoint string
	// Token returns the bearer token, or "" to read anonymously. It is
	// read on every request, so a leased token may rotate.
	Token func(context.Context) (string, error)
	// HTTP is the client (default one with no overall timeout: a large file
	// takes as long as it takes, and the context bounds it).
	HTTP *http.Client
	// Share is the part of each rate window this client may use, from 0
	// to 1 (default 1): a farm leaves the rest to people at the same
	// address or account.
	Share float64
	// Now and Sleep are the clock (tests replace them).
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error

	mu      sync.Mutex
	windows map[string]window
}

// window is what the Hub last said of one rate bucket: how many requests
// are left of its quota, and when it resets.
type window struct {
	quota, left int
	reset       time.Time
}

func (c *Client) endpoint() string {
	if c.Endpoint == "" {
		return Endpoint
	}
	return strings.TrimSuffix(c.Endpoint, "/")
}

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

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 0}
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

// --- requests, paced ---

// bucket is the rate bucket a request counts against: downloads are
// resolvers, the rest the API.
func bucket(p string) string {
	if strings.Contains(p, "/resolve/") {
		return "resolvers"
	}
	return "api"
}

// pace waits, before a request of bucket b, until the window has room in
// this client's share.
func (c *Client) pace(ctx context.Context, b string) error {
	c.mu.Lock()
	w, ok := c.windows[b]
	c.mu.Unlock()
	if !ok || w.quota == 0 {
		return nil
	}
	share := c.Share
	if share <= 0 || share > 1 {
		share = 1
	}
	// Keep what the share leaves to others, and one request in hand.
	keep := w.quota - int(float64(w.quota)*share) + 1
	if w.left > keep {
		return nil
	}
	if d := w.reset.Sub(c.now()); d > 0 {
		if err := c.sleep(ctx, d); err != nil {
			return err
		}
	}
	c.mu.Lock()
	delete(c.windows, b)
	c.mu.Unlock()
	return nil
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
	b := names[0][1]
	p := params(rl)
	w := window{left: p["r"], reset: c.now().Add(time.Duration(p["t"]) * time.Second)}
	w.quota = params(h.Get("RateLimit-Policy"))["q"]
	c.mu.Lock()
	if c.windows == nil {
		c.windows = map[string]window{}
	}
	c.windows[b] = w
	c.mu.Unlock()
}

// do sends a request to the Hub's own host, with the token, paced; a 429
// waits out its window and tries again.
func (c *Client) do(ctx context.Context, method, p string) (*http.Response, error) {
	u := c.endpoint() + p
	for attempt := 0; ; attempt++ {
		if err := c.pace(ctx, bucket(p)); err != nil {
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
		hc := *c.http()
		// A redirect is followed by Fetch itself, without the token.
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("huggingface: %s %s: %w", method, p, err)
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

func (c *Client) getJSON(ctx context.Context, p, what string, out any) (*http.Response, error) {
	resp, err := c.do(ctx, http.MethodGet, p)
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
	if _, err := c.getJSON(ctx, api+"/revision/"+url.PathEscape(ref), repo+"@"+ref, &info); err != nil {
		return source.Snapshot{}, err
	}
	if len(info.SHA) != 40 {
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

// List fills s.Files with the files at s.Commit that f takes, by path.
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
	p := api + "/tree/" + url.PathEscape(s.Commit) + "?recursive=true"
	var files []source.File
	for p != "" {
		var page []entry
		resp, err := c.getJSON(ctx, p, s.Repo+"@"+s.Commit+" files", &page)
		if err != nil {
			return err
		}
		for _, e := range page {
			if e.Type != "file" || !f.Takes(e.Path) {
				continue
			}
			file := source.File{Path: e.Path, Size: e.Size, GitBlob: e.OID, XetHash: e.XetHash}
			if e.LFS != nil {
				file.SHA256, file.Size = e.LFS.OID, e.LFS.Size
			}
			files = append(files, file)
		}
		if p, err = c.next(resp.Header.Get("Link")); err != nil {
			return err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	s.Files = files
	return nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

// next is the path of a listing's next page, which must be on the Hub's
// own host: the token goes nowhere else.
func (c *Client) next(link string) (string, error) {
	m := linkNext.FindStringSubmatch(link)
	if m == nil {
		return "", nil
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return "", err
	}
	base, _ := url.Parse(c.endpoint())
	if u.IsAbs() && u.Host != base.Host {
		return "", fmt.Errorf("huggingface: the next page is on %s, not the Hub", u.Host)
	}
	return u.RequestURI(), nil
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
	resp, err := c.do(ctx, http.MethodHead, resolvePath(s, small.Path))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return failure(resp, s.Repo+"@"+s.Commit)
	}
	return nil
}

// Fetch reads one file of s and stores it through put, checked against
// its hashes (source.Store), spooling in dir. A redirect to a CDN is
// followed without the token.
func (c *Client) Fetch(ctx context.Context, s source.Snapshot, f source.File, dir string, put source.Put) (source.Fetched, error) {
	resp, err := c.do(ctx, http.MethodGet, resolvePath(s, f.Path))
	if err != nil {
		return source.Fetched{}, err
	}
	defer resp.Body.Close()
	what := s.Repo + "@" + s.Commit + ": " + f.Path
	if resp.StatusCode >= 400 {
		return source.Fetched{}, failure(resp, what)
	}
	if got := resp.Header.Get("X-Repo-Commit"); got != "" && got != s.Commit {
		return source.Fetched{}, fmt.Errorf("huggingface: %s: %s: %w", what, got, source.ErrMoved)
	}
	if etag := strings.Trim(resp.Header.Get("X-Linked-Etag"), `"`); f.SHA256 != "" && etag != "" && !strings.EqualFold(etag, f.SHA256) {
		return source.Fetched{}, fmt.Errorf("huggingface: %s: the Hub's etag %s, its listing %s: %w", what, etag, f.SHA256, source.ErrMismatch)
	}
	body := resp.Body
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusFound, http.StatusTemporaryRedirect, http.StatusMovedPermanently, http.StatusSeeOther, http.StatusPermanentRedirect:
		loc, err := resp.Location()
		if err != nil {
			return source.Fetched{}, fmt.Errorf("huggingface: %s: a redirect without a location", what)
		}
		cdn, err := c.download(ctx, loc)
		if err != nil {
			return source.Fetched{}, fmt.Errorf("huggingface: %s: %w", what, err)
		}
		defer cdn.Close()
		body = cdn
	default:
		return source.Fetched{}, failure(resp, what)
	}
	return source.Store(ctx, f, body, dir, put)
}

// download follows a download's redirect. The token goes only to the
// Hub's own host.
func (c *Client) download(ctx context.Context, loc *url.URL) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(c.endpoint())
	if loc.Host == base.Host && c.Token != nil {
		if tok, err := c.Token(ctx); err == nil && tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("the download answered %s", resp.Status)
	}
	return resp.Body, nil
}

// Whoami is the Hub account the token reads as, "" when anonymous. A
// gated repository's terms were accepted by that account, so the import
// record names it.
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
	if _, err := c.getJSON(ctx, "/api/whoami-v2", "whoami", &me); err != nil {
		return "", err
	}
	return me.Name, nil
}
