package main

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/gitimport"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Ordinary git use, with no file damaged. A chain file is opened as the
// mapping records it and made again when it is not that chain, so the
// record has to be right for everything a service can write: if it is
// wrong, a healthy file is made again, or cannot be, and the ref's imports
// stop. These tests damage nothing, so any import that fails and any file
// made again is the importer's own doing.

// ordinary is a push rig whose service logs what it repairs.
type ordinary struct {
	*pushRig
	h   http.Handler
	log *bytes.Buffer
}

func newOrdinary(t *testing.T, chainMax int) *ordinary {
	r := newPushRig(t)
	o := &ordinary{pushRig: r, log: &bytes.Buffer{}}
	r.svc.log = o.log
	r.svc.chainMax = chainMax
	o.h = r.svc.handler("")
	return o
}

// push force-pushes HEAD to branch and tells the service.
func (o *ordinary) push(branch string) (int, string) {
	o.t.Helper()
	sha := o.git(o.work, "rev-parse", "HEAD")
	o.git(o.work, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+branch)
	rec, _ := o.post(o.h, `{"ref":"refs/heads/`+branch+`","after":"`+sha+`"}`, nil)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func (o *ordinary) empty() string {
	o.git(o.work, "commit", "-q", "--allow-empty", "-m", "empty")
	return o.git(o.work, "rev-parse", "HEAD")
}

// served returns the chain a reader gets for branch, checked: it reads and
// every view it names is the one its sections produce.
func (o *ordinary) served(what, branch string) {
	o.t.Helper()
	rec := httptest.NewRecorder()
	o.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chains/refs/heads/"+branch, nil))
	if rec.Code == http.StatusNotFound {
		return
	}
	if rec.Code != http.StatusOK {
		o.t.Errorf("%s: GET /chains %s: %d %s", what, branch, rec.Code, rec.Body)
		return
	}
	c, err := view.ReadChain(rec.Body.Bytes())
	if err != nil {
		o.t.Errorf("%s: the chain of %s does not read: %v", what, branch, err)
		return
	}
	if _, err := c.Verify(context.Background(), o.svc.base.Records); err != nil {
		o.t.Errorf("%s: the chain of %s does not verify: %v", what, branch, err)
	}
}

// ok pushes and fails the test if the import failed or a file was made
// again.
func (o *ordinary) ok(what, branch string) {
	o.t.Helper()
	if code, body := o.push(branch); code != http.StatusOK {
		o.t.Fatalf("%s: the import failed with nothing damaged: %d %s", what, code, body)
	}
	if o.log.Len() > 0 {
		o.t.Fatalf("%s: a healthy chain file was made again: %s", what, o.log)
	}
	o.served(what, branch)
}

// A branch fast-forwards to a commit imported under another branch, and
// then gets an empty commit (a CI trigger). The empty commit's line names
// a view the branch's own chain never reached: its chain is still where
// its last changeset left it. Taken as the chain's head, that view stopped
// every later import of the branch.
func TestAnEmptyCommitAfterAFastForward(t *testing.T) {
	o := newOrdinary(t, 0)
	o.commit(map[string]string{"a.txt": "a\n"})
	o.commit(map[string]string{"b.txt": "b\n"})
	o.ok("main", "main")
	o.git(o.work, "checkout", "-q", "-b", "feature")
	o.commit(map[string]string{"f.txt": "f\n"})
	o.ok("feature", "feature")
	o.git(o.work, "checkout", "-q", "main")
	o.git(o.work, "merge", "-q", "--ff-only", "feature")
	o.ok("main fast-forwarded to feature's commit", "main")
	o.empty()
	o.ok("an empty commit on main", "main")
	for i := range 3 {
		o.commit(map[string]string{"g.txt": fmt.Sprintf("g%d\n", i)})
		o.ok(fmt.Sprintf("commit %d after the empty one", i+1), "main")
	}
}

// A force push to an empty commit made on an older commit.
func TestAForcePushToAnEmptyCommit(t *testing.T) {
	o := newOrdinary(t, 0)
	a := o.commit(map[string]string{"a.txt": "a\n"})
	o.commit(map[string]string{"b.txt": "b\n"})
	o.ok("main", "main")
	o.git(o.work, "reset", "-q", "--hard", a)
	o.empty()
	o.ok("the force push", "main")
	o.commit(map[string]string{"c.txt": "c\n"})
	o.ok("the next commit", "main")
}

// Random ordinary histories over two branches: commits, returns to earlier
// content (so views come back), empty commits, branches moved to earlier
// commits, switches of branch; with chains sealed at two and three
// sections and never, and with and without a seal at the end of each
// import. Nothing is damaged, so no import may fail, no chain file may be
// made again, and every chain served must verify. C4_IMPORT_SEEDS raises
// the number of histories.
func TestOrdinaryGitUseNeverNeedsARepair(t *testing.T) {
	seeds := 8
	if s := os.Getenv("C4_IMPORT_SEEDS"); s != "" {
		fmt.Sscan(s, &seeds)
	}
	pushes := 0
	for seed := range seeds {
		rng := rand.New(rand.NewSource(int64(seed)))
		o := newOrdinary(t, []int{0, 2, 3}[seed%3])
		o.svc.storeChain = seed%4 == 3
		branches := []string{"main", "dev"}
		shas := []string{o.commit(map[string]string{"a.txt": "0\n"})}
		o.push("main")
		cur := "main"
		var trace []string
		for step := range 30 {
			var op string
			switch n := rng.Intn(10); {
			case n < 4:
				f := []string{"a.txt", "d/b.txt", "d/c.txt"}[rng.Intn(3)]
				shas = append(shas, o.commit(map[string]string{f: fmt.Sprintf("%d-%d\n", rng.Intn(3), step)}))
				op = "commit " + f
			case n < 5:
				o.git(o.work, "checkout", "-q", shas[rng.Intn(len(shas))], "--", ".")
				if o.git(o.work, "status", "--porcelain") == "" {
					shas = append(shas, o.empty())
				} else {
					o.git(o.work, "commit", "-q", "-m", "back")
					shas = append(shas, o.git(o.work, "rev-parse", "HEAD"))
				}
				op = "the content of an earlier commit"
			case n < 7:
				shas = append(shas, o.empty())
				op = "an empty commit"
			case n < 9:
				to := shas[rng.Intn(len(shas))]
				o.git(o.work, "reset", "-q", "--hard", to)
				op = "reset to " + to[:7]
			default:
				cur = branches[rng.Intn(2)]
				o.git(o.work, "checkout", "-q", "-B", cur)
				op = "branch " + cur
			}
			code, body := o.push(cur)
			pushes++
			trace = append(trace, fmt.Sprintf("%s, push %s: %d", op, cur, code))
			what := fmt.Sprintf("history %d (sealed at %d, at each import %v), step %d", seed, o.svc.chainMax, o.svc.storeChain, step)
			if code != http.StatusOK {
				t.Fatalf("%s: an import failed with nothing damaged: %s\n  %s", what, body, strings.Join(trace, "\n  "))
			}
			if o.log.Len() > 0 {
				t.Fatalf("%s: a healthy chain file was made again: %s\n  %s", what, o.log, strings.Join(trace, "\n  "))
			}
			for _, b := range branches {
				o.served(what, b)
			}
			if t.Failed() {
				t.Fatalf("  %s", strings.Join(trace, "\n  "))
			}
		}
	}
	t.Logf("%d histories, %d pushes: no import failed and no chain file was made again", seeds, pushes)
}

// head returns the status of a request for the chain at path and the view
// the chain served resolves to.
func (o *ordinary) head(path string) (int, string) {
	o.t.Helper()
	rec := httptest.NewRecorder()
	o.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chains/"+path, nil))
	if rec.Code != http.StatusOK {
		return rec.Code, ""
	}
	c, err := view.ReadChain(rec.Body.Bytes())
	if err != nil {
		o.t.Fatalf("the chain at %s does not read: %v", path, err)
	}
	views, err := c.Verify(context.Background(), o.svc.base.Records)
	if err != nil || len(views) == 0 {
		o.t.Fatalf("the chain at %s: %d views, %v", path, len(views), err)
	}
	return rec.Code, views[len(views)-1].String()
}

// Two branches whose chain files have one name (a chain file's name does
// not tell a slash from an underscore). The file can hold only one of
// their chains, and a reader was served the file, so whichever branch was
// imported last. Now each import makes the file again as its own ref's
// chain, which is logged, and a reader who names a ref gets that ref's
// chain whichever the file holds: the exact name wins. A request that
// names neither, in the file's own spelling, gets the first in order, the
// same one every time; a ref the mapping does not have is not found.
func TestTwoRefsWithOneChainFileName(t *testing.T) {
	o := newOrdinary(t, 0)
	o.commit(map[string]string{"a.txt": "0\n"})
	o.push("main")
	views := map[string]string{}
	push := func(branch, content string) {
		t.Helper()
		o.git(o.work, "checkout", "-q", "-B", branch, "main")
		sha := o.commit(map[string]string{"a.txt": content})
		o.git(o.work, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+branch)
		rec, res := o.post(o.h, `{"ref":"refs/heads/`+branch+`","after":"`+sha+`"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("push %s: %d %s", branch, rec.Code, rec.Body)
		}
		views[branch] = res.View
	}
	push("a/b", "slash\n")
	o.log.Reset()
	push("a_b", "underscore\n")
	if !strings.Contains(o.log.String(), "chain file for refs/heads/a_b made again") {
		t.Fatalf("the file held the other ref's chain and was not made again: %q", o.log)
	}
	if views["a/b"] == views["a_b"] {
		t.Fatal("the two branches have one view; the test tells nothing")
	}
	for round := range 2 {
		for _, b := range []string{"a/b", "a_b"} {
			if code, head := o.head("refs/heads/" + b); code != http.StatusOK || head != views[b] {
				t.Fatalf("round %d: a reader of %s gets %d, a chain to %s, want %s", round, b, code, head, views[b])
			}
		}
		if code, head := o.head("refs_heads_a_b"); code != http.StatusOK || head != views["a/b"] {
			t.Fatalf("round %d: a request in the file's spelling gets %d, a chain to %s, want the first in order (%s)", round, code, head, views["a/b"])
		}
		// The second round is with the file holding the other one's chain.
		push("a/b", fmt.Sprintf("slash %d\n", round))
	}
	if code, _ := o.head("refs/heads/none"); code != http.StatusNotFound {
		t.Fatalf("a ref the mapping does not have: %d", code)
	}
	// Each went on from its own chain, not the other's.
	for _, b := range []string{"a/b", "a_b"} {
		if code, head := o.head("refs/heads/" + b); code != http.StatusOK || head != views[b] {
			t.Fatalf("at the end: a reader of %s gets %d, a chain to %s, want %s", b, code, head, views[b])
		}
	}
}

// --export-chain writes the chain the mapping records, as a reader of the
// listener is served it: with the chain file torn (cut in an entry's name,
// where it still parses) it writes what it wrote from the whole file, and
// it leaves the file alone.
func TestExportChainIsTheRecordedChain(t *testing.T) {
	o := newOrdinary(t, 0)
	o.commit(map[string]string{"a.txt": "a\n"})
	o.ok("the first commit", "main")
	o.commit(map[string]string{"dir/more.txt": "more\n"})
	o.ok("the second commit", "main")
	export := func(ref string) ([]byte, error) {
		t.Helper()
		out, err := os.Create(filepath.Join(t.TempDir(), "out"))
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		stdout := os.Stdout
		os.Stdout = out
		defer func() { os.Stdout = stdout }()
		err = runImportGit([]string{"--repo", o.remote, "--store", o.shards[0], "--store", o.shards[1], "--out", o.svc.out, "--export-chain", ref})
		data, _ := os.ReadFile(out.Name())
		return data, err
	}
	whole, err := export("refs/heads/main")
	if err != nil || len(whole) == 0 {
		t.Fatalf("export: %d bytes, %v", len(whole), err)
	}
	p := o.svc.chainPath("refs/heads/main")
	file, _ := os.ReadFile(p)
	torn := file[:bytes.LastIndex(file, []byte("more.txt"))+len("mo")]
	if err := os.WriteFile(p, torn, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := export("refs/heads/main")
	if err != nil || !bytes.Equal(got, whole) {
		t.Fatalf("export from a torn chain file: %v\n%s\nwant\n%s", err, got, whole)
	}
	if now, _ := os.ReadFile(p); !bytes.Equal(now, torn) {
		t.Fatal("the export wrote the chain file")
	}
	// By the chain file's spelling of the ref, as a request to the
	// listener may name it.
	if got, err := export("refs_heads_main"); err != nil || !bytes.Equal(got, whole) {
		t.Fatalf("export by the file's spelling of the ref: %d bytes, %v", len(got), err)
	}
	if _, err := export("refs/heads/none"); err == nil || !strings.Contains(err.Error(), "records no chain") {
		t.Fatalf("export of a ref the mapping does not have: %v", err)
	}
}

// A mapping and a chain file as the importer before this one could leave
// them. A crash cut an append after a directory's line, a fragment that
// parses as a section and changes nothing; that importer took the file as
// it stood, went on, and counted the fragment, so the mapping counts 1, 2,
// 3, 5, 6 with no line missing and the file has a section no commit made.
// The chain is read by what each changeset applies to, not by the counts:
// the next push imports, the file is made again once without the
// fragment, and the chain served has a section for each commit.
func TestAMappingWhoseCountsSkipANumber(t *testing.T) {
	o := newOrdinary(t, 0)
	for i, files := range []map[string]string{
		{"a/x.txt": "x\n", "a/b/y.txt": "y\n", "top.txt": "t\n"},
		{"top.txt": "t2\n"},
		{"a/x.txt": "x3\n"},
		{"a/b/y.txt": "y4\n"},
		{"top.txt": "t5\n"},
	} {
		o.commit(files)
		o.ok(fmt.Sprintf("commit %d", i+1), "main")
	}
	// The state left: the fragment (the first two lines of the fourth
	// changeset) before the fourth changeset, and the counts after it one
	// more than they were.
	m, err := gitimport.ReadMapping(o.svc.out)
	if err != nil || len(m) != 5 {
		t.Fatalf("the mapping has %d lines, %v", len(m), err)
	}
	p := o.svc.chainPath("refs/heads/main")
	file, _ := os.ReadFile(p)
	fourth := bytes.LastIndex(file, []byte(m[2].View.String()+"\n"))
	lines := bytes.SplitAfterN(file[fourth:], []byte("\n"), 3)
	left := append(append(append([]byte(nil), file[:fourth]...), append(lines[0], lines[1]...)...), file[fourth:]...)
	if err := os.WriteFile(p, left, 0o644); err != nil {
		t.Fatal(err)
	}
	if ch, err := gitimport.OpenChainFile(context.Background(), p, o.svc.base.Records); err != nil || ch.Sections != 6 || ch.Head != m[4].View {
		t.Fatalf("the file with the fragment in it opens as %+v (%v); the test needs it to open as six sections to the same view", ch, err)
	}
	os.Remove(o.svc.out)
	w, err := gitimport.AppendMapping(o.svc.out)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range m {
		if i >= 3 {
			line.ChainSeq++
		}
		if err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	o.commit(map[string]string{"top.txt": "t6\n"})
	code, body := o.push("main")
	if code != http.StatusOK {
		t.Fatalf("the next push: %d %s", code, body)
	}
	if !strings.Contains(o.log.String(), "chain file for refs/heads/main made again") {
		t.Fatalf("the file with the fragment in it was not made again: %q", o.log)
	}
	rec := httptest.NewRecorder()
	o.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chains/refs/heads/main", nil))
	c, err := view.ReadChain(rec.Body.Bytes())
	if err != nil || len(c.Sections) != 6 || c.Base != view.EmptyID() {
		t.Fatalf("the chain served: %d, %d sections (%v), want one for each of the six commits", rec.Code, len(c.Sections), err)
	}
	if _, err := c.Verify(context.Background(), o.svc.base.Records); err != nil {
		t.Fatal(err)
	}
	// And once: the next push finds the file as recorded.
	o.log.Reset()
	o.commit(map[string]string{"top.txt": "t7\n"})
	o.ok("the push after", "main")
}

// A lost changeset is made again only as what the mapping records. Each
// way that cannot be done is an error that says which, and nothing is
// taken to stand for the changeset: the commit comes to something else,
// the line records the changeset in the form written before 2026-11-09,
// the commit is no longer in the repository.
func TestALostChangesetThatCannotBeMadeAgain(t *testing.T) {
	ctx := context.Background()
	o := newOrdinary(t, 0)
	a := o.commit(map[string]string{"a.txt": "a\n"})
	o.commit(map[string]string{"b.txt": "b\n"})
	o.ok("two commits", "main")
	m, err := gitimport.ReadMapping(o.svc.out)
	if err != nil || len(m) != 2 || m[1].Changeset.IsNil() {
		t.Fatalf("the mapping: %d lines, %v", len(m), err)
	}
	opts := o.svc.base
	opts.Known, opts.KnownFiles = gitimport.KnownViews(m), gitimport.KnownFiles(m)

	// With nothing lost, each commit comes to what its line records, and
	// nothing is written.
	for i, line := range m {
		if err := gitimport.Remake(ctx, opts, line); err != nil {
			t.Fatalf("line %d imported again: %v", i+1, err)
		}
	}
	if now, _ := gitimport.ReadMapping(o.svc.out); len(now) != 2 {
		t.Fatalf("the mapping has %d lines after two commits were imported again", len(now))
	}
	// A line that is not a record of its commit.
	other := m[1]
	other.View = m[0].View
	if err := gitimport.Remake(ctx, opts, other); err == nil || !strings.Contains(err.Error(), "and the mapping records the view") {
		t.Fatalf("a line whose view is not its commit's: %v", err)
	}
	other = m[1]
	other.Changeset = m[0].Changeset
	if err := gitimport.Remake(ctx, opts, other); err == nil || !strings.Contains(err.Error(), "and the mapping records the view") {
		t.Fatalf("a line whose changeset is not its commit's: %v", err)
	}
	// A line written before 2026-11-09: its changeset's C4 ID is of the
	// text followed by the view's C4 ID.
	stored, err := o.svc.base.Records.Record(ctx, m[1].Changeset)
	if err != nil {
		t.Fatal(err)
	}
	old := m[1]
	old.Changeset = c4.Identify(bytes.NewReader(append(append([]byte(nil), stored...), m[1].View.String()+"\n"...)))
	if err := gitimport.Remake(ctx, opts, old); err == nil || !strings.Contains(err.Error(), "before 2026-11-09") {
		t.Fatalf("a line with a changeset in the earlier form: %v", err)
	}

	// The commit is gone from the repository (the branch was moved back
	// and the repository pruned), its changeset from the store, and the
	// chain file with them. The import says so; a reader is not served a
	// chain.
	if p := o.object(m[1].Changeset); p == "" || os.Remove(p) != nil {
		t.Fatal("the changeset was not in the store to remove")
	}
	os.Remove(o.svc.chainPath("refs/heads/main"))
	o.git(o.remote, "update-ref", "refs/heads/main", a)
	o.git(o.remote, "reflog", "expire", "--expire=now", "--all")
	o.git(o.remote, "gc", "-q", "--prune=now")
	rec, _ := o.post(o.h, `{"ref":"refs/heads/main","after":"`+a+`"}`, nil)
	for _, want := range []string{m[1].Changeset.String(), "(mapping line 2) is not in the store and could not be made again from the repository", "commit " + m[1].Commit + " is not in the repository"} {
		if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("with the commit gone: %d %s\nwant %q in it", rec.Code, rec.Body, want)
		}
	}
	if code, _ := o.head("refs/heads/main"); code == http.StatusOK {
		t.Fatal("a reader was served a chain the store cannot make")
	}
}
