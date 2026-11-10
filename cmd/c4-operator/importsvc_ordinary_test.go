package main

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
