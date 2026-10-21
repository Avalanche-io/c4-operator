// Package hftest is a fake Hugging Face Hub for testing the source
// (internal/source/huggingface) offline. It serves the calls the source
// makes, in the shapes the Hub answers (read live on 2026-10-21): a
// revision's commit, gating and card license; a recursive file listing
// with git blob SHA-1, LFS SHA-256 and Xet hash, paged by Link; a file's
// resolve, answered directly for a small file and by redirect to a CDN on
// another host for a large one, with X-Repo-Commit and X-Linked-Etag;
// whoami; the error codes (GatedRepo, RepoNotFound, RevisionNotFound,
// EntryNotFound); and RateLimit headers on every answer, with a 429 once a
// window is spent.
//
// It answers by name: the Hub at Hub (hub.example), its CDN at CDNHost
// (cdn.hub.example, a subdomain, by default), any other name a test
// redirects to at the CDN too. Client() is an HTTP client that reaches
// those names. Its knobs make the Hub misbehave the ways a hostile or
// broken one can.
package hftest

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// LFSThreshold is the size from which a file is stored in LFS: listed
// with its SHA-256 and served by redirect to the CDN.
const LFSThreshold = 64

// Hub is the fake Hub's name.
const Hub = "hub.example"

// Signature is the query a CDN location carries, as a presigned URL does:
// it must never appear in an error.
const Signature = "X-Amz-Signature=sig-secret"

// Server is the fake Hub and its CDN.
type Server struct {
	hub, cdnSrv *httptest.Server
	// Endpoint is the Hub's address for a client.
	Endpoint string
	// Tokens maps a token to its account.
	Tokens map[string]string
	// PageSize is how many entries a listing page holds (default 2, so
	// tests page).
	PageSize int
	// Quota is each rate bucket's requests per window (0: unlimited).
	// NoPolicy leaves out RateLimit-Policy; RetryAfter sets Retry-After on
	// a 429.
	Quota      int
	NoPolicy   bool
	RetryAfter int

	// CDNHost is where a large file's resolve redirects (default
	// cdn.hub.example).
	CDNHost string
	// SameHostHop redirects first to a relative path on the Hub, which
	// then redirects to the CDN, as a renamed repository does.
	SameHostHop bool
	// MultiHop is how many times the CDN redirects to itself before it
	// serves.
	MultiHop int
	// LieCommit answers every resolve with this X-Repo-Commit; NoCommit
	// answers with none.
	LieCommit string
	NoCommit  bool
	// Oversize serves this many bytes more than a file has.
	Oversize int
	// ExtraEntries are added to a listing's first page as they are.
	ExtraEntries []map[string]any
	// SelfNext makes a listing's next page itself; EndlessPages gives it a
	// next page forever, empty past the last file.
	SelfNext, EndlessPages bool
	// FailNext answers the next requests 502 with no RateLimit, as a proxy
	// in front of the Hub may; DropNext closes their connections unanswered.
	FailNext, DropNext int
	// Renames redirects a repository's old name to its new one, on the Hub,
	// as the Hub does for a renamed repository.
	Renames map[string]string
	// HTMLError answers a revision with a page of HTML and a 500.
	HTMLError bool
	// Endless serves a file's bytes without end.
	Endless bool
	// HopBack makes the CDN redirect back to the Hub, which serves the
	// file naming HopBackCommit ("" the pinned one, "-" none). CDNCommit
	// is the commit the CDN names, if any.
	HopBack       bool
	HopBackCommit string
	CDNCommit     string

	mu        sync.Mutex
	repos     map[string]*Repo
	used      map[string]int
	followed  int // redirects off the Hub that carried a token: must stay 0
	hubHops   int // hops back to the Hub that carried a token
	throttled int
	corrupt   map[string]bool
	calls     []string
}

// Repo is one repository.
type Repo struct {
	Type, Name string
	Gated      string // "", "auto", "manual"
	Private    bool
	License    string
	// Access is the accounts let into a gated or private repository.
	Access  map[string]bool
	refs    map[string]string
	commits map[string]map[string][]byte
}

// New starts the fake Hub and its CDN.
func New() *Server {
	s := &Server{Tokens: map[string]string{}, PageSize: 2, CDNHost: "cdn." + Hub, Endpoint: "http://" + Hub,
		repos: map[string]*Repo{}, used: map[string]int{}, corrupt: map[string]bool{}}
	s.cdnSrv = httptest.NewServer(http.HandlerFunc(s.cdn))
	s.hub = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Close stops both servers.
func (s *Server) Close() { s.hub.Close(); s.cdnSrv.Close() }

// Client reaches the fake's names: the Hub at Hub, any other .example
// name at the CDN, and a name starting down. not at all.
func (s *Server) Client() *http.Client {
	hub, cdn := s.hub.Listener.Addr().String(), s.cdnSrv.Listener.Addr().String()
	d := &net.Dialer{}
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		switch {
		case strings.HasPrefix(host, "down."):
			return nil, fmt.Errorf("hftest: %s is down", host)
		case host == Hub:
			return d.DialContext(ctx, network, hub)
		case strings.HasSuffix(host, ".example"):
			return d.DialContext(ctx, network, cdn)
		}
		return nil, fmt.Errorf("hftest: no such host %s", host)
	}}}
}

// Commit adds a commit of files to the repository (owner/name, type model
// or dataset) and points ref at it, returning its 40-hex SHA.
func (s *Server) Commit(repoType, name, ref string, files map[string][]byte) (*Repo, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if repoType == "" {
		repoType = "model"
	}
	k := repoType + "/" + name
	r := s.repos[k]
	if r == nil {
		r = &Repo{Type: repoType, Name: name, Access: map[string]bool{}, refs: map[string]string{}, commits: map[string]map[string][]byte{}}
		s.repos[k] = r
	}
	h := sha1.New()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00%x\x00", p, sha256.Sum256(files[p]))
	}
	fmt.Fprintf(h, "%d", len(r.commits))
	sha := hex.EncodeToString(h.Sum(nil))
	cp := map[string][]byte{}
	for p, b := range files {
		cp[p] = append([]byte(nil), b...)
	}
	r.commits[sha] = cp
	r.refs[ref] = sha
	return r, sha
}

// Corrupt serves wrong bytes for path from now on.
func (s *Server) Corrupt(path string) { s.mu.Lock(); s.corrupt[path] = true; s.mu.Unlock() }

// ResetWindow starts every rate window afresh, as time passing does.
func (s *Server) ResetWindow() { s.mu.Lock(); s.used = map[string]int{}; s.mu.Unlock() }

// FollowedWithToken is how many redirects off the Hub (to its CDN) carried
// a token.
func (s *Server) FollowedWithToken() int { s.mu.Lock(); defer s.mu.Unlock(); return s.followed }

// HubHopsWithToken is how many redirects back to the Hub carried a token.
func (s *Server) HubHopsWithToken() int { s.mu.Lock(); defer s.mu.Unlock(); return s.hubHops }

// Throttled is how many requests were answered 429.
func (s *Server) Throttled() int { s.mu.Lock(); defer s.mu.Unlock(); return s.throttled }

// Calls is the requests served, as "METHOD path".
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func gitBlob(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func sha(b []byte) string { x := sha256.Sum256(b); return hex.EncodeToString(x[:]) }

func errorAnswer(w http.ResponseWriter, status int, code, msg string) {
	if code != "" {
		w.Header().Set("X-Error-Code", code)
	}
	w.Header().Set("X-Error-Message", msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// rate counts a request against its bucket and says so in the headers;
// false when the window is spent (the request is answered 429).
func (s *Server) rate(w http.ResponseWriter, bucket string) bool {
	if s.Quota == 0 {
		return true
	}
	s.used[bucket]++
	left := max(s.Quota-s.used[bucket], 0)
	w.Header().Set("RateLimit", fmt.Sprintf(`"%s";r=%d;t=120`, bucket, left))
	if !s.NoPolicy {
		w.Header().Set("RateLimit-Policy", fmt.Sprintf(`"fixed window";"%s";q=%d;w=300`, bucket, s.Quota))
	}
	return s.used[bucket] <= s.Quota
}

func (s *Server) account(r *http.Request) (string, bool) {
	a := r.Header.Get("Authorization")
	if a == "" {
		return "", true
	}
	acct, ok := s.Tokens[strings.TrimPrefix(a, "Bearer ")]
	return acct, ok
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+r.URL.RequestURI())
	p := r.URL.EscapedPath()
	if s.DropNext > 0 {
		s.DropNext--
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
	}
	if s.FailNext > 0 {
		s.FailNext--
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway from the proxy</body></html>"))
		return
	}
	for old, nu := range s.Renames {
		for _, pre := range []string{"/api/models/", "/api/datasets/", "/", "/datasets/"} {
			if strings.HasPrefix(p, pre+old+"/") {
				w.Header().Set("Location", pre+nu+strings.TrimPrefix(p, pre+old))
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
		}
	}
	if strings.HasPrefix(p, "/served/") {
		// a hop back to the Hub from the CDN: the Hub serves the file
		if r.Header.Get("Authorization") != "" {
			s.hubHops++
		}
		parts := strings.SplitN(strings.TrimPrefix(p, "/served/"), "/", 2)
		path, _ := url.PathUnescape(parts[1])
		switch s.HopBackCommit {
		case "":
			w.Header().Set("X-Repo-Commit", parts[0])
		case "-":
		default:
			w.Header().Set("X-Repo-Commit", s.HopBackCommit)
		}
		for _, rp := range s.repos {
			if b, ok := rp.commits[parts[0]][path]; ok {
				s.write(w, path, b)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(p, "/hop/") {
		// a same-host hop is the Hub: it names the commit, and a client
		// follows it with the token
		if r.Header.Get("Authorization") != "" {
			s.hubHops++
		}
		w.Header().Set("X-Repo-Commit", strings.SplitN(strings.TrimPrefix(p, "/hop/"), "/", 2)[0])
		w.Header().Set("Location", "http://"+s.CDNHost+"/xet-bridge/"+strings.TrimPrefix(p, "/hop/")+"?"+Signature)
		w.WriteHeader(http.StatusFound)
		return
	}
	bucket := "api"
	if strings.Contains(p, "/resolve/") {
		bucket = "resolvers"
	}
	if !s.rate(w, bucket) {
		s.throttled++
		if s.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(s.RetryAfter))
		}
		errorAnswer(w, http.StatusTooManyRequests, "", "Too many requests")
		return
	}
	acct, ok := s.account(r)
	if !ok {
		errorAnswer(w, http.StatusUnauthorized, "", "Invalid credentials in Authorization header")
		return
	}
	switch {
	case p == "/api/whoami-v2":
		if acct == "" {
			errorAnswer(w, http.StatusUnauthorized, "", "Invalid credentials in Authorization header")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": acct, "type": "user"})
	case strings.HasPrefix(p, "/api/models/"), strings.HasPrefix(p, "/api/datasets/"):
		s.api(w, r, p, acct)
	default:
		s.resolve(w, r, p, acct)
	}
}

// repo finds a repository the account may see.
func (s *Server) repo(w http.ResponseWriter, typ, name, acct string) *Repo {
	rp := s.repos[typ+"/"+name]
	if rp == nil || (rp.Private && !rp.Access[acct]) {
		errorAnswer(w, http.StatusUnauthorized, "RepoNotFound", "Repository Not Found for url: "+name)
		return nil
	}
	return rp
}

func (s *Server) api(w http.ResponseWriter, r *http.Request, p, acct string) {
	typ := "model"
	rest := strings.TrimPrefix(p, "/api/models/")
	if strings.HasPrefix(p, "/api/datasets/") {
		typ, rest = "dataset", strings.TrimPrefix(p, "/api/datasets/")
	}
	parts := strings.SplitN(rest, "/", 4) // owner, name, revision|tree, rest
	if len(parts) < 4 {
		errorAnswer(w, http.StatusNotFound, "", "not found")
		return
	}
	rp := s.repo(w, typ, parts[0]+"/"+parts[1], acct)
	if rp == nil {
		return
	}
	arg, _ := url.PathUnescape(parts[3])
	switch parts[2] {
	case "revision":
		if s.HTMLError {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<!doctype html><html><head><title>500</title></head><body>Internal error, request id hidden-detail</body></html>"))
			return
		}
		commit := rp.refs[arg]
		if commit == "" && rp.commits[arg] != nil {
			commit = arg
		}
		if commit == "" {
			errorAnswer(w, http.StatusNotFound, "RevisionNotFound", "Invalid rev id: "+arg)
			return
		}
		var siblings []map[string]string
		for path := range rp.commits[commit] {
			siblings = append(siblings, map[string]string{"rfilename": path})
		}
		var gated any = false
		if rp.Gated != "" {
			gated = rp.Gated
		}
		out := map[string]any{"id": rp.Name, "sha": commit, "gated": gated, "private": rp.Private, "siblings": siblings}
		if rp.License != "" {
			out["cardData"] = map[string]any{"license": rp.License}
		}
		_ = json.NewEncoder(w).Encode(out)
	case "tree":
		files := rp.commits[arg]
		if files == nil {
			errorAnswer(w, http.StatusNotFound, "RevisionNotFound", "Invalid rev id: "+arg)
			return
		}
		paths := make([]string, 0, len(files))
		for path := range files {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		start = min(start, len(paths))
		end := min(start+s.PageSize, len(paths))
		var page []map[string]any
		for _, path := range paths[start:end] {
			b := files[path]
			e := map[string]any{"type": "file", "path": path, "size": len(b), "oid": gitBlob(b)}
			if len(b) >= LFSThreshold {
				e["lfs"] = map[string]any{"oid": sha(b), "size": len(b), "pointerSize": 133}
				e["xetHash"] = "xet-" + sha(b)[:16]
			}
			page = append(page, e)
		}
		if start == 0 {
			page = append(page, map[string]any{"type": "directory", "path": "onnx", "oid": "0000"})
			page = append(page, s.ExtraEntries...)
		}
		switch {
		case s.EndlessPages:
			q := r.URL.Query()
			q.Set("cursor", strconv.Itoa(max(end, start+1)))
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, s.Endpoint, p, q.Encode()))
			if start > len(paths) {
				page = []map[string]any{}
			}
		case s.SelfNext:
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, s.Endpoint, p, r.URL.RawQuery))
		case end < len(paths):
			q := r.URL.Query()
			q.Set("cursor", strconv.Itoa(end))
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, s.Endpoint, p, q.Encode()))
		}
		_ = json.NewEncoder(w).Encode(page)
	default:
		errorAnswer(w, http.StatusNotFound, "", "not found")
	}
}

// resolve serves /<owner>/<name>/resolve/<commit>/<path> (models) and
// /datasets/<owner>/<name>/resolve/... .
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, p, acct string) {
	typ := "model"
	if strings.HasPrefix(p, "/datasets/") {
		typ, p = "dataset", strings.TrimPrefix(p, "/datasets")
	}
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 5) // owner, name, resolve, commit, path
	if len(parts) < 5 || parts[2] != "resolve" {
		errorAnswer(w, http.StatusNotFound, "", "not found")
		return
	}
	rp := s.repo(w, typ, parts[0]+"/"+parts[1], acct)
	if rp == nil {
		return
	}
	if rp.Gated != "" && !rp.Access[acct] {
		status := http.StatusUnauthorized
		msg := "Access to model " + rp.Name + " is restricted. You must have access to it and be authenticated to access it. Please log in."
		if acct != "" {
			status = http.StatusForbidden
			msg = "Access to model " + rp.Name + " is restricted and you are not in the authorized list. Visit https://" + Hub + "/" + rp.Name + " to ask for access."
		}
		errorAnswer(w, status, "GatedRepo", msg)
		return
	}
	ref, _ := url.PathUnescape(parts[3])
	commit := rp.refs[ref]
	if commit == "" {
		commit = ref
	}
	files := rp.commits[commit]
	path, _ := url.PathUnescape(parts[4])
	b, ok := files[path]
	if files == nil || !ok {
		errorAnswer(w, http.StatusNotFound, "EntryNotFound", path+" does not exist on \""+ref+"\"")
		return
	}
	switch {
	case s.NoCommit:
	case s.LieCommit != "":
		w.Header().Set("X-Repo-Commit", s.LieCommit)
	default:
		w.Header().Set("X-Repo-Commit", commit)
	}
	if len(b) >= LFSThreshold {
		w.Header().Set("X-Linked-Etag", `"`+sha(files[path])+`"`)
		w.Header().Set("X-Linked-Size", strconv.Itoa(len(b)))
		if s.SameHostHop {
			w.Header().Set("Location", "/hop/"+commit+"/"+url.PathEscape(path))
		} else {
			w.Header().Set("Location", "http://"+s.CDNHost+"/xet-bridge/"+commit+"/"+url.PathEscape(path)+"?"+Signature)
		}
		w.WriteHeader(http.StatusFound)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(s.served(path, b))))
		return
	}
	s.write(w, path, b)
}

// write serves a file's bytes as the knobs serve them, endlessly with
// Endless (until the client goes).
func (s *Server) write(w http.ResponseWriter, path string, b []byte) {
	b = s.served(path, b)
	if !s.Endless {
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
		return
	}
	chunk := make([]byte, 32<<10)
	for {
		if _, err := w.Write(chunk); err != nil {
			return
		}
	}
}

// served is a file's bytes as the knobs serve them.
func (s *Server) served(path string, b []byte) []byte {
	if s.corrupt[path] {
		b = append([]byte("corrupt "), b...)[:len(b)]
	}
	if s.Oversize > 0 {
		b = append(append([]byte(nil), b...), make([]byte, s.Oversize)...)
	}
	return b
}

// cdn serves a large file's bytes from the redirect, after MultiHop
// redirects to itself.
func (s *Server) cdn(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "" {
		s.followed++
	}
	if n, _ := strconv.Atoi(r.URL.Query().Get("hop")); n < s.MultiHop {
		q := r.URL.Query()
		q.Set("hop", strconv.Itoa(n+1))
		w.Header().Set("Location", "http://"+r.Host+r.URL.EscapedPath()+"?"+q.Encode())
		w.WriteHeader(http.StatusFound)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/xet-bridge/"), "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	path, _ := url.PathUnescape(parts[1])
	if s.HopBack {
		w.Header().Set("Location", "http://"+Hub+"/served/"+parts[0]+"/"+parts[1])
		w.WriteHeader(http.StatusFound)
		return
	}
	if s.CDNCommit != "" {
		w.Header().Set("X-Repo-Commit", s.CDNCommit)
	}
	for _, rp := range s.repos {
		if b, ok := rp.commits[parts[0]][path]; ok {
			s.write(w, path, b)
			return
		}
	}
	http.NotFound(w, r)
}
