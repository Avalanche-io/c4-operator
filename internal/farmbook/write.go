package farmbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Submission is one entry to write for the seal: the entry, pending, and
// the record it cites, when the writer has it to store.
type Submission struct {
	Entry  any
	Record []byte
}

// Submit writes entries for the seal, moving the State from st: each
// record goes to the store first, then each entry, then the State lists
// the entries pending, so the State never names what the store lacks. It
// fails with ErrMoved when another writer or the sealer moved the State
// first; the caller reads it again, judges again, and submits again. It
// returns the entries' IDs, in order.
func Submit(ctx context.Context, store Store, ptr Pointer, st State, subs []Submission) ([]string, error) {
	if len(st.Pending)+len(subs) > MaxPending {
		return nil, fmt.Errorf("the farm book has %d entries waiting for the seal: is the queue running?", len(st.Pending))
	}
	var ids []string
	for _, s := range subs {
		if s.Record != nil {
			if err := store.Put(ctx, c4id(s.Record), s.Record); err != nil {
				return nil, err
			}
		}
		raw, err := json.Marshal(s.Entry)
		if err != nil {
			return nil, err
		}
		e, err := ParseEntry(raw)
		if err != nil {
			return nil, err
		}
		if e.Order != OrderPending || e.Position != 0 || e.Prev != "" {
			return nil, fmt.Errorf("an entry is written pending, with no position: this one is in order %q at %d", e.Order, e.Position)
		}
		b, id, err := canonical(json.RawMessage(raw))
		if err != nil {
			return nil, err
		}
		if err := store.Put(ctx, id, b); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	next := State{Head: st.Head, Pending: append(append([]string(nil), st.Pending...), ids...)}
	if err := ptr.Write(ctx, st, next); err != nil {
		return nil, err
	}
	return ids, nil
}

// Writer appends entries for a writer that judges nothing against the
// book first, as the HUD books its control records.
type Writer struct {
	Store   Store
	Pointer Pointer
}

// Append writes entries for the seal, reading the State again when
// another writer moved it first.
func (w *Writer) Append(ctx context.Context, subs ...Submission) ([]string, error) {
	for try := 0; ; try++ {
		st, err := w.Pointer.Read(ctx)
		if err != nil {
			return nil, err
		}
		ids, err := Submit(ctx, w.Store, w.Pointer, st, subs)
		if errors.Is(err, ErrMoved) && try < 8 {
			continue
		}
		return ids, err
	}
}
