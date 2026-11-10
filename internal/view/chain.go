package view

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// A changeset is a c4m patch over a view: the parent view's ID on the first
// line, then the changed entries, exactly as the c4m library's EncodePatch
// writes it. Nothing follows the entries. A c4m file never ends with a bare
// C4 ID (it could not be the ID of the file that holds it), so the view a
// changeset produces is not written in it: whoever makes the changeset
// carries that view's ID beside it, and anyone can recompute it by applying
// the patch.
//
// A Chain is changesets one after another, which is c4m's append-only patch
// chain read from an external base. Every bare C4 ID line in it names the
// view before the section that follows: the base on the first line, and
// after that the view the sections so far produce. So appending a changeset
// to a chain is concatenation, each bare ID can be checked by applying the
// sections above it along changed paths, and the head, the view the last
// section produces, is recomputed, never read.
type Chain struct {
	// Base is the view the chain starts from (an external base on the
	// chain's first line).
	Base     c4.ID
	Sections []Section
	// Seal is set only for a chain written before 2026-11-09. Those ended
	// with the bare ID of the view they resolve to, which the format now
	// forbids. They are stored objects, named by ID in signed records, so
	// they are still read: Verify checks the line like any other bare ID,
	// and nothing here writes one.
	Seal c4.ID
}

// Section is one patch of a chain.
type Section struct {
	// Base is the bare C4 ID line before the section: the view it applies
	// to. The first section's is the chain's.
	Base  c4.ID
	Patch *c4m.Manifest
}

// EncodeChangeset writes patch as a changeset over parent.
func EncodeChangeset(parent c4.ID, patch *c4m.Manifest) ([]byte, error) {
	var buf bytes.Buffer
	if err := c4m.NewEncoder(&buf).EncodePatch(&c4m.PatchResult{Patch: patch, OldID: parent}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadChain decodes a changeset or a chain that starts from an external
// base: the form every changeset and every stream in this system has. The
// c4m library finds the sections.
func ReadChain(data []byte) (*Chain, error) {
	body, seal := splitSeal(data)
	sections, err := c4m.DecodePatchChain(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ch := &Chain{Seal: seal}
	if len(sections) == 0 {
		// No entries at all: a chain with no sections is its base alone.
		line := strings.TrimSpace(string(body))
		if line == "" {
			return nil, errors.New("empty chain")
		}
		if ch.Base, err = c4.Parse(line); err != nil {
			return nil, errors.New("chain has no base: its first line must name a view")
		}
		return ch, nil
	}
	if sections[0].BaseID.IsNil() {
		return nil, errors.New("chain has no base: its first line must name a view")
	}
	ch.Base = sections[0].BaseID
	for _, s := range sections {
		m := &c4m.Manifest{Version: "1.0", Entries: s.Entries}
		m.SortEntries()
		ch.Sections = append(ch.Sections, Section{Base: s.BaseID, Patch: m})
	}
	return ch, nil
}

// Encode writes the chain as it is stored and served: its changesets one
// after another. A chain with no sections is its base's ID alone. A Seal is
// never written, so reading a chain in the old form and encoding it gives
// the same chain in the form the format allows.
func (ch *Chain) Encode() ([]byte, error) {
	if len(ch.Sections) == 0 {
		return []byte(ch.Base.String() + "\n"), nil
	}
	var out bytes.Buffer
	for i, sec := range ch.Sections {
		base := sec.Base
		if i == 0 {
			base = ch.Base // one fact, read from one place, as Verify does
		}
		cs, err := EncodeChangeset(base, sec.Patch)
		if err != nil {
			return nil, err
		}
		out.Write(cs)
	}
	return out.Bytes(), nil
}

// Verify applies each section to the view before it, along changed paths
// only, and returns the view after each section, in order. Every bare ID in
// the chain is checked against them: a section's Base must be the view the
// sections before it produce, and so must the Seal of a chain that has one.
// The last view is the chain's head. Nothing in the chain names it, so a
// reader that was told the head compares the two. On an error, the views
// returned are the ones the chain's own bare IDs confirmed.
func (ch *Chain) Verify(ctx context.Context, src RecordSource) ([]c4.ID, error) {
	if len(ch.Sections) == 0 && !ch.Seal.IsNil() && ch.Seal != ch.Base {
		return nil, fmt.Errorf("the chain has no sections and ends with %s, which is not its base %s", ch.Seal, ch.Base)
	}
	cur := ch.Base
	var out []c4.ID
	for i, sec := range ch.Sections {
		res, err := ApplyPatch(ctx, cur, sec.Patch, src)
		if err != nil {
			return out, fmt.Errorf("section %d: %w", i+1, err)
		}
		// The bare ID after the section, when the chain has one: the next
		// section's base, or the seal of a chain in the old form.
		named := ch.Seal
		if i+1 < len(ch.Sections) {
			named = ch.Sections[i+1].Base
		}
		if !named.IsNil() && res.ID != named {
			return out, fmt.Errorf("section %d: the patch produces %s, the chain says %s", i+1, res.ID, named)
		}
		cur = res.ID
		out = append(out, cur)
	}
	return out, nil
}

// splitSeal separates a chain over an external base from the bare C4 ID
// that ended it when it was written before 2026-11-09 (the view it resolves
// to, appended by hand). It returns that chain without the line, and the ID.
// Anything else comes back unchanged with a nil ID: a chain in the current
// form, a base alone, and a description with no external base, whose
// trailing ID is the c4m library's to judge.
//
// The line is recognised without reading entries: it is the last line that
// is not blank, it is a C4 ID and nothing else, and the first such line of
// the data is one too. The c4m library reads everything that remains.
func splitSeal(data []byte) ([]byte, c4.ID) {
	start, last := lastLine(data)
	seal, ok := bareID(last)
	if !ok {
		return data, c4.ID{}
	}
	first := firstLine(data[:start])
	if first == "" {
		return data, c4.ID{} // the only line: a base, not a seal
	}
	if _, ok := bareID(first); !ok {
		return data, c4.ID{} // no external base: not one of ours
	}
	return data[:start], seal
}

// firstLine returns the first line of data that is not blank, trimmed.
func firstLine(data []byte) string {
	for len(data) > 0 {
		line, rest, _ := bytes.Cut(data, []byte("\n"))
		if t := strings.TrimSpace(string(line)); t != "" {
			return t
		}
		data = rest
	}
	return ""
}

// lastLine returns the last line of data that is not blank, trimmed, and
// where it starts. The line is "" when every line is blank.
func lastLine(data []byte) (start int, line string) {
	end := len(data)
	for end > 0 {
		stop := end
		if data[stop-1] == '\n' {
			stop--
		}
		begin := bytes.LastIndexByte(data[:stop], '\n') + 1
		if t := strings.TrimSpace(string(data[begin:stop])); t != "" {
			return begin, t
		}
		end = begin
	}
	return 0, ""
}

// bareID reports whether a trimmed line is one the c4m library reads as a
// bare C4 ID: 90 characters starting "c4" that parse. It is the library's
// test, so a line taken off here is exactly a line the library would have
// read as a boundary, and is checked instead of dropped.
func bareID(line string) (c4.ID, bool) {
	if len(line) != 90 || !strings.HasPrefix(line, "c4") {
		return c4.ID{}, false
	}
	id, err := c4.Parse(line)
	if err != nil {
		return c4.ID{}, false
	}
	return id, true
}

// ApplyPatch applies a c4m patch to the view base, the view it was made
// over, with c4m's patch semantics, matched by path: an entry identical to
// the base's, with no children under it, removes that path; a differing
// entry replaces it; a new path is added; a directory entry followed by
// children merges them. It reads only the base's records along the patch's
// paths.
func ApplyPatch(ctx context.Context, base c4.ID, patch *c4m.Manifest, src RecordSource) (*Applied, error) {
	t := &atree{ctx: ctx, src: src, root: &anode{base: base}}
	if err := applyPatchFrom(t, t, patch); err != nil {
		return nil, err
	}
	res := &Applied{Parent: base}
	root := t.identify(t.root, &res.Records)
	res.ID = root.C4ID
	return res, nil
}

// ApplyPatchFrom applies to the view target a patch that was made over the
// view base. A patch entry says what it means only against its own base:
// identical to what the base holds, it removes the path; anything else, it
// is the entry now at that path. Applied to another view as it stands, a
// write of something that view already holds would read as a removal. So
// each entry is read against base, and what it means is done to target: a
// path the patch removes is removed from target, and an entry the patch
// writes is written to target, over whatever target holds there. There is
// no conflict rule in this; a caller that wants one compares the paths.
//
// A directory entry with children under it is context for them and writes
// nothing itself: a write below it makes the directory where the target has
// none, and a removal below it does not. A write takes the place of what
// the target holds at its path whatever its kind, so a file written where
// the target has a directory takes the directory and all under it, and the
// result's Dropped names what went that way.
//
// With target equal to base this is ApplyPatch. It is idempotent: applied
// again to its own result it changes nothing. The result carries the
// changeset over target that describes it, empty when nothing changed, and
// that changeset is an ordinary one: a chain it is appended to verifies by
// ApplyPatch alone.
//
// The c4m library has the same operation, with the same name, on whole c4m
// files. This is the form for a view loaded from records along the changed
// paths, and it follows the library's rule that a name is one thing
// whatever its kind. Three readings here are the ones proposed for the
// library and not yet ruled on (2026-11-09), and are to follow the ruling:
// a removal leaves a thing of the other kind that has taken the path since;
// a directory written by its ID alone takes the place of the one the target
// holds; and an empty directory written where the target holds a directory
// says only that the directory exists, and leaves it as it is (a unit that
// ran mkdir does not remove what another unit wrote there).
func ApplyPatchFrom(ctx context.Context, target, base c4.ID, patch *c4m.Manifest, src RecordSource) (*Applied, error) {
	tt := &atree{ctx: ctx, src: src, root: &anode{base: target}}
	tb := tt
	if base != target {
		tb = &atree{ctx: ctx, src: src, root: &anode{base: base}}
	}
	if err := applyPatchFrom(tt, tb, patch); err != nil {
		return nil, err
	}
	res, err := tt.applied(target)
	if err != nil {
		return nil, err
	}
	res.Dropped = tt.dropped
	return res, nil
}

// applyPatchFrom walks a patch once. What each entry means is read from
// tb, the tree the patch was made over; the edit is made in tt. They are
// one tree when a patch is applied to its own base, and tb is never edited
// otherwise.
func applyPatchFrom(tt, tb *atree, patch *c4m.Manifest) error {
	same := tt == tb
	var stack []string
	entries := patch.Entries
	for i, e := range entries {
		if e.Depth > len(stack) {
			return fmt.Errorf("malformed patch: %q at depth %d has no parent", e.Name, e.Depth)
		}
		stack = stack[:e.Depth]
		if err := CheckName(e.Name); err != nil {
			return err
		}
		dirs := splitDir(strings.Join(stack, ""))
		pe := Project(e)
		name := pe.Name
		hasChildren := i+1 < len(entries) && entries[i+1].Depth > e.Depth

		// What the patch's own base holds at this path decides what the
		// entry means, whatever an earlier entry of this patch has done
		// since: a directory replaced by a file is written as the file,
		// then the directory restated (files sort first), and by then the
		// file's entry has taken the directory out. Applied in place, the
		// directories on the way are made, as a write below them needs.
		from, err := tb.walkTo(dirs, same)
		if err != nil {
			return err
		}
		var existing *c4m.Entry
		had := false
		if from != nil {
			existing, had = from.orig[name]
		}
		path := strings.Join(stack, "")
		if had && !hasChildren && sameEntry(existing, pe) {
			// An exact restatement: the patch removes the path. In another
			// tree there may be nothing there to remove.
			to := from
			if !same {
				if to, err = tt.walkTo(dirs, false); err != nil {
					return err
				}
			}
			if to != nil {
				delete(to.files, name)
				if to.dirs[name] != nil {
					delete(to.dirs, name)
					tt.dropped = append(tt.dropped, path+name)
				}
			}
			if pe.IsDir() {
				stack = append(stack, name)
			}
			continue
		}
		if !same && pe.IsDir() && hasChildren {
			// A directory restated for what follows under it: the patch
			// changes those paths, not the directory. A write under it
			// makes it where the target has none; a removal under it must
			// not, or a directory the target no longer has would come back
			// empty.
			stack = append(stack, name)
			continue
		}
		to := from
		if !same {
			if to, err = tt.walkTo(dirs, true); err != nil {
				return err
			}
		}
		if !pe.IsDir() {
			if to.dirs[name+"/"] != nil {
				delete(to.dirs, name+"/")
				tt.dropped = append(tt.dropped, path+name+"/")
			}
			pe.Depth = 0
			to.files[name] = pe
			continue
		}
		if f := strings.TrimSuffix(name, "/"); to.files[f] != nil {
			delete(to.files, f)
			tt.dropped = append(tt.dropped, path+f)
		}
		child, ok := to.dirs[name]
		switch {
		case !ok && !hasChildren && !pe.C4ID.IsNil():
			// A new directory by ID alone: its record says what it holds.
			to.dirs[name] = &anode{name: name, base: pe.C4ID, entry: pe}
		case !ok:
			// A new directory with its children following, or one that
			// names no contents at all, which is taken as empty. It is
			// dirty from the start, so it is identified from what it holds
			// even when no entry follows under it.
			to.dirs[name] = &anode{name: name, loaded: true, dirty: true, files: map[string]*c4m.Entry{}, dirs: map[string]*anode{}}
		case hasChildren, pe.C4ID.IsNil(), pe.C4ID == emptyID, pe.C4ID == child.base && !child.dirty:
			// The directory is there. Its children follow, or the entry
			// brings nothing the directory lacks: a directory made empty
			// beside one that others have filled (two units both running
			// mkdir -p) leaves what the others wrote.
		default:
			// Written by ID alone where the tree holds another directory:
			// the path now holds the listing the ID names, and what was
			// there goes. That is so over the patch's own base too, which
			// is what lets the changeset this writes for such a patch be
			// read back by the plain rule. Capture never writes one (a
			// changed directory is followed by its changes).
			to.dirs[name] = &anode{name: name, base: pe.C4ID, entry: pe}
			tt.dropped = append(tt.dropped, path+name)
		}
		stack = append(stack, name)
	}
	return nil
}

// ExportChain writes a chain with its base listing in full instead of an
// external base reference, so tools that read only the chain text (the c4
// CLI's log) see every version as a whole tree. The base's ID follows the
// listing as the boundary before the first section; a chain with no
// sections is the listing alone.
func ExportChain(ctx context.Context, chain []byte, src RecordSource) ([]byte, error) {
	ch, err := ReadChain(chain)
	if err != nil {
		return nil, err
	}
	base, err := Load(ctx, ch.Base, src, 16)
	if err != nil {
		return nil, err
	}
	listing, err := base.Marshal()
	if err != nil {
		return nil, err
	}
	if len(ch.Sections) == 0 {
		return listing, nil
	}
	sections, err := ch.Encode()
	if err != nil {
		return nil, err
	}
	return append(listing, sections...), nil
}
