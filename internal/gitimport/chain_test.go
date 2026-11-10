package gitimport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// history is a few changesets over the empty view, each adding or
// changing a file, with their records in a RAM store.
type history struct {
	ram        *store.RAM
	src        view.RecordSource
	views      []c4.ID  // views[0] is the empty view
	changesets [][]byte // changesets[i] takes views[i] to views[i+1]
}

func newHistory(t *testing.T, steps int) *history {
	t.Helper()
	h := &history{ram: store.NewRAM(), views: []c4.ID{view.EmptyID()}}
	h.src = view.StoreRecords(h.ram)
	h.put(t, nil) // the empty directory's record
	for i := range steps {
		content := fmt.Sprintf("content %d\n", i)
		edits := []view.Edit{
			{Path: fmt.Sprintf("dir/f%d.txt", i), Entry: view.FileEntry("", int64(len(content)), c4.Identify(strings.NewReader(content)), false)},
			{Path: "same.txt", Entry: view.FileEntry("", int64(len(content)), c4.Identify(strings.NewReader(content)), i%2 == 0)},
		}
		a, err := view.Apply(context.Background(), h.views[i], edits, h.src)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range a.Records {
			h.put(t, r.Data)
		}
		h.views = append(h.views, a.ID)
		h.changesets = append(h.changesets, a.Changeset)
	}
	return h
}

func (h *history) put(t *testing.T, data []byte) {
	t.Helper()
	id := c4.Identify(bytes.NewReader(data))
	if h.ram.Has(id) {
		return
	}
	w, err := h.ram.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	w.Close()
}

// oldFile writes the first n changesets as a chain file written before
// 2026-11-09 held them: the base, then each changeset without its first
// line and with the bare ID of the view it produces after it.
func (h *history) oldFile(t *testing.T, n int) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString(h.views[0].String() + "\n")
	for i := range n {
		ch, err := view.ReadChain(h.changesets[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := c4m.NewEncoder(&out).Encode(ch.Sections[0].Patch); err != nil {
			t.Fatal(err)
		}
		out.WriteString(h.views[i+1].String() + "\n")
	}
	return out.Bytes()
}

func lastLineOf(data []byte) string {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	return lines[len(lines)-1]
}

func isBareID(line string) bool {
	id, err := c4.Parse(line)
	return err == nil && len(line) == 90 && id.String() == line
}

// sinkRecorder keeps what a chain stores.
type sinkRecorder struct{ objs map[c4.ID][]byte }

func (s *sinkRecorder) Put(_ context.Context, id c4.ID, _ int64, r io.Reader) (bool, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return false, err
	}
	s.objs[id] = data
	return true, nil
}

func (s *sinkRecorder) Copy(context.Context, c4.ID, int64, Source) (bool, error) {
	return false, nil
}

// A chain file is its commits' changesets, one after another: an append is
// a concatenation, the file never ends with a bare C4 ID, and reopening it
// recomputes the head.
func TestChainFileIsItsChangesetsJoined(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 4)
	p := filepath.Join(t.TempDir(), "main.c4m")
	ch, err := OpenChainFile(ctx, p, h.src)
	if err != nil || !ch.Empty() {
		t.Fatalf("a chain file that does not exist: %v", err)
	}
	var want []byte
	for i, cs := range h.changesets {
		if err := ch.Append(cs, h.views[i], h.views[i+1]); err != nil {
			t.Fatal(err)
		}
		want = append(want, cs...)
		data, _ := os.ReadFile(p)
		if !bytes.Equal(data, want) {
			t.Fatalf("after %d appends the file is not the changesets joined\n%s", i+1, data)
		}
		if isBareID(lastLineOf(data)) {
			t.Fatalf("after %d appends the file ends with a bare C4 ID", i+1)
		}
		if ch.Head != h.views[i+1] || ch.Sections != i+1 || ch.Base != h.views[0] {
			t.Fatalf("after %d appends: %+v", i+1, ch)
		}
		// Reopened, the head is recomputed from the last section.
		again, err := OpenChainFile(ctx, p, h.src)
		if err != nil || again.Head != ch.Head || again.Sections != ch.Sections || again.Base != ch.Base {
			t.Fatalf("reopened after %d appends: %+v, %v", i+1, again, err)
		}
	}
	// The whole chain verifies to the views.
	data, _ := os.ReadFile(p)
	c, err := view.ReadChain(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Verify(ctx, h.src)
	if err != nil || len(got) != len(h.views)-1 || got[len(got)-1] != h.views[len(h.views)-1] {
		t.Fatalf("the chain file verifies to %d views: %v", len(got), err)
	}
	// What is served and stored is the file.
	text, err := ReadChainFile(p)
	if err != nil || !bytes.Equal(text, data) {
		t.Fatalf("the served text is not the file: %v", err)
	}

	// A changeset that does not apply to the head is refused, and so is
	// one that does not start with the parent it is said to have.
	if err := ch.Append(h.changesets[0], h.views[0], h.views[1]); err == nil {
		t.Fatal("appended a changeset whose parent is not the head")
	}
	if err := ch.Append(h.changesets[0], ch.Head, h.views[1]); err == nil {
		t.Fatal("appended a changeset that does not start with the parent given")
	}
	if now, _ := os.ReadFile(p); !bytes.Equal(now, data) {
		t.Fatal("a refused append changed the file")
	}
}

// A re-based chain is its base's ID alone until its first changeset, which
// restates that base and so replaces the line.
func TestRebasedChainFile(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 3)
	p := filepath.Join(t.TempDir(), "main.c4m")
	ch, _ := OpenChainFile(ctx, p, h.src)
	if err := ch.Append(h.changesets[0], h.views[0], h.views[1]); err != nil {
		t.Fatal(err)
	}
	if err := ch.Rebase(h.views[1]); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != h.views[1].String()+"\n" {
		t.Fatalf("a re-based chain file: %q", data)
	}
	again, err := OpenChainFile(ctx, p, h.src)
	if err != nil || again.Base != h.views[1] || again.Head != h.views[1] || again.Sections != 0 || again.Empty() {
		t.Fatalf("a re-based chain reopened: %+v, %v", again, err)
	}
	if err := again.Append(h.changesets[1], h.views[1], h.views[2]); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p)
	if !bytes.Equal(data, h.changesets[1]) {
		t.Fatalf("the first changeset after a re-base does not replace the base line\n%s", data)
	}
	if err := again.Append(h.changesets[2], h.views[2], h.views[3]); err != nil {
		t.Fatal(err)
	}
	last, err := OpenChainFile(ctx, p, h.src)
	if err != nil || last.Base != h.views[1] || last.Head != h.views[3] || last.Sections != 2 {
		t.Fatalf("after two appends: %+v, %v", last, err)
	}
}

// A chain file written before 2026-11-09 ends with its head's ID. It is
// opened (the line checked against the head recomputed), appended to, and
// never stored or served in that form.
func TestAnOldChainFileIsStillRead(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 4)
	p := filepath.Join(t.TempDir(), "main.c4m")
	old := h.oldFile(t, 3)
	if !isBareID(lastLineOf(old)) {
		t.Fatal("the old form ends with a bare ID")
	}
	if err := os.WriteFile(p, old, 0o644); err != nil {
		t.Fatal(err)
	}
	ch, err := OpenChainFile(ctx, p, h.src)
	if err != nil || ch.Head != h.views[3] || ch.Sections != 3 || ch.Base != h.views[0] {
		t.Fatalf("an old chain file: %+v, %v", ch, err)
	}
	// Served or stored before any append, it has the current form already.
	current := bytes.Join(h.changesets[:3], nil)
	text, err := ReadChainFile(p)
	if err != nil || !bytes.Equal(text, current) {
		t.Fatalf("an old chain file is served as\n%s\nwant\n%s\n(%v)", text, current, err)
	}
	sink := &sinkRecorder{objs: map[c4.ID][]byte{}}
	id, _, err := ch.Store(ctx, sink)
	if err != nil || !bytes.Equal(sink.objs[id], current) || id != c4.Identify(bytes.NewReader(current)) {
		t.Fatalf("an old chain file is stored in the old form: %v", err)
	}
	// Appended to, the old last line is the boundary before the new
	// section, and the chain verifies end to end.
	if err := ch.Append(h.changesets[3], h.views[3], h.views[4]); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	c, err := view.ReadChain(data)
	if err != nil || !c.Seal.IsNil() {
		t.Fatalf("an old chain file after an append: %v", err)
	}
	if got, err := c.Verify(ctx, h.src); err != nil || len(got) != 4 || got[3] != h.views[4] {
		t.Fatalf("an old chain file after an append verifies to %d views: %v", len(got), err)
	}
	if text, err := ReadChainFile(p); err != nil || !bytes.Equal(text, bytes.Join(h.changesets, nil)) {
		t.Fatalf("an old chain file after an append is not served in the current form: %v", err)
	}
	again, err := OpenChainFile(ctx, p, h.src)
	if err != nil || again.Head != h.views[4] || again.Sections != 4 {
		t.Fatalf("reopened: %+v, %v", again, err)
	}

	// An old file whose last line is not the view it resolves to is
	// refused, and says so.
	forged := c4.Identify(strings.NewReader("forged"))
	bad := append(append([]byte(nil), old[:len(old)-91]...), forged.String()+"\n"...)
	if err := os.WriteFile(p, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenChainFile(ctx, p, h.src); err == nil || !strings.Contains(err.Error(), forged.String()) {
		t.Fatalf("an old chain file with a wrong last line opened: %v", err)
	}
	// A file that does not parse is an error, not a chain to append to.
	if err := os.WriteFile(p, append(append([]byte(nil), current...), "- - torn"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenChainFile(ctx, p, h.src); err == nil {
		t.Fatal("a chain file with a torn last line opened")
	}
}

// kindChange is a history whose last commit replaces a directory with a
// file: base holds lib/a.txt; the commit removes lib/ and writes lib.
func kindChange(t *testing.T) *history {
	t.Helper()
	h := &history{ram: store.NewRAM(), views: []c4.ID{view.EmptyID()}}
	h.src = view.StoreRecords(h.ram)
	h.put(t, nil)
	file := func(content string) *c4m.Entry {
		return view.FileEntry("", int64(len(content)), c4.Identify(strings.NewReader(content)), false)
	}
	for _, edits := range [][]view.Edit{
		{{Path: "lib/a.txt", Entry: file("a")}, {Path: "keep.txt", Entry: file("keep")}},
		{{Path: "lib/"}, {Path: "lib", Entry: file("now a file")}},
	} {
		view.SortEdits(edits)
		a, err := view.Apply(context.Background(), h.views[len(h.views)-1], edits, h.src)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range a.Records {
			h.put(t, r.Data)
		}
		h.views = append(h.views, a.ID)
		h.changesets = append(h.changesets, a.Changeset)
	}
	return h
}

// A chain file whose last commit turns a directory into a file reopens
// with that commit's view as its head, in the current form and in the old
// one. The head is recomputed from the last section, so a wrong answer
// there would seal and re-base the chain at the next ordinary push, and
// would refuse an old file whose last line is true.
func TestChainFileHeadAfterADirectoryBecomesAFile(t *testing.T) {
	ctx := context.Background()
	h := kindChange(t)
	head := h.views[len(h.views)-1]
	p := filepath.Join(t.TempDir(), "main.c4m")
	ch, _ := OpenChainFile(ctx, p, h.src)
	for i, cs := range h.changesets {
		if err := ch.Append(cs, h.views[i], h.views[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	again, err := OpenChainFile(ctx, p, h.src)
	if err != nil || again.Head != head || again.Sections != 2 {
		t.Fatalf("reopened: head %s, want %s; %d sections (%v)", again.Head, head, again.Sections, err)
	}
	if err := os.WriteFile(p, h.oldFile(t, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := OpenChainFile(ctx, p, h.src)
	if err != nil || old.Head != head {
		t.Fatalf("the same chain in the old form: head %v, want %s (%v)", old, head, err)
	}
}

// A chain file that exists and is empty is an empty chain, as a missing
// one is: the first changeset gives it its base.
func TestAnEmptyChainFileIsAnEmptyChain(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 1)
	p := filepath.Join(t.TempDir(), "main.c4m")
	for _, content := range []string{"", "\n \n"} {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		ch, err := OpenChainFile(ctx, p, h.src)
		if err != nil || !ch.Empty() || ch.Sections != 0 {
			t.Fatalf("an empty chain file (%q): %+v, %v", content, ch, err)
		}
		if err := ch.Append(h.changesets[0], h.views[0], h.views[1]); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(p); !bytes.Equal(data, h.changesets[0]) {
			t.Fatalf("after the first append to an empty file: %q", data)
		}
		if _, err := os.Stat(p + ".tmp"); err == nil {
			t.Fatal("the first append left its temporary file")
		}
	}
}
