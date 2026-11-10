package view

import (
	"bytes"
	"context"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"
)

// buildChain makes steps random changes to a tree on disk, captures each
// as a changeset with Apply, joins the changesets end to end, and returns
// that chain, the views the filesystem actually had, and the store.
func buildChain(t *testing.T, seed uint64, steps int) ([]byte, []c4.ID, *store.RAM) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 3))
	dir := t.TempDir()
	writeTree(t, dir, show)
	base, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ram := store.NewRAM()
	publish(t, ram, base)
	var chain []byte
	views := []c4.ID{base.ID}
	cur := base.ID
	for step := range steps {
		edits := mutate(t, rng, dir, step)
		applied, err := Apply(context.Background(), cur, edits, StoreRecords(ram))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := ScanDir(dir)
		if applied.ID != want.ID {
			t.Fatalf("step %d: Apply %s, filesystem %s", step, applied.ID, want.ID)
		}
		if applied.ID == cur {
			continue
		}
		for _, r := range applied.Records {
			putBytes(t, ram, r.ID, r.Data)
		}
		// Appending a changeset to a chain is concatenation.
		chain = append(chain, applied.Changeset...)
		cur = applied.ID
		views = append(views, cur)
	}
	return chain, views, ram
}

// oldForm writes a chain the way it was written before 2026-11-09: the
// base, then each section's entries followed by the bare ID of the view
// they produce, the last one included.
func oldForm(t *testing.T, ch *Chain, views []c4.ID) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString(ch.Base.String() + "\n")
	for i, sec := range ch.Sections {
		if err := c4m.NewEncoder(&out).Encode(sec.Patch); err != nil {
			t.Fatal(err)
		}
		out.WriteString(views[i+1].String() + "\n")
	}
	return out.Bytes()
}

// endsWithBareID reports whether the last line of data is a C4 ID alone.
func endsWithBareID(data []byte) bool {
	_, line := lastLine(data)
	_, ok := bareID(line)
	return ok
}

func equalIDs(a, b []c4.ID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChainResolvesToTheViews(t *testing.T) {
	ctx := context.Background()
	for seed := range 10 {
		chain, views, ram := buildChain(t, uint64(seed), 12)
		if chain == nil {
			continue
		}
		ch, err := ReadChain(chain)
		if err != nil {
			t.Fatal(err)
		}
		if ch.Base != views[0] || len(ch.Sections) != len(views)-1 || !ch.Seal.IsNil() {
			t.Fatalf("seed %d: chain base, sections or seal do not match the history", seed)
		}
		// A c4m file never ends with a bare C4 ID, and every bare ID in the
		// chain is the view before the section that follows it.
		if endsWithBareID(chain) {
			t.Fatalf("seed %d: the chain ends with a bare C4 ID\n%s", seed, chain)
		}
		for i, sec := range ch.Sections {
			if sec.Base != views[i] {
				t.Fatalf("seed %d: section %d applies to %s, the filesystem had %s", seed, i+1, sec.Base, views[i])
			}
		}
		// The views are recomputed, the head with them.
		got, err := ch.Verify(ctx, StoreRecords(ram))
		if err != nil {
			t.Fatalf("seed %d: %v\n%s", seed, err, chain)
		}
		if !equalIDs(got, views[1:]) {
			t.Fatalf("seed %d: the chain resolves to %d views that are not the filesystem's %d", seed, len(got), len(views)-1)
		}
		// Read and written again by the library, the chain is the same bytes.
		again, err := ch.Encode()
		if err != nil || !bytes.Equal(again, chain) {
			t.Fatalf("seed %d: the chain does not encode to itself (%v)\n%s\n---\n%s", seed, err, chain, again)
		}
		// The c4 library reads the same chain as the same sections, so
		// c4 log and c4 split work on it.
		secs, err := c4m.DecodePatchChain(bytes.NewReader(chain))
		if err != nil || len(secs) != len(ch.Sections) {
			t.Fatalf("seed %d: c4m sees %d sections (%v), we see %d", seed, len(secs), err, len(ch.Sections))
		}
		// Stored, the chain resolves like any description: one view per
		// section, the head last.
		chainID := c4.Identify(bytes.NewReader(chain))
		putBytes(t, ram, chainID, chain)
		r, err := FromID(chainID, ram, Filter{})
		if err != nil {
			t.Fatalf("seed %d: resolve chain: %v", seed, err)
		}
		if r.View().ID != views[len(views)-1] || r.Levels != len(views) {
			t.Fatalf("seed %d: resolved head %s with %d levels, want %s and %d", seed, r.View().ID, r.Levels, views[len(views)-1], len(views))
		}
		for i, v := range r.Chain {
			off := r.Levels - len(r.Chain)
			if v.ID != views[off+i] {
				t.Fatalf("seed %d: level %d is %s, want %s", seed, off+i, v.ID, views[off+i])
			}
		}
	}
}

// A changeset is exactly what the c4m library's EncodePatch writes: the
// parent's ID, then the entries, and no line after them.
func TestChangesetIsWhatEncodePatchWrites(t *testing.T) {
	chain, views, ram := buildChain(t, 3, 1)
	if len(views) != 2 {
		t.Fatalf("want one change, got %d views", len(views))
	}
	ch, err := ReadChain(chain)
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := c4m.NewEncoder(&want).EncodePatch(&c4m.PatchResult{Patch: ch.Sections[0].Patch, OldID: views[0]}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(chain, want.Bytes()) {
		t.Fatalf("the changeset is not EncodePatch's output\n%s\n---\n%s", chain, want.Bytes())
	}
	if !bytes.HasPrefix(chain, []byte(views[0].String()+"\n")) || endsWithBareID(chain) {
		t.Fatalf("a changeset starts with its parent and ends on an entry\n%s", chain)
	}
	// The view it produces is recomputed from it.
	got, err := ch.Verify(context.Background(), StoreRecords(ram))
	if err != nil || len(got) != 1 || got[0] != views[1] {
		t.Fatalf("the changeset resolves to %v (%v), want %s", got, err, views[1])
	}
}

// chainBefore is a chain this package wrote before 2026-11-09 (commit
// dca7c352, C4_CHAIN_OUT with TestWriteChainForCLI: seed 5, six steps),
// kept byte for byte. It ends with the ID of the view it resolves to.
const (
	chainBefore   = "testdata/chain-before-2026-11-09.c4m"
	chainBeforeID = "c44fvShw16VyuKi8SBYuUPPz93CvcGr9A35jNTzEXK4RkGmRFckCta4S1whizG4G4Rm49vD2g91NrUmY2vdwGaQbX6"
)

// Chains and changesets stored before the rule are objects named by ID in
// signed records, so they are still read: the line they end with is
// checked like every other bare ID, and never written again.
func TestAChainInTheOldFormIsStillRead(t *testing.T) {
	ctx := context.Background()
	old, err := os.ReadFile(chainBefore)
	if err != nil {
		t.Fatal(err)
	}
	if got := c4.Identify(bytes.NewReader(old)).String(); got != chainBeforeID {
		t.Fatalf("%s is not the file that was written: its ID is %s", chainBefore, got)
	}
	// The same history, written now.
	chain, views, ram := buildChain(t, 5, 6)
	head := views[len(views)-1]
	if !endsWithBareID(old) || endsWithBareID(chain) {
		t.Fatal("the old form ends with a bare ID and the current one does not")
	}

	ch, err := ReadChain(old)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Seal != head || ch.Base != views[0] || len(ch.Sections) != len(views)-1 {
		t.Fatalf("old chain: seal %s, base %s, %d sections; want %s, %s, %d", ch.Seal, ch.Base, len(ch.Sections), head, views[0], len(views)-1)
	}
	got, err := ch.Verify(ctx, StoreRecords(ram))
	if err != nil || !equalIDs(got, views[1:]) {
		t.Fatalf("old chain verifies to %d views: %v", len(got), err)
	}
	// Read and written again, it is the chain in the current form, byte for
	// byte: the only difference between the forms is the last line.
	again, err := ch.Encode()
	if err != nil || !bytes.Equal(again, chain) {
		t.Fatalf("the old chain does not encode to the current form (%v)\n%s\n---\n%s", err, again, chain)
	}
	// The helper the other tests use writes that same old form.
	now, err := ReadChain(chain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldForm(t, now, views), old) {
		t.Fatal("oldForm does not write what was written before 2026-11-09")
	}

	// Stored, it resolves as it always did.
	putBytes(t, ram, c4.Identify(bytes.NewReader(old)), old)
	r, err := FromID(c4.Identify(bytes.NewReader(old)), ram, Filter{})
	if err != nil || r.View().ID != head || r.Levels != len(views) {
		t.Fatalf("old chain resolves: %v", err)
	}

	// A last line that names another view is refused wherever the head is
	// recomputed.
	forged := c4.Identify(strings.NewReader("forged"))
	bad := append(append([]byte(nil), old[:len(old)-91]...), forged.String()+"\n"...)
	bch, err := ReadChain(bad)
	if err != nil || bch.Seal != forged {
		t.Fatalf("read a chain with a wrong last line: %v", err)
	}
	got, err = bch.Verify(ctx, StoreRecords(ram))
	if err == nil || !equalIDs(got, views[1:len(views)-1]) {
		t.Fatalf("a chain whose last line names another view verified (%v), %d views confirmed", err, len(got))
	}
	if _, err := FromBytes(bad, ram, Filter{}); err == nil {
		t.Fatal("a chain whose last line names another view resolved")
	}
	// Under a filter the view is partial, its ID is another, and the line
	// can only be dropped.
	f, err := NewFilter([]string{"shots"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FromBytes(bad, ram, f); err != nil {
		t.Fatalf("a filtered resolution: %v", err)
	}
}

// Every way the old form reached a file: one changeset, changesets joined
// end to end (each repeating the last line of the one before), and a
// changeset in the current form appended to an old chain.
func TestOldAndCurrentFormsJoin(t *testing.T) {
	ctx := context.Background()
	chain, views, ram := buildChain(t, 11, 8)
	ch, err := ReadChain(chain)
	if err != nil || len(ch.Sections) < 3 {
		t.Fatalf("need a longer chain: %v", err)
	}
	src := StoreRecords(ram)
	// One old changeset per section: parent, entries, view.
	var olds [][]byte
	for i, sec := range ch.Sections {
		one := &Chain{Base: sec.Base, Sections: []Section{sec}}
		olds = append(olds, oldForm(t, one, views[i:i+2]))
	}
	for i, old := range olds {
		c, err := ReadChain(old)
		if err != nil || c.Seal != views[i+1] || c.Base != views[i] || len(c.Sections) != 1 {
			t.Fatalf("old changeset %d: %v", i, err)
		}
		if got, err := c.Verify(ctx, src); err != nil || len(got) != 1 || got[0] != views[i+1] {
			t.Fatalf("old changeset %d verifies to %v: %v", i, got, err)
		}
		// It resolves from the store as a description, too.
		if r, err := FromBytes(old, ram, Filter{}); err != nil || r.View().ID != views[i+1] {
			t.Fatalf("old changeset %d resolves: %v", i, err)
		}
	}
	// Old changesets end to end.
	joined, err := ReadChain(bytes.Join(olds, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := joined.Verify(ctx, src); err != nil || !equalIDs(got, views[1:]) || joined.Seal != views[len(views)-1] {
		t.Fatalf("old changesets joined verify to %d views: %v", len(got), err)
	}
	// An old chain with a current changeset appended: the old last line
	// becomes an ordinary boundary, stated twice.
	n := len(ch.Sections)
	oldPrefix := oldForm(t, &Chain{Base: ch.Base, Sections: ch.Sections[:n-1]}, views[:n])
	last, err := EncodeChangeset(ch.Sections[n-1].Base, ch.Sections[n-1].Patch)
	if err != nil {
		t.Fatal(err)
	}
	mixed, err := ReadChain(append(oldPrefix, last...))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := mixed.Verify(ctx, src); err != nil || !equalIDs(got, views[1:]) || !mixed.Seal.IsNil() {
		t.Fatalf("an old chain with a current changeset appended verifies to %d views: %v", len(got), err)
	}
	// Each of them encodes to the one current form.
	for name, c := range map[string]*Chain{"joined": joined, "mixed": mixed} {
		if again, err := c.Encode(); err != nil || !bytes.Equal(again, chain) {
			t.Fatalf("%s does not encode to the current form: %v", name, err)
		}
	}
}

func TestReadChainEdges(t *testing.T) {
	a := c4.Identify(strings.NewReader("a"))
	b := c4.Identify(strings.NewReader("b"))
	entry := "- - 1 f.txt " + c4.Identify(strings.NewReader("f")).String() + "\n"
	for _, c := range []struct {
		name     string
		data     string
		base     c4.ID
		sections int
		seal     c4.ID
		bad      bool
	}{
		{name: "a base alone", data: a.String() + "\n", base: a},
		{name: "a base alone, no newline", data: a.String(), base: a},
		{name: "a base alone among blank lines", data: "\n\n " + a.String() + " \n\n", base: a},
		{name: "a changeset", data: a.String() + "\n" + entry, base: a, sections: 1},
		{name: "a changeset, no last newline", data: a.String() + "\n" + strings.TrimSuffix(entry, "\n"), base: a, sections: 1},
		{name: "an old changeset", data: a.String() + "\n" + entry + b.String() + "\n", base: a, sections: 1, seal: b},
		{name: "an old changeset, spaces and blank lines after", data: a.String() + "\n" + entry + " " + b.String() + " \n\n \n", base: a, sections: 1, seal: b},
		{name: "an old changeset, no last newline", data: a.String() + "\n" + entry + b.String(), base: a, sections: 1, seal: b},
		{name: "a base and a last line, no sections", data: a.String() + "\n" + a.String() + "\n", base: a, seal: a},
		{name: "empty", data: "", bad: true},
		{name: "blank", data: "\n \n", bad: true},
		{name: "a listing with no external base", data: entry, bad: true},
		{name: "a listing with no external base that ends with an ID", data: entry + b.String() + "\n", bad: true},
	} {
		ch, err := ReadChain([]byte(c.data))
		if c.bad {
			if err == nil {
				t.Errorf("%s: read as a chain", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if ch.Base != c.base || len(ch.Sections) != c.sections || ch.Seal != c.seal {
			t.Errorf("%s: base %s, %d sections, seal %s", c.name, ch.Base, len(ch.Sections), ch.Seal)
		}
	}
	// A description with no external base is the c4m library's to judge,
	// its last line with it: nothing is taken off.
	listing := []byte(entry + b.String() + "\n")
	if body, seal := splitSeal(listing); !seal.IsNil() || !bytes.Equal(body, listing) {
		t.Fatal("a line was taken off a description that has no external base")
	}
	// A chain with no sections and a last line that is not its base.
	ch, err := ReadChain([]byte(a.String() + "\n" + b.String() + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Verify(context.Background(), StoreRecords(store.NewRAM())); err == nil {
		t.Fatal("a chain with no sections verified against a last line that is not its base")
	}
}

func TestTamperedSectionKeepsTheVerifiedPrefix(t *testing.T) {
	chain, views, ram := buildChain(t, 99, 10)
	ch, err := ReadChain(chain)
	if err != nil || len(ch.Sections) < 4 {
		t.Fatalf("need a longer chain: %v", err)
	}
	// Change one entry's ID in the third section.
	target := ch.Sections[2].Patch.Entries[len(ch.Sections[2].Patch.Entries)-1].C4ID.String()
	forged := c4.Identify(strings.NewReader("forged")).String()
	idx := bytes.LastIndex(chain, []byte(target))
	tampered := append(append(append([]byte(nil), chain[:idx]...), forged...), chain[idx+len(target):]...)
	tch, err := ReadChain(tampered)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tch.Verify(context.Background(), StoreRecords(ram))
	if err == nil {
		t.Fatal("a tampered section verified")
	}
	if len(got) < 1 || len(got) > len(views)-1 {
		t.Fatalf("verified prefix of %d views", len(got))
	}
	for i, id := range got {
		if id != views[i+1] {
			t.Fatal("the verified prefix is wrong")
		}
	}
}

// Nothing in a chain names the view its last section produces, so a change
// to that section is found by the reader that holds the head, not by the
// chain. (A line after it never could: whoever changes the section can
// recompute the line. The chain's own C4 ID is what keeps its bytes.)
func TestTheHeadIsComparedByWhoeverHoldsIt(t *testing.T) {
	chain, views, ram := buildChain(t, 99, 10)
	ch, err := ReadChain(chain)
	if err != nil || len(ch.Sections) < 2 {
		t.Fatalf("need a longer chain: %v", err)
	}
	last := ch.Sections[len(ch.Sections)-1].Patch.Entries
	target := last[len(last)-1].C4ID.String()
	forged := c4.Identify(strings.NewReader("forged")).String()
	idx := bytes.LastIndex(chain, []byte(target))
	tampered := append(append(append([]byte(nil), chain[:idx]...), forged...), chain[idx+len(target):]...)
	tch, err := ReadChain(tampered)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tch.Verify(context.Background(), StoreRecords(ram))
	if err != nil || len(got) != len(views)-1 {
		t.Fatalf("verify: %v", err)
	}
	if !equalIDs(got[:len(got)-1], views[1:len(views)-1]) {
		t.Fatal("the views before the last are wrong")
	}
	if got[len(got)-1] == views[len(views)-1] {
		t.Fatal("a changed last section resolves to the same head")
	}
}

func TestAChangesetFromAnotherHeadDoesNotVerify(t *testing.T) {
	chain, views, ram := buildChain(t, 7, 4)
	ch, _ := ReadChain(chain)
	other := Edit{Path: "elsewhere.txt", Entry: FileEntry("", 1, c4.Identify(strings.NewReader("e")), false)}
	applied, err := Apply(context.Background(), ch.Base, []Edit{other}, StoreRecords(ram))
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := ReadChain(append(append([]byte(nil), chain...), applied.Changeset...))
	if err != nil {
		t.Fatal(err)
	}
	got, err := wrong.Verify(context.Background(), StoreRecords(ram))
	if err == nil {
		t.Fatal("a chain verified with a changeset whose parent is not its head")
	}
	// Every view up to the one the wrong changeset contradicts is confirmed.
	if !equalIDs(got, views[1:len(views)-1]) {
		t.Fatalf("%d views confirmed, want %d", len(got), len(views)-2)
	}
}

// An exported chain has its base listed in full, then the base's ID as the
// boundary before the first section. It ends on an entry as well, whatever
// form the chain it was made from has.
func TestExportChain(t *testing.T) {
	ctx := context.Background()
	chain, views, ram := buildChain(t, 5, 6)
	src := StoreRecords(ram)
	full, err := ExportChain(ctx, chain, src)
	if err != nil {
		t.Fatal(err)
	}
	if endsWithBareID(full) {
		t.Fatalf("the exported chain ends with a bare C4 ID\n%s", full)
	}
	if !bytes.HasSuffix(full, chain) {
		t.Fatal("the exported chain does not end with the chain")
	}
	// The c4m library resolves it, checking every boundary, to the head.
	m, err := c4m.NewDecoder(bytes.NewReader(full)).Decode()
	if err != nil {
		t.Fatalf("c4m does not resolve the exported chain: %v", err)
	}
	root, err := nodeFromManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if id := root.Identify(); id != views[len(views)-1] {
		t.Fatalf("the exported chain resolves to %s, want %s", id, views[len(views)-1])
	}
	// From a chain in the old form the export is the same bytes.
	ch, _ := ReadChain(chain)
	fromOld, err := ExportChain(ctx, oldForm(t, ch, views), src)
	if err != nil || !bytes.Equal(fromOld, full) {
		t.Fatalf("the export of an old chain differs: %v", err)
	}
	// A chain with no sections exports as its base's listing alone.
	alone, err := (&Chain{Base: views[0]}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	listing, err := ExportChain(ctx, alone, src)
	if err != nil || endsWithBareID(listing) {
		t.Fatalf("a chain with no sections exports with a bare ID at its end: %v", err)
	}
	if v, err := Parse(listing, views[0]); err != nil || v.ID != views[0] {
		t.Fatalf("a chain with no sections does not export as its base: %v", err)
	}
}

// TestWriteChainForCLI writes a chain for checking with the c4 CLI:
// C4_CHAIN_OUT=/tmp/x.c4m go test -run TestWriteChainForCLI ./internal/view
func TestWriteChainForCLI(t *testing.T) {
	out := os.Getenv("C4_CHAIN_OUT")
	if out == "" {
		t.Skip("set C4_CHAIN_OUT")
	}
	chain, views, ram := buildChain(t, 5, 6)
	if err := os.WriteFile(out, chain, 0o644); err != nil {
		t.Fatal(err)
	}
	full, err := ExportChain(context.Background(), chain, StoreRecords(ram))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+".full", full, 0o644); err != nil {
		t.Fatal(err)
	}
	for i, v := range views {
		t.Logf("view %d %s", i, v)
	}
}

// everyReader applies edits to parent and checks that every reader of the
// changeset agrees with Apply: the chain verifier, the encoder, the
// resolver, and the c4m library's own patch rule.
func everyReader(t *testing.T, name string, ram *store.RAM, parent c4.ID, edits []Edit) *Applied {
	t.Helper()
	ctx := context.Background()
	applied, err := Apply(ctx, parent, edits, StoreRecords(ram))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if applied.ID == parent {
		t.Fatalf("%s: the edits changed nothing", name)
	}
	for _, r := range applied.Records {
		putBytes(t, ram, r.ID, r.Data)
	}
	ch, err := ReadChain(applied.Changeset)
	if err != nil || len(ch.Sections) != 1 || ch.Base != parent {
		t.Fatalf("%s: the changeset reads as %v (%v)\n%s", name, ch, err, applied.Changeset)
	}
	if got, err := ch.Verify(ctx, StoreRecords(ram)); err != nil || len(got) != 1 || got[0] != applied.ID {
		t.Errorf("%s: the chain verifier gives %v (%v), Apply gave %s\n%s", name, got, err, applied.ID, applied.Changeset)
	}
	if again, err := ch.Encode(); err != nil || !bytes.Equal(again, applied.Changeset) {
		t.Errorf("%s: the changeset does not encode to itself (%v)", name, err)
	}
	if r, err := FromBytes(applied.Changeset, ram, Filter{}); err != nil || r.View().ID != applied.ID {
		t.Errorf("%s: the resolver disagrees with Apply (%v)\n%s", name, err, applied.Changeset)
	}
	// The c4m library's rule, on the parent as a whole c4m file.
	pv, err := Load(ctx, parent, StoreRecords(ram), 4)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if root, err := nodeFromManifest(c4m.ApplyPatch(pv.Manifest(), ch.Sections[0].Patch)); err != nil || root.Identify() != applied.ID {
		t.Errorf("%s: the c4m library's ApplyPatch disagrees with Apply (%v)\n%s", name, err, applied.Changeset)
	}
	return applied
}

// A path that changes kind: a directory becomes a file, a file a
// directory. The file sorts before the directory of the same name, so a
// directory replaced by a file is written as the file and then the
// directory restated; read against the tree as edited so far, the
// restatement put the directory back and the changeset applied to nothing.
func TestAPathThatChangesKind(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a/x.txt":     "x",
		"a/sub/y.txt": "y",
		"b":           "a file",
		"c/z":         "z",
		"keep.txt":    "keep",
	})
	base, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ram := store.NewRAM()
	publish(t, ram, base)
	cur := base.ID
	file := func(content string) *c4m.Entry {
		return FileEntry("", int64(len(content)), c4.Identify(strings.NewReader(content)), false)
	}
	step := func(name string, change func(), edits ...Edit) {
		t.Helper()
		change()
		SortEdits(edits)
		a := everyReader(t, name, ram, cur, edits)
		want, err := ScanDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if a.ID != want.ID {
			t.Fatalf("%s: Apply gives %s, the filesystem %s", name, a.ID, want.ID)
		}
		cur = a.ID
	}
	step("a directory becomes a file", func() {
		os.RemoveAll(dir + "/a")
		os.WriteFile(dir+"/a", []byte("now a file"), 0o644)
	}, Edit{Path: "a/"}, Edit{Path: "a", Entry: file("now a file")})
	step("a file becomes a directory", func() {
		os.Remove(dir + "/b")
		os.MkdirAll(dir+"/b", 0o755)
		os.WriteFile(dir+"/b/inner.txt", []byte("inner"), 0o644)
	}, Edit{Path: "b"}, Edit{Path: "b/inner.txt", Entry: file("inner")})
	step("a file becomes an empty directory", func() {
		os.Remove(dir + "/keep.txt")
		os.MkdirAll(dir+"/keep.txt", 0o755)
	}, Edit{Path: "keep.txt"}, Edit{Path: "keep.txt/", Entry: &c4m.Entry{Name: "keep.txt/"}})
	step("a directory becomes a file, with no removal stated", func() {
		os.RemoveAll(dir + "/c")
		os.WriteFile(dir+"/c", []byte("c file"), 0o644)
	}, Edit{Path: "c", Entry: file("c file")})
	step("and back to the directory it was", func() {
		os.Remove(dir + "/c")
		os.MkdirAll(dir+"/c", 0o755)
		os.WriteFile(dir+"/c/z", []byte("z"), 0o644)
	}, Edit{Path: "c/z", Entry: file("z")})
	step("a directory becomes a symlink", func() {
		os.RemoveAll(dir + "/b")
		os.Symlink("keep.txt", dir+"/b")
	}, Edit{Path: "b/"}, Edit{Path: "b", Entry: &c4m.Entry{Name: "b", Mode: os.ModeSymlink, Target: "keep.txt", Timestamp: c4m.NullTimestamp(), Size: -1}})
}

// The c4 CLI through 1.0.24 ended `c4 diff` with the new description's
// ID: its own, with dates and modes, not a view's. These two files are its
// real output (a.c4m from `c4 id a/`, d.c4m from `c4 diff a.c4m b.c4m`),
// kept byte for byte. Such a patch over a stored c4m resolved before the
// last line was checked, and still does: the line names the description
// as recorded.
func TestAC4DiffInTheOldFormIsStillRead(t *testing.T) {
	base, err := os.ReadFile("testdata/c4-diff-1.0.24/a.c4m")
	if err != nil {
		t.Fatal(err)
	}
	diff, err := os.ReadFile("testdata/c4-diff-1.0.24/d.c4m")
	if err != nil {
		t.Fatal(err)
	}
	if !endsWithBareID(diff) {
		t.Fatal("the fixture is not in the old form")
	}
	ram := store.NewRAM()
	putBytes(t, ram, c4.Identify(bytes.NewReader(base)), base)
	r, err := FromBytes(diff, ram, Filter{})
	if err != nil {
		t.Fatalf("a c4 diff from 1.0.24 is refused: %v", err)
	}
	_, last := lastLine(diff)
	if r.RootID.String() != last {
		t.Fatalf("the diff's last line is %s; the description resolves to %s", last, r.RootID)
	}
	if len(r.View().Files) != 3 || r.Levels != 2 {
		t.Fatalf("resolved to %d files in %d levels", len(r.View().Files), r.Levels)
	}
	// The same check under a filter, where the view is partial.
	f, _ := NewFilter([]string{"one.txt"})
	if _, err := FromBytes(diff, ram, f); err != nil {
		t.Fatalf("under a filter: %v", err)
	}
	// A last line that is neither ID is refused.
	forged := c4.Identify(strings.NewReader("forged"))
	bad := append(append([]byte(nil), diff[:len(diff)-91]...), forged.String()+"\n"...)
	if _, err := FromBytes(bad, ram, Filter{}); err == nil {
		t.Fatal("a c4 diff whose last line names something else resolved")
	}
}

// An old changeset whose parent is another old changeset: the line is
// taken off at every depth of a resolution, not only the first.
func TestOldChangesetsNest(t *testing.T) {
	chain, views, ram := buildChain(t, 13, 6)
	ch, err := ReadChain(chain)
	if err != nil || len(ch.Sections) < 3 {
		t.Fatalf("need a longer chain: %v", err)
	}
	// Each section as an old changeset stored under its own ID, the next
	// one naming it as its parent.
	parent := views[0]
	var top []byte
	for i, sec := range ch.Sections[:3] {
		one := &Chain{Base: parent, Sections: []Section{{Base: parent, Patch: sec.Patch}}}
		top = oldForm(t, one, []c4.ID{parent, views[i+1]})
		parent = c4.Identify(bytes.NewReader(top))
		putBytes(t, ram, parent, top)
	}
	r, err := FromBytes(top, ram, Filter{})
	if err != nil {
		t.Fatalf("old changesets over old changesets: %v", err)
	}
	if r.View().ID != views[3] || r.Levels != 4 {
		t.Fatalf("resolved to %s in %d levels, want %s in 4", r.View().ID, r.Levels, views[3])
	}
}

// A last line the c4m library reads as a bare ID, though no ID prints that
// way, is taken off and checked like any other, not left for the library
// to drop.
func TestALastLineNoIDPrintsAsIsStillChecked(t *testing.T) {
	chain, _, ram := buildChain(t, 3, 2)
	odd := "c4" + strings.Repeat("z", 88)
	if id, err := c4.Parse(odd); err != nil || id.String() == odd {
		t.Skipf("%q is not a line that parses to an ID printed otherwise (%v)", odd, err)
	}
	ch, err := ReadChain(append(append([]byte(nil), chain...), odd+"\n"...))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Seal.IsNil() {
		t.Fatal("the line was left in the chain for the library to drop")
	}
	if _, err := ch.Verify(context.Background(), StoreRecords(ram)); err == nil {
		t.Fatal("a chain ending with a line that names nothing it resolves to verified")
	}
}

// A directory is replaced wholesale and a subdirectory of the same name is
// made again inside it: rm -rf build, mkdir -p build/obj, write
// build/obj/new.o, where the view had build/obj/old.o. The changeset has
// to say old.o is gone. The writer compared the remade obj/ with nothing,
// stated only what was added, and every reader of the changeset kept the
// old files.
func TestADirectoryMadeAgainInsideAReplacedOne(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"build/obj/old.o":       "old",
		"build/obj/keep.o":      "keep",
		"build/obj/deep/x.o":    "x",
		"build/obj/deep/more/y": "y",
		"build/top.txt":         "top",
		"src/a.c":               "a",
	})
	base, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ram := store.NewRAM()
	publish(t, ram, base)
	file := func(content string) *c4m.Entry {
		return FileEntry("", int64(len(content)), c4.Identify(strings.NewReader(content)), false)
	}
	// What an overlay's upper shows after that: build/ replaced (opaque),
	// build/obj/ and build/obj/deep/ plain directories, and the files
	// written: one new, one the same bytes as before.
	os.RemoveAll(dir + "/build")
	os.MkdirAll(dir+"/build/obj/deep", 0o755)
	os.WriteFile(dir+"/build/obj/new.o", []byte("new"), 0o644)
	os.WriteFile(dir+"/build/obj/keep.o", []byte("keep"), 0o644)
	edits := []Edit{
		{Path: "build/", Entry: &c4m.Entry{Name: "build/"}, Opaque: true},
		{Path: "build/obj/", Entry: &c4m.Entry{Name: "obj/"}},
		{Path: "build/obj/deep/", Entry: &c4m.Entry{Name: "deep/"}},
		{Path: "build/obj/new.o", Entry: file("new")},
		{Path: "build/obj/keep.o", Entry: file("keep")},
	}
	SortEdits(edits)
	a := everyReader(t, "a directory made again", ram, base.ID, edits)
	want, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != want.ID {
		t.Fatalf("Apply gives %s, the filesystem %s", a.ID, want.ID)
	}
	// And by the c4m library's own rule, over the base as a whole c4m file.
	pm, err := c4m.NewDecoder(bytes.NewReader(a.Changeset)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	root, err := nodeFromManifest(c4m.ApplyPatch(base.Manifest(), pm))
	if err != nil {
		t.Fatal(err)
	}
	if id := root.Identify(); id != want.ID {
		t.Fatalf("by the c4m library the changeset gives %s, the filesystem %s\n%s", id, want.ID, a.Changeset)
	}
}
