package gitimport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// ChainFile is one branch's chain (DESIGN.md sections 2.3 and 2.5): the
// changesets of its commits, one after another, kept as a local file. Each
// changeset starts with the view it applies to, so the file is a c4m chain
// as it stands, and an append costs the size of the commit's changeset,
// not of the chain.
//
// The head, the view the last changeset produces, is not in the file (a
// c4m file never ends with a bare C4 ID). Opening the file recomputes it
// from the last section, and an append is told it.
//
// The chain is stored as segments: each commit's changeset is a stored
// object, and the changesets listed in the mapping concatenate into this
// chain. The whole chain is stored as one object only when it is sealed
// (re-based, or at the end of a one-shot import), so a growing chain never
// rewrites stored objects.
type ChainFile struct {
	Path     string
	Base     c4.ID
	Head     c4.ID // the view the chain resolves to; Base while it has no sections
	Sections int
}

// OpenChainFile reads the chain at p, or returns an empty chain when p
// does not exist. src reads the records the last section applies to, to
// recompute the head. A file written before 2026-11-09 ends with its head's
// ID; that line must agree with the head recomputed.
func OpenChainFile(ctx context.Context, p string, src view.RecordSource) (*ChainFile, error) {
	data, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ch, _, err := openChain(ctx, p, data, src)
	return ch, err
}

// openChain is OpenChainFile on the file's bytes; none is an empty chain.
// It returns the chain as read as well, nil for an empty one.
func openChain(ctx context.Context, p string, data []byte, src view.RecordSource) (*ChainFile, *view.Chain, error) {
	ch := &ChainFile{Path: p}
	if len(bytes.TrimSpace(data)) == 0 {
		return ch, nil, nil
	}
	c, err := view.ReadChain(data)
	if err != nil {
		return nil, nil, fmt.Errorf("chain file %s: %w", p, err)
	}
	ch.Base, ch.Head, ch.Sections = c.Base, c.Base, len(c.Sections)
	if n := len(c.Sections); n > 0 {
		if src == nil {
			return nil, nil, fmt.Errorf("chain file %s: its head is recomputed from the store's records, and there is no store to read", p)
		}
		last := c.Sections[n-1]
		res, err := view.ApplyPatch(ctx, last.Base, last.Patch, src)
		if err != nil {
			return nil, nil, fmt.Errorf("chain file %s: %w", p, err)
		}
		ch.Head = res.ID
	}
	if !c.Seal.IsNil() && c.Seal != ch.Head {
		return nil, nil, fmt.Errorf("chain file %s ends with %s and resolves to %s", p, c.Seal, ch.Head)
	}
	return ch, c, nil
}

// Empty reports whether the chain has no base yet.
func (ch *ChainFile) Empty() bool { return ch.Base.IsNil() }

// Append adds one changeset over parent, which must be the chain's head,
// and takes produced, the view the changeset makes, as the new head. An
// empty chain takes parent as its base.
//
// The changeset is written whole: its first line is the view it applies
// to, which is the boundary a chain needs before a section, so appending
// is concatenation. While the chain has no sections the file is empty or
// holds the base's ID alone, which the changeset's first line restates, so
// the changeset replaces it.
func (ch *ChainFile) Append(changeset []byte, parent, produced c4.ID) error {
	if !bytes.HasPrefix(changeset, []byte(parent.String()+"\n")) {
		return errors.New("changeset does not start with its parent view ID")
	}
	if !ch.Empty() && parent != ch.Head {
		return fmt.Errorf("changeset applies to %s, the chain's head is %s", parent, ch.Head)
	}
	if ch.Sections == 0 {
		// In one step, so no reader, and no crash, finds the file empty
		// between the base line going and the changeset arriving.
		if err := replaceFile(ch.Path, changeset); err != nil {
			return err
		}
	} else {
		f, err := os.OpenFile(ch.Path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.Write(changeset); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if ch.Empty() {
		ch.Base = parent
	}
	ch.Head = produced
	ch.Sections++
	return nil
}

// Rebase starts the chain over with base as its external base, the way a
// chain is compacted (DESIGN.md section 2.5) or a branch that no longer
// extends its chain (a force push) starts a new one.
func (ch *ChainFile) Rebase(base c4.ID) error {
	alone, err := (&view.Chain{Base: base}).Encode()
	if err != nil {
		return err
	}
	if err := replaceFile(ch.Path, alone); err != nil {
		return err
	}
	ch.Base, ch.Head, ch.Sections = base, base, 0
	return nil
}

// syncFile is how a file's bytes are made durable before it takes a name.
// A test puts its own here to watch the order.
var syncFile = func(f *os.File) error { return f.Sync() }

// replaceFile puts data at path in one step: written beside it, synced,
// then renamed over it. The rename replaces a file that was whole, so the
// bytes are synced before they take its name; renamed first, a crash could
// leave the name over a file whose bytes never reached the disk, and the
// chain that was there gone. On an error the file at path is as it was.
func replaceFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = syncFile(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Bytes returns the chain's text.
func (ch *ChainFile) Bytes() ([]byte, error) {
	if ch.Empty() {
		return nil, nil
	}
	return ReadChainFile(ch.Path)
}

// ReadChainFile returns the text of the chain file at p as it is stored
// and served: read by the c4m library and written by it. For a file in the
// current form that is the file's own bytes. A file written before
// 2026-11-09 comes out without the bare ID it ends with, so nothing stored
// or served from here has the form the format forbids.
func ReadChainFile(p string) ([]byte, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	c, err := view.ReadChain(data)
	if err != nil {
		return nil, fmt.Errorf("chain file %s: %w", p, err)
	}
	return c.Encode()
}

// Store stores the whole chain as one object and returns its ID.
func (ch *ChainFile) Store(ctx context.Context, sink Sink) (c4.ID, int64, error) {
	data, err := ch.Bytes()
	if err != nil || len(data) == 0 {
		return c4.ID{}, 0, err
	}
	id := c4.Identify(bytes.NewReader(data))
	_, err = sink.Put(ctx, id, int64(len(data)), bytes.NewReader(data))
	return id, int64(len(data)), err
}

// ChainFileName turns a ref into a file name: refs/heads/main becomes
// refs_heads_main.c4m.
func ChainFileName(ref string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	return r.Replace(strings.TrimPrefix(ref, "/")) + ".c4m"
}

// RecordedChain is what the mapping records of a ref's chain file: the
// view it starts from, the changesets appended since, in order, and the
// view it resolves to.
type RecordedChain struct {
	Base, Head c4.ID
	Changesets []c4.ID
	// Lines and Commits say where each changeset was recorded: the number
	// of its line in the mapping, counted from 1, and its commit. Blank
	// lines are not counted (a mapping has none unless someone put them
	// there), so the number is never of a later line than the real one,
	// and a mapping cut back to it loses nothing that is not made again.
	Lines   []int
	Commits []string
	// Views are the views the changesets produce, as their lines record;
	// the last is Head.
	Views []c4.ID
	// MissingBefore is the last mapping line of the chain before which
	// lines are missing from the mapping (the commits they recorded are no
	// longer published): the line the chain starts at when it starts there
	// only because that line does not apply to the view before it, or a
	// line whose commit's parent has no line. Zero when no line of the
	// chain shows a gap; a chain the importer starts again clears it.
	MissingBefore int
}

// reimport is the way out when the mapping cannot be read from one line
// on. The cut takes every ref's lines from there on with it, so every such
// ref has to be imported again.
const reimport = "cut the mapping back to before line %d and import again every ref that has a line from there on: their commits are imported again, and until a ref is imported again it is published as the shorter mapping has it"

// ChainRecorded returns what the mapping m records of ref's chain file,
// and false when it records none. The mapping is the authority for a
// chain: each commit's line carries its changeset's C4 ID, the view it
// applies to and the view it produces, and is written only after the
// changeset is in the store. The chain file is those changesets joined,
// kept beside the mapping so an append costs one changeset; it can always
// be made again from the mapping, and is never the source of anything
// (OpenRecordedChain).
//
// Only a line with a changeset touched the file. A commit that changes
// nothing, a line that only moves the ref, and a line that records a seal
// say nothing of it: the ref can move to a commit imported under another
// ref, and have empty commits made on it, while its chain stays where its
// last changeset left it.
//
// A changeset goes on the chain when it applies to the view the chain is
// at, which can be checked, and not when its count says so, which cannot
// (a file with a torn section left on it was once counted as it stood, so
// mappings exist whose counts skip a number with no line missing). A line
// counted first started a chain (after a seal, a force push, or a move of
// the ref), from the view it applies to. So does a line that does not
// apply to the view before it: lines are missing from the mapping, and
// the chain recorded is the one from where they resume, which is as much
// as the mapping still knows.
func ChainRecorded(m []Commit, ref string) (RecordedChain, bool) {
	rc, ok := chainsRecorded(m, ref)[ref]
	if !ok {
		return RecordedChain{}, false
	}
	return *rc, true
}

// ChainsRecorded returns what the mapping records of every ref's chain
// (ChainRecorded), read in one pass.
func ChainsRecorded(m []Commit) map[string]RecordedChain {
	out := map[string]RecordedChain{}
	for ref, rc := range chainsRecorded(m, "") {
		out[ref] = *rc
	}
	return out
}

// chainsRecorded reads the chains of every ref, or of only when it is set.
//
// Lines missing from the mapping are found two ways. A changeset that does
// not apply to the view before it starts the chain again. And a line of an
// imported commit whose git parent has no line before it: the importer
// writes a line for a commit's parent before the commit (a parent not
// imported yet is imported first, in full if need be), so a parent with no
// line is a line gone. That second way finds a gap the views read across,
// when the commits after it come back to a view from before it (a revert),
// and it is checked on every imported commit's line, a commit that changes
// nothing included, since the commit after a gap may be one. A commit
// imported in full needs no parent, and a line that only moves a ref
// imports nothing.
func chainsRecorded(m []Commit, only string) map[string]*RecordedChain {
	out := map[string]*RecordedChain{}
	seen := map[string]bool{} // commits with a line so far, of any ref
	for i := range m {
		c := &m[i]
		gap := c.Commit != "" && !c.RefOnly && !c.Base && c.Parent != "" && !seen[c.Parent]
		if c.Commit != "" {
			seen[c.Commit] = true
		}
		if c.Ref == "" || only != "" && c.Ref != only {
			continue
		}
		if c.Changeset.IsNil() || c.ChainSeq < 1 {
			if cur := out[c.Ref]; gap && cur != nil {
				cur.MissingBefore = i + 1
			}
			continue
		}
		cur := out[c.Ref]
		if cur == nil || c.ChainSeq == 1 || c.ParentView != cur.Head {
			cur = &RecordedChain{Base: c.ParentView}
			if c.ChainSeq != 1 {
				cur.MissingBefore = i + 1
			}
			out[c.Ref] = cur
		}
		if gap {
			cur.MissingBefore = i + 1
		}
		cur.Changesets = append(cur.Changesets, c.Changeset)
		cur.Lines = append(cur.Lines, i+1)
		cur.Commits = append(cur.Commits, c.Commit)
		cur.Views = append(cur.Views, c.View)
		cur.Head = c.View
	}
	return out
}

// holds reports whether an opened chain file is the one the mapping
// records: it resolves to the recorded head, as the recorded chain or as
// that chain sealed and started again from its head (which leaves no line
// of its own; with no sections a chain's base is its head).
func (rc RecordedChain) holds(ch *ChainFile) bool {
	if ch.Head != rc.Head {
		return false
	}
	return ch.Base == rc.Base && ch.Sections == len(rc.Changesets) || ch.Sections == 0
}

// differs returns the number of the first section of c, counted from 1,
// that is not the changeset the mapping records in that place, and 0 when
// every section is. A section is compared as the c4m library writes it:
// its C4 ID is the one its line records, or, for a line written before
// 2026-11-09, the one that text has with its view's C4 ID after it. So
// every section is checked against the mapping, not only the last one
// applied, and by hashing alone: the store is not read.
func (rc RecordedChain) differs(c *view.Chain) (int, error) {
	for i, sec := range c.Sections {
		text, err := view.EncodeChangeset(sec.Base, sec.Patch)
		if err != nil {
			return 0, err
		}
		if c4.Identify(bytes.NewReader(text)) != rc.Changesets[i] && !oldForm(text, rc.Views[i], rc.Changesets[i]) {
			return i + 1, nil
		}
	}
	return 0, nil
}

// CannotRemake is why a recorded chain could not be made again from the
// store: the changeset the store does not hold, or holds wrongly, and
// where the mapping records it. A changeset the store does not hold is
// made again from the repository by the next import of the ref (Remake).
// A wrong object under its C4 ID is not: a store keeps the object it has,
// so that one has to be removed first.
type CannotRemake struct {
	Changeset c4.ID
	Commit    string
	Line      int  // the mapping line that records it, counted from 1
	Wrong     bool // the store holds other bytes under the C4 ID
	Err       error
}

func (e *CannotRemake) Error() string {
	if e.Wrong {
		return fmt.Sprintf("the changeset %s of commit %s (mapping line %d) is a wrong object in the store: its bytes are not what that C4 ID names. A store keeps the object it has, so it has to be removed from the store; the next import of the ref then makes it again from the repository",
			e.Changeset, e.Commit, e.Line)
	}
	return fmt.Sprintf("the changeset %s of commit %s (mapping line %d) cannot be read from the store (%v). The next import of the ref makes it again from the repository",
		e.Changeset, e.Commit, e.Line, e.Err)
}

func (e *CannotRemake) Unwrap() error { return e.Err }

// CutBack says what is left when the changeset cannot be made again from
// the repository either: the mapping cut back to before its line, with
// what that costs every other ref.
func (e *CannotRemake) CutBack() string { return fmt.Sprintf(reimport, e.Line) }

// bytes is the recorded chain as a chain file holds it, made from the
// stored changesets. Each is checked against its C4 ID and read on its own
// (one stored before 2026-11-09 ends with the view it produces, which is
// compared with the view it does produce and not carried on), and the
// whole is checked from the recorded base to the recorded head. What comes
// back is the chain the mapping records, in the form the format allows, or
// an error.
func (rc RecordedChain) bytes(ctx context.Context, src view.RecordSource) ([]byte, error) {
	c := &view.Chain{Base: rc.Base}
	seals := make([]c4.ID, len(rc.Changesets))
	for i, id := range rc.Changesets {
		lost := &CannotRemake{Changeset: id, Commit: rc.Commits[i], Line: rc.Lines[i]}
		data, err := src.Record(ctx, id)
		if errors.Is(err, view.ErrNotItsID) {
			// not present to a reader, and still under the ID in the store
			lost.Wrong = true
			return nil, lost
		}
		if err != nil {
			lost.Err = err
			return nil, lost
		}
		if got := c4.Identify(bytes.NewReader(data)); got != id {
			lost.Wrong = true
			return nil, lost
		}
		one, err := view.ReadChain(data)
		if err != nil {
			// The bytes are the ones the C4 ID names, so this importer, or
			// one before it, wrote them: the c4m library wrote what it does
			// not read. Since the round-trip guard nothing is stored that
			// does not read back; one written before it is read again by a
			// c4m library that reads it, or else is cut back past.
			return nil, fmt.Errorf("the stored changeset %s (mapping line %d) is the object its C4 ID names and does not read with this c4m library (%v). It was written before the importer checked that what it stores reads back; a c4m library that reads it makes the chain again with no edit, and until then what is left is to "+reimport,
				id, rc.Lines[i], err, rc.Lines[i])
		}
		if len(one.Sections) != 1 {
			return nil, fmt.Errorf("the stored object %s (mapping line %d) is not one changeset: it reads as %d", id, rc.Lines[i], len(one.Sections))
		}
		c.Sections = append(c.Sections, view.Section{Base: one.Base, Patch: one.Sections[0].Patch})
		seals[i] = one.Seal
	}
	if len(c.Sections) > 0 {
		views, err := c.Verify(ctx, src)
		if err != nil {
			return nil, err
		}
		for i, seal := range seals {
			if !seal.IsNil() && seal != views[i] {
				return nil, fmt.Errorf("the stored changeset %s (mapping line %d) ends with %s and produces %s", rc.Changesets[i], rc.Lines[i], seal, views[i])
			}
		}
		if views[len(views)-1] != rc.Head {
			return nil, fmt.Errorf("the mapping's changesets resolve to %s, and it records %s", views[len(views)-1], rc.Head)
		}
	}
	return c.Encode()
}

// tailIsWhole reports whether a chain file's bytes end where a write
// ended: with a newline, and, once the chain has sections, not with a bare
// C4 ID. A bare ID there is the first line of a changeset whose entries
// never arrived (or the last line of a file written before 2026-11-09,
// which is then written again in the form the format allows).
func tailIsWhole(data []byte, sections int) bool {
	if len(data) == 0 {
		return true
	}
	if data[len(data)-1] != '\n' {
		return false
	}
	if sections == 0 {
		return true
	}
	line := data[bytes.LastIndexByte(data[:len(data)-1], '\n')+1 : len(data)-1]
	_, err := c4.Parse(string(bytes.TrimSpace(line)))
	return err != nil
}

// check reads a chain file's bytes against the record, and says what is
// wrong with them; nothing when they are the recorded chain.
func (rc RecordedChain) check(ctx context.Context, p string, data []byte, recorded bool, src view.RecordSource) (ch *ChainFile, wrong string) {
	ch, c, err := openChain(ctx, p, data, src)
	switch {
	case err != nil:
		return nil, err.Error()
	case !recorded && ch.Empty():
		return ch, ""
	case !recorded:
		return ch, fmt.Sprintf("it holds a chain to %s, and the mapping records none for the ref", ch.Head)
	case !rc.holds(ch):
		wrong = fmt.Sprintf("it resolves to %s in %d sections from %s, and the mapping records %s in %d from %s", ch.Head, ch.Sections, ch.Base, rc.Head, len(rc.Changesets), rc.Base)
		if rc.MissingBefore != 0 {
			wrong += fmt.Sprintf(", and lines of the chain are missing from the mapping before line %d", rc.MissingBefore)
		}
		return ch, wrong
	case ch.Sections > 0:
		n, err := rc.differs(c)
		if err != nil {
			return ch, err.Error()
		}
		if n != 0 {
			return ch, fmt.Sprintf("its section %d is not the changeset %s the mapping records for it at line %d", n, rc.Changesets[n-1], rc.Lines[n-1])
		}
	}
	if !tailIsWhole(data, ch.Sections) {
		// The chain is right and the file does not end where a write
		// ended. Appended to as it is, the next changeset would run on
		// from a cut line.
		return ch, "its last line is cut, or is a changeset's first line with nothing after it"
	}
	return ch, ""
}

// OpenRecordedChain opens ref's chain file at p as the mapping m records
// it. The file is read and its head recomputed; if it cannot be read as a
// chain, or is not the chain the mapping records for the ref (its head,
// its base and its length), or does not end where a write ended, it is
// made again from the mapping's stored changesets, and what was wrong is
// returned in repaired. So a file whose last changeset was cut by a crash
// (which can still parse, as a chain to a view no commit made), one that
// is ahead of the mapping, and one that is behind it all come back as the
// chain the mapping records. With no chain recorded for the ref, a file
// that holds one is removed.
func OpenRecordedChain(ctx context.Context, p string, src view.RecordSource, m []Commit, ref string) (ch *ChainFile, repaired string, err error) {
	rc, recorded := ChainRecorded(m, ref)
	data, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	ch, repaired = rc.check(ctx, p, data, recorded, src)
	if repaired == "" {
		return ch, "", nil
	}
	if !recorded {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		return &ChainFile{Path: p}, repaired, nil
	}
	made, err := rc.bytes(ctx, src)
	if err != nil {
		return nil, "", fmt.Errorf("chain file %s is not the chain the mapping records (%s), and cannot be made again: %w", p, repaired, err)
	}
	if err := replaceFile(p, made); err != nil {
		return nil, "", err
	}
	return &ChainFile{Path: p, Base: rc.Base, Head: rc.Head, Sections: len(rc.Changesets)}, repaired, nil
}

// RecordedChainBytes returns ref's chain for a reader: the file at p when
// it is the chain the mapping records, and otherwise that chain made from
// the stored changesets, without writing anything. Nil when there is none.
// The file is read once, so what is served is what was checked.
func RecordedChainBytes(ctx context.Context, p string, src view.RecordSource, m []Commit, ref string) ([]byte, error) {
	rc, recorded := ChainRecorded(m, ref)
	if !recorded {
		return nil, nil
	}
	data, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, wrong := rc.check(ctx, p, data, recorded, src); wrong == "" {
		c, err := view.ReadChain(data)
		if err != nil {
			return nil, err
		}
		return c.Encode()
	}
	return rc.bytes(ctx, src)
}
