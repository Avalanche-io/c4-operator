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
package hftest

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// Server is the fake Hub, and CDN the host large files redirect to.
type Server struct {
	*httptest.Server
	CDN *httptest.Server
	// Tokens maps a token to its account.
	Tokens map[string]string
	// PageSize is how many entries a listing page holds (default 2, so
	// tests page).
	PageSize int
	// Quota is each rate bucket's requests per window (0: unlimited).
	Quota int

	mu    sync.Mutex
	repos map[string]*Repo
	used  map[string]int
	// CDNAuth counts CDN requests that carried a token: it must stay 0.
	cdnAuth int
	// corrupt serves wrong bytes for a path.
	corrupt   map[string]bool
	calls     []string
	throttled int
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
	s := &Server{Tokens: map[string]string{}, PageSize: 2, repos: map[string]*Repo{}, used: map[string]int{}, corrupt: map[string]bool{}}
	s.CDN = httptest.NewServer(http.HandlerFunc(s.cdn))
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Close stops both servers.
func (s *Server) Close() { s.Server.Close(); s.CDN.Close() }

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

// CDNAuth is how many CDN requests carried a token.
func (s *Server) CDNAuth() int { s.mu.Lock(); defer s.mu.Unlock(); return s.cdnAuth }

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
	left := s.Quota - s.used[bucket]
	if left < 0 {
		left = 0
	}
	w.Header().Set("RateLimit", fmt.Sprintf(`"%s";r=%d;t=120`, bucket, left))
	w.Header().Set("RateLimit-Policy", fmt.Sprintf(`"fixed window";"%s";q=%d;w=300`, bucket, s.Quota))
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
	bucket := "api"
	if strings.Contains(r.URL.Path, "/resolve/") {
		bucket = "resolvers"
	}
	if !s.rate(w, bucket) {
		s.throttled++
		errorAnswer(w, http.StatusTooManyRequests, "", "Too many requests")
		return
	}
	acct, ok := s.account(r)
	if !ok {
		errorAnswer(w, http.StatusUnauthorized, "", "Invalid credentials in Authorization header")
		return
	}
	p := r.URL.EscapedPath()
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
		}
		if end < len(paths) {
			q := r.URL.Query()
			q.Set("cursor", strconv.Itoa(end))
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, s.URL, p, q.Encode()))
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
		errorAnswer(w, http.StatusUnauthorized, "GatedRepo", "Access to model "+rp.Name+" is restricted. You must have access to it and be authenticated to access it. Please log in.")
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
	w.Header().Set("X-Repo-Commit", commit)
	if s.corrupt[path] {
		b = append([]byte("corrupt "), b...)[:len(b)]
	}
	if len(b) >= LFSThreshold {
		w.Header().Set("X-Linked-Etag", `"`+sha(files[path])+`"`)
		w.Header().Set("X-Linked-Size", strconv.Itoa(len(b)))
		w.Header().Set("Location", s.CDN.URL+"/xet-bridge/"+commit+"/"+url.PathEscape(path))
		w.WriteHeader(http.StatusFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}

// cdn serves a large file's bytes from the redirect.
func (s *Server) cdn(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "" {
		s.cdnAuth++
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/xet-bridge/"), "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	path, _ := url.PathUnescape(parts[1])
	for _, rp := range s.repos {
		if b, ok := rp.commits[parts[0]][path]; ok {
			if s.corrupt[path] {
				b = append([]byte("corrupt "), b...)[:len(b)]
			}
			_, _ = w.Write(b)
			return
		}
	}
	http.NotFound(w, r)
}
