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
//
// The union writes a deletion or metadata marker only for a name a lower
// has, so a marker whose name isn't in v was written under another
// spelling of a view name (".wh.DOCS" for docs, on a case-insensitive
// host). Read byte for byte it would delete or change nothing, and the
// capture would silently miss the agent's change, so the walk fails
// instead.
//
// A write through another spelling copies the file up under that spelling,
// beside the view's own name, which a byte-exact reader takes for a second
// file. lower, the view's folder on this host, catches it: a new name that
// isn't in v but that the lower resolves is another spelling of a lower
// entry, folded by the host itself (case, or Unicode normalisation), so the
// walk fails unless the upper deletes that entry or replaces it with an
// opaque directory, as replacing docs with Docs does. An empty lower skips
// the check.
func WalkUnionOver(v *view.View, lower string) func(upper string) ([]overlay.Entry, error) {
	files := make(map[string]view.File, len(v.Files))
	for _, f := range v.Files {
		files[f.Path] = f
	}
	inView := map[string]bool{}
	for _, d := range v.Dirs {
		inView[strings.TrimSuffix(d, "/")] = true
	}
	for _, f := range v.Files {
		inView[f.Path] = true
	}
	for _, l := range v.Symlinks {
		inView[l.Path] = true
	}
	return func(upper string) ([]overlay.Entry, error) {
		out, err := walkUnion(upper, func(rel string, info fs.FileInfo) (*overlay.Entry, error) {
			f, ok := files[rel]
			if !ok {
				if inView[rel] {
					return nil, nil // a directory or symlink's metadata: not part of a view
				}
				return nil, fmt.Errorf("checkout: the union changed %s, which the view doesn't have (another spelling of a view name?)", rel)
			}
			exec := info.Mode()&0o111 != 0
			if f.Exec == exec {
				return nil, nil
			}
			return &overlay.Entry{Path: rel, Kind: overlay.File, Size: f.Size, Exec: exec, ID: f.ID}, nil
		})
		if err != nil {
			return nil, err
		}
		deleted := map[string]bool{}
		for _, e := range out {
			if e.Kind != overlay.Whiteout {
				continue
			}
			if !inView[e.Path] {
				return nil, fmt.Errorf("checkout: the union deleted %s, which the view doesn't have (another spelling of a view name?)", e.Path)
			}
			deleted[e.Path] = true
		}
		if lower == "" {
			return out, nil
		}
		for _, e := range out {
			p := strings.TrimSuffix(e.Path, "/")
			if e.Kind == overlay.Whiteout || inView[p] {
				continue
			}
			names, err := aliasOf(lower, p)
			if err != nil {
				return nil, err
			}
			var kept []string
			for _, name := range names {
				if !deleted[name] {
					kept = append(kept, name)
				}
			}
			if len(kept) == 0 {
				continue
			}
			// An opaque directory carries its whole content, so one under
			// another spelling replaces the view's entry outright (rm -rf docs,
			// then mkdir Docs; the union's own deletion marker folded away).
			// Anything else is ambiguous: a file under another spelling is
			// both what a rename leaves and what a write through that spelling
			// leaves, and the two mean different trees.
			if e.Kind == overlay.Dir && e.Opaque && len(kept) == 1 {
				out = append(out, overlay.Entry{Path: kept[0], Kind: overlay.Whiteout})
				deleted[kept[0]] = true
				continue
			}
			return nil, fmt.Errorf("checkout: the union wrote %s, another spelling of the view's %s", p, spelledLike(p, kept))
		}
		return out, nil
	}
}

// aliasOf returns the lower entries that p, a name not in the lower byte for
// byte, resolves to on this host, or none when it resolves to nothing.
// It's every entry of that directory that is the same file, not the first:
// the layer hard-links files of the same content, so two view names can be
// one inode, and only the host knows which of them p spells.
func aliasOf(lower, p string) ([]string, error) {
	full := filepath.Join(lower, filepath.FromSlash(p))
	info, err := os.Lstat(full)
	if err != nil {
		return nil, nil // no such entry: a new name
	}
	dir := filepath.Dir(full)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range ents {
		other, err := os.Lstat(filepath.Join(dir, d.Name()))
		if err != nil || !os.SameFile(info, other) {
			continue
		}
		rel, err := filepath.Rel(lower, filepath.Join(dir, d.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, filepath.ToSlash(rel))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("checkout: %s resolves in the view's folder but matches no entry there", p)
	}
	return out, nil
}

// spelledLike names, for a message, the entry of names that p most likely
// spells: one equal under simple case folding, else all of them.
func spelledLike(p string, names []string) string {
	for _, n := range names {
		if strings.EqualFold(n, p) {
			return n
		}
	}
	return strings.Join(names, " or ")
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

func walkUnion(upper string, meta func(rel string, info fs.FileInfo) (*overlay.Entry, error)) ([]overlay.Entry, error) {
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
			e, err := meta(dir+target, info)
			if err != nil {
				return err
			}
			if e != nil {
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
