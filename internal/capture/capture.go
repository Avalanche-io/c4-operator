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
//
// A capture never changes the directory it reads. Each new file is first
// copied into the node cache (a clone where the filesystem has one), and
// it is the copy that is hashed and stored: the ID names exactly the
// bytes kept, whatever is written to the directory meanwhile or
// afterwards, and the directory's files keep their modes and dates. A
// workspace that stays in use after a capture depends on this.
//
// Input.Consume gives that up for speed, for an upper that is dead: the
// overlay has been unmounted (not merely detached from a holder that can
// still write) and the directory is removed once its capture succeeds. Its
// files are then hardlinked into the cache rather than copied.
//
// What each costs. Without Consume, where the filesystem has no clones,
// every candidate file is copied before it is hashed, so a capture needs
// that much free space, and the copies sit in the cache's tmp directory
// until the capture ends: the cache must have one owner, since opening it
// clears tmp. With Consume, each captured executable is stored twice, as
// the linked executable variant and as a copied plain object.
//
// A file that is gone, or is no longer a regular file, by the time it is
// copied fails the capture; so does a file that cannot be read. Nothing is
// stored for a capture that fails, and running it again is safe.
package capture

import (
	"context"
	"fmt"
	"os"
	"strings"
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
	// Progress, if set, is told how many of the new objects' bytes have
	// reached the store, as they do.
	Progress func(done, of int64)
	// Walk reads Upper into its changes. Each kind of workspace keeps its
	// writes in its own form (overlayfs, a union's .wh. markers, a clone
	// compared with its base); all of them come back as the same entries,
	// so the changeset never depends on the form. Nil means overlay.Walk.
	Walk func(upper string) ([]overlay.Entry, error)
	// Consume says Upper is dead: nothing will read or write it again, and
	// it is removed once its capture succeeds. Its new files are then
	// hardlinked into the cache, not copied, and so take the cache's mode
	// and date. Leave it false for a workspace that stays in use: a link
	// would make the workspace's file and the cache's object one inode,
	// and the next write to the file would change the object under its ID.
	Consume bool
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
// the same upper gives the same result and writes nothing new, including
// after a run that failed part way. Unless Input.Consume is set it leaves
// the upper exactly as it found it.
func Run(ctx context.Context, in Input) (*Result, error) {
	start := time.Now()
	res := &Result{Output: in.Parent}
	walk := in.Walk
	if walk == nil {
		walk = overlay.Walk
	}
	entries, err := walk(in.Upper)
	if err != nil {
		return nil, fmt.Errorf("walk upper: %w", err)
	}

	var edits []view.Edit
	var files []overlay.Entry
	for _, e := range entries {
		if IsControl(e.Path) {
			continue
		}
		switch e.Kind {
		case overlay.Whiteout:
			edits = append(edits, view.Edit{Path: e.Path})
		case overlay.Dir:
			_, name := view.SplitPath(e.Path)
			edits = append(edits, view.Edit{Path: e.Path, Entry: &c4m.Entry{Name: name}, Opaque: e.Opaque})
		case overlay.Symlink:
			edits = append(edits, view.Edit{Path: e.Path, Entry: view.SymlinkEntry("", e.Target)})
		case overlay.File:
			if e.Full == "" && !e.ID.IsNil() {
				// Metadata only: the bytes are the parent's, already stored.
				edits = append(edits, view.Edit{Path: e.Path, Entry: view.FileEntry("", e.Size, e.ID, e.Exec)})
				continue
			}
			files = append(files, e)
		default:
			res.Unusual = append(res.Unusual, e.Path+": "+e.Unusual)
		}
	}

	var handed sync.Map // snapshots given to the cache
	if !in.Consume {
		// The upper stays in use: work from private copies, so what is
		// hashed is what is stored, and the upper is never touched.
		snaps, err := snapshotAll(in.Cache, files, in.Workers)
		defer func() {
			// Remove the copies nothing took: files that turned out
			// unchanged, and all but one of several with one content. A
			// copy handed to the cache is the cache's, and its name may
			// already belong to someone else.
			for _, s := range snaps {
				if _, taken := handed.Load(s); !taken {
					os.Remove(s)
				}
			}
		}()
		if err != nil {
			return nil, err
		}
	}
	ids, err := hashAll(files, in.Workers)
	if err != nil {
		return nil, err
	}
	paths := make(map[c4.ID]string, len(files))
	execs := make(map[c4.ID]bool, len(files))
	for i, f := range files {
		edits = append(edits, view.Edit{Path: f.Path, Entry: view.FileEntry("", f.Size, ids[i], f.Exec)})
		paths[ids[i]] = f.Full
		execs[ids[i]] = f.Exec
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

	// New objects: keep them in the node cache (so later stages on this
	// node read them without a fetch) and write them to the store.
	changed := map[c4.ID]bool{}
	written := 0 // changed files whose bytes are in the upper
	for _, f := range applied.Files {
		if _, ok := paths[f.ID]; ok {
			changed[f.ID] = true
			written++
		}
	}
	res.Reused = len(files) - written
	if err := upload(ctx, in, changed, paths, execs, &handed, res); err != nil {
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

// ControlDir is where a unit talks to the farm while it runs (its
// progress line); it is never part of the unit's output.
const ControlDir = ".c4/"

// IsControl reports whether an upper path is in the control directory at
// the volume's root.
func IsControl(p string) bool {
	return p == ControlDir || p == ".c4" || strings.HasPrefix(p, ControlDir)
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
				if err == nil && id.IsNil() {
					// A read that failed part way hashes to no ID at all;
					// it must never be stored or named as one.
					err = fmt.Errorf("hash %s: the file could not be read", files[i].Path)
				}
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

// snapshotAll replaces each file's Full with a private copy in the cache's
// tmp directory, and its Size with the copy's. It returns the copies made,
// for the caller to remove, also when it fails.
func snapshotAll(cache *objcache.Cache, files []overlay.Entry, workers int) ([]string, error) {
	if workers <= 0 {
		workers = 4
	}
	snaps := make([]string, len(files))
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
				s, err := cache.Snapshot(files[i].Full)
				var st os.FileInfo
				if err == nil {
					snaps[i] = s
					st, err = os.Stat(s)
				}
				if err != nil {
					mu.Lock()
					if first == nil {
						first = fmt.Errorf("copy %s: %w", files[i].Path, err)
					}
					mu.Unlock()
					continue
				}
				files[i].Full, files[i].Size = s, st.Size()
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	made := snaps[:0]
	for _, s := range snaps {
		if s != "" {
			made = append(made, s)
		}
	}
	return made, first
}

func upload(ctx context.Context, in Input, changed map[c4.ID]bool, paths map[c4.ID]string, execs map[c4.ID]bool, handed *sync.Map, res *Result) error {
	workers := in.Workers
	if workers <= 0 {
		workers = 4
	}
	ch := make(chan c4.ID)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		sent  int64
		of    int64
	)
	sizes := map[c4.ID]int64{}
	for _, f := range res.Files {
		if changed[f.ID] {
			sizes[f.ID] = f.Size
		}
	}
	for _, n := range sizes {
		of += n
	}
	if in.Progress != nil {
		in.Progress(0, of)
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range ch {
				var err error
				if in.Consume {
					err = in.Cache.Adopt(paths[id], id, execs[id])
				} else {
					// paths[id] is this capture's own copy; Commit takes it,
					// and removes it if it fails.
					handed.Store(paths[id], true)
					err = in.Cache.Commit(paths[id], id)
				}
				if err == nil {
					err = in.Sink.PutFile(ctx, in.Project, id, in.Cache.Path(id))
				}
				mu.Lock()
				if err != nil && first == nil {
					first = fmt.Errorf("store %s: %w", id, err)
				}
				if err == nil {
					res.Uploaded++
					sent += sizes[id]
				}
				done := sent
				mu.Unlock()
				if err == nil && in.Progress != nil {
					in.Progress(done, of)
				}
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
