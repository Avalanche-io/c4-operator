// Package overlay mounts views for containers and reads what containers
// wrote.
//
// A writable volume is an overlay of the view's layers (read-only lowers)
// with the volume's own upper and work directories. A read-only volume is
// an overlay of the lowers alone, or a read-only bind mount when the view
// is one layer. The mount options are fixed (no index, no metacopy, no
// directory redirects), so what the upper contains after a run has one
// format that capture understands. Mount and Walk need Linux.
package overlay

// Entry is one thing a container left in its upper directory.
type Entry struct {
	Path    string // relative, slash-separated; directories end in "/"
	Full    string // absolute path in the upper
	Kind    EntryKind
	Opaque  bool // directory hides everything below it in the lowers
	Exec    bool
	Target  string // symlink target
	Size    int64
	Unusual string // a device, socket or fifo: recorded, not captured
}

// EntryKind classifies upper entries.
type EntryKind int

const (
	File EntryKind = iota
	Dir
	Symlink
	Whiteout
	Other
)
