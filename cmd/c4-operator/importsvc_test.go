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

// object returns the store's file of an object, or "" when the store does
// not hold it.
func (r *pushRig) object(id c4.ID) string {
	found := ""
	for _, dir := range r.shards {
		filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			if f, err := os.Open(p); err == nil {
				if c4.Identify(f) == id {
					found = p
				}
				f.Close()
			}
			return nil
		})
	}
	return found
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

	// Another ref, with lines after the ones damaged below: a way out that
	// cuts the mapping back takes every ref's lines from there on.
	r.git(r.work, "checkout", "-q", "-b", "dev")
	dev := r.commit(map[string]string{"dev.txt": "dev\n"})
	r.git(r.work, "push", "-q", "origin", "dev")
	r.git(r.work, "checkout", "-q", "main")
	postDev := func() refResult {
		t.Helper()
		rec, res := r.post(h, `{"ref":"refs/heads/dev","after":"`+dev+`"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("push dev: %d %s", rec.Code, rec.Body)
		}
		return res
	}
	devWas := postDev()
	refs := func() map[string]map[string]string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/refs", nil))
		out := map[string]map[string]string{}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET /refs: %d %s", rec.Code, rec.Body)
		}
		return out
	}
	object := func(id c4.ID) string {
		t.Helper()
		found := r.object(id)
		if found == "" {
			t.Fatalf("%s is not in the store", id)
		}
		return found
	}
	// cutBack does what an error says: it cuts the mapping back to before
	// the line the error names, and imports again every ref that had a line
	// from there on. Then main's chain is as it was recorded, changeset for
	// changeset, and dev is published as it was.
	cNew := r.commit(map[string]string{"e.txt": "e\n"})
	r.git(r.work, "push", "-q", "origin", "main")
	postMain := func() (*httptest.ResponseRecorder, refResult) {
		return r.post(h, `{"ref":"refs/heads/main","after":"`+cNew+`"}`, nil)
	}
	if rec, _ := postMain(); rec.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	cutBack := func(what, body string, was gitimport.RecordedChain) {
		t.Helper()
		named := regexp.MustCompile(`cut the mapping back to before line (\d+) and import again every ref that has a line from there on`).FindStringSubmatch(body)
		if named == nil {
			t.Fatalf("%s: the error does not name the line to cut back to and every ref after it: %s", what, body)
		}
		var line int
		fmt.Sscan(named[1], &line)
		m, _ := os.ReadFile(r.svc.out)
		if err := os.WriteFile(r.svc.out, bytes.Join(bytes.SplitAfter(m, []byte("\n"))[:line-1], nil), 0o644); err != nil {
			t.Fatal(err)
		}
		rec, res := postMain()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: after cutting the mapping back to before line %d: %d %s", what, line, rec.Code, rec.Body)
		}
		headIs("after "+what+" and the cut back", res)
		after, _ := gitimport.ReadMapping(r.svc.out)
		now, _ := gitimport.ChainRecorded(after, "refs/heads/main")
		if len(now.Changesets) != len(was.Changesets) || now.Head != was.Head || now.Base != was.Base {
			t.Fatalf("%s: the chain recorded has %d changesets, had %d", what, len(now.Changesets), len(was.Changesets))
		}
		for i, id := range was.Changesets {
			if now.Changesets[i] != id {
				t.Fatalf("%s: changeset %d was not imported again to the same C4 ID", what, i+1)
			}
		}
		// The cut took dev's lines; until dev is imported again it is not
		// published.
		if at, ok := refs()["refs/heads/dev"]; ok {
			t.Fatalf("%s: dev is still published after the cut, at %v; the test tells nothing of the other refs", what, at)
		}
		if res := postDev(); res.View != devWas.View || refs()["refs/heads/dev"]["commit"] != dev || refs()["refs/heads/dev"]["view"] != devWas.View {
			t.Fatalf("%s: dev imported again is at %v, was at view %s", what, refs()["refs/heads/dev"], devWas.View)
		}
	}
	recordedNow := func() gitimport.RecordedChain {
		t.Helper()
		m, err := gitimport.ReadMapping(r.svc.out)
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := gitimport.ChainRecorded(m, "refs/heads/main")
		return rc
	}

	// A changeset the mapping names is gone from the store, and the chain
	// file with it: the first commit's, which was imported in full, and one
	// in the middle. A reader is told the next import makes it again. The
	// next import does, from the repository: the commit is imported again,
	// comes to the same changeset, and that is stored. No line of the
	// mapping is cut or added, and the other ref is as it was.
	var logged bytes.Buffer
	r.svc.log = &logged
	rc := recordedNow()
	lines, _ := gitimport.ReadMapping(r.svc.out)
	if !lines[rc.Lines[0]-1].Base {
		t.Fatal("the chain's first line is not a commit imported in full; the test tells nothing of one")
	}
	mappingWas, _ := os.ReadFile(r.svc.out)
	for _, lost := range []int{0, len(rc.Changesets) - 3} {
		what := fmt.Sprintf("with changeset %d of %d gone from the store", lost+1, len(rc.Changesets))
		os.Remove(object(rc.Changesets[lost]))
		os.Remove(chainPath)
		got := httptest.NewRecorder()
		h.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/chains/refs/heads/main", nil))
		if got.Code == http.StatusOK || !strings.Contains(got.Body.String(), rc.Changesets[lost].String()) || !strings.Contains(got.Body.String(), fmt.Sprintf("(mapping line %d) cannot be read from the store", rc.Lines[lost])) || !strings.Contains(got.Body.String(), "The next import of the ref makes it again") {
			t.Fatalf("%s, a reader: %d %s", what, got.Code, got.Body)
		}
		logged.Reset()
		rec, res := postMain()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s, the next import: %d %s", what, rec.Code, rec.Body)
		}
		if want := fmt.Sprintf("the changeset %s of commit %s (mapping line %d) was not in the store and was made again from the repository", rc.Changesets[lost], rc.Commits[lost], rc.Lines[lost]); !strings.Contains(logged.String(), want) {
			t.Fatalf("%s: logged %q, want %q", what, logged.String(), want)
		}
		object(rc.Changesets[lost])
		headIs(what, res)
		if now, _ := os.ReadFile(r.svc.out); !bytes.Equal(now, mappingWas) {
			t.Fatalf("%s: the mapping was written", what)
		}
		if at := refs()["refs/heads/dev"]; at["commit"] != dev || at["view"] != devWas.View {
			t.Fatalf("%s: dev is at %v", what, at)
		}
		full, err := view.ReadChain(chains())
		if err != nil || full.Base != rc.Base || len(full.Sections) != len(rc.Changesets) {
			t.Fatalf("%s: the chain served has %d sections (%v), want all %d", what, len(full.Sections), err, len(rc.Changesets))
		}
	}

	// A wrong object under a changeset's C4 ID (cut short). A store keeps
	// the object it has, so importing the commit again would not replace
	// it: the error says it has to be removed. Removed, the next import
	// makes the changeset again as it was, with no line of the mapping cut.
	lost := len(rc.Changesets) - 3
	wrong := object(rc.Changesets[lost])
	stored, _ := os.ReadFile(wrong)
	os.Chmod(wrong, 0o644)
	if err := os.WriteFile(wrong, stored[:len(stored)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(chainPath)
	rec, _ := postMain()
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "wrong object") || !strings.Contains(rec.Body.String(), "has to be removed from the store") || strings.Contains(rec.Body.String(), "cut the mapping") {
		t.Fatalf("with a wrong object under a changeset's C4 ID: %d %s", rec.Code, rec.Body)
	}
	if now, _ := os.ReadFile(wrong); len(now) != len(stored)/2 {
		t.Fatal("the wrong object was written over")
	}
	if err := os.Remove(wrong); err != nil {
		t.Fatal(err)
	}
	rec, res = postMain()
	if rec.Code != http.StatusOK {
		t.Fatalf("after the wrong object was removed: %d %s", rec.Code, rec.Body)
	}
	headIs("after the wrong object was removed", res)
	if now, _ := os.ReadFile(object(rc.Changesets[lost])); !bytes.Equal(now, stored) {
		t.Fatal("the changeset made again is not the bytes that were stored")
	}
	if now, _ := os.ReadFile(r.svc.out); !bytes.Equal(now, mappingWas) {
		t.Fatal("the mapping was written")
	}

	// A line of zeros in the middle of the mapping, with whole lines after
	// it (a block that never reached the disk under later ones that did).
	// The mapping cannot be read past it, and what it held is not guessed:
	// every import and reader of the repository stops with the line to cut
	// back to.
	rc = recordedNow()
	m, _ := os.ReadFile(r.svc.out)
	raw := bytes.SplitAfter(m, []byte("\n"))
	at := rc.Lines[len(rc.Lines)-3] - 1
	raw[at] = append(make([]byte, len(raw[at])-1), 0x0a)
	if err := os.WriteFile(r.svc.out, bytes.Join(raw, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, _ = postMain()
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), fmt.Sprintf("mapping line %d does not read", at+1)) {
		t.Fatalf("a line of zeros in the middle of the mapping: %d %s", rec.Code, rec.Body)
	}
	got := httptest.NewRecorder()
	h.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/chains/refs/heads/dev", nil))
	if got.Code == http.StatusOK {
		t.Fatal("a reader was served a chain from a mapping that does not read")
	}
	cutBack("a line of zeros in the middle of the mapping", rec.Body.String(), rc)

	// A line gone from the middle of the mapping, the lines round it
	// whole. Nothing stops and no chain is read into the missing line: the
	// chain recorded, made and served is the one from where the lines
	// resume, which still resolves to the ref's view. The commit whose line
	// is gone is no longer published, and nothing brings it back but a cut
	// to before it.
	rc = recordedNow()
	m, _ = os.ReadFile(r.svc.out)
	raw = bytes.SplitAfter(m, []byte("\n"))
	at = rc.Lines[len(rc.Lines)-3] - 1
	goneCommit := rc.Commits[len(rc.Commits)-3]
	if err := os.WriteFile(r.svc.out, bytes.Join(append(append([][]byte(nil), raw[:at]...), raw[at+1:]...), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	logged.Reset()
	rec, res = postMain()
	if rec.Code != http.StatusOK || !strings.Contains(logged.String(), "chain file for refs/heads/main made again") {
		t.Fatalf("with a line gone from the middle of the mapping: %d %s; logged %q", rec.Code, rec.Body, logged.String())
	}
	headIs("with a line gone from the middle of the mapping", res)
	// It is said where an operator will see it: in the log line, in the
	// import's result, and in /refs for that ref and no other.
	resumed := at + 1 // the line after the one gone is where it was
	if want := fmt.Sprintf("starts at mapping line %d because the lines before it are missing", resumed); !strings.Contains(logged.String(), want) {
		t.Fatalf("logged %q, want %q in it", logged.String(), want)
	}
	if res.ChainMissingBefore != resumed || refs()["refs/heads/main"]["chainMissingBefore"] != fmt.Sprint(resumed) {
		t.Fatalf("the result says lines are missing before line %d and /refs %v, want %d", res.ChainMissingBefore, refs()["refs/heads/main"], resumed)
	}
	if _, ok := refs()["refs/heads/dev"]["chainMissingBefore"]; ok || devWas.ChainMissingBefore != 0 {
		t.Fatalf("dev, whose lines are all there, is said to have lines missing: %v", refs()["refs/heads/dev"])
	}
	if now := recordedNow(); len(now.Changesets) != 2 || now.Head != rc.Head || now.Changesets[1] != rc.Changesets[len(rc.Changesets)-1] {
		t.Fatalf("the chain recorded after a line went: %d changesets from %s", len(now.Changesets), now.Base)
	}
	got = httptest.NewRecorder()
	h.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/commits/"+goneCommit, nil))
	if got.Code != http.StatusNotFound {
		t.Fatalf("the commit whose line is gone: %d", got.Code)
	}
}
