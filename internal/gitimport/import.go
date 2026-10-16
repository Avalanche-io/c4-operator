// Package gitimport imports a git repository's first-parent history as
// views: each commit becomes a view, and its diff against its first parent
// becomes a changeset over the parent's view (DESIGN.md sections 2 and
// 8.2).
//
// The importer reads git through the git CLI: rev-list for the history,
// ls-tree for the one commit imported in full, one long-running diff-tree
// for every later commit, and one long-running cat-file for blobs. Each
// commit is applied to its parent's view with view.Apply, which reads and
// writes records only along changed paths, so the cost of a commit is
// proportional to its change, not to the size of the repository.
//
// Blob content is hashed once, here, which is the verify-once point for
// source files (DESIGN.md section 1.1). Git blob IDs are remembered with
// their C4 IDs, so a blob is never read twice.
//
// Files managed by c4git are stored in git as their bare C4 ID. For those
// the importer uses the ID as is, takes the size from the c4git store, and
// copies the object into the project store without rehashing it. That is a
// trust boundary: c4git computed the ID when it stored the content at `git
// add`, and the importer accepts the c4git store's binding of ID to bytes.
// A c4git store that holds wrong bytes under an ID passes them on.
// Whoever can write the c4git store is therefore trusted like an ingest
// point. Bytes that do not come from such a store are always hashed.
package gitimport

import (
	"bytes"
	"context"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Options configures an import.
type Options struct {
	// Repo is the repository directory (a work tree or a bare repository).
	Repo string
	// Rev is the revision whose first-parent history is imported; HEAD by
	// default.
	Rev string
	// MaxCommits keeps only the newest N commits when positive.
	MaxCommits int
	// Since imports only commits after this one.
	Since string

	// Sink receives file objects, records and changesets.
	Sink Sink
	// Records reads records written by an earlier import, so a run can
	// continue from a commit in Known. It may be nil for a fresh import.
	Records view.RecordSource
	// Known maps commits imported earlier to their view IDs. They are
	// skipped, and a later commit's parent view is taken from here.
	Known map[string]c4.ID

	// C4Git is the c4git store. Nil means none: pointer files that
	// cannot be resolved are reported missing.
	C4Git Source
	// Filter is the gitattributes filter driver c4git uses; "c4" by
	// default.
	Filter string

	// Progress is called after each commit.
	Progress func(Commit)
	// Warn receives warnings (skipped submodules, missing c4git objects).
	Warn func(string)
}

// Commit is what one commit's import did. It is also one line of the
// mapping file.
type Commit struct {
	Commit string `json:"commit"`
	Parent string `json:"parent,omitempty"`
	// Base marks a commit imported in full from ls-tree because it is the
	// parent of the imported range (or a root commit). Its changeset is
	// relative to the empty view.
	Base       bool  `json:"base,omitempty"`
	View       c4.ID `json:"view"`
	ParentView c4.ID `json:"parentView"`
	// Changeset is the ID of the stored changeset (ParentView on its first
	// line). Nil when the commit changes nothing the view records.
	Changeset c4.ID `json:"changeset,omitempty"`

	// Git-level counts of files and symlinks (submodules excluded).
	Added    int `json:"added"`
	Modified int `json:"modified"`
	Deleted  int `json:"deleted"`
	Files    int `json:"files"` // files and symlinks in the view

	ObjectsUploaded int   `json:"objectsUploaded"`
	BytesUploaded   int64 `json:"bytesUploaded"`
	ObjectsPresent  int   `json:"objectsPresent"`
	BytesPresent    int64 `json:"bytesPresent"`
	// C4GitObjects counts distinct c4git objects this commit's changes
	// referenced, each used by ID without rehashing.
	C4GitObjects int `json:"c4gitObjects"`
	// C4GitMissing lists c4git-managed paths whose object is in neither
	// the c4git store nor the project store.
	C4GitMissing []string `json:"c4gitMissing,omitempty"`
	Submodules   int      `json:"submodules,omitempty"`

	BlobsRead      int `json:"blobsRead"`      // git blobs read through cat-file
	BlobsHashed    int `json:"blobsHashed"`    // of those, hashed as content
	RecordsRead    int `json:"recordsRead"`    // parent records Apply read
	RecordsWritten int `json:"recordsWritten"` // new records of this view

	Millis float64 `json:"ms"`
}

// Summary is the result of an import.
type Summary struct {
	Commits    []Commit
	Skipped    int // commits already in Known
	Submodules int // submodule entries skipped
	Missing    int // c4git-managed paths without content
	Elapsed    time.Duration
}

// blobInfo is what the importer knows about one git blob.
type blobInfo struct {
	size int64 // git blob size
	id   c4.ID // C4 ID of the blob's bytes
	data []byte
	// ptr is the C4 ID the blob names, when the blob is a c4git pointer
	// (exactly a C4 ID, optionally followed by a newline).
	ptr     c4.ID
	present bool // the blob's own bytes are in the sink
}

// smallBlob bounds the blobs whose bytes are kept (pointers, symlink
// targets).
const smallBlob = 256

// spoolAbove is the blob size above which content is spooled to a
// temporary file instead of memory between hashing and upload.
const spoolAbove = 32 << 20

type importer struct {
	ctx  context.Context
	opts Options
	cat  *catFile

	blobs map[string]*blobInfo
	// c4git objects known to be in the sink, with their sizes.
	c4git map[c4.ID]int64
	// managed caches the filter attribute by path. Any commit that touches
	// a .gitattributes file clears it.
	managed map[string]bool
	records *recordCache

	cur *Commit
	sum *Summary
}

// Run imports opts.Rev's first-parent history.
func Run(ctx context.Context, opts Options) (*Summary, error) {
	start := time.Now()
	if opts.Rev == "" {
		opts.Rev = "HEAD"
	}
	if opts.Filter == "" {
		opts.Filter = "c4"
	}
	if opts.Sink == nil {
		return nil, errors.New("no sink")
	}
	if opts.Warn == nil {
		opts.Warn = func(string) {}
	}
	history, err := firstParentHistory(ctx, opts.Repo, opts.Rev, opts.Since, opts.MaxCommits)
	if err != nil {
		return nil, err
	}
	sum := &Summary{}
	// Commits imported earlier form a prefix of the history.
	var prev c4.ID
	havePrev := false
	for len(history) > 0 {
		id, ok := opts.Known[history[0].sha]
		if !ok {
			break
		}
		prev, havePrev = id, true
		history = history[1:]
		sum.Skipped++
	}
	if len(history) == 0 {
		sum.Elapsed = time.Since(start)
		return sum, nil
	}

	cat, err := startCatFile(ctx, opts.Repo)
	if err != nil {
		return nil, err
	}
	defer cat.close()
	im := &importer{
		ctx: ctx, opts: opts, cat: cat, sum: sum,
		blobs:   map[string]*blobInfo{},
		c4git:   map[c4.ID]int64{},
		managed: map[string]bool{},
		records: newRecordCache(opts.Records),
	}

	// The view the first imported commit applies to.
	first := history[0]
	if !havePrev && first.parent != "" {
		prev, havePrev = opts.Known[first.parent]
	}
	if !havePrev {
		base := commit{sha: first.parent}
		if first.parent == "" {
			base = first
			history = history[1:]
		}
		c, err := im.importBase(base)
		if err != nil {
			return nil, err
		}
		prev = c.View
	}

	if len(history) > 0 {
		if prev, err = im.importDiffs(history, prev); err != nil {
			return nil, err
		}
	}
	sum.Elapsed = time.Since(start)
	return sum, nil
}

// importBase imports one commit in full from ls-tree, over the empty view.
func (im *importer) importBase(c commit) (*Commit, error) {
	start := time.Now()
	im.cur = &Commit{Commit: c.sha, Parent: c.parent, Base: true, ParentView: view.EmptyID()}
	list, err := lsTree(im.ctx, im.opts.Repo, c.sha)
	if err != nil {
		return nil, err
	}
	var puts []treeEntry
	for _, e := range list {
		if e.mode == modeGitlink {
			im.submodule(e.path)
			continue
		}
		puts = append(puts, e)
	}
	edits, err := im.entries(c.sha, puts)
	if err != nil {
		return nil, err
	}
	im.cur.Added = len(puts)
	im.cur.Files = len(puts)
	return im.finish(view.EmptyID(), edits, start)
}

// importDiffs imports commits whose parent's view is known, one diff-tree
// process for all of them.
func (im *importer) importDiffs(history []commit, prev c4.ID) (c4.ID, error) {
	d, err := startDiffer(im.ctx, im.opts.Repo)
	if err != nil {
		return prev, err
	}
	qerr := make(chan error, 1)
	go func() {
		defer d.closeInput()
		for _, c := range history {
			if err := d.queue(c); err != nil {
				qerr <- err
				return
			}
		}
		qerr <- nil
	}()
	ok := false
	defer func() {
		if !ok {
			d.cmd.Process.Kill()
			d.cmd.Wait()
		}
	}()
	files := -1
	for _, c := range history {
		start := time.Now()
		changes, err := d.next(c.sha)
		if err != nil {
			return prev, err
		}
		if files < 0 {
			files, err = im.countFiles(c.parent)
			if err != nil {
				return prev, err
			}
		}
		im.cur = &Commit{Commit: c.sha, Parent: c.parent, ParentView: prev}
		edits, err := im.changes(c.sha, changes)
		if err != nil {
			return prev, err
		}
		files += im.cur.Added - im.cur.Deleted
		im.cur.Files = files
		res, err := im.finish(prev, edits, start)
		if err != nil {
			return prev, err
		}
		prev = res.View
	}
	if err := <-qerr; err != nil {
		return prev, err
	}
	ok = true
	return prev, d.wait()
}

// countFiles counts the files and symlinks of a commit, for the running
// file count of a continued import. It reads the tree, not the blobs.
func (im *importer) countFiles(rev string) (int, error) {
	if len(im.sum.Commits) > 0 {
		return im.sum.Commits[len(im.sum.Commits)-1].Files, nil
	}
	list, err := lsTree(im.ctx, im.opts.Repo, rev)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range list {
		if e.mode != modeGitlink {
			n++
		}
	}
	return n, nil
}

func (im *importer) submodule(p string) {
	im.cur.Submodules++
	im.sum.Submodules++
	im.opts.Warn(fmt.Sprintf("%s: submodule %s skipped", short(im.cur.Commit), p))
}

// changes turns one commit's diff-tree records into view edits.
func (im *importer) changes(rev string, changes []change) ([]view.Edit, error) {
	var edits []view.Edit
	var puts []treeEntry
	var attrDirs []string
	for _, ch := range changes {
		if path.Base(ch.new.path) == ".gitattributes" {
			clear(im.managed)
			dir, _ := view.SplitPath(ch.new.path)
			attrDirs = append(attrDirs, dir)
		}
		oldIsItem := ch.old.mode != modeNone && ch.old.mode != modeTree && ch.old.mode != modeGitlink
		newIsItem := ch.new.mode != modeNone && ch.new.mode != modeTree && ch.new.mode != modeGitlink
		switch {
		case ch.status == 'D' && ch.old.mode == modeTree:
			edits = append(edits, view.Edit{Path: ch.old.path + "/"})
		case ch.new.mode == modeTree || ch.old.mode == modeTree:
			// A tree added or changed: implied by the blobs under it.
		case newIsItem:
			puts = append(puts, ch.new)
			if oldIsItem {
				im.cur.Modified++
			} else {
				im.cur.Added++
			}
		case oldIsItem:
			edits = append(edits, view.Edit{Path: ch.old.path})
			im.cur.Deleted++
		}
		if ch.new.mode == modeGitlink {
			im.submodule(ch.new.path)
		}
	}
	if len(attrDirs) > 0 {
		more, err := im.pointerFiles(rev, attrDirs, puts)
		if err != nil {
			return nil, err
		}
		puts = append(puts, more...)
	}
	put, err := im.entries(rev, puts)
	if err != nil {
		return nil, err
	}
	return append(edits, put...), nil
}

// pointerFiles lists the pointer-sized files under dirs that the commit
// did not change. A changed .gitattributes can turn c4git management on or
// off for files whose blobs did not change, which changes what a checkout
// holds, so those files are put again and their meaning re-evaluated. Only
// blobs of exactly pointer size are candidates, and Apply drops puts that
// change nothing.
func (im *importer) pointerFiles(rev string, dirs []string, changed []treeEntry) ([]treeEntry, error) {
	for _, d := range dirs {
		if d == "" {
			dirs = nil // the top-level .gitattributes governs everything
			break
		}
	}
	list, err := lsTree(im.ctx, im.opts.Repo, rev, dirs...)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(changed))
	for _, e := range changed {
		seen[e.path] = true
	}
	var out []treeEntry
	for _, e := range list {
		if (e.mode != modeFile && e.mode != modeExec) || (e.size != 90 && e.size != 91) || seen[e.path] {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// entries returns the edits that put each file or symlink in place,
// reading, hashing and uploading blobs not seen before.
func (im *importer) entries(rev string, list []treeEntry) ([]view.Edit, error) {
	// Read every new blob once.
	for _, e := range list {
		if err := im.read(e); err != nil {
			return nil, err
		}
	}
	// Which pointer-shaped files are c4git-managed, as of this commit.
	var ask []string
	for _, e := range list {
		b := im.blobs[e.sha]
		if e.mode == modeLink || b.ptr.IsNil() {
			continue
		}
		if _, ok := im.managed[e.path]; !ok {
			ask = append(ask, e.path)
		}
	}
	if len(ask) > 0 {
		attrs, err := checkAttrFilter(im.ctx, im.opts.Repo, rev, ask)
		if err != nil {
			return nil, err
		}
		for p, v := range attrs {
			im.managed[p] = v == im.opts.Filter
		}
	}

	edits := make([]view.Edit, 0, len(list))
	used := map[c4.ID]bool{}
	for _, e := range list {
		b := im.blobs[e.sha]
		_, name := view.SplitPath(e.path)
		if e.mode == modeLink {
			if b.data == nil {
				return nil, fmt.Errorf("%s: symlink target of %d bytes not kept", e.path, b.size)
			}
			edits = append(edits, view.Edit{Path: e.path, Entry: view.SymlinkEntry(name, string(b.data))})
			continue
		}
		exec := e.mode == modeExec
		if !b.ptr.IsNil() && im.managed[e.path] {
			size, err := im.c4gitObject(e.path, b.ptr)
			if err != nil {
				return nil, err
			}
			used[b.ptr] = true
			edits = append(edits, view.Edit{Path: e.path, Entry: view.FileEntry(name, size, b.ptr, exec)})
			continue
		}
		if err := im.ensureRaw(e.sha, b); err != nil {
			return nil, err
		}
		edits = append(edits, view.Edit{Path: e.path, Entry: view.FileEntry(name, b.size, b.id, exec)})
	}
	im.cur.C4GitObjects += len(used)
	return edits, nil
}

// read loads what the importer needs to know about a blob the first time
// it is seen: its C4 ID, and its bytes when it is small. Content that is
// not a c4git pointer is uploaded here, in the same pass that hashes it.
func (im *importer) read(e treeEntry) error {
	if b, ok := im.blobs[e.sha]; ok && (e.mode != modeLink || b.data != nil) {
		return nil
	}
	size, r, done, err := im.cat.blob(e.sha)
	if err != nil {
		return err
	}
	im.cur.BlobsRead++
	b := &blobInfo{size: size}
	keep := size <= smallBlob || e.mode == modeLink
	switch {
	case keep:
		b.data = make([]byte, size)
		if _, err := io.ReadFull(r, b.data); err != nil {
			return fmt.Errorf("blob %s: %w", e.sha, err)
		}
		b.id = c4.Identify(bytes.NewReader(b.data))
		b.ptr = parsePointer(b.data)
		if err := done(); err != nil {
			return err
		}
	default:
		if err := im.hashAndUpload(e.sha, b, r); err != nil {
			return err
		}
		if err := done(); err != nil {
			return err
		}
	}
	im.cur.BlobsHashed++
	im.blobs[e.sha] = b
	return nil
}

// hashAndUpload hashes a blob's bytes while holding them (in memory, or in
// a temporary file when large), then stores them under the computed ID.
func (im *importer) hashAndUpload(sha string, b *blobInfo, r io.Reader) error {
	h := sha512.New()
	if b.size <= spoolAbove {
		buf := make([]byte, b.size)
		if _, err := io.ReadFull(r, buf); err != nil {
			return fmt.Errorf("blob %s: %w", sha, err)
		}
		h.Write(buf)
		copy(b.id[:], h.Sum(nil))
		return im.put(b, bytes.NewReader(buf))
	}
	f, err := os.CreateTemp("", "c4-gitimport-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := io.Copy(io.MultiWriter(f, h), r); err != nil {
		return fmt.Errorf("blob %s: %w", sha, err)
	}
	copy(b.id[:], h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return im.put(b, f)
}

// ensureRaw stores a small blob's own bytes, for a pointer-shaped file
// that is not c4git-managed at this path.
func (im *importer) ensureRaw(sha string, b *blobInfo) error {
	if b.present {
		return nil
	}
	if b.data == nil {
		return fmt.Errorf("blob %s: content not kept", sha)
	}
	return im.put(b, bytes.NewReader(b.data))
}

func (im *importer) put(b *blobInfo, r io.Reader) error {
	stored, err := im.opts.Sink.Put(im.ctx, b.id, b.size, r)
	if err != nil {
		return fmt.Errorf("store %s: %w", b.id, err)
	}
	b.present = true
	im.count(stored, b.size)
	return nil
}

func (im *importer) count(stored bool, size int64) {
	if stored {
		im.cur.ObjectsUploaded++
		im.cur.BytesUploaded += size
		return
	}
	im.cur.ObjectsPresent++
	im.cur.BytesPresent += size
}

// c4gitObject makes the object a c4git pointer names present in the sink,
// copying it from the c4git store by ID without rehashing, and returns its
// size. The size comes from the store that holds it. An object in neither
// store is reported missing and recorded with a null size.
func (im *importer) c4gitObject(p string, id c4.ID) (int64, error) {
	if size, ok := im.c4git[id]; ok {
		return size, nil
	}
	if im.opts.C4Git != nil {
		size, err := im.opts.C4Git.Stat(id)
		switch {
		case err == nil:
			stored, err := im.opts.Sink.Copy(im.ctx, id, size, im.opts.C4Git)
			if err != nil {
				return 0, fmt.Errorf("copy c4git object %s (%s): %w", id, p, err)
			}
			im.count(stored, size)
			im.c4git[id] = size
			return size, nil
		case !errors.Is(err, os.ErrNotExist):
			return 0, fmt.Errorf("c4git store: %s: %w", id, err)
		}
	}
	if st, ok := im.opts.Sink.(Sizer); ok {
		size, present, err := st.Size(im.ctx, id)
		if err != nil {
			return 0, err
		}
		if present {
			im.count(false, size)
			im.c4git[id] = size
			return size, nil
		}
	}
	im.cur.C4GitMissing = append(im.cur.C4GitMissing, p)
	im.sum.Missing++
	im.opts.Warn(fmt.Sprintf("%s: %s: c4git object %s is missing; recorded with a null size", short(im.cur.Commit), p, id))
	return -1, nil
}

// finish applies a commit's edits to its parent's view and stores the new
// records and the changeset.
func (im *importer) finish(parent c4.ID, edits []view.Edit, start time.Time) (*Commit, error) {
	view.SortEdits(edits)
	im.records.reads = 0
	res, err := view.Apply(im.ctx, parent, edits, im.records)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", short(im.cur.Commit), err)
	}
	im.cur.RecordsRead = im.records.reads
	im.cur.View = res.ID
	// Deepest first, so the root record is written last: when it exists,
	// the whole view does.
	for _, r := range res.Records {
		if _, err := im.opts.Sink.Put(im.ctx, r.ID, int64(len(r.Data)), bytes.NewReader(r.Data)); err != nil {
			return nil, fmt.Errorf("store record %s: %w", r.ID, err)
		}
		im.records.add(r.ID, r.Data)
	}
	im.cur.RecordsWritten = len(res.Records)
	if len(res.Changeset) > 0 {
		if _, err := im.opts.Sink.Put(im.ctx, res.ChangesetID, int64(len(res.Changeset)), bytes.NewReader(res.Changeset)); err != nil {
			return nil, fmt.Errorf("store changeset %s: %w", res.ChangesetID, err)
		}
		im.cur.Changeset = res.ChangesetID
	}
	im.cur.Millis = float64(time.Since(start).Microseconds()) / 1000
	c := *im.cur
	im.sum.Commits = append(im.sum.Commits, c)
	if im.opts.Progress != nil {
		im.opts.Progress(c)
	}
	return &c, nil
}

// parsePointer returns the C4 ID a c4git pointer blob names: exactly 90
// bytes of C4 ID, or 91 with a trailing newline, as c4git's clean filter
// writes and accepts. Anything else is content. c4.Parse alone accepts
// some 90-byte strings that are not canonical IDs (it does not check the
// "c4" prefix), so the ID must also print back as the same text.
func parsePointer(data []byte) c4.ID {
	s := string(data)
	if len(s) == 91 && s[90] == '\n' {
		s = s[:90]
	}
	if len(s) != 90 || !strings.HasPrefix(s, "c4") {
		return c4.ID{}
	}
	id, err := c4.Parse(s)
	if err != nil || id.String() != s {
		return c4.ID{}
	}
	return id
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// recordCache serves records to Apply: those this import wrote or read,
// then the store. It counts reads, so a test can check that a commit reads
// only the records along its changed paths.
type recordCache struct {
	src   view.RecordSource
	m     map[c4.ID][]byte
	bytes int
	reads int
}

// recordCacheBytes bounds the cache; past it the cache starts over and
// records come from the store.
const recordCacheBytes = 512 << 20

func newRecordCache(src view.RecordSource) *recordCache {
	return &recordCache{src: src, m: map[c4.ID][]byte{}}
}

func (c *recordCache) Record(ctx context.Context, id c4.ID) ([]byte, error) {
	c.reads++
	if b, ok := c.m[id]; ok {
		return b, nil
	}
	if c.src == nil {
		return nil, fmt.Errorf("record %s is not in this import and no record source is configured", id)
	}
	b, err := c.src.Record(ctx, id)
	if err != nil {
		return nil, err
	}
	c.add(id, b)
	return b, nil
}

func (c *recordCache) add(id c4.ID, data []byte) {
	if _, ok := c.m[id]; ok {
		return
	}
	if c.bytes+len(data) > recordCacheBytes {
		clear(c.m)
		c.bytes = 0
	}
	c.m[id] = data
	c.bytes += len(data)
}
