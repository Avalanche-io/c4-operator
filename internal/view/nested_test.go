package view

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
)

// A view's ID must equal c4m's ID of the same tree written as one nested
// manifest, which is how `c4 log` and `c4 split` identify each version of
// a chain. A directory holding a symlink has a null size (the symlink's
// size is null), and its parent's record must say so too; c4m's
// PropagateMetadata over a one-level listing would read it as an empty
// directory and record 0.
func TestViewIDEqualsNestedManifestID(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string, mode os.FileMode) {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "top\n", 0o644)
	write("tools/build.sh", "#!/bin/sh\n", 0o755)
	write("x/c.txt", "c\n", 0o644)
	write("x/y/deep.txt", "deep\n", 0o644)
	write("plain/only.txt", "only\n", 0o644)
	os.MkdirAll(filepath.Join(dir, "empty"), 0o755)
	for target, link := range map[string]string{"build.sh": "tools/b", "c.txt": "x/l", "../README.md": "x/y/up"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	v, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Manifest().ComputeC4ID(); got != v.ID {
		t.Fatalf("view %s, nested manifest %s", v.ID, got)
	}
	// Every record hashes to its ID and lists x/ with x's own entry.
	for _, r := range v.Records() {
		if c4.Identify(strings.NewReader(string(r.Data))) != r.ID {
			t.Fatalf("record %s does not hash to its ID", r.ID)
		}
	}
	root := string(v.Root.Record())
	if want := v.Root.Dirs["x/"].Entry.Canonical(); !strings.Contains(root, want+"\n") {
		t.Fatalf("root record\n%s\ndoes not list x/ as %q", root, want)
	}
	if v.Root.Dirs["x/"].Entry.Size != -1 || v.Root.Dirs["plain/"].Entry.Size <= 0 {
		t.Fatalf("sizes: x/ %d (want null), plain/ %d", v.Root.Dirs["x/"].Entry.Size, v.Root.Dirs["plain/"].Entry.Size)
	}

	// The same holds for a view built by Apply.
	st := newMemStore()
	for _, r := range v.Records() {
		st.m[r.ID] = r.Data
	}
	res, err := Apply(context.Background(), v.ID, []Edit{
		{Path: "x/y/new.txt", Entry: FileEntry("new.txt", 3, c4.Identify(strings.NewReader("new")), false)},
		{Path: "plain/l2", Entry: SymlinkEntry("l2", "only.txt")},
	}, st)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Records {
		st.m[r.ID] = r.Data
	}
	after, err := Load(context.Background(), res.ID, st, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Manifest().ComputeC4ID(); got != res.ID {
		t.Fatalf("applied view %s, nested manifest %s", res.ID, got)
	}
}

type memStore struct{ m map[c4.ID][]byte }

func newMemStore() *memStore { return &memStore{m: map[c4.ID][]byte{}} }

func (s *memStore) Record(_ context.Context, id c4.ID) ([]byte, error) {
	b, ok := s.m[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}
