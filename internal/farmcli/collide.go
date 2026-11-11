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

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Names a folder takes for one (case on APFS and NTFS, Unicode form on
// APFS; none on a case-sensitive volume) are measured, never predicted
// with a folding table, and measured in a scratch folder of the get's own,
// never in the user's tree: for each directory of the view, its names are
// made as empty files in a fresh probe folder, in byte order, and a name
// whose making lands on one made before it is taken for that one. Of each
// set, the last in byte order is the one written, whole: its exact
// spelling and its bytes. The others, and everything under them, are not
// written at all, and are reported with it; nothing already in the folder
// is removed. Git leaves the first spelling on disk holding the last one's
// bytes, an entry that is neither, which a scan of the folder would read
// as a wrong one; here a scan reads the winner exactly.

// losers measures which of a view's paths the folder at dir takes for
// another of them, and returns each such path with the path written in its
// place.
func losers(dir string, files []api.ViewFile, dirs []string, links []api.ViewLink) (map[string]string, error) {
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
	if len(seen) < 2 {
		return out, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	probe, err := os.MkdirTemp(dir, ".c4-names-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(probe)
	// A folder that tells case and Unicode form apart takes no two names
	// for one, and needs nothing more measured.
	exact := true
	for i, pair := range [][2]string{{"A", "a"}, {"é", "é"}} {
		if same, err := oneEntry(filepath.Join(probe, "exact"+strconv.Itoa(i)), pair[0], pair[1]); err != nil {
			return nil, err
		} else if same {
			exact = false
		}
	}
	if exact {
		return out, nil
	}
	parents := make([]string, 0, len(children))
	for p := range children {
		parents = append(parents, p)
	}
	sort.Strings(parents)
	for i, parent := range parents {
		names := children[parent]
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		folder := filepath.Join(probe, strconv.Itoa(i))
		if err := os.Mkdir(folder, 0o755); err != nil {
			return nil, err
		}
		made := make([]os.FileInfo, 0, len(names))
		madeName := make([]string, 0, len(names))
		group := map[string]string{} // name to the first name of its set
		for _, name := range names {
			full := filepath.Join(folder, name)
			f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err == nil {
				f.Close()
				fi, err := os.Lstat(full)
				if err != nil {
					return nil, err
				}
				made = append(made, fi)
				madeName = append(madeName, name)
				group[name] = name
				continue
			}
			if !errors.Is(err, fs.ErrExist) {
				continue // a name this folder will not make: its write says why
			}
			there, err := os.Lstat(full)
			if err != nil {
				return nil, err
			}
			for j, fi := range made {
				if os.SameFile(fi, there) {
					group[name] = group[madeName[j]]
					break
				}
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
	return out, nil
}

// oneEntry reports whether the folder at dir (made here) takes b for a.
func oneEntry(dir, a, b string) (bool, error) {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(dir, a), nil, 0o644); err != nil {
		return false, err
	}
	_, err := os.Lstat(filepath.Join(dir, b))
	return err == nil, nil
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
		for other := range names {
			fi, err := os.Lstat(filepath.Join(parent, other))
			if err != nil || !os.SameFile(fi, there) {
				continue
			}
			if err := os.Rename(filepath.Join(parent, other), filepath.Join(parent, name)); err != nil {
				left = append(left, fmt.Sprintf("%s: the folder lists it as %s and will not take its own spelling: %v", show(p), show(other), err))
			} else {
				delete(names, other)
				names[name] = true
			}
			break
		}
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
