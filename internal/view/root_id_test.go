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
	// a/ is null and holds d/, known only by its ID, with a null size: the
	// pinned normaliser reads d/ as empty and counts 0 into a/.
	"collapsed-null.c4m": "the pinned c4m library reads a collapsed directory's null size as 0; fixed in c4 50ba056",
	// a/ is null and holds e/, with neither an ID nor children: the pinned
	// normaliser reads e/ as empty too.
	"empty-unknown.c4m": "the pinned c4m library reads a directory nobody read as empty; fixed in c4 0a27b6d",
	// a/ states its own time; the pinned normaliser replaces it with its
	// newest child's.
	"stated-time.c4m":       "the pinned c4m normaliser replaces a stated directory time with the newest child's; fixed in c4 f13056e",
	"changeset-dirline.c4m": "the pinned c4m normaliser replaces a stated directory time with the newest child's; fixed in c4 f13056e",
}

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
	}
	if n != 21 {
		t.Fatalf("root-ids.txt holds %d sources, not 21", n)
	}
}
