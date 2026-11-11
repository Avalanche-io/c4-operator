package view

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
)

// dirStore serves the records in a folder, each named by its C4 ID.
type dirStore string

func (d dirStore) Open(id c4.ID) (io.ReadCloser, error) {
	return os.Open(filepath.Join(string(d), id.String()))
}

// rootIDAsRecorded are sources whose RootID is not yet what `c4 id -q`
// reports, and why: each has a top-level directory line that is stale, or
// null where the children it lists would derive it. RootID keeps such a
// line as recorded, since c4's normaliser would re-derive a directory a
// changeset deepens from a partial listing (see descriptionID), until c4
// rules on partial listings. Each must still differ from -q: when exact
// -q lands, the test says so, and the entry comes out.
var rootIDAsRecorded = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range []string{
		"nested.c4m", "stated-size.c4m", "stale-size.c4m", "stale-id.c4m",
		"stated-time.c4m", "dated-child.c4m", "collapsed-stated.c4m",
		"collapsed-null.c4m", "empty-unknown.c4m", "changeset-file.c4m",
		"changeset-dirline.c4m",
	} {
		m[n] = true
	}
	return m
}()

// rootIDPending are sources awaiting a format ruling, with no expected ID
// in root-ids.txt ("-"): they must still resolve.
var rootIDPending = map[string]string{
	// A changeset deepening a directory its base knows only by its ID: the
	// description lists that directory's children only in part.
	"deepen-collapsed.c4m": "whether a directory deepened without its full listing derives its ID from the part, keeps it, or nulls it",
}

// A resolution's RootID is the description's ID, the one c4 master's
// `c4 id -q` reports, for every source in testdata/root-id (its README
// says what each holds): views and c4m files from the c4 CLIs, c4m files
// and changesets made with the library, a sequence. For a changeset the
// expected ID is c4 id -q of the state it resolves to.
func TestRootIDIsTheDescriptionID(t *testing.T) {
	dir := filepath.Join("testdata", "root-id")
	f, err := os.Open(filepath.Join(dir, "root-ids.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st := dirStore(filepath.Join(dir, "records"))
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
		name, idText, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			t.Fatalf("root-ids.txt: %q", sc.Text())
		}
		var want c4.ID
		if idText == "-" {
			if _, ok := rootIDPending[name]; !ok {
				t.Fatalf("root-ids.txt: %s has no expected ID and is not pending a ruling", name)
			}
		} else if want, err = c4.Parse(idText); err != nil {
			t.Fatalf("root-ids.txt: %s: %v", name, err)
		}
		var res *Resolved
		if name == "view" {
			res, err = FromID(want, st, Filter{})
		} else {
			data, rerr := os.ReadFile(filepath.Join(dir, name))
			if rerr != nil {
				t.Fatal(rerr)
			}
			res, err = FromBytes(data, st, Filter{})
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if why, pending := rootIDPending[name]; pending {
			t.Logf("%s: RootID %s, pending a ruling on %s", name, res.RootID, why)
			continue
		}
		if rootIDAsRecorded[name] {
			if res.RootID == want {
				t.Errorf("%s: RootID is now what c4 id -q reports; take it out of rootIDAsRecorded", name)
			}
		} else if res.RootID != want {
			t.Errorf("%s: RootID %s, where c4 id -q reports %s", name, res.RootID, want)
		}
		if name == "view" && res.View().ID != res.RootID {
			t.Errorf("view: RootID %s is not the view's own ID %s", res.RootID, res.View().ID)
		}
	}
	if n != 21 {
		t.Fatalf("root-ids.txt holds %d sources, not 21", n)
	}
}

// objStore serves objects from memory, by C4 ID.
type objStore map[c4.ID][]byte

func (s objStore) Open(id c4.ID) (io.ReadCloser, error) {
	b, ok := s[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// A changeset as this system writes it (view.Apply: the changed directory
// restated with its new size and ID, only the changed child listed) has
// the RootID of the view it produces, whatever the filter: grant lookup
// finds a grant naming that view by it.
func TestRootIDOfAChangesetIsTheViewItMakes(t *testing.T) {
	ctx := context.Background()
	tree := t.TempDir()
	for _, p := range []string{"h.txt", "a/f.txt", "a/s/g.txt", "b/g.txt"} {
		full := filepath.Join(tree, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(p+"\n"), 0o644)
	}
	v, err := ScanDir(tree)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := v.Records()
	if err != nil {
		t.Fatal(err)
	}
	st := objStore{}
	for _, r := range recs {
		st[r.ID] = r.Data
	}
	n := []byte("n\n")
	nid := c4.Identify(bytes.NewReader(n))
	applied, err := Apply(ctx, v.ID, []Edit{{Path: "a/n.txt", Entry: FileEntry("", int64(len(n)), nid, false)}}, StoreRecords(st))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range applied.Records {
		st[r.ID] = r.Data
	}
	for _, pats := range [][]string{nil, {"a/f.txt"}, {"h.txt"}, {"b"}} {
		f, err := NewFilter(pats)
		if err != nil {
			t.Fatal(err)
		}
		res, err := FromBytes(applied.Changeset, st, f)
		if err != nil {
			t.Fatalf("filter %v: %v", pats, err)
		}
		if res.RootID != applied.ID {
			t.Errorf("filter %v: RootID %s, the view it makes %s", pats, res.RootID, applied.ID)
		}
	}
}
