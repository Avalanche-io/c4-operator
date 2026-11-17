package view

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	"github.com/Avalanche-io/c4/store"
)

// misread makes the read-back checks read every entry named name with its
// link target altered, for the rest of the test: a decoder that disagrees
// with the encoder, which the c4m library this package is built with has
// no known case of (the null-mode link to host:path was the last, until
// views kept a link's type). It goes through readBack, the checks' one
// seam, so the guard is exercised exactly as a real disagreement would be.
func misread(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(MisreadForTest(name))
}

// locationTargets are link targets that look like a location (host:path,
// a drive letter, a remote folder). Under a null mode the library reads a
// link to one back as a flow link; a view records a link's type, so it
// reads back as the link it is.
var locationTargets = []string{"host:path", "C:foo", "nas:plates/"}

// readsBackAsLink requires the record data to hold name as a link to
// target, with the link's type, as the library decodes it and as every
// reader takes it, and to write again to the same bytes.
func readsBackAsLink(t *testing.T, data []byte, name, target string) {
	t.Helper()
	raw, err := parseRecordRaw(data)
	if err != nil {
		t.Fatalf("%s -> %s: %v", name, target, err)
	}
	back, err := ParseRecord(data)
	if err != nil {
		t.Fatalf("%s -> %s: %v", name, target, err)
	}
	if again := listingOf(back).Canonical(); again != string(data) {
		t.Errorf("%s -> %s: the record does not write again to its bytes:\n%s---\n%s", name, target, data, again)
	}
	for i, e := range raw {
		if e.Name != name {
			continue
		}
		p := back[i]
		if !IsLink(e) || e.Target != target || e.Mode != LinkMode || e.FlowDirection != c4m.FlowNone || e.FlowTarget != "" {
			t.Errorf("%s -> %s: decoded as mode %v, target %q, flow %v %q", name, target, e.Mode, e.Target, e.FlowDirection, e.FlowTarget)
		}
		if !IsLink(p) || p.Target != target || p.Mode != LinkMode {
			t.Errorf("%s -> %s: read as mode %v, target %q", name, target, p.Mode, p.Target)
		}
		return
	}
	t.Errorf("%s -> %s: not in the record:\n%s", name, target, data)
}

// recordOf returns the data of the record of dir ("" for the root) among
// recs.
func recordOf(t *testing.T, recs []Record, dir string) []byte {
	t.Helper()
	for _, r := range recs {
		if r.dir == dir {
			return r.Data
		}
	}
	t.Fatalf("no record of %q", dir)
	return nil
}

// A record or changeset the c4m library writes and does not read back as
// written is never returned to be stored: Apply refuses the edits with an
// error that names the entry. The library this package is built with
// wrote a symlink whose target starts with "c4" in a form it read as a hard
// link (fixed in c4 #92), and a null-mode link to host:path in a form it
// read as a flow link (gone since views record a link's type, LinkMode);
// those targets are taken now. No entry is known that the library does not
// read back, so the disagreement refused here is injected (misread), and
// any real one is caught the same way.
func TestApplyRefusesWhatDoesNotReadBack(t *testing.T) {
	ctx := context.Background()
	ram := store.NewRAM()
	src := StoreRecords(ram)
	putBytes(t, ram, EmptyID(), nil)
	idText := c4.Identify(strings.NewReader("some content")).String()
	ok := append([]string{"a.txt", "../objects/" + idText, "studio/c4", idText, idText + ".exr", "c4studio"}, locationTargets...)

	for _, target := range ok {
		if _, err := Apply(ctx, EmptyID(), []Edit{{Path: "d/link", Entry: SymlinkEntry("", target)}}, src); err != nil {
			t.Errorf("a link to %q: %v", target, err)
		}
	}
	// What is checked is what was written: a record that does not come
	// back the same is caught even when every entry, written alone, does.
	if err := checkRecord([]byte("not a record\n"), nil, ""); err == nil {
		t.Fatal("a record that does not parse was taken")
	}
	var nr *NotReadBack
	if err := checkChangeset(EmptyID(), []byte(EmptyID().String()+"\n"), nil); !errors.As(err, &nr) || nr.Entry != "" {
		t.Fatalf("a changeset with no section: %v", err)
	}

	misread(t, "link")
	// Among other entries of the same directory, before and after it.
	res, err := Apply(ctx, EmptyID(), []Edit{fileEdit("keep.txt", "keep"), fileEdit("d/a.txt", "a"), {Path: "d/link", Entry: SymlinkEntry("", "a.txt")}, fileEdit("d/z.txt", "z")}, src)
	nr = nil
	if !errors.As(err, &nr) {
		t.Fatalf("a link the library reads back otherwise was not refused: %v", err)
	}
	if res != nil || nr.Entry != "d/link" || nr.What != "record" {
		t.Errorf("refused as %+v, with a result %v", nr, res != nil)
	}
	if !strings.Contains(err.Error(), `entry "d/link"`) || !strings.Contains(err.Error(), "so it is not stored") || !strings.Contains(nr.Reason, "written as") || !strings.Contains(nr.Reason, "~misread") {
		t.Errorf("the refusal: %v", err)
	}
}

// A link whose target reads as a location (`host:path`, `C:foo`,
// `nas:plates/`) is stored as the link it is: under a null mode the
// library read it back as a flow link, and Apply refused it; a view now
// records a link's type (lrwxrwxrwx), so its record and its changeset read
// back as a link to that target, and the view loads with it.
func TestALinkToALocationIsStoredAsALink(t *testing.T) {
	ctx := context.Background()
	for _, target := range locationTargets {
		ram := store.NewRAM()
		src := StoreRecords(ram)
		putBytes(t, ram, EmptyID(), nil)
		res, err := Apply(ctx, EmptyID(), []Edit{fileEdit("keep.txt", "keep"), {Path: "d/link", Entry: SymlinkEntry("", target)}, fileEdit("d/z.txt", "z")}, src)
		if err != nil {
			t.Fatalf("a link to %q: %v", target, err)
		}
		readsBackAsLink(t, recordOf(t, res.Records, "d/"), "link", target)
		c, err := ReadChain(res.Changeset)
		if err != nil || len(c.Sections) != 1 {
			t.Fatalf("a link to %q: the changeset: %v", target, err)
		}
		found := false
		for _, e := range c.Sections[0].Patch.Entries {
			if e.Name == "link" {
				found = true
				if !IsLink(e) || e.Target != target || e.Mode != LinkMode || e.FlowDirection != c4m.FlowNone {
					t.Errorf("a link to %q: the changeset reads mode %v, target %q, flow %v %q", target, e.Mode, e.Target, e.FlowDirection, e.FlowTarget)
				}
			}
		}
		if !found {
			t.Errorf("a link to %q: not in the changeset:\n%s", target, res.Changeset)
		}
		for _, r := range res.Records {
			putBytes(t, ram, r.ID, r.Data)
		}
		v, err := Load(ctx, res.ID, src, 4)
		if err != nil {
			t.Fatalf("a link to %q: %v", target, err)
		}
		if len(v.Symlinks) != 1 || v.Symlinks[0] != (Symlink{Path: "d/link", Target: target}) {
			t.Errorf("a link to %q: the view loads with %v", target, v.Symlinks)
		}
	}
}

// The changeset is checked as its readers read it: one section, over the
// parent it was written over, that writes again to the same bytes. An
// entry the library does not read back as written is named in a changeset
// as in a record.
func TestAChangesetIsCheckedAsItIsRead(t *testing.T) {
	parent := EmptyID()
	ok := c4m.NewManifest()
	ok.Entries = []*c4m.Entry{SymlinkEntry("link", "a.txt")}
	good, err := EncodeChangeset(parent, ok)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkChangeset(parent, good, ok.Entries); err != nil {
		t.Fatalf("a changeset that reads back: %v", err)
	}
	var nr *NotReadBack
	other := c4.Identify(strings.NewReader("another view"))
	if err := checkChangeset(other, good, ok.Entries); !errors.As(err, &nr) || !strings.Contains(err.Error(), "sections over") {
		t.Fatalf("a changeset over another parent than the one it was written over: %v", err)
	}
	// A link to a location reads back as one in a changeset too.
	for _, target := range locationTargets {
		loc := c4m.NewManifest()
		loc.Entries = []*c4m.Entry{SymlinkEntry("link", target)}
		text, err := EncodeChangeset(parent, loc)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkChangeset(parent, text, loc.Entries); err != nil {
			t.Errorf("a changeset with a link to %q: %v", target, err)
		}
	}
	misread(t, "link")
	nr = nil
	if err := checkChangeset(parent, good, ok.Entries); !errors.As(err, &nr) || nr.What != "changeset" || nr.Entry != "link" || !strings.Contains(err.Error(), "of a changeset") {
		t.Fatalf("a changeset the library does not read back as written: %v", err)
	}
}

// The reason says what was written and what was read back, line for line,
// when the text comes back different.
func TestTheReasonSaysWhatCameBack(t *testing.T) {
	if w, r := firstDiff("a\nb\nc", "a\nB\nc"); w != "b" || r != "B" {
		t.Fatalf("firstDiff: %q, %q", w, r)
	}
	if w, r := firstDiff("a\nb", "a"); w != "b" || r != "" {
		t.Fatalf("firstDiff of a longer text: %q, %q", w, r)
	}
	if w, r := firstDiff("a", "a\nb"); w != "" || r != "b" {
		t.Fatalf("firstDiff of a shorter text: %q, %q", w, r)
	}
}

// A view scanned from a real tree, as ingest and the other writers of
// View.Records make one, is checked where its records are made. A link to
// host:path on disk is recorded as that link and reads back as one; an
// entry the library does not read back is an error naming its path, and
// no record is handed out to be stored.
func TestRecordsOfAScannedTreeAreChecked(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tools", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "tools", "latest")
	for _, target := range locationTargets {
		os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		v, err := ScanDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		recs, err := v.Records()
		if err != nil || len(recs) != 2 {
			t.Fatalf("the records of a scanned tree with a link to %q: %d, %v", target, len(recs), err)
		}
		readsBackAsLink(t, recordOf(t, recs, "tools/"), "latest", target)
	}
	misread(t, "latest")
	v, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := v.Records()
	var nr *NotReadBack
	if !errors.As(err, &nr) || nr.Entry != "tools/latest" || nr.What != "record" || recs != nil {
		t.Fatalf("the records of a scanned tree with a link the library reads back otherwise: %d, %v", len(recs), err)
	}
	// Without the link, every record is handed out.
	os.Remove(link)
	v, _ = ScanDir(dir)
	if recs, err := v.Records(); err != nil || len(recs) != 2 {
		t.Fatalf("the records of an ordinary tree: %d, %v", len(recs), err)
	}
}

// A changeset names the entry it does not read back by its path, from the
// depths of the entries above it, and among other entries: the projection
// and the naming reach every entry, not only the first.
func TestAChangesetNamesItsEntryByPath(t *testing.T) {
	parent := EmptyID()
	dir := func(name string, depth int) *c4m.Entry {
		e := &c4m.Entry{Name: name + "/", Size: -1, Timestamp: c4m.NullTimestamp(), Depth: depth}
		return e
	}
	at := func(e *c4m.Entry, depth int) *c4m.Entry { e.Depth = depth; return e }
	m := c4m.NewManifest()
	// The bad entry comes after the patch has gone down two directories and
	// come back up, so its path is only right if the levels left are left.
	m.Entries = []*c4m.Entry{
		at(SymlinkEntry("a", "a.txt"), 0),
		dir("x", 0),
		at(SymlinkEntry("ok", "b.txt"), 1),
		dir("y", 1),
		at(SymlinkEntry("deep", "d.txt"), 2),
		dir("w", 0),
		at(SymlinkEntry("bad", "host:path"), 1),
		at(SymlinkEntry("z", "c.txt"), 0),
	}
	text, err := EncodeChangeset(parent, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkChangeset(parent, text, m.Entries); err != nil {
		t.Fatalf("a changeset with a link to host:path: %v", err)
	}
	misread(t, "bad")
	var nr *NotReadBack
	if err := checkChangeset(parent, text, m.Entries); !errors.As(err, &nr) || nr.Entry != "w/bad" || nr.What != "changeset" {
		t.Fatalf("a changeset with a bad entry after two levels left: %v", err)
	}
}

// The entry a view's records name is the same every time, whichever of
// several identical subtrees the records keep.
func TestRecordsNameTheSameEntryEveryTime(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"d", "c", "b", "a"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("host:path", filepath.Join(dir, d, "l")); err != nil {
			t.Fatal(err)
		}
	}
	misread(t, "l")
	for range 20 {
		v, err := ScanDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = v.Records()
		var nr *NotReadBack
		if !errors.As(err, &nr) || nr.Entry != "a/l" {
			t.Fatalf("named %v, want a/l every time", err)
		}
	}
}

// A name or a link target ending in a backslash is a legal c4m name. The
// library pinned before c4 #98 wrote it in a form it read back with one
// more backslash, and the read-back check refused it; the pin now carries
// #98, so it reads back as written and is taken, like a backslash anywhere
// else.
func TestATrailingBackslashReadsBack(t *testing.T) {
	ctx := context.Background()
	ram := store.NewRAM()
	src := StoreRecords(ram)
	putBytes(t, ram, EmptyID(), nil)
	for _, e := range []Edit{fileEdit(`ends\`, "x"), {Path: "l", Entry: SymlinkEntry("", `t\`)}} {
		if _, err := Apply(ctx, EmptyID(), []Edit{e}, src); err != nil {
			t.Errorf("%s: %v", e.Path, err)
		}
	}
	for _, p := range []string{`mid\dle`, `two\\in\\a\\row.txt`, `d\/x`} {
		if _, err := Apply(ctx, EmptyID(), []Edit{fileEdit(p, "x")}, src); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// The c4m name rules, and nothing more: a backslash, a colon, a trailing
// dot and a device name are names; "/" and NUL are not, nor are "", "."
// and "..".
func TestCheckNameGuardsTheC4mRulesOnly(t *testing.T) {
	for _, n := range []string{`back\slash`, `a:b`, "trailing.", "CON", "a b", `x\`, "dir/"} {
		if err := CheckName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", "/", ".", "..", "a/b", "nul\x00", "./"} {
		if err := CheckName(n); err == nil {
			t.Errorf("%q was taken", n)
		}
	}
}

// The read-back seam is for tests only: outside a _test.go file nothing in
// the module calls MisreadForTest or assigns readBack (but its declaration
// and MisreadForTest itself), so no build of a release can misread an entry.
func TestTheReadBackSeamIsForTestsOnly(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".worktrees" || d.Name() == "vendor" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && p != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		own := filepath.Base(p) == "roundtrip.go" && filepath.Base(filepath.Dir(p)) == "view"
		for i, line := range strings.Split(string(data), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") {
				continue
			}
			if strings.Contains(code, "MisreadForTest(") && !(own && strings.HasPrefix(code, "func MisreadForTest(")) {
				t.Errorf("%s:%d calls MisreadForTest outside a test", p, i+1)
			}
			if own {
				continue
			}
			if strings.Contains(code, "readBack =") || strings.Contains(code, "readBack=") {
				t.Errorf("%s:%d assigns the read-back seam", p, i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
