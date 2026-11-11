package farmcli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Names a folder takes for one (case on APFS and NTFS, Unicode form on
// APFS; none on a case-sensitive volume) are measured, never predicted
// with a folding table. For each directory of the view, its names are made
// as empty files, in byte order, in a probe folder of the get's own, made
// where the directory is (so in the user's tree) and removed as soon as
// that directory is measured; a name whose making lands on one made before
// it is taken for that one. Of each set, the last in byte order is the one
// written, whole: its exact spelling and its bytes. The others, and
// everything under them, are not written at all, and are reported with it.
// Nothing a get did not make is removed, a probe folder another get left
// included. For files that is what git does; for directories git merges a
// set under its first spelling, and here the winner is written alone, so
// that what this get writes is one c4m entry exactly.

// probePrefix names a get's probe folders, so one an interrupted get left
// is recognised and reported by the next.
const probePrefix = ".c4-names-"

// losers measures which of a view's paths the folder at dir takes for
// another of them, and returns each such path with the path written in its
// place, and lines to report: probe folders found that this get did not
// make, and any of its own it could not remove. Each directory of the view
// is measured where it is, or would be: in a probe folder made in its own
// folder when that exists, or else in the nearest folder above it that
// does, never through a link, since case rules can be set per directory
// (ext4 casefold, NTFS) and a directory can be another volume. Where no
// probe folder can be made (a read-only folder or volume), what is already
// there is measured instead: names that are one entry there are one set.
// Directories are measured from the top, and none under a loser. The
// caller has left out everything under a link already in the folder.
func losers(dir string, files []api.ViewFile, dirs []string, links []api.ViewLink) (map[string]string, []string, error) {
	children := map[string][]string{} // view parent to its names
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSuffix(p, "/")
		for ; p != "." && p != "" && !seen[p]; p = path.Dir(p) {
			seen[p] = true
			children[path.Dir(p)] = append(children[path.Dir(p)], path.Base(p))
		}
	}
	for _, f := range files {
		add(f.Path)
	}
	for _, d := range dirs {
		add(d)
	}
	for _, l := range links {
		add(l.Path)
	}
	out := map[string]string{}
	parents := make([]string, 0, len(children))
	for p, names := range children {
		if len(names) > 1 {
			parents = append(parents, p)
		}
	}
	if len(parents) == 0 {
		return out, nil, nil
	}
	sort.Strings(parents)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	var notes []string
	exact := map[string]bool{} // a probe place to whether it folds nothing
	looked := map[string]bool{}
	for _, parent := range parents {
		if q, _ := lostTo(out, parent); q != "" {
			continue // under a loser: not written, not measured
		}
		names := children[parent]
		sort.Strings(names)
		where := nearest(dir, parent)
		if !looked[where] {
			notes = append(notes, strays(where)...)
			looked[where] = true
		}
		probe, err := os.MkdirTemp(where, probePrefix)
		var group map[string]string
		if err != nil {
			if !cannotMake(err) {
				return nil, nil, err
			}
			group = measureThere(filepath.Join(dir, filepath.FromSlash(parent)), names)
		} else {
			folds, ok := exact[where]
			if !ok {
				folds, err = foldsAny(probe)
				exact[where] = folds
			}
			if err == nil && folds {
				group, err = measure(probe, names)
			}
			if rerr := os.RemoveAll(probe); rerr != nil {
				notes = append(notes, fmt.Sprintf("%s: this get's probe folder, which it could not remove: %v", probe, rerr))
			}
			if err != nil {
				return nil, nil, err
			}
		}
		last := map[string]string{} // first name of a set to its last
		for _, name := range names {
			if g, ok := group[name]; ok {
				last[g] = name // names are in byte order, so the last one stays
			}
		}
		for _, name := range names {
			g, ok := group[name]
			if !ok || last[g] == name {
				continue
			}
			out[path.Join(parent, name)] = path.Join(parent, last[g])
		}
	}
	return out, notes, nil
}

// nearest returns the folder at dir/rel, or the nearest folder above it
// that exists, going down from dir and never through a link: an entry that
// is not a folder (a link included) ends the walk.
func nearest(dir, rel string) string {
	at := dir
	if rel == "." || rel == "" {
		return at
	}
	for _, seg := range strings.Split(rel, "/") {
		next := filepath.Join(at, seg)
		fi, err := os.Lstat(next)
		if err != nil || !fi.IsDir() {
			return at
		}
		at = next
	}
	return at
}

// strays reports, and never removes, what folder holds under a probe
// folder's name: one a get left when it was interrupted, one a get running
// now is measuring in, or a user's own entry so named. No get can tell
// these apart for certain, so none is touched.
func strays(folder string) []string {
	found, _ := filepath.Glob(filepath.Join(folder, probePrefix+"*"))
	var out []string
	for _, f := range found {
		out = append(out, fmt.Sprintf("%s: named like a get's probe folder (an interrupted get leaves one; a get running now has one); left as it is", f))
	}
	return out
}

// cannotMake reports whether err says the folder takes no new entry at
// all (permission, or a read-only volume), so what is there is measured.
func cannotMake(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// foldsAny reports whether the folder probe sits in takes A for a, or é
// for e + U+0301; one that does neither takes no two names for one.
func foldsAny(probe string) (bool, error) {
	for i, pair := range [][2]string{{"A", "a"}, {"\u00e9", "e\u0301"}} {
		same, err := oneEntry(filepath.Join(probe, "exact"+strconv.Itoa(i)), pair[0], pair[1])
		if err != nil || same {
			return same, err
		}
	}
	return false, nil
}

// measure makes names, in byte order, as empty files in a fresh folder in
// probe, and returns each name made or landed on with the first name of
// its set. A name the folder will not make is left out: its write says
// why. Any other failure (the probe folder gone, say) is an error, never a
// set left unmeasured.
func measure(probe string, names []string) (map[string]string, error) {
	folder, err := os.MkdirTemp(probe, "d")
	if err != nil {
		return nil, err
	}
	group := map[string]string{}
	ids := identities{}
	for _, name := range names {
		full := filepath.Join(folder, name)
		f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			f.Close()
			fi, err := os.Lstat(full)
			if err != nil {
				return nil, err
			}
			ids.add(fi, name)
			group[name] = name
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			continue // a name the folder refuses, or the folder gone: see below
		}
		there, err := os.Lstat(full)
		if err != nil {
			return nil, err
		}
		if first := ids.of(there); first != "" {
			group[name] = group[first]
		}
	}
	// The folder lists exactly the entries made, so no landing went unseen:
	// one removed meanwhile would have let a later name be made afresh, and
	// one added, or the folder gone, would have made a landing look like a
	// refusal.
	made := 0
	for name, g := range group {
		if name == g {
			made++
		}
	}
	ents, err := os.ReadDir(folder)
	if err != nil {
		return nil, fmt.Errorf("measuring names: %w", err)
	}
	if len(ents) != made {
		return nil, fmt.Errorf("measuring names: the probe folder %s lists %d entries where this get made %d; something else changed it", folder, len(ents), made)
	}
	return group, nil
}

// measureThere groups the names already in folder by the entry each one
// is, where nothing can be made to measure them.
func measureThere(folder string, names []string) map[string]string {
	group := map[string]string{}
	ids := identities{}
	for _, name := range names {
		fi, err := os.Lstat(filepath.Join(folder, name))
		if err != nil {
			continue
		}
		if first := ids.of(fi); first != "" {
			group[name] = group[first]
			continue
		}
		ids.add(fi, name)
		group[name] = name
	}
	return group
}

// identities finds which of the entries seen a file is, by its file
// identity where the platform gives one (device and inode), else by
// comparing with each.
type identities struct {
	byKey map[fileKey]string
	all   []os.FileInfo
	names []string
}

func (s *identities) add(fi os.FileInfo, name string) {
	if k, ok := keyOf(fi); ok {
		if s.byKey == nil {
			s.byKey = map[fileKey]string{}
		}
		s.byKey[k] = name
		return
	}
	s.all = append(s.all, fi)
	s.names = append(s.names, name)
}

// rename makes fi known by name from now on.
func (s *identities) rename(fi os.FileInfo, name string) {
	if k, ok := keyOf(fi); ok {
		s.byKey[k] = name
		return
	}
	for i, o := range s.all {
		if os.SameFile(o, fi) {
			s.names[i] = name
		}
	}
}

func (s *identities) of(fi os.FileInfo) string {
	if k, ok := keyOf(fi); ok {
		return s.byKey[k]
	}
	for i, o := range s.all {
		if os.SameFile(o, fi) {
			return s.names[i]
		}
	}
	return ""
}

// oneEntry reports whether the folder at dir (made here) takes b for a.
func oneEntry(dir, a, b string) (bool, error) {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(dir, a), nil, 0o644); err != nil {
		return false, err
	}
	if _, err := os.Lstat(filepath.Join(dir, b)); err == nil {
		return true, nil
	}
	_, err := os.Lstat(filepath.Join(dir, a))
	return false, err // a, gone, says nothing about b
}

// lostTo returns the loser p is or is under, and the path written in its
// place, or "".
func lostTo(lost map[string]string, p string) (string, string) {
	p = strings.TrimSuffix(p, "/")
	for q := p; q != "." && q != "" && q != "/"; q = path.Dir(q) {
		if w, ok := lost[q]; ok {
			return q, w
		}
	}
	return "", ""
}

// withoutLosers leaves out of a listing every path a folder takes for
// another (losers), and every link whose target falls on one, and says
// of each why.
func withoutLosers(lost map[string]string, files []api.ViewFile, dirs []string, links []api.ViewLink) ([]api.ViewFile, []string, []api.ViewLink, []string) {
	if len(lost) == 0 {
		return files, dirs, links, nil
	}
	var left []string
	why := func(p, q, w string) string {
		if p == q {
			return fmt.Sprintf("%s: this folder takes it for %s; %s, the last of them in byte order, is written", show(q), show(w), show(w))
		}
		return fmt.Sprintf("%s: under %s, which this folder takes for %s; %s, the last of them in byte order, is written", show(p), show(q), show(w), show(w))
	}
	var kf []api.ViewFile
	for _, f := range files {
		if q, w := lostTo(lost, f.Path); q != "" {
			left = append(left, why(f.Path, q, w))
			continue
		}
		kf = append(kf, f)
	}
	var kd []string
	for _, d := range dirs {
		p := strings.TrimSuffix(d, "/")
		if q, w := lostTo(lost, p); q != "" {
			left = append(left, why(p, q, w))
			continue
		}
		kd = append(kd, d)
	}
	var kl []api.ViewLink
	for _, l := range links {
		if q, w := lostTo(lost, l.Path); q != "" {
			left = append(left, why(l.Path, q, w))
			continue
		}
		// A link into a name that is not written would read the winner.
		if t := path.Join(path.Dir(l.Path), l.Target); !path.IsAbs(l.Target) {
			if q, w := lostTo(lost, t); q != "" {
				left = append(left, fmt.Sprintf("%s -> %s: it points at %s, which this folder takes for %s and is not written", show(l.Path), show(l.Target), show(q), show(w)))
				continue
			}
		}
		kl = append(kl, l)
	}
	// Every loser by its own name too, a directory implied only by what is
	// under it included.
	for q, w := range lost {
		left = append(left, why(q, q, w))
	}
	sort.Strings(left)
	left = compact(left)
	return kf, kd, kl, left
}

// compact drops repeated lines of a sorted list.
func compact(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// respell renames, after a get, every entry the folder lists under another
// spelling than the view's (one an earlier get, or anything else, left) to
// the view's exactly, in place: a case-only or Unicode-form rename, which
// APFS and NTFS make. Nothing is removed; an entry that will not rename is
// reported. Each folder is listed once.
func respell(dir string, paths []string) []string {
	var left []string
	listed := map[string]map[string]bool{}
	byID := map[string]*identities{}
	sort.Strings(paths)
	for _, p := range paths {
		p = strings.TrimSuffix(p, "/")
		parent := filepath.Join(dir, filepath.FromSlash(path.Dir(p)))
		names, ok := listed[parent]
		if !ok {
			ents, err := os.ReadDir(parent)
			if err != nil {
				continue
			}
			names = make(map[string]bool, len(ents))
			for _, e := range ents {
				names[e.Name()] = true
			}
			listed[parent] = names
		}
		name := path.Base(p)
		if names[name] {
			continue
		}
		there, err := os.Lstat(filepath.Join(parent, name))
		if err != nil {
			continue
		}
		// The folder's entries by identity, made on the first miss in it,
		// so a folder full of names to respell costs one Lstat per entry.
		ids, ok := byID[parent]
		if !ok {
			ids = &identities{}
			listedNames := make([]string, 0, len(names))
			for n := range names {
				listedNames = append(listedNames, n)
			}
			sort.Strings(listedNames) // of two names of one entry, the first
			for _, n := range listedNames {
				if fi, err := os.Lstat(filepath.Join(parent, n)); err == nil && ids.of(fi) == "" {
					ids.add(fi, n)
				}
			}
			byID[parent] = ids
		}
		other := ids.of(there)
		if other == "" || other == name {
			continue
		}
		if err := os.Rename(filepath.Join(parent, other), filepath.Join(parent, name)); err != nil {
			left = append(left, fmt.Sprintf("%s: the folder lists it as %s and will not take its own spelling: %v", show(p), show(other), err))
			continue
		}
		delete(names, other)
		names[name] = true
		ids.rename(there, name)
	}
	return left
}

// show writes a path as it is when it is printable ASCII, and quoted with
// its other bytes escaped otherwise: names a folder takes for one often
// look alike (é and e + U+0301), and a report must tell them apart.
func show(p string) string {
	for i := 0; i < len(p); i++ {
		if p[i] >= 0x80 || p[i] < 0x20 {
			return strconv.QuoteToASCII(p)
		}
	}
	return p
}

// notThroughLinks leaves out every entry whose folder in dir is, or is
// under, a link already there: a get writes only into folders, never
// through a link an earlier get (or anything else) left where the view
// now has a directory.
func notThroughLinks(dir string, files []api.ViewFile, dirs []string, links []api.ViewLink) ([]api.ViewFile, []string, []api.ViewLink, []string) {
	var left []string
	isLink := map[string]bool{}
	through := func(p string) string {
		var up []string
		for d := path.Dir(strings.TrimSuffix(p, "/")); d != "." && d != "/" && d != ""; d = path.Dir(d) {
			up = append(up, d)
		}
		for i := len(up) - 1; i >= 0; i-- {
			d := up[i]
			l, ok := isLink[d]
			if !ok {
				fi, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(d)))
				l = err == nil && fi.Mode()&os.ModeSymlink != 0
				isLink[d] = l
			}
			if l {
				return d
			}
		}
		return ""
	}
	why := func(p, d string) string {
		return fmt.Sprintf("%s: %s is a link in the folder where the view has a directory, and nothing is written through it", show(p), show(d))
	}
	var kf []api.ViewFile
	for _, f := range files {
		if d := through(f.Path); d != "" {
			left = append(left, why(f.Path, d))
			continue
		}
		kf = append(kf, f)
	}
	var kd []string
	for _, d := range dirs {
		if l := through(d); l != "" {
			left = append(left, why(d, l))
			continue
		}
		kd = append(kd, d)
	}
	var kl []api.ViewLink
	for _, l := range links {
		if d := through(l.Path); d != "" {
			left = append(left, why(l.Path, d))
			continue
		}
		kl = append(kl, l)
	}
	return kf, kd, kl, left
}

// leftovers reports, never removes, what a written directory holds under
// a name that belongs to a losing spelling of it: an entry an earlier get
// wrote under that spelling (Docs/a, when the folder now holds docs/ as
// the view's winner) or a user's file there. What this get wrote is the
// view exactly; anything else found where it wrote is said.
func leftovers(dir string, lost map[string]string, files []api.ViewFile, dirs []string, links []api.ViewLink) []string {
	if len(lost) == 0 {
		return nil
	}
	childrenOf := map[string]map[string]bool{}
	note := func(p string) {
		p = strings.TrimSuffix(p, "/")
		for ; p != "." && p != ""; p = path.Dir(p) {
			parent := path.Dir(p)
			if childrenOf[parent] == nil {
				childrenOf[parent] = map[string]bool{}
			}
			childrenOf[parent][path.Base(p)] = true
		}
	}
	for _, f := range files {
		note(f.Path)
	}
	for _, d := range dirs {
		note(d)
	}
	for _, l := range links {
		note(l.Path)
	}
	// What a folder lists, exactly: a name looked up would also find the
	// winner's own entry under another spelling (docs/A for docs/a).
	listing := map[string]map[string]bool{}
	listed := func(folder, name string) bool {
		names, ok := listing[folder]
		if !ok {
			names = map[string]bool{}
			ents, _ := os.ReadDir(filepath.Join(dir, filepath.FromSlash(folder)))
			for _, e := range ents {
				names[e.Name()] = true
			}
			listing[folder] = names
		}
		return names[name]
	}
	var out []string
	for loser, winner := range lost {
		for name := range childrenOf[loser] {
			if childrenOf[winner][name] || !listed(winner, name) {
				continue
			}
			out = append(out, fmt.Sprintf("%s: left over from the spelling %s (the view's %s, not written), and left as it is", show(path.Join(winner, name)), show(loser), show(path.Join(loser, name))))
		}
	}
	sort.Strings(out)
	return out
}

// linksInside leaves out every link whose target would reach past the
// folder, as makeLinks would, and says which.
func linksInside(links []api.ViewLink) ([]api.ViewLink, []string) {
	var kept []api.ViewLink
	var left []string
	for _, l := range links {
		rel := filepath.FromSlash(l.Path)
		target := filepath.FromSlash(l.Target)
		if filepath.IsAbs(target) || !filepath.IsLocal(filepath.Join(filepath.Dir(rel), target)) {
			left = append(left, fmt.Sprintf("%s -> %s: the link points outside the folder", l.Path, l.Target))
			continue
		}
		kept = append(kept, l)
	}
	return kept, left
}
