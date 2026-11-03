package checkout

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/Avalanche-io/c4/c4m"

	"github.com/Avalanche-io/c4-operator/internal/capture"
	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/overlay"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Clone makes each workspace a copy-on-write clone of its view's full
// layer: one clonefile(2) of the layer's directory on APFS, sharing every
// block. The layer is the farm's own (hard links into the object cache),
// so a view's base costs directory entries once per machine, and the
// clone is independent of it: removing the layer never touches a
// workspace. A clone has no upper; its writes are found by comparing it
// with its view (WalkClone).
type Clone struct {
	Layers *layer.Store
}

func (c *Clone) Name() string { return "clone" }

func (c *Clone) Mount(_ context.Context, v *view.View, target string) (*Workspace, error) {
	m, err := c.Layers.BuildFull(v)
	if err != nil {
		return nil, fmt.Errorf("clone workspace of %s: %w", v.ID, err)
	}
	if err := cloneDir(c.Layers.Dir(m.Key), target); err != nil {
		return nil, fmt.Errorf("clone workspace of %s: %w", v.ID, err)
	}
	return &Workspace{View: v, Target: target, Upper: target, Env: GitEnv(nil)}, nil
}

func (c *Clone) Capture(ctx context.Context, ws *Workspace, in capture.Input) (*capture.Result, error) {
	in.Parent, in.Upper = ws.View.ID, ws.Target
	in.Walk = func(dir string) ([]overlay.Entry, error) { return WalkClone(dir, ws.View) }
	return capture.Run(ctx, in)
}

func (c *Clone) Unmount(_ context.Context, ws *Workspace) error { return os.RemoveAll(ws.Target) }

// WalkClone reads a workspace that is a whole tree (a clone, or any copy)
// into its changes against v, as capture's entries.
//
// A view's file is unchanged when it is still a regular file of the
// view's size and exec bit, with the time a view's file is created at: a
// view's timestamps are null, and a file created from a null timestamp
// gets the epoch (C4-PRIMER, the null rules). Anything else is reported
// as a file, and capture hashes it: a file rewritten with the same bytes
// comes back unchanged, so a wrong guess here costs a hash, never a wrong
// result. A file edited in place to the same size with its time put back
// to the epoch is not seen; git's minimal stat check has the same limit.
//
// Directories are compared by what they hold, never by time. A path of
// the view that is gone is a whiteout, named once at the highest
// directory that went.
func WalkClone(dir string, v *view.View) ([]overlay.Entry, error) {
	files := make(map[string]view.File, len(v.Files))
	for _, f := range v.Files {
		files[f.Path] = f
	}
	links := make(map[string]string, len(v.Symlinks))
	for _, l := range v.Symlinks {
		links[l.Path] = l.Target
	}
	dirs := make(map[string]bool, len(v.Dirs))
	for _, d := range v.Dirs {
		dirs[d] = true
	}
	null := c4m.NullTimestamp()

	var out []overlay.Entry
	seen := map[string]bool{} // paths present, directories ending in "/"
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			seen[rel+"/"] = true
			if dirs[rel+"/"] {
				return nil
			}
			// A new directory, or one where the view had a file or link,
			// which it replaces. A clone never needs an opaque directory:
			// what a replaced directory no longer holds is a whiteout.
			out = append(out, overlay.Entry{Path: rel + "/", Full: p, Kind: overlay.Dir})
		case mode&fs.ModeSymlink != 0:
			seen[rel] = true
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if was, ok := links[rel]; ok && was == target {
				return nil
			}
			out = append(out, overlay.Entry{Path: rel, Full: p, Kind: overlay.Symlink, Target: target})
		case mode.IsRegular():
			seen[rel] = true
			exec := mode&0o111 != 0
			if f, ok := files[rel]; ok && f.Size == info.Size() && f.Exec == exec && info.ModTime().Equal(null) {
				return nil
			}
			out = append(out, overlay.Entry{Path: rel, Full: p, Kind: overlay.File, Size: info.Size(), Exec: exec})
		default:
			seen[rel] = true
			out = append(out, overlay.Entry{Path: rel, Full: p, Kind: overlay.Other, Unusual: mode.Type().String()})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	gone := func(p string) {
		parent := path.Dir(p)
		if parent == "." || seen[parent+"/"] {
			out = append(out, overlay.Entry{Path: p, Kind: overlay.Whiteout})
		}
	}
	for p := range files {
		if !seen[p] && !seen[p+"/"] {
			gone(p)
		}
	}
	for p := range links {
		if !seen[p] && !seen[p+"/"] {
			gone(p)
		}
	}
	for d := range dirs {
		p := d[:len(d)-1]
		if !seen[d] && !seen[p] {
			gone(p)
		}
	}
	return out, nil
}
