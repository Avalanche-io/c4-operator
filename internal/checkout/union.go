package checkout

import (
	"fmt"
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
	// The union's own names (".wh..wh." and after): they delete nothing
	// and are never part of the merged tree.
	internalPrefix = ".wh..wh."
	// metaPrefix + name in a directory of the writable layer: name's new
	// metadata (mode, times, owner) on a file whose bytes stayed in a
	// lower. The marker's own attributes are that metadata.
	metaPrefix = ".wh..wh.meta."
)

// reservedName reports a name the union keeps for its markers: one whose
// first four bytes are ".wh." in any ASCII case. The union refuses to
// create such a name on every host, because on a case-insensitive one
// ".WH.x" is the same file as the marker ".wh.x" (no other code point
// folds into the prefix). Markers themselves are recognised exactly.
func reservedName(name string) bool {
	return len(name) >= len(whiteoutPrefix) && strings.EqualFold(name[:len(whiteoutPrefix)], whiteoutPrefix)
}

// WalkUnionOver is WalkUnion for a workspace of v, which also reads the
// union's metadata markers: a file whose mode changed while its bytes
// stayed in the lower. A view records only the exec bit, so a marker whose
// exec bit differs from v's file is that file with its content (its C4 ID)
// unchanged; any other marker (times, owner, other mode bits) is no change.
func WalkUnionOver(v *view.View) func(upper string) ([]overlay.Entry, error) {
	files := make(map[string]view.File, len(v.Files))
	for _, f := range v.Files {
		files[f.Path] = f
	}
	return func(upper string) ([]overlay.Entry, error) {
		out, err := walkUnion(upper, func(rel string, info fs.FileInfo) *overlay.Entry {
			f, ok := files[rel]
			exec := info.Mode()&0o111 != 0
			if !ok || f.Exec == exec {
				return nil
			}
			return &overlay.Entry{Path: rel, Kind: overlay.File, Size: f.Size, Exec: exec, ID: f.ID}
		})
		return out, err
	}
}

// WalkUnion reads a union's writable layer into its changes, as capture's
// entries: an empty file .wh.<name> deletes name from its directory, a
// .wh.__dir_opaque file makes its directory replace the lower one
// entirely, and everything else is what was written there. Metadata
// markers are skipped: WalkUnionOver reads them against the view. Any
// other reserved name (".WH.x") can't have come through the union, which
// refuses to make one, so it fails the walk rather than being taken as a
// file or dropped.
func WalkUnion(upper string) ([]overlay.Entry, error) { return walkUnion(upper, nil) }

func walkUnion(upper string, meta func(rel string, info fs.FileInfo) *overlay.Entry) ([]overlay.Entry, error) {
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
		case strings.HasPrefix(name, metaPrefix) && meta != nil:
			target := strings.TrimPrefix(name, metaPrefix)
			if target == "" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if e := meta(dir+target, info); e != nil {
				out = append(out, *e)
			}
			return nil
		case strings.HasPrefix(name, internalPrefix):
			return nil
		case name == opaqueMarker:
			opaque[dir] = true
			return nil
		case strings.HasPrefix(name, whiteoutPrefix):
			if target := strings.TrimPrefix(name, whiteoutPrefix); target != "" {
				out = append(out, overlay.Entry{Path: dir + target, Kind: overlay.Whiteout})
			}
			return nil
		case reservedName(name):
			return fmt.Errorf("%w: %s is in the writable layer, which the union never makes", ErrReservedNames, rel)
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
// carry: a name starting with .wh., in any ASCII case, reads as a marker
// or can't be made in the writable layer (reservedName). A backend built
// on such a union refuses these views (another backend serves them).
func UnionReserved(v *view.View) []string {
	var out []string
	reserved := func(p string) bool {
		name := p[strings.LastIndex(strings.TrimSuffix(p, "/"), "/")+1:]
		return reservedName(name)
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
