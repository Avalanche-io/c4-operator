package gitimport

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// The tests drive a real c4git when one is installed (the worktree's
// bin/c4git, from `go install github.com/Avalanche-io/c4git/cmd/c4git`, or
// c4git on PATH). Without one, the test binary itself stands in as the
// clean/smudge filter with c4git's semantics, so the tests still run.
func TestMain(m *testing.M) {
	switch os.Getenv("GITIMPORT_FAKE_FILTER") {
	case "clean":
		os.Exit(fakeFilter(true))
	case "smudge":
		os.Exit(fakeFilter(false))
	}
	os.Exit(m.Run())
}

// fakeFilter is c4git's filter.Clean and filter.Smudge over the TreeStore
// at $C4_STORE.
func fakeFilter(clean bool) int {
	ts, err := store.NewTreeStore(os.Getenv("C4_STORE"))
	if err != nil {
		return 1
	}
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 1
	}
	if clean {
		if id := parsePointer(in); !id.IsNil() {
			os.Stdout.WriteString(id.String())
			return 0
		}
		id, err := ts.Put(bytes.NewReader(in))
		if err != nil {
			return 1
		}
		os.Stdout.WriteString(id.String())
		return 0
	}
	id, err := c4.Parse(strings.TrimSpace(string(in)))
	if err != nil {
		os.Stdout.Write(in)
		return 0
	}
	rc, err := ts.Open(id)
	if err != nil {
		os.Stdout.WriteString(id.String())
		return 0
	}
	defer rc.Close()
	io.Copy(os.Stdout, rc)
	return 0
}

// repo is a test repository with a c4git filter and store.
type repo struct {
	t      *testing.T
	dir    string
	c4git  string // c4git store directory ($C4_STORE for the filter)
	env    []string
	filter string // what runs the filter, for logs
}

// newRepo creates a repository whose *.exr files go through the c4 filter.
func newRepo(t *testing.T) *repo {
	t.Helper()
	root := t.TempDir()
	r := &repo{t: t, dir: filepath.Join(root, "repo"), c4git: filepath.Join(root, "c4git-store")}
	if err := os.MkdirAll(r.c4git, 0o755); err != nil {
		t.Fatal(err)
	}
	r.env = append(os.Environ(),
		"C4_STORE="+r.c4git,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-10-16T00:00:00Z", "GIT_COMMITTER_DATE=2026-10-16T00:00:00Z",
	)
	r.git("init", "-q", "-b", "main", r.dir)
	// git commit starts a detached `git maintenance run --auto`, which
	// repacks and deletes loose objects while the test goes on writing
	// commits and the importer reads them; under load a reader then fails
	// with "Could not read <sha>" or "bad tree object HEAD".
	r.git("config", "maintenance.auto", "false")
	r.git("config", "gc.auto", "0")
	clean, smudge := filterCommands(t)
	r.filter = clean
	r.git("config", "filter.c4.clean", clean)
	r.git("config", "filter.c4.smudge", smudge)
	r.git("config", "filter.c4.required", "true")
	return r
}

var (
	c4gitOnce sync.Once
	c4gitPath string
)

// filterCommands returns the clean and smudge commands: a real c4git if
// one is installed, else this test binary.
func filterCommands(t *testing.T) (string, string) {
	c4gitOnce.Do(func() {
		if p, err := filepath.Abs("../../bin/c4git"); err == nil {
			if _, err := os.Stat(p); err == nil {
				c4gitPath = p
				return
			}
		}
		if p, err := exec.LookPath("c4git"); err == nil {
			c4gitPath = p
		}
	})
	if c4gitPath != "" {
		return shellQuote(c4gitPath) + " clean %f", shellQuote(c4gitPath) + " smudge %f"
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return "GITIMPORT_FAKE_FILTER=clean " + shellQuote(self), "GITIMPORT_FAKE_FILTER=smudge " + shellQuote(self)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	if !(len(args) > 0 && args[0] == "init") {
		cmd = exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	}
	cmd.Env = r.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) write(p, content string) {
	r.t.Helper()
	full := filepath.Join(r.dir, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) chmod(p string, mode os.FileMode) {
	r.t.Helper()
	if err := os.Chmod(filepath.Join(r.dir, p), mode); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) symlink(target, p string) {
	r.t.Helper()
	full := filepath.Join(r.dir, p)
	os.Remove(full)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) remove(p string) {
	r.t.Helper()
	if err := os.RemoveAll(filepath.Join(r.dir, p)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// checkoutID checks a commit out into a fresh worktree, smudging c4git
// files, and returns view.ScanDir's ID of it without .git.
func (r *repo) checkoutID(sha string) c4.ID {
	r.t.Helper()
	dir := filepath.Join(r.t.TempDir(), "wt")
	r.git("worktree", "add", "-q", "--detach", dir, sha)
	defer r.git("worktree", "prune")
	if err := os.Remove(filepath.Join(dir, ".git")); err != nil {
		r.t.Fatal(err)
	}
	v, err := view.ScanDir(dir)
	if err != nil {
		r.t.Fatal(err)
	}
	return v.ID
}

// history lists the first-parent commits of HEAD, oldest first.
func (r *repo) history() []string {
	return strings.Fields(r.git("rev-list", "--first-parent", "--reverse", "HEAD"))
}

// importInto runs an import into a fresh TreeStore and returns the summary
// and the store.
func (r *repo) importInto(t *testing.T, sink Sink, opts Options) *Summary {
	t.Helper()
	opts.Repo = r.dir
	if opts.Sink == nil {
		opts.Sink = sink
	}
	if opts.C4Git == nil {
		src, err := OpenTreeSource(r.c4git)
		if err != nil {
			t.Fatal(err)
		}
		opts.C4Git = src
	}
	// The importer's own git processes must not see the user's config.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("C4_STORE", r.c4git)
	sum, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func newTreeSink(t *testing.T) *TreeSink {
	t.Helper()
	ts, err := store.NewTreeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &TreeSink{TS: ts}
}

// countingSink records every Put and Copy by ID.
type countingSink struct {
	Sink
	mu     sync.Mutex
	puts   map[c4.ID]int
	copies map[c4.ID]int
}

func newCountingSink(s Sink) *countingSink {
	return &countingSink{Sink: s, puts: map[c4.ID]int{}, copies: map[c4.ID]int{}}
}

func (s *countingSink) Put(ctx context.Context, id c4.ID, size int64, r io.Reader) (bool, error) {
	s.mu.Lock()
	s.puts[id]++
	s.mu.Unlock()
	return s.Sink.Put(ctx, id, size, r)
}

func (s *countingSink) Copy(ctx context.Context, id c4.ID, size int64, src Source) (bool, error) {
	s.mu.Lock()
	s.copies[id]++
	s.mu.Unlock()
	return s.Sink.Copy(ctx, id, size, src)
}

func idOf(s string) c4.ID { return c4.Identify(strings.NewReader(s)) }
