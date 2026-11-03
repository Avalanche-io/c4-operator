package checkout

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/overlay"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// The markers a union filesystem in the aufs and OCI form (absfs/unionfs,
// which the NFS workspace uses) leaves in its writable layer. When the NFS
// backend brings unionfs in as a dependency, these give way to its
// exported IsWhiteout, IsOpaqueMarker and WhiteoutTarget.
const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh.__dir_opaque"
)

// WalkUnion reads a union's writable layer into its changes, as capture's
// entries: an empty file .wh.<name> deletes name from its directory, a
// .wh.__dir_opaque file makes its directory replace the lower one
// entirely, and everything else is what was written there.
func WalkUnion(upper string) ([]overlay.Entry, error) {
	var out []overlay.Entry
	opaque := map[string]bool{} // directories (relative, ending in "/") with the marker
	err := filepath.WalkDir(upper, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(upper, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		dir, name := "", rel
		if i := strings.LastIndex(rel, "/"); i >= 0 {
			dir, name = rel[:i+1], rel[i+1:]
		}
		switch {
		case name == opaqueMarker:
			opaque[dir] = true
			return nil
		case strings.HasPrefix(name, whiteoutPrefix):
			if target := strings.TrimPrefix(name, whiteoutPrefix); target != "" {
				out = append(out, overlay.Entry{Path: dir + target, Kind: overlay.Whiteout})
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			out = append(out, overlay.Entry{Path: rel + "/", Full: p, Kind: overlay.Dir})
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out = append(out, overlay.Entry{Path: rel, Full: p, Kind: overlay.Symlink, Target: target})
		case mode.IsRegular():
			out = append(out, overlay.Entry{Path: rel, Full: p, Kind: overlay.File, Size: info.Size(), Exec: mode&0o111 != 0})
		default:
			out = append(out, overlay.Entry{Path: rel, Full: p, Kind: overlay.Other, Unusual: mode.Type().String()})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Kind == overlay.Dir && opaque[out[i].Path] {
			out[i].Opaque = true
		}
	}
	return out, nil
}

// UnionReserved lists the paths of v that a union of this form can't
// carry: a name starting with .wh. reads as a marker, so the file would
// vanish from the merged view. A backend built on such a union refuses
// these views (another backend serves them).
func UnionReserved(v *view.View) []string {
	var out []string
	reserved := func(p string) bool {
		name := p[strings.LastIndex(strings.TrimSuffix(p, "/"), "/")+1:]
		return strings.HasPrefix(name, whiteoutPrefix)
	}
	for _, d := range v.Dirs {
		if reserved(d) {
			out = append(out, d)
		}
	}
	for _, f := range v.Files {
		if reserved(f.Path) {
			out = append(out, f.Path)
		}
	}
	for _, l := range v.Symlinks {
		if reserved(l.Path) {
			out = append(out, l.Path)
		}
	}
	return out
}
