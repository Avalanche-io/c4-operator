package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/gitimport"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// pushRig is a bare remote, a clone that pushes to it, and an import
// service over the remote.
type pushRig struct {
	t      *testing.T
	remote string
	work   string
	svc    *importService
	st     *storeurl.Sharded
	env    []string
}

func newPushRig(t *testing.T) *pushRig {
	t.Helper()
	root := t.TempDir()
	r := &pushRig{t: t, remote: filepath.Join(root, "remote.git"), work: filepath.Join(root, "work")}
	r.env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	r.git("", "init", "-q", "--bare", "-b", "main", r.remote)
	r.git("", "clone", "-q", r.remote, r.work)
	st, err := storeurl.OpenSharded([]storeurl.Spec{storeurl.ParseSpec(t.TempDir()), storeurl.ParseSpec(t.TempDir())}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	r.st = st
	r.svc = &importService{
		base: gitimport.Options{Repo: r.remote, Sink: &gitimport.ShardedSink{S: st}, Records: view.StoreRecords(st)},
		out:  filepath.Join(root, "map.jsonl"), name: "remote", quiet: true, log: io.Discard,
	}
	return r
}

func (r *pushRig) git(dir string, args ...string) string {
	r.t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files in the clone and commits them.
func (r *pushRig) commit(files map[string]string) string {
	r.t.Helper()
	for p, c := range files {
		full := filepath.Join(r.work, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git(r.work, "add", "-A")
	r.git(r.work, "commit", "-q", "-m", "c")
	return r.git(r.work, "rev-parse", "HEAD")
}

func (r *pushRig) post(h http.Handler, body string, hdr map[string]string) (*httptest.ResponseRecorder, refResult) {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/import", strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var res refResult
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			r.t.Fatal(err)
		}
	}
	return rec, res
}

func TestWebhookImportsOnlyNewCommits(t *testing.T) {
	r := newPushRig(t)
	h := r.svc.handler("")
	r.commit(map[string]string{"a.txt": "a\n", "src/x.go": "package x\n"})
	c2 := r.commit(map[string]string{"src/x.go": "package x // 2\n"})
	r.git(r.work, "push", "-q", "origin", "main")

	rec, res := r.post(h, `{"ref":"refs/heads/main","after":"`+c2+`"}`, nil)
	if rec.Code != http.StatusOK || len(res.Commits) != 2 || res.Head != c2 {
		t.Fatalf("first push: %d %s", rec.Code, rec.Body)
	}

	c3 := r.commit(map[string]string{"src/y.go": "package x\n"})
	c4 := r.commit(map[string]string{"a.txt": "a2\n"})
	r.git(r.work, "push", "-q", "origin", "main")
	_, res = r.post(h, `{"ref":"refs/heads/main","after":"`+c4+`"}`, nil)
	if len(res.Commits) != 2 || res.Commits[0].Commit != c3 || res.Since != c2 || res.Commits[0].Base {
		t.Fatalf("second push imported %d commits since %q: %+v", len(res.Commits), res.Since, res.Commits)
	}
	if res.View != res.Commits[1].View.String() {
		t.Fatal("published view is not the head commit's")
	}
	// The published view resolves from the store through its changeset.
	resolved, err := view.FromID(res.Commits[1].Changeset, r.st, view.Filter{})
	if err != nil || resolved.View().ID != res.Commits[1].View {
		t.Fatalf("changeset does not resolve to the view: %v", err)
	}

	// A new branch at a commit already imported: nothing to import, but
	// the ref is published.
	r.git(r.work, "push", "-q", "origin", "main:refs/heads/feature")
	_, res = r.post(h, `{"ref":"refs/heads/feature","after":"`+c4+`"}`, nil)
	if len(res.Commits) != 0 || res.View == "" {
		t.Fatalf("new branch: %+v", res)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/refs", nil))
	var refs map[string]map[string]string
	json.Unmarshal(rec.Body.Bytes(), &refs)
	if refs["refs/heads/feature"]["commit"] != c4 || refs["refs/heads/main"]["view"] != res.View {
		t.Fatalf("refs: %s", rec.Body)
	}

	// Deleting the branch unpublishes it.
	r.post(h, `{"ref":"refs/heads/feature","after":"0000000000000000000000000000000000000000"}`, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/refs", nil))
	refs = nil
	json.Unmarshal(rec.Body.Bytes(), &refs)
	if _, ok := refs["refs/heads/feature"]; ok || len(refs) != 1 {
		t.Fatalf("after delete: %s", rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/commits/"+c3, nil))
	var line gitimport.Commit
	json.Unmarshal(rec.Body.Bytes(), &line)
	if rec.Code != http.StatusOK || line.Commit != c3 || line.View.IsNil() {
		t.Fatalf("commit lookup: %d %s", rec.Code, rec.Body)
	}
}

func TestWebhookAuth(t *testing.T) {
	r := newPushRig(t)
	c := r.commit(map[string]string{"a": "a"})
	r.git(r.work, "push", "-q", "origin", "main")
	h := r.svc.handler("s3cret")
	body := `{"ref":"refs/heads/main","after":"` + c + `"}`
	if rec, _ := r.post(h, body, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", rec.Code)
	}
	if rec, _ := r.post(h, body, map[string]string{"X-Hub-Signature-256": "sha256=00"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", rec.Code)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if rec, _ := r.post(h, `{"zen":"x"}`, map[string]string{"X-Hub-Signature-256": sig, "X-GitHub-Event": "ping"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("signature over another body: %d", rec.Code)
	}
	if rec, res := r.post(h, body, map[string]string{"X-Hub-Signature-256": sig}); rec.Code != http.StatusOK || len(res.Commits) != 1 {
		t.Fatalf("signed: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := r.post(h, body, map[string]string{"X-Gitlab-Token": "s3cret"}); rec.Code != http.StatusOK {
		t.Fatalf("gitlab token: %d", rec.Code)
	}
	if rec, _ := r.post(h, `{"ref":"refs/heads/main","repo":"other"}`, map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusNotFound {
		t.Fatalf("another repo: %d", rec.Code)
	}
}

func TestPostReceive(t *testing.T) {
	r := newPushRig(t)
	c1 := r.commit(map[string]string{"a": "a"})
	c2 := r.commit(map[string]string{"b": "b"})
	r.git(r.work, "push", "-q", "origin", "main")
	var out bytes.Buffer
	in := "0000000000000000000000000000000000000000 " + c2 + " refs/heads/main\n"
	if err := r.svc.postReceive(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	c3 := r.commit(map[string]string{"a": "a2"})
	r.git(r.work, "push", "-q", "origin", "main")
	in = c2 + " " + c3 + " refs/heads/main\n"
	out.Reset()
	if err := r.svc.postReceive(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 commits imported") {
		t.Fatalf("post-receive output %q", out.String())
	}
	m, _ := gitimport.ReadMapping(r.svc.out)
	if len(m) != 3 || m[0].Commit != c1 || m[2].Commit != c3 || m[2].Ref != "refs/heads/main" {
		t.Fatalf("mapping: %+v", m)
	}
}
