package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"

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
	shards []string // the store's folders
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
	r.shards = []string{t.TempDir(), t.TempDir()}
	st, err := storeurl.OpenSharded([]storeurl.Spec{storeurl.ParseSpec(r.shards[0]), storeurl.ParseSpec(r.shards[1])}, storeurl.Credentials{})
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

// A crash can cut the chain file's last changeset or the mapping's last
// line. Through the service, with real pushes: after each kind of damage
// the next push imports, the mapping reads, the chain served is the chain
// of the commits imported, and the ref's view is the head commit's. Before,
// a chain file cut in an entry's name opened as a chain to a view no
// commit made, and most other cuts stopped the ref's imports for good.
func TestTheNextPushAfterATornFile(t *testing.T) {
	r := newPushRig(t)
	h := r.svc.handler("")
	chains := func() []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chains/refs/heads/main", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /chains: %d %s", rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	push := func(files map[string]string) (string, refResult) {
		t.Helper()
		c := r.commit(files)
		r.git(r.work, "push", "-q", "origin", "main")
		rec, res := r.post(h, `{"ref":"refs/heads/main","after":"`+c+`"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("push: %d %s", rec.Code, rec.Body)
		}
		return c, res
	}
	// The chain a reader is served must resolve to the view of head.
	headIs := func(what string, res refResult) {
		t.Helper()
		c, err := view.ReadChain(chains())
		if err != nil {
			t.Fatalf("%s: the chain served does not read: %v", what, err)
		}
		views, err := c.Verify(context.Background(), r.svc.base.Records)
		if err != nil || len(views) == 0 || views[len(views)-1].String() != res.View {
			t.Fatalf("%s: the chain served resolves to %v (%v), the ref's view is %s", what, views, err, res.View)
		}
	}
	r.commit(map[string]string{"a.txt": "a\n", "dir/same.txt": "one\n"})
	push(map[string]string{"dir/same.txt": "two\n"})
	_, res := push(map[string]string{"dir/more.txt": "more\n"})
	headIs("before any damage", res)
	chainPath := r.svc.chainPath("refs/heads/main")
	whole, err := os.ReadFile(chainPath)
	if err != nil {
		t.Fatal(err)
	}

	// The chain file cut in its last entry's name: it still parses.
	cut := bytes.LastIndex(whole, []byte("more.txt")) + len("mo")
	if err := os.WriteFile(chainPath, whole[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	if got := chains(); !bytes.Equal(got, whole) {
		t.Fatalf("a reader was served the torn chain:\n%s", got)
	}
	_, res = push(map[string]string{"b.txt": "b\n"})
	if len(res.Commits) != 1 {
		t.Fatalf("after a torn chain file the push imported %d commits", len(res.Commits))
	}
	headIs("after a torn chain file", res)

	// The chain file cut where it no longer parses.
	whole, _ = os.ReadFile(chainPath)
	if err := os.WriteFile(chainPath, whole[:len(whole)-30], 0o644); err != nil {
		t.Fatal(err)
	}
	_, res = push(map[string]string{"c.txt": "c\n"})
	headIs("after a chain file that does not parse", res)

	// The mapping's last line cut, and then two whole lines gone as well
	// (an append that never reached the disk), and then its tail zeros:
	// those commits are imported again, to the same changesets, onto the
	// chain as the shorter mapping records it.
	for _, damage := range []struct {
		name string
		do   func(m []byte) []byte
	}{
		{"a cut last line", func(m []byte) []byte { return m[:len(m)-25] }},
		{"the last two lines gone", func(m []byte) []byte {
			m = m[:bytes.LastIndexByte(m[:len(m)-1], '\n')+1]
			return m[:bytes.LastIndexByte(m[:len(m)-1], '\n')+1]
		}},
		{"a tail of zeros", func(m []byte) []byte {
			cut := bytes.LastIndexByte(m[:len(m)-1], '\n') + 10
			return append(append([]byte(nil), m[:cut]...), make([]byte, len(m)-cut)...)
		}},
	} {
		m, err := os.ReadFile(r.svc.out)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := gitimport.ReadMapping(r.svc.out)
		if err := os.WriteFile(r.svc.out, damage.do(m), 0o644); err != nil {
			t.Fatal(err)
		}
		c, res := push(map[string]string{"d.txt": damage.name + "\n"})
		after, err := gitimport.ReadMapping(r.svc.out)
		if err != nil {
			t.Fatalf("%s: the mapping does not read: %v", damage.name, err)
		}
		if gitimport.LastByRef(after)["refs/heads/main"] != c {
			t.Fatalf("%s: main is at %s", damage.name, gitimport.LastByRef(after)["refs/heads/main"])
		}
		// Every commit the mapping had is in it again with the same view
		// and the same changeset.
		now := map[string]gitimport.Commit{}
		for _, line := range after {
			if line.Commit != "" && !line.RefOnly {
				now[line.Commit] = line
			}
		}
		for _, line := range before {
			if line.Commit == "" || line.RefOnly {
				continue
			}
			if got, ok := now[line.Commit]; !ok || got.View != line.View || got.Changeset != line.Changeset {
				t.Fatalf("%s: commit %s is not in the mapping as it was: %+v", damage.name, line.Commit, got)
			}
		}
		headIs("after "+damage.name, res)
	}

	// Made again is not thrown away: the chain served still starts at the
	// first commit's parent and has a section for every commit that
	// changed something.
	full, err := view.ReadChain(chains())
	if err != nil {
		t.Fatal(err)
	}
	final, _ := gitimport.ReadMapping(r.svc.out)
	changed := map[string]bool{}
	for _, line := range final {
		if line.Ref == "refs/heads/main" && !line.Changeset.IsNil() {
			changed[line.Commit] = true
		}
	}
	if full.Base != view.EmptyID() || len(full.Sections) != len(changed) {
		t.Fatalf("the chain has %d sections from %s, want %d from the empty view", len(full.Sections), full.Base, len(changed))
	}

	// A changeset the mapping names is gone from the store, and the chain
	// file with it. The import says which, where the mapping records it,
	// and what to do; done, the next push imports those commits again to
	// the same changesets.
	rc, _ := gitimport.ChainRecorded(final, "refs/heads/main")
	lost := len(rc.Changesets) - 2
	removed := 0
	for _, dir := range r.shards {
		filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			if f, err := os.Open(p); err == nil {
				id := c4.Identify(f)
				f.Close()
				if id == rc.Changesets[lost] {
					os.Remove(p)
					removed++
				}
			}
			return nil
		})
	}
	if removed == 0 {
		t.Fatal("the changeset was not found in the store to remove")
	}
	os.Remove(chainPath)
	cNew := r.commit(map[string]string{"e.txt": "e\n"})
	r.git(r.work, "push", "-q", "origin", "main")
	rec, _ := r.post(h, `{"ref":"refs/heads/main","after":"`+cNew+`"}`, nil)
	wantLine := fmt.Sprintf("cut the mapping back to before line %d", rc.Lines[lost])
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), rc.Changesets[lost].String()) || !strings.Contains(rec.Body.String(), wantLine) {
		t.Fatalf("with a recorded changeset gone from the store: %d %s\nwant an error naming it and %q", rec.Code, rec.Body, wantLine)
	}
	// The way out the error names.
	m, _ := os.ReadFile(r.svc.out)
	lines := bytes.SplitAfter(m, []byte("\n"))
	if err := os.WriteFile(r.svc.out, bytes.Join(lines[:rc.Lines[lost]-1], nil), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, res = r.post(h, `{"ref":"refs/heads/main","after":"`+cNew+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("after cutting the mapping back: %d %s", rec.Code, rec.Body)
	}
	headIs("after cutting the mapping back", res)
	again, _ := gitimport.ReadMapping(r.svc.out)
	rc2, _ := gitimport.ChainRecorded(again, "refs/heads/main")
	if len(rc2.Changesets) != len(rc.Changesets)+1 || rc2.Changesets[lost] != rc.Changesets[lost] {
		t.Fatalf("the commits were not imported again to the same changesets: %d recorded, were %d", len(rc2.Changesets), len(rc.Changesets))
	}

	// Damage in the middle of the mapping, with whole lines after it (blocks
	// that never reached the disk under later ones that did): a line gone,
	// and a line of zeros. Neither is mended by guessing. The import and
	// the readers stop with the line to cut the mapping back to; done, the
	// same push imports those commits again, to the same changesets.
	for _, damage := range []struct {
		name, says string
		do         func(lines [][]byte, at int) [][]byte
	}{
		{"a line gone from the middle", "lines are missing", func(lines [][]byte, at int) [][]byte {
			return append(append([][]byte(nil), lines[:at]...), lines[at+1:]...)
		}},
		{"a line of zeros in the middle", "does not read", func(lines [][]byte, at int) [][]byte {
			out := append([][]byte(nil), lines...)
			out[at] = append(make([]byte, len(lines[at])-1), 0x0a)
			return out
		}},
	} {
		before, _ := gitimport.ReadMapping(r.svc.out)
		rc, _ := gitimport.ChainRecorded(before, "refs/heads/main")
		m, _ := os.ReadFile(r.svc.out)
		lines := bytes.SplitAfter(m, []byte("\n"))
		at := rc.Lines[len(rc.Lines)-3] - 1
		if err := os.WriteFile(r.svc.out, bytes.Join(damage.do(lines, at), nil), 0o644); err != nil {
			t.Fatal(err)
		}
		rec, _ := r.post(h, `{"ref":"refs/heads/main","after":"`+cNew+`"}`, nil)
		named := regexp.MustCompile(`cut the mapping back to before line (\d+) `).FindStringSubmatch(rec.Body.String())
		if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), damage.says) || named == nil {
			t.Fatalf("%s: %d %s\nwant an error that says %q and names the line to cut back to", damage.name, rec.Code, rec.Body, damage.says)
		}
		got := httptest.NewRecorder()
		h.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/chains/refs/heads/main", nil))
		if got.Code == http.StatusOK {
			t.Fatalf("%s: a reader was served a chain", damage.name)
		}
		// The way out the error names.
		var line int
		fmt.Sscan(named[1], &line)
		m, _ = os.ReadFile(r.svc.out)
		if err := os.WriteFile(r.svc.out, bytes.Join(bytes.SplitAfter(m, []byte("\n"))[:line-1], nil), 0o644); err != nil {
			t.Fatal(err)
		}
		rec, res = r.post(h, `{"ref":"refs/heads/main","after":"`+cNew+`"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: after cutting the mapping back to before line %d: %d %s", damage.name, line, rec.Code, rec.Body)
		}
		headIs("after "+damage.name+" and the cut back", res)
		after, _ := gitimport.ReadMapping(r.svc.out)
		now, _ := gitimport.ChainRecorded(after, "refs/heads/main")
		if len(now.Changesets) != len(rc.Changesets) || now.Head != rc.Head {
			t.Fatalf("%s: the chain recorded has %d changesets, had %d", damage.name, len(now.Changesets), len(rc.Changesets))
		}
		for i, id := range rc.Changesets {
			if now.Changesets[i] != id {
				t.Fatalf("%s: changeset %d was not imported again to the same C4 ID", damage.name, i+1)
			}
		}
	}
}
