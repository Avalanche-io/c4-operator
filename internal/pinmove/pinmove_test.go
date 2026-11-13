package pinmove

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// ram is a project store in memory that counts what is put.
type ram struct {
	*store.RAM
	puts int
	fail error // every Open fails with it, when set
}

func (r *ram) Open(id c4.ID) (io.ReadCloser, error) {
	if r.fail != nil {
		return nil, r.fail
	}
	return r.RAM.Open(id)
}

func (r *ram) PutBytes(_ context.Context, data []byte) (c4.ID, error) {
	id := c4.Identify(bytes.NewReader(data))
	if r.Has(id) {
		return id, nil
	}
	r.puts++
	return r.Put(bytes.NewReader(data))
}

// fixture is the view package's store written at the c4 pin before the
// move (internal/view/testdata/pin19c9): its views and changesets by name.
type fixture struct {
	st                *ram
	views, changesets map[string]c4.ID
}

// renamedByPin are the fixture's views c4 #107 gives new IDs: a
// null-sized entry two or more directories down.
var renamedByPin = []string{"chain1", "chain2", "link-depth2", "mixed-deep", "null-depth2"}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	dir := filepath.Join("..", "view", "testdata", "pin19c9")
	f := fixture{st: &ram{RAM: store.NewRAM()}, views: map[string]c4.ID{}, changesets: map[string]c4.ID{}}
	files, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range files {
		data, err := os.ReadFile(filepath.Join(dir, "objects", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.Put(bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(index), "\n") {
		w := strings.Fields(l)
		if len(w) < 3 || strings.HasPrefix(l, "#") {
			continue
		}
		id, err := c4.Parse(w[2])
		if err != nil {
			t.Fatal(err)
		}
		if w[0] == "view" {
			f.views[w[1]] = id
		} else {
			f.changesets[w[1]] = id
		}
	}
	return f
}

// Every C4 ID a holder names is found, by its path; text that holds an ID
// among other text is not one.
func TestIDsInFindsEveryIDAHolderNames(t *testing.T) {
	a, b, c := c4.Identify(strings.NewReader("a")), c4.Identify(strings.NewReader("b")), c4.Identify(strings.NewReader("c"))
	j := &c4v1.WorkJob{}
	j.Status.Output = &c4v1.WorkOutput{View: a.String(), Chain: "blocks:" + b.String()}
	j.Status.UnitOutputs = map[string]string{"3": c.String()}
	held, err := IDsIn("WorkJob ns/j", j)
	if err != nil {
		t.Fatal(err)
	}
	got := map[c4.ID]string{}
	for _, h := range held {
		got[h.ID] = h.Path
	}
	if len(got) != 2 || got[a] != ".status.output.view" || got[c] != ".status.unitOutputs.3" {
		t.Fatalf("found %v", got)
	}
}

// The survey re-identifies each view its holders name and leaves the rest:
// a changeset and content are not views, and an ID the store does not
// hold is missing. Writing stores every renamed view's records, then the
// table; a second run finds nothing to do and writes nothing.
func TestASurveyRenamesTheViewsAndOnlyTheViews(t *testing.T) {
	ctx := context.Background()
	f := loadFixture(t)
	var held []Held
	for name, id := range f.views {
		held = append(held, Held{ID: id, Holder: "Project p", Path: ".spec.grants." + name})
	}
	absent := c4.Identify(strings.NewReader("a view the store never held"))
	content := c4.Identify(strings.NewReader("t")) // a file in the fixture's views
	held = append(held,
		Held{ID: f.changesets["chain1"], Holder: "Materialization ns/m", Path: ".spec.source.id"},
		Held{ID: content, Holder: "WorkJob ns/j", Path: ".status.output.view"},
		Held{ID: absent, Holder: "WorkJob ns/j", Path: ".status.resume.0"},
		Held{ID: f.views["null-depth2"], Holder: "Materialization ns/m", Path: ".status.viewID"},
	)
	p, err := Survey(ctx, f.st, nil, held, 4)
	if err != nil {
		t.Fatal(err)
	}
	renamed := p.Renames()
	if len(renamed) != len(renamedByPin) {
		t.Fatalf("%d renamed, want %d", len(renamed), len(renamedByPin))
	}
	for _, name := range renamedByPin {
		if _, ok := renamed[f.views[name]]; !ok {
			t.Errorf("%s not renamed", name)
		}
	}
	for _, fd := range p.Findings {
		switch fd.ID {
		case f.changesets["chain1"], content:
			if fd.NotAView == "" {
				t.Errorf("%s (%s) taken for a view", fd.ID, fd.Held[0].Path)
			}
		case absent:
			if !fd.Missing {
				t.Errorf("the absent ID: %+v", fd)
			}
		case f.views["null-depth2"]:
			if len(fd.Held) != 2 {
				t.Errorf("null-depth2 held by %d", len(fd.Held))
			}
		}
	}
	if f.st.puts != 0 {
		t.Fatalf("the survey stored %d objects", f.st.puts)
	}

	table, err := p.WriteTable(ctx, f.st, c4.ID{})
	if err != nil {
		t.Fatal(err)
	}
	back, err := view.ReadRenames(f.st, table)
	if err != nil || !bytes.Equal(back.Bytes(), p.Table.Bytes()) {
		t.Fatalf("the stored table: %v", err)
	}
	for old, cur := range back {
		if _, err := view.Load(ctx, cur, view.StoreRecords(f.st), 4); err != nil {
			t.Errorf("%s renamed to %s, which does not load: %v", old, cur, err)
		}
		if got, err := view.ResolveOld(f.st, table, old); err != nil || got != cur {
			t.Errorf("%s resolves to %s (%v)", old, got, err)
		}
	}

	puts := f.st.puts
	again, err := Survey(ctx, f.st, back, held, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Renames()) != 0 {
		t.Fatalf("a second run renames %v", again.Renames())
	}
	if id, err := again.WriteTable(ctx, f.st, table); err != nil || id != table || f.st.puts != puts {
		t.Fatalf("a second run wrote %d objects, table %s (%v)", f.st.puts-puts, id, err)
	}
}

// A store that cannot be read stops the survey: an ID left unread is never
// taken for one that is not a view.
func TestAStoreThatCannotBeReadStopsTheSurvey(t *testing.T) {
	f := loadFixture(t)
	f.st.fail = errors.New("injected: the store is down")
	_, err := Survey(context.Background(), f.st, nil, []Held{{ID: f.views["plain"], Holder: "Project p"}}, 4)
	if err == nil || !strings.Contains(err.Error(), "the store is down") {
		t.Fatalf("survey: %v", err)
	}
}

// A grant on a renamed view gains an entry for the new ID with the same
// paths, once.
func TestAGrantGainsTheNewIDOnce(t *testing.T) {
	a, b, c := c4.Identify(strings.NewReader("a")), c4.Identify(strings.NewReader("b")), c4.Identify(strings.NewReader("c"))
	r := view.Renames{a: b}
	grants := []c4v1.Grant{{Tree: a.String(), Paths: []string{"plates/**"}}, {Tree: c.String()}}
	out, added := ExtendGrants(grants, r)
	if len(added) != 1 || len(out) != 3 || out[2].Tree != b.String() || len(out[2].Paths) != 1 || out[2].Paths[0] != "plates/**" {
		t.Fatalf("grants %+v, added %+v", out, added)
	}
	if again, added := ExtendGrants(out, r); len(added) != 0 || len(again) != 3 {
		t.Fatalf("again: %+v, added %+v", again, added)
	}
}
