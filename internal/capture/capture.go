// Package capture turns what a container wrote into a changeset.
//
// After the overlay is unmounted, the upper directory holds exactly the
// container's changes: new and copied-up files, whiteouts for deletions,
// and opaque markers for replaced directories. Capture hashes each file
// (the verify-once point for new content), applies the changes to the
// parent view along changed paths only, and writes the new objects, the
// new directory records and the changeset c4m. A file that was copied up
// without changing (a metadata-only write) has the parent's ID and is not
// a change.
package capture

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	"github.com/Avalanche-io/c4-operator/internal/metrics"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/overlay"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Sink writes captured content to the project's store: through the
// regional tier (which recomputes every ID), or directly in Direct mode.
type Sink interface {
	PutFile(ctx context.Context, project string, id c4.ID, path string) error
	PutBytes(ctx context.Context, project string, id c4.ID, data []byte) error
}

// Input is one capture.
type Input struct {
	Project string
	Parent  c4.ID  // the view the volume mounted
	Upper   string // the volume's upper directory
	Cache   *objcache.Cache
	Records view.RecordSource
	Sink    Sink
	Workers int
}

// Result describes a capture.
type Result struct {
	Output    c4.ID // the output view; equal to Parent when nothing changed
	Changeset c4.ID // nil when nothing changed
	Added     int
	Modified  int
	Deleted   int
	Bytes     int64 // bytes of added and modified files
	Uploaded  int   // objects written to the sink
	Reused    int   // files the container rewrote without changing them
	Hashed    int64 // bytes hashed
	Unusual   []string
	Duration  time.Duration
	Records   int
	// Files are the added and modified regular files, for the task record.
	Files []view.ChangedFile
}

// Run captures one upper directory. It is idempotent: running it again on
// the same upper gives the same result and writes nothing new.
func Run(ctx context.Context, in Input) (*Result, error) {
	start := time.Now()
	res := &Result{Output: in.Parent}
	entries, err := overlay.Walk(in.Upper)
	if err != nil {
		return nil, fmt.Errorf("walk upper: %w", err)
	}

	var edits []view.Edit
	var files []overlay.Entry
	for _, e := range entries {
		switch e.Kind {
		case overlay.Whiteout:
			edits = append(edits, view.Edit{Path: e.Path})
		case overlay.Dir:
			_, name := view.SplitPath(e.Path)
			edits = append(edits, view.Edit{Path: e.Path, Entry: &c4m.Entry{Name: name}, Opaque: e.Opaque})
		case overlay.Symlink:
			edits = append(edits, view.Edit{Path: e.Path, Entry: view.SymlinkEntry("", e.Target)})
		case overlay.File:
			files = append(files, e)
		default:
			res.Unusual = append(res.Unusual, e.Path+": "+e.Unusual)
		}
	}

	ids, err := hashAll(files, in.Workers)
	if err != nil {
		return nil, err
	}
	paths := make(map[c4.ID]string, len(files))
	for i, f := range files {
		edits = append(edits, view.Edit{Path: f.Path, Entry: view.FileEntry("", f.Size, ids[i], f.Exec)})
		paths[ids[i]] = f.Full
		res.Hashed += f.Size
	}
	metrics.NodeHashedBytes.WithLabelValues("capture").Add(float64(res.Hashed))

	view.SortEdits(edits)
	applied, err := view.Apply(ctx, in.Parent, edits, in.Records)
	if err != nil {
		return nil, fmt.Errorf("apply changes: %w", err)
	}
	res.Duration = time.Since(start)
	if applied.ID == in.Parent {
		res.Reused = len(files)
		return res, nil
	}
	res.Output, res.Changeset = applied.ID, applied.ChangesetID
	res.Added, res.Modified, res.Deleted, res.Bytes = applied.Added, applied.Modified, applied.Deleted, applied.Bytes
	res.Records = len(applied.Records)
	res.Files = applied.Files

	// New objects: keep a copy in the node cache (a hardlink from the
	// upper, so later stages on this node read them without a fetch) and
	// write them to the store.
	changed := map[c4.ID]bool{}
	for _, f := range applied.Files {
		changed[f.ID] = true
	}
	res.Reused = len(files) - len(applied.Files)
	if err := upload(ctx, in, changed, paths, res); err != nil {
		return nil, err
	}
	// Records, then the changeset: a reader that finds the changeset finds
	// everything it names.
	for _, r := range applied.Records {
		if err := in.Cache.Put(r.ID, r.Data); err != nil {
			return nil, err
		}
		if err := in.Sink.PutBytes(ctx, in.Project, r.ID, r.Data); err != nil {
			return nil, fmt.Errorf("store record: %w", err)
		}
	}
	if err := in.Cache.Put(applied.ChangesetID, applied.Changeset); err != nil {
		return nil, err
	}
	if err := in.Sink.PutBytes(ctx, in.Project, applied.ChangesetID, applied.Changeset); err != nil {
		return nil, fmt.Errorf("store changeset: %w", err)
	}
	res.Duration = time.Since(start)
	return res, nil
}

func hashAll(files []overlay.Entry, workers int) ([]c4.ID, error) {
	if workers <= 0 {
		workers = 4
	}
	ids := make([]c4.ID, len(files))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		next  = make(chan int)
	)
	for range min(workers, max(len(files), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				id, _, err := view.HashFile(files[i].Full)
				if err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
					continue
				}
				ids[i] = id
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	return ids, first
}

func upload(ctx context.Context, in Input, changed map[c4.ID]bool, paths map[c4.ID]string, res *Result) error {
	workers := in.Workers
	if workers <= 0 {
		workers = 4
	}
	ch := make(chan c4.ID)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range ch {
				err := in.Cache.Adopt(paths[id], id)
				if err == nil {
					err = in.Sink.PutFile(ctx, in.Project, id, in.Cache.Path(id))
				}
				mu.Lock()
				if err != nil && first == nil {
					first = fmt.Errorf("store %s: %w", id, err)
				}
				if err == nil {
					res.Uploaded++
				}
				mu.Unlock()
			}
		}()
	}
	for id := range changed {
		ch <- id
	}
	close(ch)
	wg.Wait()
	return first
}

// AnnotationValues renders a result as the values of the pod annotations
// output-tree, output-changeset and output-changes.
func (r *Result) AnnotationValues() (tree, changeset, changes string) {
	if r.Changeset.IsNil() {
		return r.Output.String(), "", "added=0 modified=0 deleted=0 bytes=0"
	}
	return r.Output.String(), r.Changeset.String(),
		fmt.Sprintf("added=%d modified=%d deleted=%d bytes=%d", r.Added, r.Modified, r.Deleted, r.Bytes)
}
