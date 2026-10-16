package view

import (
	"context"
	"fmt"
	"sync"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// RecordSource returns a record's bytes by ID. On a node it is the tier
// client, so records come from the node cache, a peer, or the regional
// tier, like any object.
type RecordSource interface {
	Record(ctx context.Context, id c4.ID) ([]byte, error)
}

// RecordFunc adapts a function to RecordSource.
type RecordFunc func(ctx context.Context, id c4.ID) ([]byte, error)

// Record calls f.
func (f RecordFunc) Record(ctx context.Context, id c4.ID) ([]byte, error) { return f(ctx, id) }

// StoreRecords reads records from a Store.
func StoreRecords(st Store) RecordSource {
	return RecordFunc(func(_ context.Context, id c4.ID) ([]byte, error) { return ReadObject(st, id) })
}

// Load fetches a whole view top-down from its ID: the root record, then
// every level's subdirectory records in parallel. The recomputed root ID
// must equal id, so the tree's structure is checked end to end while file
// content is not rehashed.
func Load(ctx context.Context, id c4.ID, src RecordSource, workers int) (*View, error) {
	root := NewNode("")
	if err := loadInto(ctx, root, id, src, workers); err != nil {
		return nil, err
	}
	v := FromNode(root)
	if v.ID != id {
		return nil, fmt.Errorf("records of %s recompute to %s", id, v.ID)
	}
	return v, nil
}

// loadInto fills n from the record id and every record below it.
func loadInto(ctx context.Context, n *Node, id c4.ID, src RecordSource, workers int) error {
	if workers <= 0 {
		workers = 16
	}
	type job struct {
		n  *Node
		id c4.ID
	}
	level := []job{{n, id}}
	for len(level) > 0 {
		var (
			mu    sync.Mutex
			next  []job
			first error
			wg    sync.WaitGroup
			sem   = make(chan struct{}, workers)
		)
		for _, j := range level {
			if j.id == emptyID || j.id.IsNil() {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(j job) {
				defer wg.Done()
				defer func() { <-sem }()
				entries, err := fetchRecord(ctx, src, j.id)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if first == nil {
						first = err
					}
					return
				}
				for _, e := range entries {
					if !e.IsDir() {
						j.n.Files[e.Name] = e
						continue
					}
					child := NewNode(e.Name)
					j.n.Dirs[e.Name] = child
					next = append(next, job{child, e.C4ID})
				}
			}(j)
		}
		wg.Wait()
		if first != nil {
			return first
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		level = next
	}
	return nil
}

func fetchRecord(ctx context.Context, src RecordSource, id c4.ID) ([]*c4m.Entry, error) {
	data, err := src.Record(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("record %s: %w", id, err)
	}
	entries, err := ParseRecord(data)
	if err != nil {
		return nil, fmt.Errorf("record %s: %w", id, err)
	}
	return entries, nil
}
