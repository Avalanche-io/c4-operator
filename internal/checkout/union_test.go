package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/capture"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/overlay"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Each case is one change, written twice: into a union's writable layer
// in its marker form, and on a plain copy of the base. Capturing the
// layer with WalkUnion must give exactly the plain copy's view.
func TestWalkUnionCapturesWhatTheMergedTreeShows(t *testing.T) {
	put := func(dir, p, content string, mode os.FileMode) {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(full, mode)
	}
	cases := []struct {
		name  string
		upper func(u string)
		plain func(r string)
	}{
		{"modify a file (copied up)", func(u string) {
			put(u, "internal/a/a.go", "package a\n\nvar X = 1\n", 0o644)
		}, func(r string) {
			put(r, "internal/a/a.go", "package a\n\nvar X = 1\n", 0o644)
		}},
		{"add a file in a new directory", func(u string) {
			put(u, "internal/n/n.go", "package n\n", 0o644)
		}, func(r string) {
			put(r, "internal/n/n.go", "package n\n", 0o644)
		}},
		{"delete a file", func(u string) {
			put(u, "internal/a/.wh.b.go", "", 0o644)
		}, func(r string) {
			os.Remove(filepath.Join(r, "internal/a/b.go"))
		}},
		{"delete a directory", func(u string) {
			put(u, "internal/.wh.old", "", 0o644)
		}, func(r string) {
			os.RemoveAll(filepath.Join(r, "internal/old"))
		}},
		{"replace a directory with a new one (opaque)", func(u string) {
			put(u, "internal/old/.wh.__dir_opaque", "", 0o644)
			put(u, "internal/old/z.go", "package old // z\n", 0o644)
		}, func(r string) {
			os.RemoveAll(filepath.Join(r, "internal/old"))
			put(r, "internal/old/z.go", "package old // z\n", 0o644)
		}},
		{"add a symlink", func(u string) {
			os.MkdirAll(filepath.Join(u, "docs"), 0o755)
			os.Symlink("../go.mod", filepath.Join(u, "docs/mod"))
		}, func(r string) {
			os.Symlink("../go.mod", filepath.Join(r, "docs/mod"))
		}},
		{"make a file executable (copied up)", func(u string) {
			put(u, "go.mod", "module example.com/w\n", 0o755)
		}, func(r string) {
			os.Chmod(filepath.Join(r, "go.mod"), 0o755)
		}},
		{"add an empty directory", func(u string) {
			os.MkdirAll(filepath.Join(u, "tmp/cache"), 0o755)
		}, func(r string) {
			os.MkdirAll(filepath.Join(r, "tmp/cache"), 0o755)
		}},
		{"the union's own names change nothing", func(u string) {
			put(u, ".wh..wh.meta.go.mod", "", 0o644)
			put(u, "internal/.wh..wh.tmp.x", "", 0o644)
		}, func(string) {}},
		{"chmod +x, bytes left in the lower (metadata marker)", func(u string) {
			put(u, ".wh..wh.meta.go.mod", "", 0o755)
		}, func(r string) {
			os.Chmod(filepath.Join(r, "go.mod"), 0o755)
		}},
		{"chmod -x, bytes left in the lower (metadata marker)", func(u string) {
			put(u, "bin/.wh..wh.meta.build.sh", "", 0o644)
		}, func(r string) {
			os.Chmod(filepath.Join(r, "bin/build.sh"), 0o644)
		}},
		{"a new time only (metadata marker)", func(u string) {
			put(u, "docs/.wh..wh.meta.same.txt", "", 0o644)
			now := time.Now()
			os.Chtimes(filepath.Join(u, "docs/.wh..wh.meta.same.txt"), now, now)
		}, func(string) {}},
		{"rewrite a file unchanged (copied up)", func(u string) {
			put(u, "README.md", "readme\n", 0o644)
		}, func(string) {}},
	}

	root := t.TempDir()
	src := filepath.Join(root, "src")
	writeBase(t, src)
	v, err := view.ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	set, err := objcache.OpenSet(filepath.Join(root, "cache"), objcache.Options{Budget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := set.Partition("p-show")
	store := &memStore{objs: map[c4.ID][]byte{}}
	for _, f := range v.Files {
		data, _ := os.ReadFile(filepath.Join(src, f.Path))
		cache.Put(f.ID, data)
	}
	for _, r := range v.Records() {
		cache.Put(r.ID, r.Data)
		store.PutBytes(context.Background(), "", r.ID, r.Data)
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upper := filepath.Join(root, "upper", string(rune('a'+i)))
			os.MkdirAll(upper, 0o755)
			c.upper(upper)
			ref := filepath.Join(root, "ref", string(rune('a'+i)))
			copyTree(t, src, ref)
			c.plain(ref)
			want, err := view.ScanDir(ref)
			if err != nil {
				t.Fatal(err)
			}
			res, err := capture.Run(context.Background(), capture.Input{
				Project: "show", Parent: v.ID, Upper: upper, Walk: WalkUnionOver(v, ""),
				Cache: cache, Sink: store, Workers: 2,
				Records: view.RecordFunc(func(_ context.Context, id c4.ID) ([]byte, error) { return cache.ReadAll(id) }),
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Output != want.ID {
				t.Fatalf("captured %s, the merged tree is %s", res.Output, want.ID)
			}
		})
	}
}

// A view with a name a union reads as a marker is named, so a union
// backend can refuse it rather than lose the file.
func TestUnionReserved(t *testing.T) {
	root := t.TempDir()
	writeBase(t, root)
	os.WriteFile(filepath.Join(root, "docs/.wh.fixture"), []byte("a real file\n"), 0o644)
	os.MkdirAll(filepath.Join(root, ".wh.dir"), 0o755)
	// Reserved in any ASCII case; look-alikes aren't.
	for _, n := range []string{".WH.upper", ".Wh.mixed", ".WHx", "x.WH.y", "..wh.x", ".wh"} {
		os.WriteFile(filepath.Join(root, "docs", n), []byte("a real file\n"), 0o644)
	}
	v, err := view.ScanDir(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range UnionReserved(v) {
		got[p] = true
	}
	want := []string{".wh.dir/", "docs/.wh.fixture", "docs/.WH.upper", "docs/.Wh.mixed"}
	for _, p := range want {
		if !got[p] {
			t.Errorf("%s isn't reserved", p)
		}
	}
	if len(got) != len(want) {
		t.Errorf("reserved %v, want exactly %v", got, want)
	}
	clean := filepath.Join(t.TempDir(), "clean")
	writeBase(t, clean)
	cv, _ := view.ScanDir(clean)
	if r := UnionReserved(cv); len(r) != 0 {
		t.Fatalf("a clean view reserved %q", r)
	}
}

// A reserved name that isn't an exact marker can't come through the union,
// so finding one in the writable layer fails the walk.
func TestWalkUnionRefusesAFoldedMarkerName(t *testing.T) {
	u := t.TempDir()
	os.WriteFile(filepath.Join(u, ".WH.x"), []byte("planted\n"), 0o644)
	if _, err := WalkUnion(u); !errors.Is(err, ErrReservedNames) {
		t.Fatalf("walking a planted .WH.x: %v", err)
	}
}

// A marker the union writes under another spelling of a view name
// (".wh.DOCS" for docs on a case-insensitive host) would delete or change
// nothing if read byte for byte, so walking it against the view fails
// rather than silently missing the agent's change.
func TestWalkUnionOverRefusesAMarkerForANameNotInTheView(t *testing.T) {
	src := t.TempDir()
	writeBase(t, src)
	v, err := view.ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{".wh.DOCS", ".wh..wh.meta.GO.MOD", "docs/.wh.SAME.txt"} {
		u := t.TempDir()
		os.MkdirAll(filepath.Join(u, filepath.Dir(marker)), 0o755)
		os.WriteFile(filepath.Join(u, marker), nil, 0o755)
		if _, err := WalkUnionOver(v, "")(u); err == nil {
			t.Errorf("%s walked without complaint", marker)
		}
	}
	// The exact spellings are changes as before.
	u := t.TempDir()
	os.WriteFile(filepath.Join(u, ".wh.docs"), nil, 0o644)
	os.WriteFile(filepath.Join(u, ".wh..wh.meta.go.mod"), nil, 0o755)
	entries, err := WalkUnionOver(v, "")(u)
	if err != nil || len(entries) != 2 {
		t.Fatalf("exact markers: %v, %v", entries, err)
	}
}

// With the view's folder to ask, a new name the host folds onto a view name
// fails the walk, unless it's an opaque directory, which replaces the view's
// entry. On a host that doesn't fold, both are simply new names.
func TestWalkUnionOverAsksTheHostAboutSpellings(t *testing.T) {
	lower := t.TempDir()
	writeBase(t, lower)
	v, err := view.ScanDir(lower)
	if err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Lstat(filepath.Join(lower, "README.MD"))
	folds := statErr == nil

	u := t.TempDir()
	os.WriteFile(filepath.Join(u, "README.MD"), []byte("rewritten\n"), 0o644)
	_, err = WalkUnionOver(v, lower)(u)
	if folds != (err != nil) {
		t.Errorf("README.MD beside README.md, host folds %v: %v", folds, err)
	}

	u = t.TempDir()
	os.MkdirAll(filepath.Join(u, "Docs"), 0o755)
	os.WriteFile(filepath.Join(u, "Docs", ".wh.__dir_opaque"), nil, 0o644)
	os.WriteFile(filepath.Join(u, "Docs", "new.txt"), []byte("new\n"), 0o644)
	entries, err := WalkUnionOver(v, lower)(u)
	if err != nil {
		t.Fatal(err)
	}
	deletesDocs := false
	for _, e := range entries {
		if e.Kind == overlay.Whiteout && e.Path == "docs" {
			deletesDocs = true
		}
	}
	if deletesDocs != folds {
		t.Errorf("an opaque Docs, host folds %v: deletes docs %v (%+v)", folds, deletesDocs, entries)
	}
}

// The union's own names (".wh..wh." and after) are not changes: they
// delete nothing, so they make no entries at all.
func TestWalkUnionSkipsTheUnionsOwnNames(t *testing.T) {
	u := t.TempDir()
	for _, p := range []string{".wh..wh.meta.go.mod", "internal/.wh..wh.tmp.x"} {
		os.MkdirAll(filepath.Join(u, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(u, p), nil, 0o644)
	}
	entries, err := WalkUnion(u)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind != overlay.Dir {
			t.Errorf("a union-internal name made an entry: %+v", e)
		}
	}
}
