package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/scan"
	"github.com/Avalanche-io/c4/store"
)

// writeTree creates files under root; content is the map value.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

var show = map[string]string{
	"shots/sh010/plates/frame.0001.exr": "plate-a-1",
	"shots/sh010/plates/frame.0002.exr": "plate-a-2",
	"shots/sh010/notes.txt":             "notes 010",
	"shots/sh020/plates/frame.0001.exr": "plate-a-2", // same bytes as sh010 frame 2
	"shots/sh020/plates/frame.0002.exr": "plate-b-2",
	"shots/sh020/empty/":                "",
	"shots/sh030/plates/frame.0001.exr": "never wanted",
	"weights/model.bin":                 "weights",
}

func scanFull(t *testing.T, dir string) *c4m.Manifest {
	t.Helper()
	m, err := scan.Dir(dir, scan.WithMode(scan.ModeFull))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestViewIDMatchesContentScan(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, show)
	data, err := c4m.Marshal(scanFull(t, src))
	if err != nil {
		t.Fatal(err)
	}

	r, err := FromBytes(data, nil, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := ScanContentID(src)
	if err != nil {
		t.Fatal(err)
	}
	if r.View.ID != want {
		t.Fatalf("view ID %s, content scan %s", r.View.ID, want)
	}
	if r.RootID != scanFull(t, src).ComputeC4ID() {
		t.Fatalf("root ID is not the snapshot ID")
	}
	tot := r.View.Totals()
	if tot.Files != 7 || tot.UniqueObjects != 6 {
		t.Fatalf("totals %+v", tot)
	}
}

func TestFilteredViewMatchesFilteredTree(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, show)
	data, _ := c4m.Marshal(scanFull(t, src))

	f, err := NewFilter([]string{"shots/sh020/", "weights"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := FromBytes(data, nil, f)
	if err != nil {
		t.Fatal(err)
	}

	// Build the expected subset by hand and scan it.
	sub := t.TempDir()
	subset := map[string]string{}
	for p, c := range show {
		if strings.HasPrefix(p, "shots/sh020/") || strings.HasPrefix(p, "weights/") {
			subset[p] = c
		}
	}
	writeTree(t, sub, subset)
	want, _ := ScanContentID(sub)
	if r.View.ID != want {
		t.Fatalf("filtered view %s, subset scan %s", r.View.ID, want)
	}
	if got := len(r.View.Files); got != 3 {
		t.Fatalf("files = %d, want 3", got)
	}
	wantDirs := []string{"shots/", "shots/sh020/", "shots/sh020/empty/", "shots/sh020/plates/", "weights/"}
	if fmt.Sprint(r.View.Dirs) != fmt.Sprint(wantDirs) {
		t.Fatalf("dirs %v", r.View.Dirs)
	}
}

func TestViewRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, show)
	data, _ := c4m.Marshal(scanFull(t, src))
	r, err := FromBytes(data, nil, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.View.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	v, err := ParseView(b, r.View.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != r.View.ID || len(v.Files) != len(r.View.Files) {
		t.Fatal("round trip changed the view")
	}
	// A view file renamed to another ID is refused.
	if _, err := ParseView(b, c4.Identify(strings.NewReader("x"))); err == nil {
		t.Fatal("ParseView accepted a mismatched ID")
	}
}

// countingStore records which IDs were opened.
type countingStore struct {
	store.Store
	mu     sync.Mutex
	opened map[c4.ID]int
}

func (s *countingStore) Open(id c4.ID) (io.ReadCloser, error) {
	s.mu.Lock()
	s.opened[id]++
	s.mu.Unlock()
	return s.Store.Open(id)
}

// storeRecords puts every directory's one-level record into s, the way
// `c4 id -s` does, and returns the root (snapshot) ID.
func storeRecords(t *testing.T, s store.Store, m *c4m.Manifest) (map[string]c4.ID, c4.ID) {
	t.Helper()
	ids := map[string]c4.ID{}
	for _, e := range m.Entries {
		if !e.IsDir() {
			continue
		}
		listing := c4m.NewManifest()
		for _, c := range m.Children(e) {
			cp := *c
			cp.Depth = 0
			listing.AddEntry(&cp)
		}
		id, err := s.Put(strings.NewReader(listing.Canonical()))
		if err != nil {
			t.Fatal(err)
		}
		if id != e.C4ID {
			t.Fatalf("record for %s hashes to %s, entry says %s", e.Name, id, e.C4ID)
		}
		ids[m.EntryPath(e)] = id
	}
	root := c4m.NewManifest()
	for _, e := range m.Entries {
		if e.Depth == 0 {
			cp := *e
			root.AddEntry(&cp)
		}
	}
	id, err := s.Put(strings.NewReader(root.Canonical()))
	if err != nil {
		t.Fatal(err)
	}
	if id != m.ComputeC4ID() {
		t.Fatalf("root record %s != snapshot ID %s", id, m.ComputeC4ID())
	}
	return ids, id
}

func TestFromIDExpandsRecordsSparsely(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, show)
	m := scanFull(t, src)

	ts, err := store.NewTreeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := &countingStore{Store: ts, opened: map[c4.ID]int{}}
	dirIDs, rootID := storeRecords(t, ts, m)

	f, _ := NewFilter([]string{"shots/sh020/"})
	r, err := FromID(rootID, cs, f)
	if err != nil {
		t.Fatal(err)
	}
	if r.RootID != rootID {
		t.Fatal("root ID changed")
	}
	if cs.opened[dirIDs["shots/sh030/"]] != 0 || cs.opened[dirIDs["shots/sh010/"]] != 0 || cs.opened[dirIDs["weights/"]] != 0 {
		t.Fatalf("resolution fetched records outside the filter: %v", cs.opened)
	}
	if cs.opened[dirIDs["shots/sh020/plates/"]] != 1 {
		t.Fatal("did not expand the included directory")
	}

	// Same tree from the full c4m text gives the same view.
	data, _ := c4m.Marshal(m)
	r2, err := FromBytes(data, nil, f)
	if err != nil {
		t.Fatal(err)
	}
	if r2.View.ID != r.View.ID {
		t.Fatalf("record view %s != text view %s", r.View.ID, r2.View.ID)
	}
}

func TestMissingRecordIsIncomplete(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, show)
	m := scanFull(t, src)
	ts, _ := store.NewTreeStore(t.TempDir())
	dirIDs, rootID := storeRecords(t, ts, m)
	if err := ts.Remove(dirIDs["weights/"]); err != nil {
		t.Fatal(err)
	}
	_, err := FromID(rootID, ts, Filter{})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	// The same store serves a filter that never reaches weights/.
	f, _ := NewFilter([]string{"shots/"})
	if _, err := FromID(rootID, ts, f); err != nil {
		t.Fatalf("sparse resolution failed: %v", err)
	}
}

func TestCorruptStoreObjectRefused(t *testing.T) {
	ts, _ := store.NewTreeStore(t.TempDir())
	id, _ := ts.Put(strings.NewReader("hello.txt\n"))
	p, _ := ts.ContentPath(id)
	os.Chmod(p, 0o644)
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVerified(ts, id); err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("err = %v", err)
	}
}

func TestFilter(t *testing.T) {
	f, err := NewFilter([]string{"shots/*/plates", "weights/model.bin", "editorial/"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		p    string
		inc  bool
		may  bool
		isDr bool
	}{
		{"shots/sh010/plates/frame.0001.exr", true, true, false},
		{"shots/sh010/notes.txt", false, true, false},
		{"shots/", false, true, true},
		{"shots/sh010/", false, true, true},
		{"weights/model.bin", true, true, false},
		{"weights/other.bin", false, true, false},
		{"editorial/cut.mov", true, true, false},
		{"audio/", false, false, true},
	}
	for _, c := range cases {
		if got := f.Includes(c.p); got != c.inc {
			t.Errorf("Includes(%q) = %v", c.p, got)
		}
		if c.isDr {
			if got := f.MayContain(c.p); got != c.may {
				t.Errorf("MayContain(%q) = %v", c.p, got)
			}
		}
	}
	for _, bad := range []string{"", "../x", "a/[", "a//b/.."} {
		if _, err := NewFilter([]string{bad}); err == nil {
			t.Errorf("NewFilter(%q) accepted", bad)
		}
	}
}

func TestUnsafeNamesRefused(t *testing.T) {
	// A hand-built manifest with a traversal name never reaches disk.
	m := c4m.NewManifest()
	m.AddEntry(&c4m.Entry{Name: "..", Size: 1, C4ID: c4.Identify(bytes.NewReader([]byte("x")))})
	r := &resolver{seen: map[c4.ID]bool{}}
	if err := r.walk(m, ""); err == nil {
		t.Fatal("walk accepted ..")
	}
}
