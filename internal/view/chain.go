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

// A Chain is c4m's append-only patch chain: a base, then patch sections,
// each sealed by a bare C4 ID line that must equal the ID of the
// accumulated state above it. Every checkpoint is therefore a view ID, and
// every sealed prefix of a chain is a view that can be verified by applying
// the sections along changed paths. A changeset is the one-section case.
type Chain struct {
	// Base is the view the chain starts from (an external base on the
	// chain's first line).
	Base     c4.ID
	Sections []Section
}

// Section is one patch and the checkpoint that seals it. An unsealed
// trailing section (still being written) has a nil Checkpoint.
type Section struct {
	Patch      *c4m.Manifest
	Checkpoint c4.ID
}

// ErrUnsealed reports a section without a checkpoint.
var ErrUnsealed = errors.New("chain section is not sealed")

// ReadChain decodes a chain that starts from an external base: the form
// every changeset and every stream in this system has.
func ReadChain(data []byte) (*Chain, error) {
	lines := bytes.Split(data, []byte("\n"))
	ch := &Chain{}
	var cur []byte
	sawBase := false
	flush := func(seal c4.ID) error {
		if len(bytes.TrimSpace(cur)) == 0 {
			// Consecutive checkpoints (a segment boundary repeats the
			// previous checkpoint): the later one stands.
			if len(ch.Sections) > 0 && !seal.IsNil() && ch.Sections[len(ch.Sections)-1].Checkpoint != seal {
				return fmt.Errorf("checkpoint %s follows %s with no patch between them", seal, ch.Sections[len(ch.Sections)-1].Checkpoint)
			}
			return nil
		}
		m, err := c4m.NewDecoder(bytes.NewReader(cur)).Decode()
		if err != nil {
			return err
		}
		ch.Sections = append(ch.Sections, Section{Patch: m, Checkpoint: seal})
		cur = cur[:0]
		return nil
	}
	for _, line := range lines {
		t := strings.TrimSpace(string(line))
		if t == "" {
			continue
		}
		if len(t) == 90 && strings.HasPrefix(t, "c4") && !strings.ContainsAny(t, " \t") {
			id, err := c4.Parse(t)
			if err != nil {
				return nil, err
			}
			if !sawBase {
				ch.Base, sawBase = id, true
				continue
			}
			if err := flush(id); err != nil {
				return nil, err
			}
			continue
		}
		if !sawBase {
			return nil, errors.New("chain has no base: its first line must name a view")
		}
		cur = append(cur, line...)
		cur = append(cur, '\n')
	}
	if err := flush(c4.ID{}); err != nil {
		return nil, err
	}
	if !sawBase {
		return nil, errors.New("empty chain")
	}
	return ch, nil
}

// Head is the last sealed checkpoint, or the base.
func (ch *Chain) Head() c4.ID {
	for i := len(ch.Sections) - 1; i >= 0; i-- {
		if !ch.Sections[i].Checkpoint.IsNil() {
			return ch.Sections[i].Checkpoint
		}
	}
	return ch.Base
}

// Verify applies each sealed section to the view before it, along changed
// paths only, and checks that the result is the section's checkpoint. It
// returns the verified checkpoints in order. A subscriber can trust any
// prefix this accepts; an unsealed trailing section is ignored.
func (ch *Chain) Verify(ctx context.Context, src RecordSource) ([]c4.ID, error) {
	cur := ch.Base
	var out []c4.ID
	for i, sec := range ch.Sections {
		if sec.Checkpoint.IsNil() {
			if i == len(ch.Sections)-1 {
				break
			}
			return out, fmt.Errorf("section %d: %w", i+1, ErrUnsealed)
		}
		res, err := ApplyPatch(ctx, cur, sec.Patch, src)
		if err != nil {
			return out, fmt.Errorf("section %d: %w", i+1, err)
		}
		if res.ID != sec.Checkpoint {
			return out, fmt.Errorf("section %d: patch produces %s, checkpoint says %s", i+1, res.ID, sec.Checkpoint)
		}
		cur = sec.Checkpoint
		out = append(out, cur)
	}
	return out, nil
}

// AppendSection appends one sealed section (a changeset's body) to chain
// bytes. A changeset is "parent\npatch\ncheckpoint\n"; appending it to a
// chain whose head is parent drops the repeated parent line.
func AppendSection(chain, changeset []byte) ([]byte, error) {
	head, err := ReadChain(chain)
	if err != nil {
		return nil, err
	}
	cs, err := ReadChain(changeset)
	if err != nil {
		return nil, err
	}
	if cs.Base != head.Head() {
		return nil, fmt.Errorf("changeset applies to %s, the chain's head is %s", cs.Base, head.Head())
	}
	body := changeset[bytes.IndexByte(changeset, '\n')+1:]
	out := append(append([]byte(nil), chain...), body...)
	return out, nil
}

// ApplyPatch applies a c4m patch to the view base with c4m's patch
// semantics, matched by path: an entry identical to the base's, with no
// children under it, removes that path; a differing entry replaces it; a
// new path is added; a directory entry followed by children merges them.
// It reads only the base's records along the patch's paths.
func ApplyPatch(ctx context.Context, base c4.ID, patch *c4m.Manifest, src RecordSource) (*Applied, error) {
	t := &atree{ctx: ctx, src: src, root: &anode{base: base}}
	var stack []string
	entries := patch.Entries
	for i, e := range entries {
		if e.Depth > len(stack) {
			return nil, fmt.Errorf("malformed patch: %q at depth %d has no parent", e.Name, e.Depth)
		}
		stack = stack[:e.Depth]
		if err := CheckName(e.Name); err != nil {
			return nil, err
		}
		dir := strings.Join(stack, "")
		pe := Project(e)
		hasChildren := i+1 < len(entries) && entries[i+1].Depth > e.Depth
		parent, err := t.walkTo(splitDir(dir), true)
		if err != nil {
			return nil, err
		}
		name := pe.Name
		existing, had := parent.orig[name]
		if _, still := parent.files[name]; !still {
			if _, stillDir := parent.dirs[name]; !stillDir {
				had = false
			}
		}
		if had && !hasChildren && sameEntry(existing, pe) && existing.C4ID == pe.C4ID {
			// An exact restatement removes the path.
			delete(parent.files, name)
			delete(parent.dirs, name)
			if pe.IsDir() {
				stack = append(stack, name)
			}
			continue
		}
		if !pe.IsDir() {
			delete(parent.dirs, strings.TrimSuffix(name, "/")+"/")
			pe.Depth = 0
			parent.files[name] = pe
			continue
		}
		delete(parent.files, strings.TrimSuffix(name, "/"))
		child, ok := parent.dirs[name]
		switch {
		case !ok:
			// A new directory: by ID alone (expand from its record) or
			// with its children following.
			child = &anode{name: name, base: pe.C4ID, entry: pe}
			if hasChildren || pe.C4ID.IsNil() {
				child = &anode{name: name, loaded: true, files: map[string]*c4m.Entry{}, dirs: map[string]*anode{}}
			}
			parent.dirs[name] = child
		case !hasChildren && child.base != pe.C4ID:
			// Clobbered by ID alone: c4m keeps the existing children.
		}
		stack = append(stack, name)
	}
	res := &Applied{Parent: base}
	root := t.identify(t.root, &res.Records)
	res.ID = root.C4ID
	return res, nil
}

// ExportChain writes a chain with its base listing in full instead of an
// external base reference, so tools that read only the chain text (the c4
// CLI's log) see every version as a whole tree, and each version's ID is
// the checkpoint.
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
	var out bytes.Buffer
	out.Write(listing)
	out.WriteString(ch.Base.String() + "\n")
	out.Write(chain[bytes.IndexByte(chain, '\n')+1:])
	return out.Bytes(), nil
}
