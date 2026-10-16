package gitimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// Sink is where an import writes objects: the project store.
type Sink interface {
	// Put stores size bytes from r under id. The importer computed id from
	// those same bytes, so Put does not rehash. It reports false, without
	// consuming r, when the store already holds id.
	Put(ctx context.Context, id c4.ID, size int64, r io.Reader) (stored bool, err error)
	// Copy copies the object id from src, trusting src's binding of id to
	// its bytes (nothing is rehashed). It reports false when the store
	// already holds id.
	Copy(ctx context.Context, id c4.ID, size int64, src Source) (stored bool, err error)
}

// Source is a store objects are copied from, such as a c4git store.
type Source interface {
	// Stat returns the size of id, or an error wrapping os.ErrNotExist.
	Stat(id c4.ID) (int64, error)
	Open(id c4.ID) (io.ReadCloser, error)
}

// TreeSource reads a c4 TreeStore, the format of c4git's .c4/store.
type TreeSource struct{ TS *store.TreeStore }

// OpenTreeSource opens the TreeStore at dir.
func OpenTreeSource(dir string) (*TreeSource, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	ts, err := store.NewTreeStore(dir)
	if err != nil {
		return nil, err
	}
	return &TreeSource{TS: ts}, nil
}

// Stat returns the stored size of id.
func (s *TreeSource) Stat(id c4.ID) (int64, error) {
	p, ok := s.TS.ContentPath(id)
	if !ok {
		return 0, fmt.Errorf("%s: %w", id, os.ErrNotExist)
	}
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Open opens id.
func (s *TreeSource) Open(id c4.ID) (io.ReadCloser, error) { return s.TS.Open(id) }

// TreeSink writes to a local c4 TreeStore.
type TreeSink struct{ TS *store.TreeStore }

// Put implements Sink.
func (s *TreeSink) Put(_ context.Context, id c4.ID, _ int64, r io.Reader) (bool, error) {
	if s.TS.Has(id) {
		return false, nil
	}
	return true, writeTree(s.TS, id, r)
}

// Copy implements Sink.
func (s *TreeSink) Copy(_ context.Context, id c4.ID, _ int64, src Source) (bool, error) {
	if s.TS.Has(id) {
		return false, nil
	}
	rc, err := src.Open(id)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	return true, writeTree(s.TS, id, rc)
}

// Open reads an object back (view.Store), for records.
func (s *TreeSink) Open(id c4.ID) (io.ReadCloser, error) { return s.TS.Open(id) }

func writeTree(ts *store.TreeStore, id c4.ID, r io.Reader) error {
	w, err := ts.Create(id)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// ShardedSink writes to the project's sharded authoritative store. Each
// object goes to its rendezvous shard, after a Head that skips objects the
// shard already holds.
type ShardedSink struct{ S *storeurl.Sharded }

// Put implements Sink.
func (s *ShardedSink) Put(ctx context.Context, id c4.ID, size int64, r io.Reader) (bool, error) {
	present, err := s.has(ctx, id)
	if err != nil || present {
		return false, err
	}
	return true, s.S.Put(ctx, id, r, size)
}

// Copy implements Sink.
func (s *ShardedSink) Copy(ctx context.Context, id c4.ID, size int64, src Source) (bool, error) {
	present, err := s.has(ctx, id)
	if err != nil || present {
		return false, err
	}
	rc, err := src.Open(id)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	return true, s.S.Put(ctx, id, rc, size)
}

// Open reads an object back (view.Store), for records.
func (s *ShardedSink) Open(id c4.ID) (io.ReadCloser, error) { return s.S.Open(id) }

func (s *ShardedSink) has(ctx context.Context, id c4.ID) (bool, error) {
	_, err := s.S.Head(ctx, id)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, storeurl.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// Sizer is implemented by sinks that can report an object's size. The
// importer uses it for a c4git object the c4git store lacks but the
// project store already holds.
type Sizer interface {
	Size(ctx context.Context, id c4.ID) (size int64, present bool, err error)
}

// Size implements Sizer.
func (s *TreeSink) Size(_ context.Context, id c4.ID) (int64, bool, error) {
	p, ok := s.TS.ContentPath(id)
	if !ok {
		return 0, false, nil
	}
	st, err := os.Stat(p)
	if err != nil {
		return 0, false, err
	}
	return st.Size(), true, nil
}

// Size implements Sizer.
func (s *ShardedSink) Size(ctx context.Context, id c4.ID) (int64, bool, error) {
	n, err := s.S.Head(ctx, id)
	if err == nil {
		return n, true, nil
	}
	if errors.Is(err, storeurl.ErrNotFound) {
		return 0, false, nil
	}
	return 0, false, err
}
