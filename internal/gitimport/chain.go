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
	flags := os.O_APPEND | os.O_CREATE | os.O_WRONLY
	if ch.Sections == 0 {
		flags = os.O_TRUNC | os.O_CREATE | os.O_WRONLY
	}
	f, err := os.OpenFile(ch.Path, flags, 0o644)
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
	if err := os.WriteFile(ch.Path, alone, 0o644); err != nil {
		return err
	}
	ch.Base, ch.Head, ch.Sections = base, base, 0
	return nil
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
