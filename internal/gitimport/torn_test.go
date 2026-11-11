package gitimport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
)

// recorded is the mapping a service writes for the first n commits of h on
// one ref, one chain from the empty view, with each changeset in the store
// as it is before its line is written.
func (h *history) recorded(t *testing.T, ref string, n int) []Commit {
	t.Helper()
	var m []Commit
	for i := range n {
		h.put(t, h.changesets[i])
		m = append(m, Commit{
			Commit: fmt.Sprintf("%040d", i+1), Ref: ref,
			View: h.views[i+1], ParentView: h.views[i],
			Changeset: c4.Identify(bytes.NewReader(h.changesets[i])),
			ChainBase: h.views[0], ChainSeq: i + 1,
		})
	}
	return m
}

// chainFile writes the chain file a service has after n commits.
func (h *history) chainFile(t *testing.T, p string, n int) []byte {
	t.Helper()
	os.Remove(p)
	ch, err := OpenChainFile(context.Background(), p, h.src)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := ch.Append(h.changesets[i], h.views[i], h.views[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(p)
	if err != nil && n > 0 {
		t.Fatal(err)
	}
	return data
}

// A chain file is appended to in place, so a crash can cut its last
// changeset anywhere, and a c4m entry cut in its name still parses. The
// file is therefore never taken at its own word: it is opened as the
// mapping records it, and made again from the stored changesets when it
// does not resolve to the recorded head. Cut at every byte, and with the
// lost tail zero-filled, it always comes back as the recorded chain:
// nothing refused, nothing silently wrong.
func TestAChainFileCutAnywhereIsMadeAgain(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 3)
	m := h.recorded(t, "main", 3)
	p := filepath.Join(t.TempDir(), "main.c4m")
	whole := h.chainFile(t, p, 3)
	counts := map[string]int{}
	check := func(what string, damaged []byte) {
		t.Helper()
		if err := os.WriteFile(p, damaged, 0o644); err != nil {
			t.Fatal(err)
		}
		ch, repaired, err := OpenRecordedChain(ctx, p, h.src, m, "main")
		now, _ := os.ReadFile(p)
		switch {
		case err != nil:
			counts["refused"]++
			t.Errorf("%s: refused: %v", what, err)
		case ch.Head != h.views[3] || ch.Base != h.views[0] || ch.Sections != 3 || !bytes.Equal(now, whole):
			counts["silently wrong"]++
			t.Errorf("%s: opens as %d sections to %s, the file %d bytes; want the recorded chain", what, ch.Sections, ch.Head, len(now))
		case repaired != "":
			counts["made again"]++
		default:
			counts["as it was"]++
		}
	}
	for cut := 0; cut <= len(whole); cut++ {
		check(fmt.Sprintf("cut at %d of %d", cut, len(whole)), whole[:cut])
	}
	last := len(whole) - len(h.changesets[2])
	for cut := last; cut < len(whole); cut++ {
		zeroed := append(append([]byte(nil), whole[:cut]...), make([]byte, len(whole)-cut)...)
		check(fmt.Sprintf("zero-filled from %d", cut), zeroed)
	}
	t.Logf("%d damaged files: %v", len(whole)+1+len(h.changesets[2]), counts)
	if counts["refused"] != 0 || counts["silently wrong"] != 0 || counts["as it was"] != 1 {
		t.Fatalf("want every damaged file made again and the whole one left as it was: %v", counts)
	}
}

// The mapping is appended to as well. A line is one write and ends with a
// newline, and a JSON object cut short does not parse, so a cut mapping is
// never taken for a whole one: the cut text is left out by a reader and
// taken off by the next writer, and the commit it was for is imported
// again. Cut at every byte of its last line, the mapping reads, the chain
// opens as that mapping records it, and the import goes on.
func TestAMappingCutAnywhereIsReadAndMended(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 3)
	lines := h.recorded(t, "main", 3)
	dir := t.TempDir()
	mp, cp := filepath.Join(dir, "mapping.jsonl"), filepath.Join(dir, "main.c4m")
	w, err := AppendMapping(mp)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range lines {
		if err := w.Write(c); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	whole, _ := os.ReadFile(mp)
	lastLine := bytes.LastIndexByte(whole[:len(whole)-1], '\n') + 1
	counts := map[int]int{}
	for cut := lastLine; cut <= len(whole); cut++ {
		if err := os.WriteFile(mp, whole[:cut], 0o644); err != nil {
			t.Fatal(err)
		}
		h.chainFile(t, cp, 3) // the chain file got its third changeset before the crash
		// A reader, with no lock.
		seen, err := ReadMapping(mp)
		if err != nil || len(seen) < 2 || len(seen) > 3 {
			t.Fatalf("cut at %d: a reader gets %d lines, %v", cut, len(seen), err)
		}
		// The next import: the writer first, then the mapping, then the chain.
		w, err := AppendMapping(mp)
		if err != nil {
			t.Fatalf("cut at %d: %v", cut, err)
		}
		m, err := ReadMapping(mp)
		if err != nil || len(m) != len(seen) {
			t.Fatalf("cut at %d: the writer reads %d lines, the reader read %d (%v)", cut, len(m), len(seen), err)
		}
		counts[len(m)]++
		ch, _, err := OpenRecordedChain(ctx, cp, h.src, m, "main")
		if err != nil || ch.Head != h.views[len(m)] || ch.Sections != len(m) {
			t.Fatalf("cut at %d: the chain opens as %+v (%v), want %d sections to the last recorded view", cut, ch, err, len(m))
		}
		if len(m) == 2 { // the third commit, again
			if err := ch.Append(h.changesets[2], h.views[2], h.views[3]); err != nil {
				t.Fatalf("cut at %d: %v", cut, err)
			}
			if err := w.Write(lines[2]); err != nil {
				t.Fatal(err)
			}
		}
		w.Close()
		if after, err := ReadMapping(mp); err != nil || len(after) != 3 || after[2].View != h.views[3] {
			t.Fatalf("cut at %d: after the import went on the mapping has %d lines (%v)", cut, len(after), err)
		}
		if data, _ := os.ReadFile(mp); !bytes.Equal(data, whole) {
			t.Fatalf("cut at %d: the mended mapping is not the whole one:\n%s", cut, data)
		}
	}
	t.Logf("%d cuts of the last line: %d read as two commits, %d as three", len(whole)-lastLine+1, counts[2], counts[3])
	if counts[3] != 2 { // whole, and whole but for its newline
		t.Fatalf("a cut line was taken for a whole one: %v", counts)
	}

	// Damage that is not a cut append is an error: a line that ends and
	// does not parse.
	bad := append(append([]byte(nil), whole[:lastLine]...), []byte("{\"commit\": \n")...)
	os.WriteFile(mp, bad, 0o644)
	if _, err := ReadMapping(mp); err == nil {
		t.Fatal("a whole line that does not parse was read")
	}
	if _, err := AppendMapping(mp); err == nil {
		t.Fatal("a mapping with a damaged line in it was opened for appending")
	}
}

// What the mapping records, and the chain file states it allows.
func TestAChainFileIsOpenedAsTheMappingRecordsIt(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 4)
	all := h.recorded(t, "main", 4)
	p := filepath.Join(t.TempDir(), "main.c4m")
	open := func(m []Commit) (*ChainFile, string) {
		t.Helper()
		ch, repaired, err := OpenRecordedChain(ctx, p, h.src, m, "main")
		if err != nil {
			t.Fatal(err)
		}
		return ch, repaired
	}
	// Ahead of the mapping: the changeset was appended and its line never
	// written. The commit will be imported again, onto the recorded chain.
	h.chainFile(t, p, 3)
	if ch, repaired := open(all[:2]); repaired == "" || ch.Sections != 2 || ch.Head != h.views[2] {
		t.Fatalf("a file ahead of the mapping: %+v, %q", ch, repaired)
	}
	// Behind it: the line was written and the append lost.
	h.chainFile(t, p, 2)
	if ch, repaired := open(all[:3]); repaired == "" || ch.Sections != 3 || ch.Head != h.views[3] {
		t.Fatalf("a file behind the mapping: %+v, %q", ch, repaired)
	}
	// Gone altogether.
	os.Remove(p)
	if ch, repaired := open(all[:3]); repaired == "" || ch.Sections != 3 {
		t.Fatalf("no file: %+v, %q", ch, repaired)
	}
	// The recorded chain sealed and started again from its head: allowed.
	ch, _ := OpenChainFile(ctx, p, h.src)
	if err := ch.Rebase(h.views[3]); err != nil {
		t.Fatal(err)
	}
	if ch, repaired := open(all[:3]); repaired != "" || ch.Sections != 0 || ch.Base != h.views[3] {
		t.Fatalf("a chain started again from its head was made again: %+v, %q", ch, repaired)
	}
	// Another ref's lines say nothing of this one, and a file with no
	// chain recorded for its ref is removed.
	other := append([]Commit(nil), all[:2]...)
	for i := range other {
		other[i].Ref = "release"
	}
	h.chainFile(t, p, 2)
	if ch, repaired := open(other); repaired == "" || !ch.Empty() {
		t.Fatalf("a file with no chain recorded: %+v, %q", ch, repaired)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the unrecorded chain file is still there")
	}
	if ch, repaired := open(nil); repaired != "" || !ch.Empty() {
		t.Fatalf("no file and nothing recorded: %+v, %q", ch, repaired)
	}

	// A new chain: a force push back onto the base restarts the count, and
	// a re-base names another base. The record is of the last one.
	forced := append(append([]Commit(nil), all[:3]...), Commit{Commit: "f1", Ref: "main", View: h.views[1], ParentView: h.views[0],
		Changeset: all[0].Changeset, ChainBase: h.views[0], ChainSeq: 1})
	if rc, ok := ChainRecorded(forced, "main"); !ok || len(rc.Changesets) != 1 || rc.Head != h.views[1] {
		t.Fatalf("after a force push back onto the base: %+v", rc)
	}
	rebased := append(append([]Commit(nil), all[:2]...), Commit{Commit: "r1", Ref: "main", View: h.views[3], ParentView: h.views[2],
		Changeset: all[2].Changeset, ChainBase: h.views[2], ChainSeq: 1})
	if rc, ok := ChainRecorded(rebased, "main"); !ok || rc.Base != h.views[2] || len(rc.Changesets) != 1 || rc.Head != h.views[3] {
		t.Fatalf("after a re-base: %+v", rc)
	}
	// Another base alone makes a new chain too: the one before held no
	// changeset, so the count does not restart.
	moved := []Commit{
		{Commit: "m0", Ref: "main", View: h.views[1], ParentView: h.views[1], ChainBase: h.views[1], ChainSeq: 0},
		{Commit: "m1", Ref: "main", View: h.views[3], ParentView: h.views[2], Changeset: all[2].Changeset, ChainBase: h.views[2], ChainSeq: 1},
	}
	if rc, ok := ChainRecorded(moved, "main"); !ok || rc.Base != h.views[2] || len(rc.Changesets) != 1 {
		t.Fatalf("after a chain with no changeset, another base: %+v", rc)
	}
	// A commit that changes nothing, a ref-only line and a seal line add no
	// section.
	quiet := append(append([]Commit(nil), all[:2]...),
		Commit{Commit: "q1", Ref: "main", View: h.views[2], ParentView: h.views[2], ChainBase: h.views[0], ChainSeq: 2},
		Commit{Commit: "q0", Ref: "main", RefOnly: true, View: h.views[1]},
		Commit{ChainRef: "main", View: h.views[2], ChainBase: h.views[0], ChainSections: 2})
	if rc, ok := ChainRecorded(quiet, "main"); !ok || len(rc.Changesets) != 2 || rc.Head != h.views[2] {
		t.Fatalf("with lines that add no section: %+v", rc)
	}
}

// When the chain cannot be made again, that is an error that says why, and
// the file is left: a changeset the store does not hold, a wrong object
// under a changeset's ID, changesets that do not resolve to the recorded
// head. A reader is served the recorded chain without the file being
// written.
func TestAChainThatCannotBeMadeAgain(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 3)
	m := h.recorded(t, "main", 3)
	p := filepath.Join(t.TempDir(), "main.c4m")
	whole := h.chainFile(t, p, 3)
	torn := whole[:len(whole)-40]

	os.WriteFile(p, torn, 0o644)
	served, err := RecordedChainBytes(ctx, p, h.src, m, "main")
	if now, _ := os.ReadFile(p); err != nil || !bytes.Equal(served, whole) || !bytes.Equal(now, torn) {
		t.Fatalf("a reader of a torn file: %v, served the recorded chain %v, file left alone %v", err, bytes.Equal(served, whole), bytes.Equal(now, torn))
	}
	if served, err := RecordedChainBytes(ctx, p, h.src, nil, "main"); err != nil || served != nil {
		t.Fatalf("a reader with nothing recorded: %q, %v", served, err)
	}

	missing := append([]Commit(nil), m...)
	missing[1].Changeset = c4.Identify(strings.NewReader("a changeset the store does not hold"))
	wrongHead := append([]Commit(nil), m...)
	wrongHead[2].View = h.views[1]
	wrongBase := append([]Commit(nil), m...)
	wrongBase[0].ParentView = h.views[1]
	for name, mm := range map[string][]Commit{"a changeset the store does not hold": missing, "changesets that do not reach the recorded head": wrongHead, "a first changeset that is not over the recorded base": wrongBase} {
		os.WriteFile(p, torn, 0o644)
		if ch, _, err := OpenRecordedChain(ctx, p, h.src, mm, "main"); err == nil || !strings.Contains(err.Error(), "cannot be made again") {
			t.Errorf("%s: opened as %+v (%v)", name, ch, err)
		}
		if now, _ := os.ReadFile(p); !bytes.Equal(now, torn) {
			t.Errorf("%s: the file was written", name)
		}
	}
	// A whole file that resolves to the recorded view and is not the
	// recorded chain is not taken for it: one section short of the record,
	// and from another base than the record's.
	longer := append(append([]Commit(nil), m...), Commit{Commit: "x", Ref: "main", View: h.views[3], ParentView: h.views[3],
		Changeset: m[0].Changeset, ChainBase: h.views[0], ChainSeq: 4})
	for name, mm := range map[string][]Commit{"a file a section short of the record": longer, "a file from another base than the record's": wrongBase} {
		os.WriteFile(p, whole, 0o644)
		if ch, _, err := OpenRecordedChain(ctx, p, h.src, mm, "main"); err == nil || !strings.Contains(err.Error(), "cannot be made again") {
			t.Errorf("%s: opened as %+v (%v)", name, ch, err)
		}
	}
	// A wrong object under a changeset's ID.
	id := m[2].Changeset
	h.ram.Remove(id)
	w, err := h.ram.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("not the changeset"))
	w.Close()
	os.WriteFile(p, torn, 0o644)
	// The error says which, where the mapping records it, and that the
	// object has to go before the changeset can be made again: a store
	// keeps the object it has under a C4 ID. It is marked, so an import
	// does not try to make again what the store would not take.
	_, _, err = OpenRecordedChain(ctx, p, h.src, m, "main")
	for _, want := range []string{"wrong object", id.String(), "mapping line 3", "has to be removed from the store"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("a wrong object under a changeset's ID: %v\nwant %q in it", err, want)
		}
	}
	var lost *CannotRemake
	if !errors.As(err, &lost) || !lost.Wrong || lost.Changeset != id || lost.Line != 3 || lost.Commit != m[2].Commit {
		t.Fatalf("a wrong object under a changeset's ID: %+v", lost)
	}
	if _, err := RecordedChainBytes(ctx, p, h.src, m, "main"); err == nil || !strings.Contains(err.Error(), "wrong object") {
		t.Fatalf("a reader, with a wrong object under a changeset's ID: %v", err)
	}
}

// Changesets stored before 2026-11-09 end with the view they produce. A
// chain made again from them is in the form the format allows: the same
// chain, with no bare C4 ID at its end.
func TestAChainMadeAgainFromOldChangesets(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 3)
	var m []Commit
	for i := range 3 {
		old := append(append([]byte(nil), h.changesets[i]...), []byte(h.views[i+1].String()+"\n")...)
		h.put(t, old)
		m = append(m, Commit{Commit: fmt.Sprintf("%040d", i+1), Ref: "main", View: h.views[i+1], ParentView: h.views[i],
			Changeset: c4.Identify(bytes.NewReader(old)), ChainBase: h.views[0], ChainSeq: i + 1})
	}
	p := filepath.Join(t.TempDir(), "main.c4m")
	ch, repaired, err := OpenRecordedChain(ctx, p, h.src, m, "main")
	if err != nil || repaired == "" || ch.Head != h.views[3] || ch.Sections != 3 {
		t.Fatalf("%+v, %q, %v", ch, repaired, err)
	}
	want := bytes.Join(h.changesets, nil)
	if data, _ := os.ReadFile(p); !bytes.Equal(data, want) || isBareID(lastLineOf(data)) {
		t.Fatalf("the chain made again is not the changesets in the form the format allows:\n%s", data)
	}
	// And it is the chain those lines record: each section is the text
	// its line's C4 ID names once the view's C4 ID is put after it, so the
	// file is taken at the next open and not made again each time.
	if ch, repaired, err := OpenRecordedChain(ctx, p, h.src, m, "main"); err != nil || repaired != "" || ch.Sections != 3 {
		t.Fatalf("the chain made again from old changesets, opened again: %+v, %q, %v", ch, repaired, err)
	}
}

// Torn in the order the service writes: the changeset is appended to the
// chain file first and its mapping line written after. A crash between the
// two leaves the recorded chain with all or part of one more changeset
// after it, and no line for it. Cut at every byte of that changeset, the
// file comes back as the recorded chain, exactly: a fragment that happens
// to parse and change nothing (a changeset's first line alone, or a
// directory restated by its C4 ID with nothing under it yet) is not left
// on the end to be served and sealed.
func TestAnAppendCutBeforeItsLineIsWritten(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 4)
	m := h.recorded(t, "main", 3)
	p := filepath.Join(t.TempDir(), "main.c4m")
	recorded := h.chainFile(t, p, 3)
	next := h.changesets[3]
	counts := map[string]int{}
	for cut := 0; cut <= len(next); cut++ {
		torn := append(append([]byte(nil), recorded...), next[:cut]...)
		if err := os.WriteFile(p, torn, 0o644); err != nil {
			t.Fatal(err)
		}
		ch, repaired, err := OpenRecordedChain(ctx, p, h.src, m, "main")
		now, _ := os.ReadFile(p)
		switch {
		case err != nil:
			counts["refused"]++
			t.Errorf("cut at %d: refused: %v", cut, err)
		case ch.Head != h.views[3] || ch.Sections != 3 || !bytes.Equal(now, recorded):
			counts["silently wrong"]++
			t.Errorf("cut at %d (after %q): opens as %d sections, the file %d bytes of %d", cut, lastLineOf(torn), ch.Sections, len(now), len(recorded))
		case repaired != "":
			counts["made again"]++
		default:
			counts["as it was"]++
		}
		// A reader is served the recorded chain whatever the file holds.
		os.WriteFile(p, torn, 0o644)
		if served, err := RecordedChainBytes(ctx, p, h.src, m, "main"); err != nil || !bytes.Equal(served, recorded) {
			t.Errorf("cut at %d: a reader is served %d bytes (%v), want the recorded chain", cut, len(served), err)
		}
	}
	t.Logf("%d files: %v", len(next)+1, counts)
	if counts["refused"] != 0 || counts["silently wrong"] != 0 || counts["as it was"] != 1 {
		t.Fatalf("want every torn file made again and the recorded one left: %v", counts)
	}
}

// What the record is made of, line by line.
func TestWhatTheMappingRecordsOfAChain(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 4)
	all := h.recorded(t, "main", 4)
	gp := filepath.Join(t.TempDir(), "main.c4m")
	sections := func() (c4.ID, int) {
		t.Helper()
		data, err := os.ReadFile(gp)
		if err != nil {
			t.Fatal(err)
		}
		ch, _, err := openChain(ctx, gp, data, h.src)
		if err != nil {
			t.Fatal(err)
		}
		return ch.Base, ch.Sections
	}
	// A changeset goes on the chain when it applies to the view the chain
	// is at, whatever its count. A file with a torn section left on it was
	// once counted as it stood, so a mapping can count 1, 2, 3, 5 with no
	// line missing: that is a chain of four, and the file of four is it.
	skipped := append([]Commit(nil), all...)
	skipped[3].ChainSeq = 5
	whole := h.chainFile(t, gp, 4)
	if rc, ok := ChainRecorded(skipped, "main"); !ok || len(rc.Changesets) != 4 || rc.Base != h.views[0] || rc.Head != h.views[4] || rc.MissingBefore != 0 {
		t.Fatalf("counts that skip a number with no line missing: %+v", rc)
	}
	if ch, repaired, err := OpenRecordedChain(ctx, gp, h.src, skipped, "main"); err != nil || repaired != "" || ch.Sections != 4 {
		t.Fatalf("the file of a chain whose counts skip a number: %+v, %q, %v", ch, repaired, err)
	}
	// Lines gone from the mapping: a changeset that does not apply to the
	// view before it starts the record again from the view it does apply
	// to. No chain is read into the missing lines, nothing stops, and the
	// file is made again as the chain the mapping still knows.
	for name, c := range map[string]struct {
		lines []Commit
		base  c4.ID
		n     int
		line  int
	}{
		"the third of four lines gone": {[]Commit{all[0], all[1], all[3]}, h.views[3], 1, 3},
		"the first line gone":          {[]Commit{all[1], all[2], all[3]}, h.views[1], 3, 1},
		// A1 B2 C3 D4, a force push back onto the base, E1 F2 G3, and
		// E's line gone: F does not go on after A by its count.
		"the first line of a second chain gone": {[]Commit{all[0], all[1], all[2], all[3], all[1], all[2]}, h.views[1], 2, 5},
	} {
		if err := os.WriteFile(gp, whole, 0o644); err != nil {
			t.Fatal(err)
		}
		rc, ok := ChainRecorded(c.lines, "main")
		if !ok || rc.Base != c.base || len(rc.Changesets) != c.n || rc.Lines[0] != c.line || rc.Head != c.lines[len(c.lines)-1].View {
			t.Fatalf("%s: the record is %+v", name, rc)
		}
		// The record says where it starts and that it starts there for
		// want of lines, and so does the reason the file is made again.
		if rc.MissingBefore != c.line {
			t.Fatalf("%s: the record says lines are missing before line %d, want %d", name, rc.MissingBefore, c.line)
		}
		ch, repaired, err := OpenRecordedChain(ctx, gp, h.src, c.lines, "main")
		if err != nil || repaired == "" || ch.Base != c.base || ch.Sections != c.n || ch.Head != rc.Head {
			t.Fatalf("%s: opened as %+v, %q, %v", name, ch, repaired, err)
		}
		if want := fmt.Sprintf("lines of the chain are missing from the mapping before line %d", c.line); !strings.Contains(repaired, want) {
			t.Fatalf("%s: made again for the reason %q, want %q in it", name, repaired, want)
		}
		if base, n := sections(); base != c.base || n != c.n {
			t.Fatalf("%s: the file made again has %d sections from %s", name, n, base)
		}
		served, err := RecordedChainBytes(ctx, gp, h.src, c.lines, "main")
		if now, _ := os.ReadFile(gp); err != nil || !bytes.Equal(served, now) {
			t.Fatalf("%s: a reader is served something else than the file made again (%v)", name, err)
		}
	}
	// A gap the views read across: the commits after it come back to the
	// view before it (a revert). The changeset applies, so the chain goes
	// on, and the file is the recorded chain; but the commit's git parent
	// has no line, which the importer never leaves, so lines are missing.
	// A commit whose parent has a line, of whatever ref and whatever kind,
	// shows nothing.
	across := []Commit{
		{Commit: "c1", Ref: "main", View: h.views[1], ParentView: h.views[0], Changeset: all[0].Changeset, ChainBase: h.views[0], ChainSeq: 1},
		{Commit: "c4", Parent: "c3", Ref: "main", View: h.views[2], ParentView: h.views[1], Changeset: all[1].Changeset, ChainBase: h.views[0], ChainSeq: 4},
	}
	if rc, ok := ChainRecorded(across, "main"); !ok || rc.MissingBefore != 2 || len(rc.Changesets) != 2 || rc.Base != h.views[0] {
		t.Fatalf("a gap the views read across: %+v", rc)
	}
	for name, before := range map[string]Commit{
		"an empty commit's line":  {Commit: "c3", Parent: "c1", Ref: "main", View: h.views[1], ParentView: h.views[1], ChainBase: h.views[0], ChainSeq: 1},
		"another ref's line":      {Commit: "c3", Parent: "c1", Ref: "dev", View: h.views[1], ParentView: h.views[0], Changeset: all[0].Changeset, ChainBase: h.views[0], ChainSeq: 1},
		"a line that moved a ref": {Commit: "c3", Ref: "dev", RefOnly: true, View: h.views[1]},
	} {
		with := []Commit{across[0], before, across[1]}
		if rc, ok := ChainRecorded(with, "main"); !ok || rc.MissingBefore != 0 || len(rc.Changesets) != 2 {
			t.Fatalf("with %s for the parent: %+v", name, rc)
		}
	}
	// The commit after the gap may be one that changes nothing: its line
	// has no changeset, and its parent is the one with no line. The next
	// changeset's parent is that commit, which has a line.
	empty := Commit{Commit: "e", Parent: "c3", Ref: "main", View: h.views[1], ParentView: h.views[1], ChainBase: h.views[0], ChainSeq: 1}
	after := Commit{Commit: "c4", Parent: "e", Ref: "main", View: h.views[2], ParentView: h.views[1], Changeset: all[1].Changeset, ChainBase: h.views[0], ChainSeq: 4}
	if rc, ok := ChainRecorded([]Commit{across[0], empty, after}, "main"); !ok || rc.MissingBefore != 2 || len(rc.Changesets) != 2 {
		t.Fatalf("a gap then a commit that changes nothing: %+v", rc)
	}
	// A commit imported in full needs no parent line.
	base := Commit{Commit: "b", Parent: "outside-the-window", Base: true, Ref: "main", View: h.views[1], ParentView: h.views[0], Changeset: all[0].Changeset, ChainBase: h.views[0], ChainSeq: 1}
	if rc, ok := ChainRecorded([]Commit{base}, "main"); !ok || rc.MissingBefore != 0 {
		t.Fatalf("a commit imported in full: %+v", rc)
	}
	// A chain the importer starts again clears it.
	again := Commit{Commit: "c5", Parent: "c4", Ref: "main", View: h.views[3], ParentView: h.views[2], Changeset: all[2].Changeset, ChainBase: h.views[2], ChainSeq: 1}
	if rc, _ := ChainRecorded(append(append([]Commit(nil), across...), again), "main"); rc.MissingBefore != 0 || len(rc.Changesets) != 1 {
		t.Fatalf("a chain started again after a gap: %+v", rc)
	}
	// A chain the importer started (a line counted first) is not one that
	// starts for want of lines, whatever came before it; and each ref's
	// chain is read from its own lines, all refs in one pass.
	restarted := all[3]
	restarted.ChainSeq = 1
	mixed := []Commit{all[0], all[2], restarted}
	other := all[1]
	other.Ref = "release"
	mixed = append(mixed, other)
	if rc, ok := ChainRecorded(mixed, "main"); !ok || rc.MissingBefore != 0 || rc.Base != h.views[3] || len(rc.Changesets) != 1 {
		t.Fatalf("a chain started after lines went missing: %+v", rc)
	}
	every := ChainsRecorded(mixed)
	if rc, ok := ChainRecorded(mixed, "release"); !ok || rc.MissingBefore != 4 || len(every) != 2 || every["release"].MissingBefore != 4 || every["main"].Base != h.views[3] || every["main"].MissingBefore != 0 {
		t.Fatalf("every ref's chain in one pass: %+v", every)
	}
	// A line with a changeset and no chain fields (an import with no chain
	// kept) says nothing of a chain.
	bare := all[0]
	bare.ChainBase, bare.ChainSeq = c4.ID{}, 0
	if _, ok := ChainRecorded([]Commit{bare}, "main"); ok {
		t.Fatal("a line with no chain fields was taken for a chain's")
	}
	// A commit that changes nothing does not move the chain, wherever its
	// view is: the ref can be at a commit its own chain never reached.
	elsewhere := Commit{Commit: "empty", Ref: "main", View: h.views[4], ParentView: h.views[4], ChainBase: h.views[0], ChainSeq: 2}
	if rc, ok := ChainRecorded(append(append([]Commit(nil), all[:2]...), elsewhere), "main"); !ok || rc.Head != h.views[2] || len(rc.Changesets) != 2 {
		t.Fatalf("after a commit that changes nothing: %+v", rc)
	}

	// A stored object that is more than one changeset is not a changeset.
	two := append(append([]byte(nil), h.changesets[0]...), h.changesets[1]...)
	h.put(t, two)
	bad := []Commit{all[0]}
	bad[0].Changeset = c4.Identify(bytes.NewReader(two))
	p := filepath.Join(t.TempDir(), "main.c4m")
	if _, _, err := OpenRecordedChain(ctx, p, h.src, bad, "main"); err == nil || !strings.Contains(err.Error(), "is not one changeset") {
		t.Fatalf("a stored object of two changesets: %v", err)
	}
	// A changeset stored before 2026-11-09 that ends with a view it does
	// not produce is refused, not carried on.
	wrongEnd := append(append([]byte(nil), h.changesets[0]...), []byte(h.views[2].String()+"\n")...)
	h.put(t, wrongEnd)
	bad[0].Changeset = c4.Identify(bytes.NewReader(wrongEnd))
	if _, _, err := OpenRecordedChain(ctx, p, h.src, bad, "main"); err == nil || !strings.Contains(err.Error(), "ends with") {
		t.Fatalf("an old changeset that ends with another view: %v", err)
	}
	// And not only the first of a chain.
	wrongEnd = append(append([]byte(nil), h.changesets[1]...), []byte(h.views[3].String()+"\n")...)
	h.put(t, wrongEnd)
	second := []Commit{all[0], all[1]}
	second[1].Changeset = c4.Identify(bytes.NewReader(wrongEnd))
	if _, _, err := OpenRecordedChain(ctx, p, h.src, second, "main"); err == nil || !strings.Contains(err.Error(), "ends with") || !strings.Contains(err.Error(), "mapping line 2") {
		t.Fatalf("a second old changeset that ends with another view: %v", err)
	}

	// The file a chain is made again into is synced before it takes its
	// name, like any other replacement of it.
	synced := 0
	old := syncFile
	t.Cleanup(func() { syncFile = old })
	syncFile = func(f *os.File) error {
		if named, _ := os.ReadFile(p); len(named) != 0 {
			t.Error("the chain file had its new bytes before they were synced")
		}
		synced++
		return old(f)
	}
	os.Remove(p)
	if _, repaired, err := OpenRecordedChain(ctx, p, h.src, all[:3], "main"); err != nil || repaired == "" || synced != 1 {
		t.Fatalf("made again: %q, %v, %d syncs", repaired, err, synced)
	}
}

// Where a chain file ends. The recorded chain followed by a bare C4 ID is
// that chain with a changeset's first line after it and nothing more (an
// append cut after its first line), or a file written before 2026-11-09,
// which ended with its head: both resolve to the recorded head, and both
// are written again in the form the format allows before anything is
// appended. A chain with no sections is its base's ID alone, and is left.
func TestWhereAChainFileEnds(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 4)
	m := h.recorded(t, "main", 3)
	p := filepath.Join(t.TempDir(), "main.c4m")
	recorded := h.chainFile(t, p, 3)
	for name, tail := range map[string]string{
		"a bare C4 ID":                      h.views[3].String() + "\n",
		"a bare C4 ID with no newline":      h.views[3].String(),
		"its last line without its newline": "",
	} {
		file := append(append([]byte(nil), recorded...), tail...)
		if tail == "" {
			file = recorded[:len(recorded)-1]
		}
		os.WriteFile(p, file, 0o644)
		ch, repaired, err := OpenRecordedChain(ctx, p, h.src, m, "main")
		now, _ := os.ReadFile(p)
		if err != nil || repaired == "" || ch.Sections != 3 || ch.Head != h.views[3] || !bytes.Equal(now, recorded) {
			t.Errorf("a file that ends with %s: %+v, made again %q, %v; the file is the recorded chain: %v", name, ch, repaired, err, bytes.Equal(now, recorded))
		}
	}
	// A reader is served the chain as the c4m library writes it, not the
	// file's bytes as they are: a blank line after the chain is nothing a
	// write leaves and nothing an append runs on from, so the file is
	// taken, and what is served is the chain alone.
	os.WriteFile(p, append(append([]byte(nil), recorded...), 0x0a), 0o644)
	if served, err := RecordedChainBytes(ctx, p, h.src, m, "main"); err != nil || !bytes.Equal(served, recorded) {
		t.Fatalf("a reader of a file with a blank line after the chain is served %d bytes (%v), want the chain's %d", len(served), err, len(recorded))
	}
	if ch, _, err := OpenRecordedChain(ctx, p, h.src, m, "main"); err != nil || ch.Append(h.changesets[3], h.views[3], h.views[4]) != nil {
		t.Fatalf("an append after a blank line: %v", err)
	}
	if ch, repaired, err := OpenRecordedChain(ctx, p, h.src, h.recorded(t, "main", 4), "main"); err != nil || repaired != "" || ch.Sections != 4 || ch.Head != h.views[4] {
		t.Fatalf("the chain appended to after a blank line: %+v, %q, %v", ch, repaired, err)
	}
	os.WriteFile(p, recorded, 0o644)
	ch, _, err := OpenRecordedChain(ctx, p, h.src, m, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Rebase(h.views[3]); err != nil {
		t.Fatal(err)
	}
	if ch, repaired, err := OpenRecordedChain(ctx, p, h.src, m, "main"); err != nil || repaired != "" || ch.Sections != 0 {
		t.Fatalf("a chain of its base alone: %+v, %q, %v", ch, repaired, err)
	}
	// A chain of its base alone at any other view is not the recorded
	// chain started again: it is made again.
	if err := ch.Rebase(h.views[1]); err != nil {
		t.Fatal(err)
	}
	if ch, repaired, err := OpenRecordedChain(ctx, p, h.src, m, "main"); err != nil || repaired == "" || ch.Sections != 3 || ch.Head != h.views[3] {
		t.Fatalf("a chain of another view alone: %+v, %q, %v", ch, repaired, err)
	}
	if err := ch.Rebase(h.views[3]); err != nil {
		t.Fatal(err)
	}
	// And the next changeset goes on from it.
	if err := ch.Append(h.changesets[3], h.views[3], h.views[4]); err != nil {
		t.Fatal(err)
	}
}

// Every section of the file is compared with the changeset the mapping
// records for it, not only the last one, which is the one applied. A file
// changed before its last section can still parse, keep its count and
// resolve to the recorded view: with a line taken out of an earlier
// section, and with an earlier section that is another changeset. Each is
// made again.
func TestEverySectionIsTheChangesetRecorded(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 4)
	m := h.recorded(t, "main", 4)
	p := filepath.Join(t.TempDir(), "main.c4m")
	recorded := h.chainFile(t, p, 4)
	check := func(what string, file []byte) (taken bool) {
		t.Helper()
		if ch, _, err := openChain(ctx, p, file, h.src); err != nil || ch.Head != h.views[4] || ch.Base != h.views[0] || ch.Sections != 4 {
			return false // not a file that passes for the chain
		}
		os.WriteFile(p, file, 0o644)
		ch, repaired, err := OpenRecordedChain(ctx, p, h.src, m, "main")
		now, _ := os.ReadFile(p)
		if err != nil || repaired == "" || ch.Sections != 4 || !bytes.Equal(now, recorded) {
			t.Errorf("%s: taken as the recorded chain (%+v, %q, %v)", what, ch, repaired, err)
		}
		if !strings.Contains(repaired, "is not the changeset") || !strings.Contains(repaired, "the mapping records for it at line") {
			t.Errorf("%s: made again for the reason %q", what, repaired)
		}
		os.WriteFile(p, file, 0o644)
		if served, err := RecordedChainBytes(ctx, p, h.src, m, "main"); err != nil || !bytes.Equal(served, recorded) {
			t.Errorf("%s: a reader is served %d bytes (%v), want the recorded chain", what, len(served), err)
		}
		return true
	}
	// Each line before the last section taken out, where what is left
	// still passes for the chain.
	last := len(recorded) - len(h.changesets[3])
	lines := bytes.SplitAfter(recorded[:last], []byte("\n"))
	passed := 0
	for i := range lines {
		if len(lines[i]) == 0 {
			continue
		}
		file := append(bytes.Join(append(append([][]byte(nil), lines[:i]...), lines[i+1:]...), nil), recorded[last:]...)
		if check(fmt.Sprintf("line %d of %d taken out", i+1, len(lines)), file) {
			passed++
		}
	}
	if passed == 0 {
		t.Fatal("no file with a line taken out passed for the chain; the test tells nothing")
	}
	t.Logf("%d of %d files with a line taken out before the last section pass for the chain by its head, base and count; each is made again", passed, len(lines))
	// An earlier section that is another changeset.
	swapped := bytes.Join([][]byte{h.changesets[0], h.changesets[0], h.changesets[2], h.changesets[3]}, nil)
	if !check("the second section another changeset", swapped) {
		t.Fatal("the file with another changeset in it does not pass for the chain; the test tells nothing")
	}
}
