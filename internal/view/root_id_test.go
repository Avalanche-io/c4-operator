package view

import (
	"bufio"
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

// rootIDGaps are sources whose RootID the pinned c4m library cannot get
// right yet, and why. Each must still be wrong: when a pin move makes it
// right, the test says so, and the entry comes out.
var rootIDGaps = map[string]string{
	// a/ is null and holds d/, known only by its ID, with a null size. The
	// library's normaliser reads d/ as empty and counts 0 into a/, as
	// ComputeC4ID did at the top; c4 50ba056 keeps the null.
	"collapsed-null.c4m": "the pinned c4m library reads a collapsed directory's null size as 0; fixed in c4 50ba056",
}

// A resolution's RootID is the description's ID, the one c4 master's
// `c4 id -q` reports, for every kind of source in testdata/root-id: a
// view's ID, the view as a c4m, the c4 CLI's full c4m, c4 master's scan
// chain, and five made with the library: a top-level directory with a
// null size and ID and its child listed; one with its size stated and its
// ID null; one null throughout with a dated child; and two whose null top-level directory holds a directory known
// only by its ID (read from the store, which is not the description's and
// is left out), its size stated or null. The first two hold a directory known only
// by its ID with a null size (it holds a link), which ComputeC4ID read as
// empty; the last needs its child to derive the line.
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
		name, idText, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			t.Fatalf("root-ids.txt: %q", sc.Text())
		}
		want, err := c4.Parse(idText)
		if err != nil {
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
		if why, known := rootIDGaps[name]; known {
			if res.RootID == want {
				t.Errorf("%s: RootID is now right; take it out of rootIDGaps (%s)", name, why)
			}
		} else if res.RootID != want {
			t.Errorf("%s: RootID %s, where c4 id -q reports %s", name, res.RootID, want)
		}
		if name == "view" && res.View().ID != res.RootID {
			t.Errorf("view: RootID %s is not the view's own ID %s", res.RootID, res.View().ID)
		}
		n++
	}
	if n != 9 {
		t.Fatalf("root-ids.txt holds %d sources, not 9", n)
	}
}
