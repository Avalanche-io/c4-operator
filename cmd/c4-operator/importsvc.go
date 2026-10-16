package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/gitimport"
)

// importService runs imports of one repository into one store, one at a
// time, recording each in the mapping file. The one-shot command, the
// post-receive hook and the webhook listener all go through it.
type importService struct {
	base  gitimport.Options // repository, store and limits; no Rev or Ref
	out   string            // mapping file, or ""
	name  string            // repository name a webhook may give
	fetch bool              // fetch from origin before importing (a mirror)
	quiet bool
	log   io.Writer

	mu sync.Mutex
}

// refResult is what one import of one ref did, as the listener returns it.
type refResult struct {
	Ref     string             `json:"ref,omitempty"`
	Head    string             `json:"head,omitempty"`
	View    string             `json:"view,omitempty"`
	Since   string             `json:"since,omitempty"`
	Deleted bool               `json:"deleted,omitempty"`
	Commits []gitimport.Commit `json:"commits"`
	Skipped int                `json:"skipped"`
	FetchMs float64            `json:"fetchMs"`
	TotalMs float64            `json:"totalMs"`

	summary *gitimport.Summary
}

// importRef imports rev (ref when empty) and labels the commits with ref.
// With sinceLast it starts after the newest commit the mapping records
// for ref, so a push costs only its new commits.
func (s *importService) importRef(ctx context.Context, ref, rev string, sinceLast bool) (*refResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := time.Now()
	res := &refResult{Ref: ref}
	if s.fetch {
		t := time.Now()
		if out, err := exec.CommandContext(ctx, "git", "-C", s.base.Repo, "fetch", "--quiet", "--prune", "origin").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git fetch: %w: %s", err, strings.TrimSpace(string(out)))
		}
		res.FetchMs = msSince(t)
	}
	if rev == "" {
		rev = ref
	}
	if rev == "" {
		rev = "HEAD"
	}
	head, err := gitimport.ResolveCommit(ctx, s.base.Repo, rev)
	if err != nil {
		return nil, err
	}
	res.Head = head

	opts := s.base
	opts.Rev, opts.Ref = head, ref
	// The writer is opened (and the file locked) before the mapping is
	// read, so a concurrent import's lines are seen.
	var mw *gitimport.MappingWriter
	var prior []gitimport.Commit
	if s.out != "" {
		if mw, err = gitimport.AppendMapping(s.out); err != nil {
			return nil, err
		}
		defer mw.Close()
		if prior, err = gitimport.ReadMapping(s.out); err != nil {
			return nil, err
		}
		opts.Known = gitimport.KnownViews(prior)
		opts.KnownFiles = gitimport.KnownFiles(prior)
		if last := gitimport.LastByRef(prior)[ref]; sinceLast && ref != "" && last != "" {
			opts.Since = last
			res.Since = last
		}
	}
	var writeErr error
	opts.Progress = func(c gitimport.Commit) {
		if mw != nil && writeErr == nil {
			writeErr = mw.Write(c)
		}
		if !s.quiet {
			printCommitTo(s.log, c)
		}
	}
	sum, err := gitimport.Run(ctx, opts)
	if err != nil {
		return nil, err
	}
	if writeErr != nil {
		return nil, fmt.Errorf("mapping file: %w", writeErr)
	}
	res.summary = sum
	res.Commits = sum.Commits
	res.Skipped = sum.Skipped

	// Publish where the ref points. A ref moved to a commit imported
	// earlier (a new branch, a fast-forward to a known commit) gets a
	// ref-only line.
	known := gitimport.KnownViews(prior)
	for _, c := range sum.Commits {
		known[c.Commit] = c.View
	}
	if v, ok := known[head]; ok {
		res.View = v.String()
	}
	moved := len(sum.Commits) == 0 || sum.Commits[len(sum.Commits)-1].Commit != head
	if mw != nil && ref != "" && moved && gitimport.LastByRef(prior)[ref] != head {
		if err := mw.Write(gitimport.Commit{Commit: head, Ref: ref, RefOnly: true, View: known[head]}); err != nil {
			return nil, fmt.Errorf("mapping file: %w", err)
		}
	}
	res.TotalMs = msSince(start)
	return res, nil
}

// deleteRef records that ref no longer exists.
func (s *importService) deleteRef(ref string) (*refResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &refResult{Ref: ref, Deleted: true, Commits: []gitimport.Commit{}}
	if s.out == "" {
		return res, nil
	}
	mw, err := gitimport.AppendMapping(s.out)
	if err != nil {
		return nil, err
	}
	defer mw.Close()
	return res, mw.Write(gitimport.Commit{Ref: ref, RefOnly: true})
}

func (r *refResult) line() string {
	if r.Deleted {
		return fmt.Sprintf("c4: %s deleted", r.Ref)
	}
	return fmt.Sprintf("c4: %s at %s: %d commits imported, view %s (%.0fms)", r.Ref, short(r.Head), len(r.Commits), r.View, r.TotalMs)
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

func isZero(sha string) bool { return sha != "" && strings.Trim(sha, "0") == "" }

// postReceive reads a post-receive hook's standard input ("<old> <new>
// <ref>" per updated ref) and imports each ref's new commits. Installed in
// a self-hosted remote, it makes the push itself publish the views: git
// waits for the hook, so `git push` returns after the views are stored.
func (s *importService) postReceive(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			continue
		}
		newSHA, ref := f[1], f[2]
		var res *refResult
		var err error
		if isZero(newSHA) {
			res, err = s.deleteRef(ref)
		} else {
			res, err = s.importRef(ctx, ref, newSHA, true)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		fmt.Fprintln(out, res.line())
	}
	return sc.Err()
}

// pushEvent is the part of a push webhook the listener reads. GitHub,
// GitLab and Gitea push payloads all carry ref and after.
type pushEvent struct {
	Repo  string `json:"repo"`
	Ref   string `json:"ref"`
	After string `json:"after"`
}

// listen serves:
//
//	POST /import          a push webhook: {"ref": ..., "after": ..., "repo": ...}
//	GET  /refs            each ref's commit and view
//	GET  /commits/<sha>   the mapping line of a commit
//
// With a secret, POST /import must carry a GitHub signature
// (X-Hub-Signature-256), a GitLab token (X-Gitlab-Token), or
// "Authorization: Bearer <secret>".
func (s *importService) listen(ctx context.Context, addr, secret string) error {
	srv := &http.Server{Addr: addr, Handler: s.handler(secret), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	log.Printf("import-git: listening on %s for %s", addr, s.name)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *importService) handler(secret string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /import", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !authorized(r, body, secret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if ev := r.Header.Get("X-GitHub-Event"); ev != "" && ev != "push" {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, "ignored %s event\n", ev)
			return
		}
		var ev pushEvent
		if err := json.Unmarshal(body, &ev); err != nil || ev.Ref == "" {
			http.Error(w, "want a JSON body with ref (and after)", http.StatusBadRequest)
			return
		}
		if ev.Repo != "" && ev.Repo != s.name {
			http.Error(w, "this listener imports "+s.name, http.StatusNotFound)
			return
		}
		// The import outlives a client that stops waiting.
		var res *refResult
		if isZero(ev.After) {
			res, err = s.deleteRef(ev.Ref)
		} else {
			res, err = s.importRef(context.WithoutCancel(r.Context()), ev.Ref, ev.After, true)
		}
		if err != nil {
			log.Printf("import %s: %v", ev.Ref, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Print(res.line())
		writeJSON(w, res)
	})
	mux.HandleFunc("GET /refs", func(w http.ResponseWriter, r *http.Request) {
		m, err := gitimport.ReadMapping(s.out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		views := gitimport.KnownViews(m)
		out := map[string]map[string]string{}
		for ref, sha := range gitimport.LastByRef(m) {
			if sha == "" {
				continue
			}
			out[ref] = map[string]string{"commit": sha, "view": views[sha].String()}
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		m, err := gitimport.ReadMapping(s.out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sha := r.PathValue("sha")
		for i := len(m) - 1; i >= 0; i-- {
			if m[i].Commit == sha && !m[i].RefOnly {
				writeJSON(w, m[i])
				return
			}
		}
		http.NotFound(w, r)
	})
	return mux
}

func authorized(r *http.Request, body []byte, secret string) bool {
	if secret == "" {
		return true
	}
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(sig), []byte(want))
	}
	tok := r.Header.Get("X-Gitlab-Token")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(secret)) == 1
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
