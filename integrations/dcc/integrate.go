package dcc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Change is what turning C4 on or off in one application does. Plan makes
// one and writes nothing; Apply does it.
type Change struct {
	Line    string   // one sentence: what changes, for the person deciding
	Writes  []string // every file it writes, absolute
	Removes []string // every file it removes; a folder (ending in a separator) is removed only if it is then empty
	Notes   []string // what the person must still do themselves

	roots   *Roots
	on      bool
	mpath   string
	final   *manifest // on: the manifest once done
	intent  *manifest // on: written before anything else, so an interrupted Apply is still undoable
	writes  []fileWrite
	edits   []lineChange
	removes []fileRemove
	rmdirs  []string // deepest first; each removed only if empty
}

type fileWrite struct {
	path   string
	data   []byte
	expect string // the file's digest when planned; "" if it didn't exist
}

type fileRemove struct {
	path   string
	expect string // "" for a file Python or the integration made (any content)
}

// lineChange adds c4's marked line to a person's file, or takes it out.
type lineChange struct {
	path    string
	expect  string // the file's digest when planned; "" if it didn't exist
	data    []byte // the file afterwards
	remove  bool   // off, and the file was c4's alone: remove it instead
	existed bool
}

// manifest records exactly what one integration wrote.
type manifest struct {
	Name     string      `json:"name"`
	Version  string      `json:"version"`
	Release  string      `json:"release"`
	Complete bool        `json:"complete"`
	Files    []fileEntry `json:"files"`           // whole files c4 wrote
	Lines    []lineEntry `json:"lines,omitempty"` // lines c4 added to the person's files
	Dirs     []string    `json:"dirs,omitempty"`  // folders c4 made
}

type fileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Prev   string `json:"prev,omitempty"` // while an install is under way: the file's digest before it
}

type lineEntry struct {
	Path         string `json:"path"`
	Line         string `json:"line"`
	Created      bool   `json:"created,omitempty"`      // c4 made the file
	AddedNewline bool   `json:"addedNewline,omitempty"` // c4 ended the person's last line, which had no newline
}

// failWrite, when a test sets it, fails the write of a file (part-way
// through its data), as a full disk or a crash would.
var failWrite func(path string) error

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func readManifest(p string) (*manifest, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if m.Name == "" || m.Version == "" {
		return nil, fmt.Errorf("%s is not a c4 integration manifest", p)
	}
	return &m, nil
}

// others are every integration manifest except name-version's.
func (r *Roots) others(name, version string) []*manifest {
	ps, _ := filepath.Glob(filepath.Join(r.manifestDir(), "*.json"))
	var out []*manifest
	for _, p := range ps {
		m, err := readManifest(p)
		if err != nil || (m.Name == name && m.Version == version) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Plan says what `c4 farm integrate <f.Name> on|off` would do for f. It
// writes nothing.
func Plan(f Found, on bool) (Change, error) {
	r := f.roots
	if r == nil {
		d := DefaultRoots()
		r = &d
	}
	return r.Plan(f, on)
}

// Plan says what turning C4 on or off in f would do, under these roots.
func (r *Roots) Plan(f Found, on bool) (Change, error) {
	if f.Name == "" || f.Version == "" {
		return Change{}, errors.New("which application, and which version of it?")
	}
	if r.Home == "" || !isDir(r.Home) {
		return Change{}, fmt.Errorf("no home folder to install into (%q)", r.Home)
	}
	l, err := r.layout(f, on)
	if err != nil {
		return Change{}, err
	}
	c := Change{roots: r, on: on, mpath: r.manifestPath(f.Name, f.Version)}
	own, err := readManifest(c.mpath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Change{}, err
	}
	others := r.others(f.Name, f.Version)
	if on {
		err = c.planOn(f, l, own, others)
	} else {
		err = c.planOff(f, l, own, others)
	}
	if err != nil {
		return Change{}, err
	}
	sort.Strings(c.Writes)
	return c, nil
}

// fileState reads a file: its contents, and whether it exists.
func fileState(p string) ([]byte, bool, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// knownDigests are the digests c4 recorded for each file, in any manifest.
func knownDigests(ms ...*manifest) map[string]map[string]bool {
	k := map[string]map[string]bool{}
	for _, m := range ms {
		if m == nil {
			continue
		}
		for _, e := range m.Files {
			if k[e.Path] == nil {
				k[e.Path] = map[string]bool{}
			}
			k[e.Path][e.SHA256] = true
			if e.Prev != "" {
				k[e.Path][e.Prev] = true
			}
		}
	}
	return k
}

func (c *Change) planOn(f Found, l layout, own *manifest, others []*manifest) error {
	all := append([]*manifest{own}, others...)
	known := knownDigests(all...)
	final := &manifest{Name: f.Name, Version: f.Version, Release: release, Complete: true}
	var prev []fileEntry // what the intent manifest records beyond final
	wanted := map[string]bool{}

	paths := make([]string, 0, len(l.files))
	for p := range l.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		data := l.files[p]
		wanted[p] = true
		cur, exists, err := fileState(p)
		if err != nil {
			return err
		}
		e := fileEntry{Path: p, SHA256: digest(data)}
		switch {
		case exists && bytes.Equal(cur, data):
		case exists && !known[p][digest(cur)]:
			if _, ours := known[p]; ours {
				return fmt.Errorf("can't add C4 to %s: %s has changed since c4 put it there; set it aside and run this again", l.title, p)
			}
			if why := l.refuse[p]; why != "" {
				return fmt.Errorf("can't add C4 to %s: %s", l.title, why)
			}
			return fmt.Errorf("can't add C4 to %s: %s is already there and isn't c4's; set it aside and run this again", l.title, p)
		case exists:
			c.writes = append(c.writes, fileWrite{path: p, data: data, expect: digest(cur)})
			e.Prev = digest(cur)
		default:
			c.writes = append(c.writes, fileWrite{path: p, data: data})
		}
		final.Files = append(final.Files, fileEntry{Path: p, SHA256: e.SHA256})
		prev = append(prev, e)
	}

	// The person's own files: one marked line each.
	for _, ed := range l.edits {
		cur, exists, err := fileState(ed.path)
		if err != nil {
			return err
		}
		out, addedNL := addLine(cur, ed.line, ed.marker)
		entry := lineEntry{Path: ed.path, Line: ed.line, Created: !exists, AddedNewline: addedNL}
		backup := ed.path + ".c4-backup"
		if bytes.Equal(out, cur) && exists { // already there: keep what was recorded when it was added
			entry = lineEntry{Path: ed.path, Line: ed.line}
			for _, m := range all {
				if m == nil {
					continue
				}
				for _, le := range m.Lines {
					if le.Path == ed.path && le.Line == ed.line {
						entry = le
					}
				}
				for _, fe := range m.Files {
					if fe.Path == backup && !wanted[backup] {
						wanted[backup] = true
						final.Files = append(final.Files, fileEntry{Path: backup, SHA256: fe.SHA256})
						prev = append(prev, fileEntry{Path: backup, SHA256: fe.SHA256})
					}
				}
			}
			final.Lines = append(final.Lines, entry)
			continue
		}
		for _, m := range all { // a line an earlier release added keeps how the file was before it
			if m == nil {
				continue
			}
			for _, le := range m.Lines {
				if le.Path == ed.path {
					entry.Created, entry.AddedNewline = le.Created, le.AddedNewline || addedNL
				}
			}
		}
		lc := lineChange{path: ed.path, data: out, existed: exists}
		if exists {
			lc.expect = digest(cur)
			bcur, bexists, err := fileState(backup)
			if err != nil {
				return err
			}
			if bexists && !known[backup][digest(bcur)] {
				return fmt.Errorf("can't add C4 to %s: %s is already there and isn't c4's; set it aside and run this again", l.title, backup)
			}
			bw := fileWrite{path: backup, data: cur}
			if bexists {
				bw.expect = digest(bcur)
			}
			if !bexists || !bytes.Equal(bcur, cur) {
				c.writes = append(c.writes, bw)
			}
			wanted[backup] = true
			final.Files = append(final.Files, fileEntry{Path: backup, SHA256: digest(cur)})
			prev = append(prev, fileEntry{Path: backup, SHA256: digest(cur), Prev: bw.expect})
		}
		c.edits = append(c.edits, lc)
		c.Writes = append(c.Writes, ed.path)
		final.Lines = append(final.Lines, entry)
	}

	// An earlier release's files this one no longer has.
	shared := sharedFiles(others)
	if own != nil {
		for _, e := range own.Files {
			if wanted[e.Path] || shared[e.Path] {
				continue
			}
			cur, exists, err := fileState(e.Path)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
			if !known[e.Path][digest(cur)] {
				c.Notes = append(c.Notes, "Left "+e.Path+" where it is: it has changed since c4 put it there.")
				continue
			}
			c.removes = append(c.removes, fileRemove{path: e.Path, expect: digest(cur)})
			c.Removes = append(c.Removes, e.Path)
			prev = append(prev, e)
		}
	}

	// Folders this makes.
	made := map[string]bool{}
	if own != nil {
		for _, d := range own.Dirs {
			made[d] = true
		}
	}
	var toMake []string
	for _, w := range c.writes {
		toMake = append(toMake, c.roots.missingDirs(filepath.Dir(w.path))...)
	}
	for _, lc := range c.edits {
		toMake = append(toMake, c.roots.missingDirs(filepath.Dir(lc.path))...)
	}
	toMake = append(toMake, c.roots.missingDirs(c.roots.manifestDir())...)
	for _, d := range toMake {
		made[d] = true
	}
	// Folders another version's install made that this one's files are in
	// too: the last of them to be turned off removes the folder.
	for _, m := range others {
		for _, d := range m.Dirs {
			for _, e := range final.Files {
				if strings.HasPrefix(e.Path, d+string(filepath.Separator)) {
					made[d] = true
					break
				}
			}
		}
	}
	for d := range made {
		final.Dirs = append(final.Dirs, d)
	}
	sort.Strings(final.Dirs)

	same := own != nil && len(c.writes) == 0 && len(c.edits) == 0 && len(c.removes) == 0 && sameManifest(own, final)
	if same {
		c.Line = "C4 is already in " + l.title + "; nothing to change."
		return nil
	}
	intent := *final
	intent.Complete = false
	intent.Files = prev
	c.intent, c.final = &intent, final
	for _, w := range c.writes {
		c.Writes = append(c.Writes, w.path)
	}
	c.Writes = append(c.Writes, c.mpath)
	c.Line = l.on
	if own != nil && own.Complete && own.Release != release {
		c.Line += " It replaces the files an earlier c4 release put there."
	}
	c.Notes = append(c.Notes, l.notes...)
	return nil
}

func sameManifest(a, b *manifest) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func sharedFiles(others []*manifest) map[string]bool {
	s := map[string]bool{}
	for _, m := range others {
		for _, e := range m.Files {
			s[e.Path] = true
		}
		for _, d := range m.Dirs {
			s[d] = true
		}
	}
	return s
}

// missingDirs are dir and its ancestors that don't exist, up to the home
// folder, shallowest first.
func (r *Roots) missingDirs(dir string) []string {
	var out []string
	home := filepath.Clean(r.Home)
	for d := filepath.Clean(dir); d != home && strings.HasPrefix(d, home+string(filepath.Separator)); d = filepath.Dir(d) {
		if isDir(d) {
			break
		}
		out = append([]string{d}, out...)
	}
	return out
}

// addLine puts line at the end of cur, replacing any line carrying marker
// (an earlier release's). addedNL says it ended cur's last line first.
func addLine(cur []byte, line, marker string) ([]byte, bool) {
	base := removeLine(cur, marker)
	addedNL := len(base) > 0 && base[len(base)-1] != '\n'
	out := append([]byte{}, base...)
	if addedNL {
		out = append(out, '\n')
	}
	out = append(out, line...)
	return append(out, '\n'), addedNL
}

// removeLine takes out every line ending with marker.
func removeLine(cur []byte, marker string) []byte {
	var out []byte
	for _, l := range bytes.SplitAfter(cur, []byte("\n")) {
		if bytes.HasSuffix(bytes.TrimRight(l, "\r\n"), []byte(marker)) {
			continue
		}
		out = append(out, l...)
	}
	return out
}

func (c *Change) planOff(f Found, l layout, own *manifest, others []*manifest) error {
	if own == nil {
		c.Line = "C4 isn't in " + f.Title + "; nothing to take out."
		return nil
	}
	shared := sharedFiles(others)
	sharedLines := map[string]bool{}
	for _, m := range others {
		for _, le := range m.Lines {
			sharedLines[le.Path+"\x00"+le.Line] = true
		}
	}
	known := knownDigests(own)
	for p, data := range l.files {
		if known[p] == nil {
			known[p] = map[string]bool{}
		}
		known[p][digest(data)] = true
	}

	for _, le := range own.Lines {
		if sharedLines[le.Path+"\x00"+le.Line] {
			continue
		}
		cur, exists, err := fileState(le.Path)
		if err != nil {
			return err
		}
		marker := markerOf(le.Line)
		if !exists || marker == "" {
			continue
		}
		out := removeLine(cur, marker)
		if bytes.Equal(out, cur) {
			continue
		}
		if le.AddedNewline && bytes.HasSuffix(out, []byte("\n")) {
			out = out[:len(out)-1]
		}
		lc := lineChange{path: le.Path, expect: digest(cur), data: out, existed: true}
		if le.Created && len(out) == 0 {
			lc.remove = true
			c.Removes = append(c.Removes, le.Path)
		} else {
			c.Writes = append(c.Writes, le.Path)
		}
		c.edits = append(c.edits, lc)
	}

	dirs := map[string]bool{}
	for _, d := range own.Dirs {
		if !shared[d] {
			dirs[d] = true
		}
	}
	for _, e := range own.Files {
		if shared[e.Path] {
			continue
		}
		cur, exists, err := fileState(e.Path)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if !known[e.Path][digest(cur)] {
			c.Notes = append(c.Notes, "Left "+e.Path+" where it is: it has changed since c4 put it there.")
			continue
		}
		c.removes = append(c.removes, fileRemove{path: e.Path, expect: digest(cur)})
		c.Removes = append(c.Removes, e.Path)
		if stem, ok := strings.CutSuffix(filepath.Base(e.Path), ".py"); ok {
			// Python's compiled copies of c4's modules.
			cache := filepath.Join(filepath.Dir(e.Path), "__pycache__")
			pycs, _ := filepath.Glob(filepath.Join(cache, stem+".*.pyc"))
			for _, p := range pycs {
				c.removes = append(c.removes, fileRemove{path: p})
				c.Removes = append(c.Removes, p)
			}
			if len(pycs) > 0 {
				dirs[cache] = true
			}
		}
	}
	for _, p := range l.derived {
		if _, exists, _ := fileState(p); exists {
			c.removes = append(c.removes, fileRemove{path: p})
			c.Removes = append(c.Removes, p)
		}
	}
	c.removes = append(c.removes, fileRemove{path: c.mpath})
	c.Removes = append(c.Removes, c.mpath)
	for d := range dirs {
		c.rmdirs = append(c.rmdirs, d)
	}
	sort.Slice(c.rmdirs, func(i, j int) bool { // deepest first
		a, b := strings.Count(c.rmdirs[i], string(filepath.Separator)), strings.Count(c.rmdirs[j], string(filepath.Separator))
		if a != b {
			return a > b
		}
		return c.rmdirs[i] > c.rmdirs[j]
	})
	for _, d := range c.rmdirs {
		c.Removes = append(c.Removes, d+string(filepath.Separator))
	}
	c.Line = l.off
	return nil
}

// markerOf is the marker a line c4 added ends with.
func markerOf(line string) string {
	i := strings.LastIndex(line, "# added by c4 farm integrate ")
	if i < 0 {
		return ""
	}
	return line[i:]
}

// Apply does what Plan said: every file written whole (a temporary file
// renamed into place; a new folder built beside its place and renamed in),
// the manifest recorded before and after, and on off exactly the
// manifest's files removed. If anything on disk changed since Plan, it
// stops and says so.
func Apply(c Change) error {
	if c.roots == nil {
		return errors.New("apply a Change that Plan made")
	}
	for _, w := range c.writes {
		if err := expectState(w.path, w.expect); err != nil {
			return err
		}
	}
	for _, lc := range c.edits {
		if err := expectState(lc.path, lc.expect); err != nil {
			return err
		}
	}
	for _, rm := range c.removes {
		if rm.expect == "" {
			continue
		}
		if err := expectState(rm.path, rm.expect); err != nil && !errors.Is(err, errGone) {
			return err
		}
	}
	if c.on {
		return c.applyOn()
	}
	return c.applyOff()
}

var errGone = errors.New("gone")

func expectState(p, want string) error {
	cur, exists, err := fileState(p)
	if err != nil {
		return err
	}
	switch {
	case want == "" && exists:
		return fmt.Errorf("%s appeared since the plan; plan again", p)
	case want != "" && !exists:
		return fmt.Errorf("%s: %w since the plan", p, errGone)
	case want != "" && digest(cur) != want:
		return fmt.Errorf("%s changed since the plan; plan again", p)
	}
	return nil
}

func (c *Change) applyOn() error {
	if c.final == nil {
		return nil // nothing to change
	}
	missing := c.roots.missingDirs(c.roots.manifestDir())
	if err := os.MkdirAll(c.roots.manifestDir(), 0o755); err != nil {
		return err
	}
	// The manifest is written first, naming everything this may write, so
	// even a crash part-way leaves nothing of c4's unrecorded; off removes
	// only files whose contents are c4's. If a write fails, the manifest
	// is then cut down to what is really there.
	if err := writeManifest(c.mpath, c.intent); err != nil {
		for i := len(missing) - 1; i >= 0; i-- {
			os.Remove(missing[i]) // only the empty folders just made
		}
		return err
	}
	err := c.writeAll()
	if err == nil {
		err = writeManifest(c.mpath, c.final)
	}
	if err != nil {
		c.settle()
	}
	return err
}

// settle rewrites an interrupted install's manifest to name only what is
// on disk and c4's, or removes it (and the empty folders it made) when
// nothing is.
func (c *Change) settle() {
	m := *c.intent
	m.Files, m.Lines, m.Dirs = nil, nil, nil
	for _, e := range c.intent.Files {
		cur, exists, err := fileState(e.Path)
		if err != nil || !exists || (digest(cur) != e.SHA256 && digest(cur) != e.Prev) {
			continue
		}
		m.Files = append(m.Files, fileEntry{Path: e.Path, SHA256: digest(cur)})
	}
	for _, le := range c.intent.Lines {
		if cur, exists, _ := fileState(le.Path); exists && bytes.Contains(cur, []byte(le.Line)) {
			m.Lines = append(m.Lines, le)
		}
	}
	for _, d := range c.intent.Dirs {
		if isDir(d) {
			m.Dirs = append(m.Dirs, d)
		}
	}
	if len(m.Files) > 0 || len(m.Lines) > 0 {
		_ = writeManifest(c.mpath, &m)
		return
	}
	if err := os.Remove(c.mpath); err != nil {
		return
	}
	sort.Slice(m.Dirs, func(i, j int) bool { return len(m.Dirs[i]) > len(m.Dirs[j]) })
	for _, d := range m.Dirs {
		if ents, err := os.ReadDir(d); err == nil && len(ents) == 0 {
			os.Remove(d)
		}
	}
}

// writeAll writes the files, the lines and the removals of an on.
func (c *Change) writeAll() error {
	// Files under a folder that doesn't exist yet are built in a temporary
	// sibling of the topmost missing folder, which is then renamed in.
	trees := map[string][]fileWrite{}
	var order []string
	for _, w := range c.writes {
		top := ""
		if miss := c.roots.missingDirs(filepath.Dir(w.path)); len(miss) > 0 {
			top = miss[0]
		}
		if top == "" {
			if err := writeAtomic(w.path, w.data, 0o644); err != nil {
				return err
			}
			continue
		}
		if trees[top] == nil {
			order = append(order, top)
		}
		trees[top] = append(trees[top], w)
	}
	for _, top := range order {
		if err := writeTree(top, trees[top]); err != nil {
			return err
		}
	}

	for _, lc := range c.edits {
		if err := os.MkdirAll(filepath.Dir(lc.path), 0o755); err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if st, err := os.Stat(lc.path); err == nil {
			mode = st.Mode().Perm()
		}
		if err := writeAtomic(lc.path, lc.data, mode); err != nil {
			return err
		}
	}
	for _, rm := range c.removes {
		if err := os.Remove(rm.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (c *Change) applyOff() error {
	for _, lc := range c.edits {
		if lc.remove {
			if err := os.Remove(lc.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		mode := fs.FileMode(0o644)
		if st, err := os.Stat(lc.path); err == nil {
			mode = st.Mode().Perm()
		}
		if err := writeAtomic(lc.path, lc.data, mode); err != nil {
			return err
		}
	}
	// The manifest goes last, so an interrupted off can be run again.
	for _, rm := range c.removes {
		if rm.path == c.mpath {
			continue
		}
		if err := os.Remove(rm.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(c.mpath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, d := range c.rmdirs {
		ents, err := os.ReadDir(d)
		if err != nil || len(ents) > 0 {
			continue // gone already, or holds something that isn't c4's
		}
		if err := os.Remove(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func writeManifest(p string, m *manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(p, append(b, '\n'), 0o644)
}

// writeData writes b to f, letting a test fail it part-way.
func writeData(f *os.File, dest string, b []byte) error {
	if failWrite != nil {
		if _, err := f.Write(b[:len(b)/2]); err != nil {
			return err
		}
		if err := failWrite(dest); err != nil {
			return err
		}
		_, err := f.Write(b[len(b)/2:])
		return err
	}
	_, err := f.Write(b)
	return err
}

// writeAtomic replaces a file whole: anything reading it sees the old
// contents or the new, never part of either.
func writeAtomic(p string, b []byte, mode fs.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".c4-tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if err = writeData(f, p, b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

// writeTree builds the folder top, holding ws, as a temporary sibling and
// renames it into place, so top appears complete or not at all.
func writeTree(top string, ws []fileWrite) (err error) {
	tmp, err := os.MkdirTemp(filepath.Dir(top), "."+filepath.Base(top)+".c4-tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmp) // only ever c4's own temporary folder
		}
	}()
	if err = os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	for _, w := range ws {
		rel, rerr := filepath.Rel(top, w.path)
		if rerr != nil {
			return rerr
		}
		dst := filepath.Join(tmp, rel)
		if err = os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		f, ferr := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if ferr != nil {
			return ferr
		}
		if err = writeData(f, w.path, w.data); err != nil {
			f.Close()
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
	}
	return os.Rename(tmp, top)
}
