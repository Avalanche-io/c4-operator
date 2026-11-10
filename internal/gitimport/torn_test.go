package gitimport

import (
	"bytes"
	"context"
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
	for i := range wrongBase {
		wrongBase[i].ChainBase = h.views[1]
	}
	for name, mm := range map[string][]Commit{"a changeset the store does not hold": missing, "changesets that do not reach the recorded head": wrongHead, "a first changeset that is not over the recorded base": wrongBase} {
		os.WriteFile(p, torn, 0o644)
		if ch, _, err := OpenRecordedChain(ctx, p, h.src, mm, "main"); err == nil || !strings.Contains(err.Error(), "cannot be made again") {
			t.Errorf("%s: opened as %+v (%v)", name, ch, err)
		}
		if now, _ := os.ReadFile(p); !bytes.Equal(now, torn) {
			t.Errorf("%s: the file was written", name)
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
	if _, _, err := OpenRecordedChain(ctx, p, h.src, m, "main"); err == nil || !strings.Contains(err.Error(), "wrong object") {
		t.Fatalf("a wrong object under a changeset's ID: %v", err)
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
}
