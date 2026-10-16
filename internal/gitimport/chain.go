package gitimport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Avalanche-io/c4"
)

// ChainFile is one branch's chain (DESIGN.md sections 2.3 and 2.5): a base
// view ID on the first line, then each commit's patch section followed by
// its checkpoint, the commit's view ID. It is kept as a local file and
// appended per commit, so an append costs the size of the commit's
// changeset, not of the chain.
//
// The chain is stored as segments: each commit's changeset is a stored
// object whose first line is the previous checkpoint, so the changesets
// listed in the mapping concatenate into this chain. The whole chain is
// stored as one object only when it is sealed (re-based, or at the end of
// a one-shot import), so a growing chain never rewrites stored objects.
type ChainFile struct {
	Path     string
	Base     c4.ID
	Head     c4.ID // the last checkpoint, or Base
	Sections int
}

// OpenChainFile reads the chain at p, or returns an empty chain when p
// does not exist.
func OpenChainFile(p string) (*ChainFile, error) {
	ch := &ChainFile{Path: p}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return ch, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 16<<20)
	ids := 0
	for sc.Scan() {
		id, ok := bareID(sc.Text())
		if !ok {
			continue
		}
		if ids == 0 {
			ch.Base = id
		}
		ch.Head = id
		ids++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if ids > 0 {
		ch.Sections = ids - 1
	}
	return ch, nil
}

// bareID reports whether a line is a bare C4 ID: a chain's base or a
// checkpoint. Entry lines always carry mode, time and size fields first.
func bareID(line string) (c4.ID, bool) {
	t := strings.TrimSpace(line)
	if len(t) != 90 || !strings.HasPrefix(t, "c4") {
		return c4.ID{}, false
	}
	id, err := c4.Parse(t)
	if err != nil || id.String() != t {
		return c4.ID{}, false
	}
	return id, true
}

// Empty reports whether the chain has no base yet.
func (ch *ChainFile) Empty() bool { return ch.Base.IsNil() }

// Append adds one changeset, which must apply to the chain's head. The
// changeset's first line (its parent, equal to the head) is dropped, so
// the file holds each checkpoint once. An empty chain takes the changeset
// whole, with its parent as the base.
func (ch *ChainFile) Append(changeset []byte) error {
	nl := bytes.IndexByte(changeset, '\n')
	if nl < 0 {
		return errors.New("changeset has no parent line")
	}
	parent, ok := bareID(string(changeset[:nl]))
	if !ok {
		return errors.New("changeset does not start with its parent view ID")
	}
	end := bytes.TrimRight(changeset, "\n")
	last := end[bytes.LastIndexByte(end, '\n')+1:]
	checkpoint, ok := bareID(string(last))
	if !ok {
		return errors.New("changeset is not sealed by a checkpoint line")
	}
	body := changeset[nl+1:]
	if ch.Empty() {
		body = changeset
	} else if parent != ch.Head {
		return fmt.Errorf("changeset applies to %s, the chain's head is %s", parent, ch.Head)
	}
	f, err := os.OpenFile(ch.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if ch.Empty() {
		ch.Base = parent
	}
	ch.Head = checkpoint
	ch.Sections++
	return nil
}

// Rebase starts the chain over with base as its external base, the way a
// chain is compacted (DESIGN.md section 2.5) or a branch that no longer
// extends its chain (a force push) starts a new one.
func (ch *ChainFile) Rebase(base c4.ID) error {
	if err := os.WriteFile(ch.Path, []byte(base.String()+"\n"), 0o644); err != nil {
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
	return os.ReadFile(ch.Path)
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
