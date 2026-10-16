package gitimport

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4/store"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// buildHistory makes a repository whose history covers every kind of
// change the importer must handle. It returns the commits oldest first.
func buildHistory(r *repo) []string {
	big := strings.Repeat("frame data ", 40000) // about 440 KB
	r.write("README.md", "hello\n")
	r.write("a.txt", "a\n")
	r.write("src/main.go", "package main\n")
	r.write("src/lib/util.go", "package lib\n")
	r.write("src/lib/deep/x/y.go", "package y\n")
	r.write("scripts/run.sh", "#!/bin/sh\necho run\n")
	r.chmod("scripts/run.sh", 0o755)
	r.write("docs/guide.md", "guide\n")
	r.symlink("src/main.go", "main-link")
	r.write("empty.txt", "")
	r.commit("initial")

	// Modify, add, and delete the only file of a directory.
	r.write("src/main.go", "package main\n\nfunc main() {}\n")
	r.write("src/new.go", "package main\n")
	r.remove("docs")
	r.commit("modify, add, delete a directory")

	// Rename, and flip exec bits both ways.
	r.remove("src/lib/util.go")
	r.write("src/lib/helpers.go", "package lib\n")
	r.chmod("scripts/run.sh", 0o644)
	r.chmod("a.txt", 0o755)
	r.commit("rename and exec flips")

	// A directory replaced by a file.
	r.remove("src/lib")
	r.write("src/lib", "now a file\n")
	r.commit("directory to file")

	// A file replaced by a directory; a symlink retargeted; a symlink
	// replaced by a file and a file by a symlink.
	r.remove("a.txt")
	r.write("a.txt/inner.txt", "inner\n")
	r.symlink("src/new.go", "main-link")
	r.write("link2", "placeholder\n")
	r.commit("file to directory, symlink retarget")
	r.remove("link2")
	r.symlink("README.md", "link2")
	r.remove("README.md")
	r.write("README.md", "a file again\n")
	r.commit("file to symlink")

	// c4git: managed assets, a duplicate, and a pointer-shaped file that
	// is not managed (its own 90 bytes are its content).
	r.write(".gitattributes", "*.exr filter=c4\n")
	r.write("assets/plate.exr", big)
	r.write("assets/copy.exr", big)
	r.write("assets/sub/beauty.exr", big+"beauty")
	r.write("notes/pointer.txt", idOf("something else").String())
	r.commit("c4git assets")

	// Modify, move and delete managed files.
	r.write("assets/plate.exr", big+"v2")
	r.remove("assets/copy.exr")
	r.write("assets/moved/copy.exr", big)
	r.remove("assets/sub")
	r.commit("asset changes")

	// Executable managed file, and an unchanged blob that stops being
	// managed when .gitattributes changes (its checkout becomes the bare
	// pointer text).
	r.write("tools/render.exr", big+"tool")
	r.chmod("tools/render.exr", 0o755)
	r.commit("exec asset")
	// Only .gitattributes is staged, so the blobs stay as they were.
	r.write(".gitattributes", "assets/*.exr filter=c4\nassets/**/*.exr filter=c4\n")
	r.git("add", ".gitattributes")
	r.git("commit", "-q", "-m", "narrow the attributes")
	r.write(".gitattributes", "*.exr filter=c4\n")
	r.git("add", ".gitattributes")
	r.git("commit", "-q", "-m", "widen the attributes again")

	// An empty commit, then a commit that deletes everything but one file.
	r.git("commit", "-q", "--allow-empty", "-m", "empty")
	for _, p := range []string{"src", "scripts", "assets", "tools", "notes", "a.txt", "main-link", "link2", "empty.txt", ".gitattributes"} {
		r.remove(p)
	}
	r.commit("almost empty")
	r.write("src/again.go", "package again\n")
	r.commit("regrow")
	return r.history()
}

func TestImportMatchesCheckouts(t *testing.T) {
	r := newRepo(t)
	t.Logf("filter: %s", r.filter)
	commits := buildHistory(r)
	sink := newTreeSink(t)
	sum := r.importInto(t, sink, Options{Records: view.StoreRecords(sink)})
	if len(sum.Commits) != len(commits) {
		t.Fatalf("imported %d commits, history has %d", len(sum.Commits), len(commits))
	}
	if sum.Missing != 0 {
		t.Fatalf("%d c4git objects missing", sum.Missing)
	}
	for i, c := range sum.Commits {
		if c.Commit != commits[i] {
			t.Fatalf("commit %d is %s, want %s", i, c.Commit, commits[i])
		}
		want := r.checkoutID(c.Commit)
		if c.View != want {
			t.Errorf("commit %d %s: view %s, checkout scans to %s", i, short(c.Commit), c.View, want)
		}
		if i > 0 && c.ParentView != sum.Commits[i-1].View {
			t.Errorf("commit %d: parent view is not the previous commit's view", i)
		}
	}
	if !sum.Commits[0].Base {
		t.Error("the root commit is not marked as the base")
	}
	if sum.Commits[6].C4GitObjects != 2 {
		t.Errorf("c4git assets commit used %d c4git objects, want 2", sum.Commits[6].C4GitObjects)
	}
}

func TestChangesetsResolveToCommitViews(t *testing.T) {
	r := newRepo(t)
	buildHistory(r)
	sink := newTreeSink(t)
	sum := r.importInto(t, sink, Options{Records: view.StoreRecords(sink)})
	for i, c := range sum.Commits {
		if c.Changeset.IsNil() {
			if c.View != c.ParentView {
				t.Fatalf("commit %d changes the view but has no changeset", i)
			}
			continue
		}
		res, err := view.FromID(c.Changeset, sink, view.Filter{})
		if err != nil {
			t.Fatalf("commit %d: resolve changeset: %v", i, err)
		}
		if len(res.Chain) != 2 {
			t.Fatalf("commit %d: chain of %d views", i, len(res.Chain))
		}
		if res.Chain[0].ID != c.ParentView {
			t.Errorf("commit %d: chain base %s, parent view %s", i, res.Chain[0].ID, c.ParentView)
		}
		if res.View().ID != c.View {
			t.Errorf("commit %d: changeset resolves to %s, commit view %s", i, res.View().ID, c.View)
		}
		// The view also loads from its records alone.
		v, err := view.Load(context.Background(), c.View, view.StoreRecords(sink), 4)
		if err != nil {
			t.Fatalf("commit %d: load view: %v", i, err)
		}
		if len(v.Files)+len(v.Symlinks) != c.Files {
			t.Errorf("commit %d: view has %d files and symlinks, import counted %d", i, len(v.Files)+len(v.Symlinks), c.Files)
		}
		// Every file's content is in the sink.
		for _, f := range v.Files {
			if !sink.TS.Has(f.ID) {
				t.Errorf("commit %d: %s content %s not stored", i, f.Path, f.ID)
			}
		}
	}
}

// TestC4GitObjectsAreTrustedNotRehashed puts an object in the c4git store
// under a label that is not its hash. The importer must use the label as
// the file's ID, take the size from the store, and copy the bytes as they
// are: c4git is an ingest point that verified the ID at `git add`, so the
// importer does not verify it again. This is the trust boundary the
// package comment describes.
func TestC4GitObjectsAreTrustedNotRehashed(t *testing.T) {
	r := newRepo(t)
	label := idOf("the bytes c4git hashed")
	stored := "different bytes under that label"
	ts, err := store.NewTreeStore(r.c4git)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ts.Create(label)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, stored)
	w.Close()

	r.write(".gitattributes", "*.exr filter=c4\n")
	r.write("shot.exr", label.String()) // the pointer, cleaned as is
	r.write("plain.txt", "plain\n")
	r.commit("pointer")

	cs := newCountingSink(newTreeSink(t))
	sum := r.importInto(t, cs, Options{})
	c := sum.Commits[0]
	v, err := view.Load(context.Background(), c.View, view.StoreRecords(cs.Sink.(*TreeSink)), 2)
	if err != nil {
		t.Fatal(err)
	}
	var got *view.File
	for i := range v.Files {
		if v.Files[i].Path == "shot.exr" {
			got = &v.Files[i]
		}
	}
	if got == nil || got.ID != label || got.Size != int64(len(stored)) {
		t.Fatalf("shot.exr = %+v, want ID %s size %d", got, label, len(stored))
	}
	if cs.copies[label] != 1 || cs.puts[label] != 0 {
		t.Fatalf("label: %d copies, %d puts; want one copy and no put", cs.copies[label], cs.puts[label])
	}
	if cs.puts[idOf("plain\n")] != 1 {
		t.Fatal("plain.txt was not put under its hash")
	}
	data, err := view.ReadObject(cs.Sink.(*TreeSink), label)
	if err != nil || string(data) != stored {
		t.Fatalf("project store holds %q, %v", data, err)
	}
	if c.C4GitObjects != 1 {
		t.Fatalf("c4git objects used: %d", c.C4GitObjects)
	}
}

func TestMissingC4GitObjectIsReported(t *testing.T) {
	r := newRepo(t)
	absent := idOf("never stored")
	r.write(".gitattributes", "*.exr filter=c4\n")
	r.write("shot.exr", absent.String())
	r.commit("dangling pointer")
	var warnings []string
	sum := r.importInto(t, newTreeSink(t), Options{Warn: func(s string) { warnings = append(warnings, s) }})
	c := sum.Commits[0]
	if sum.Missing != 1 || len(c.C4GitMissing) != 1 || c.C4GitMissing[0] != "shot.exr" {
		t.Fatalf("missing: summary %d, commit %v", sum.Missing, c.C4GitMissing)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "shot.exr") {
		t.Fatalf("warnings: %q", warnings)
	}
}

func TestSubmodulesAreSkippedAndCounted(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "a\n")
	first := r.commit("one")
	r.git("update-index", "--add", "--cacheinfo", "160000,"+first+",ext/lib")
	r.git("commit", "-q", "-m", "submodule")
	r.write("b.txt", "b\n")
	r.commit("after")
	var warnings []string
	sum := r.importInto(t, newTreeSink(t), Options{Warn: func(s string) { warnings = append(warnings, s) }})
	if sum.Submodules != 1 || sum.Commits[1].Submodules != 1 || len(warnings) != 1 {
		t.Fatalf("submodules: summary %d, commit %d, warnings %q", sum.Submodules, sum.Commits[1].Submodules, warnings)
	}
	if sum.Commits[1].View != sum.Commits[0].View {
		t.Fatal("a submodule-only commit changed the view")
	}
}

// TestIncrementalImports imports a history in pieces (the oldest commits,
// then the rest continuing from the mapping) and as the newest commits
// only, and checks every piece agrees with a full import.
func TestIncrementalImports(t *testing.T) {
	r := newRepo(t)
	commits := buildHistory(r)
	full := r.importInto(t, newTreeSink(t), Options{})
	want := KnownViews(full.Commits)

	sink := newTreeSink(t)
	mapping := filepath.Join(t.TempDir(), "map.jsonl")
	mw, err := AppendMapping(mapping)
	if err != nil {
		t.Fatal(err)
	}
	r.importInto(t, sink, Options{Rev: commits[4], Progress: func(c Commit) { mw.Write(c) }})
	mw.Close()
	prior, err := ReadMapping(mapping)
	if err != nil || len(prior) != 5 {
		t.Fatalf("mapping: %d lines, %v", len(prior), err)
	}
	rest := r.importInto(t, sink, Options{Known: KnownViews(prior), Records: view.StoreRecords(sink)})
	if rest.Skipped != 5 || len(rest.Commits) != len(commits)-5 {
		t.Fatalf("continued import: skipped %d, imported %d", rest.Skipped, len(rest.Commits))
	}
	if rest.Commits[0].Base {
		t.Fatal("a continued import re-imported its base")
	}
	for _, c := range rest.Commits {
		if c.View != want[c.Commit] {
			t.Errorf("continued %s: %s, full import %s", short(c.Commit), c.View, want[c.Commit])
		}
	}

	newest := r.importInto(t, newTreeSink(t), Options{MaxCommits: 3})
	if len(newest.Commits) != 4 || !newest.Commits[0].Base || newest.Commits[0].Commit != commits[len(commits)-4] {
		t.Fatalf("max-commits 3: %d commits, base %v", len(newest.Commits), newest.Commits[0].Base)
	}
	since := r.importInto(t, newTreeSink(t), Options{Since: commits[7]})
	if len(since.Commits) != len(commits)-7 || since.Commits[0].Commit != commits[7] {
		t.Fatalf("since: %d commits starting at %s", len(since.Commits), short(since.Commits[0].Commit))
	}
	for _, s := range [][]Commit{newest.Commits, since.Commits} {
		for _, c := range s {
			if c.View != want[c.Commit] {
				t.Errorf("%s: %s, full import %s", short(c.Commit), c.View, want[c.Commit])
			}
		}
	}
}

// TestCostIsProportionalToChange imports two repositories that differ in
// size by 16 times, each followed by the same one-file commits, and checks
// that those commits read and write the same, small number of records and
// read one blob each.
func TestCostIsProportionalToChange(t *testing.T) {
	costs := map[int][]Commit{}
	for _, dirs := range []int{4, 64} {
		r := newRepo(t)
		for d := range dirs {
			for f := range 20 {
				r.write(fmt.Sprintf("svc/s%03d/pkg/f%02d.go", d, f), fmt.Sprintf("package p // %d %d\n", d, f))
			}
		}
		r.commit("big tree")
		for i := range 5 {
			r.write("svc/s001/pkg/f03.go", fmt.Sprintf("package p // edit %d\n", i))
			r.commit("edit")
		}
		r.write("svc/s002/pkg/new.go", "package p // new\n")
		r.commit("add")
		r.remove("svc/s003/pkg/f00.go")
		r.commit("delete")
		sum := r.importInto(t, newTreeSink(t), Options{})
		costs[dirs] = sum.Commits[1:]
		if base := sum.Commits[0]; base.RecordsWritten != 2*dirs+2 {
			t.Errorf("%d dirs: base wrote %d records, want %d", dirs, base.RecordsWritten, 2*dirs+2)
		}
	}
	small, large := costs[4], costs[64]
	for i := range small {
		s, l := small[i], large[i]
		// svc/sNNN/pkg/file: root, svc, sNNN and pkg.
		if l.RecordsRead != 4 || l.RecordsWritten != 4 || l.BlobsRead > 1 {
			t.Errorf("commit %d of the large repo read %d records, wrote %d, read %d blobs", i, l.RecordsRead, l.RecordsWritten, l.BlobsRead)
		}
		if s.RecordsRead != l.RecordsRead || s.RecordsWritten != l.RecordsWritten {
			t.Errorf("commit %d: records read/written %d/%d small, %d/%d large", i, s.RecordsRead, s.RecordsWritten, l.RecordsRead, l.RecordsWritten)
		}
	}
}

// TestShardedSink imports into a sharded store of three TreeStore shards
// and checks every view loads back through it.
func TestShardedSink(t *testing.T) {
	r := newRepo(t)
	buildHistory(r)
	urls := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	sh, err := openShardedForTest(urls)
	if err != nil {
		t.Fatal(err)
	}
	sum := r.importInto(t, sh, Options{Records: view.StoreRecords(sh)})
	again := r.importInto(t, sh, Options{})
	last := sum.Commits[len(sum.Commits)-1]
	if _, err := view.Load(context.Background(), last.View, view.StoreRecords(sh), 4); err != nil {
		t.Fatal(err)
	}
	var up int
	for _, c := range again.Commits {
		up += c.ObjectsUploaded
	}
	if up != 0 {
		t.Fatalf("re-import uploaded %d objects", up)
	}
}

func TestParsePointer(t *testing.T) {
	id := idOf("x")
	cases := map[string]bool{
		id.String():             true,
		id.String() + "\n":      true,
		id.String() + "\n\n":    false,
		" " + id.String():       false,
		strings.Repeat("a", 90): false,
	}
	for in, want := range cases {
		if got := !parsePointer([]byte(in)).IsNil(); got != want {
			t.Errorf("parsePointer(%q) = %v, want %v", in, got, want)
		}
	}
}

func openShardedForTest(dirs []string) (*ShardedSink, error) {
	s, err := storeurl.OpenSharded(dirs, storeurl.Credentials{})
	if err != nil {
		return nil, err
	}
	return &ShardedSink{S: s}, nil
}
