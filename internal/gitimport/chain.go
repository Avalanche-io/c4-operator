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
	ch := &ChainFile{Path: p}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return ch, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return ch, nil
	}
	c, err := view.ReadChain(data)
	if err != nil {
		return nil, fmt.Errorf("chain file %s: %w", p, err)
	}
	ch.Base, ch.Head, ch.Sections = c.Base, c.Base, len(c.Sections)
	if n := len(c.Sections); n > 0 {
		if src == nil {
			return nil, fmt.Errorf("chain file %s: its head is recomputed from the store's records, and there is no store to read", p)
		}
		last := c.Sections[n-1]
		res, err := view.ApplyPatch(ctx, last.Base, last.Patch, src)
		if err != nil {
			return nil, fmt.Errorf("chain file %s: %w", p, err)
		}
		ch.Head = res.ID
	}
	if !c.Seal.IsNil() && c.Seal != ch.Head {
		return nil, fmt.Errorf("chain file %s ends with %s and resolves to %s", p, c.Seal, ch.Head)
	}
	return ch, nil
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

// RecordedChain is what the mapping records of a ref's chain: the view it
// starts from, the changesets appended since, in order, and the view it
// resolves to.
type RecordedChain struct {
	Base, Head c4.ID
	Changesets []c4.ID
}

// ChainRecorded returns what the mapping m records of ref's chain, and
// false when it records none. The mapping is the authority for a chain:
// each commit's line carries its changeset's C4 ID, its chain's base and
// its place in it, and is written only after the changeset is in the
// store. The chain file is those changesets joined, kept beside the
// mapping so an append costs one changeset; it can always be made again
// from the mapping, and is never taken at its own word (OpenRecordedChain).
func ChainRecorded(m []Commit, ref string) (RecordedChain, bool) {
	var cur RecordedChain
	found := false
	for _, c := range m {
		if c.Ref != ref || c.RefOnly || c.Commit == "" || c.ChainBase.IsNil() {
			continue
		}
		// A commit that restarts the count, or names another base, is the
		// first of a new chain (a re-base after a seal, or a force push).
		restarted := !c.Changeset.IsNil() && c.ChainSeq <= len(cur.Changesets) || c.Changeset.IsNil() && c.ChainSeq < len(cur.Changesets)
		if !found || c.ChainBase != cur.Base || restarted {
			cur = RecordedChain{Base: c.ChainBase}
		}
		found = true
		if !c.Changeset.IsNil() {
			cur.Changesets = append(cur.Changesets, c.Changeset)
		}
		cur.Head = c.View
	}
	return cur, found
}

// holds reports whether an opened chain file is the one the mapping
// records: it resolves to the recorded head. Its base and length are not
// compared: the recorded chain sealed and started again from its head is
// the same chain to an appender, which needs only the head.
func (rc RecordedChain) holds(ch *ChainFile) bool { return ch.Head == rc.Head }

// bytes is the recorded chain as a chain file holds it, made from the
// stored changesets. Each is checked against its C4 ID and read on its own
// (one stored before 2026-11-09 ends with the view it produces, which is
// checked and not carried on), and the whole is checked from the recorded
// base to the recorded head. What comes back is the chain the mapping
// records, in the form the format allows, or an error.
func (rc RecordedChain) bytes(ctx context.Context, src view.RecordSource) ([]byte, error) {
	c := &view.Chain{Base: rc.Base}
	for i, id := range rc.Changesets {
		data, err := src.Record(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("changeset %d of %d (%s): %w", i+1, len(rc.Changesets), id, err)
		}
		if got := c4.Identify(bytes.NewReader(data)); got != id {
			return nil, fmt.Errorf("changeset %d of %d: the store holds a wrong object under %s", i+1, len(rc.Changesets), id)
		}
		one, err := view.ReadChain(data)
		if err != nil || len(one.Sections) != 1 {
			return nil, fmt.Errorf("changeset %d of %d (%s) is not one changeset: %v", i+1, len(rc.Changesets), id, err)
		}
		c.Sections = append(c.Sections, view.Section{Base: one.Base, Patch: one.Sections[0].Patch})
	}
	if len(c.Sections) > 0 {
		views, err := c.Verify(ctx, src)
		if err != nil {
			return nil, err
		}
		if views[len(views)-1] != rc.Head {
			return nil, fmt.Errorf("the mapping's changesets resolve to %s, and it records %s", views[len(views)-1], rc.Head)
		}
	}
	return c.Encode()
}

// OpenRecordedChain opens ref's chain file at p as the mapping m records
// it. The file is opened and its head recomputed; if it cannot be read as
// a chain, or does not resolve to the head the mapping records for the
// ref, it is made again from the mapping's stored changesets and what was
// wrong is returned in repaired. So a file whose last changeset was torn
// by a crash (which can still parse, as a chain to a view no commit made),
// one that is ahead of the mapping, and one that is behind it all come
// back as the chain the mapping records. With no chain recorded for the
// ref, a file that holds one is removed.
func OpenRecordedChain(ctx context.Context, p string, src view.RecordSource, m []Commit, ref string) (ch *ChainFile, repaired string, err error) {
	rc, recorded := ChainRecorded(m, ref)
	ch, openErr := OpenChainFile(ctx, p, src)
	switch {
	case openErr == nil && !recorded && ch.Empty():
		return ch, "", nil
	case openErr == nil && recorded && rc.holds(ch) && endsALine(p):
		return ch, "", nil
	case openErr == nil && recorded && rc.holds(ch):
		// The chain is right and the file is cut one byte short. Appended
		// to as it is, the next changeset's first line would run on from
		// the last C4 ID.
		repaired = "it does not end with a newline"
	case openErr != nil:
		repaired = openErr.Error()
	case !recorded:
		repaired = fmt.Sprintf("it holds a chain to %s, and the mapping records none for the ref", ch.Head)
	default:
		repaired = fmt.Sprintf("it resolves to %s in %d sections from %s, and the mapping records %s in %d from %s", ch.Head, ch.Sections, ch.Base, rc.Head, len(rc.Changesets), rc.Base)
	}
	if !recorded {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		return &ChainFile{Path: p}, repaired, nil
	}
	data, err := rc.bytes(ctx, src)
	if err != nil {
		return nil, "", fmt.Errorf("chain file %s is not the chain the mapping records (%s), and cannot be made again: %w", p, repaired, err)
	}
	if err := replaceFile(p, data); err != nil {
		return nil, "", err
	}
	return &ChainFile{Path: p, Base: rc.Base, Head: rc.Head, Sections: len(rc.Changesets)}, repaired, nil
}

// endsALine reports whether the file at p is empty or ends with a newline.
func endsALine(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err == nil
	}
	last := make([]byte, 1)
	_, err = f.ReadAt(last, st.Size()-1)
	return err == nil && last[0] == '\n'
}

// RecordedChainBytes returns ref's chain for a reader: the file at p when
// it is the chain the mapping records, and otherwise that chain made from
// the stored changesets, without writing anything. Nil when there is none.
func RecordedChainBytes(ctx context.Context, p string, src view.RecordSource, m []Commit, ref string) ([]byte, error) {
	rc, recorded := ChainRecorded(m, ref)
	if !recorded {
		return nil, nil
	}
	if ch, err := OpenChainFile(ctx, p, src); err == nil && rc.holds(ch) {
		return ReadChainFile(p)
	}
	return rc.bytes(ctx, src)
}
