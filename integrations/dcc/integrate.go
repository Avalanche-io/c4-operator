package dcc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
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
	seen    map[string]string // every manifest's digest when planned
	final   *manifest         // on: the manifest once done
	intent  *manifest         // on: written before anything else, so an interrupted Apply is still undoable
	heir    *manifest         // off: another version's manifest, given the files the versions share
	writes  []fileWrite
	edits   []lineChange
	removes []fileRemove
	stale   []string // c4's own temporary files or folders an earlier run left
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
	path    string // the file written: a symbolic link's target, not the link
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
	// Prev is another contents c4 put there: the file's before an install
	// under way, or another version's when it was handed this entry.
	Prev string `json:"prev,omitempty"`
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

// tmpMark is in the name of every temporary file or folder c4 makes. Each
// starts with a dot, which Blender's add-on and start-up scans skip.
const tmpMark = ".c4-tmp-"

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
	sort.Strings(ps)
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

// manifestDigests are every manifest file's digest, to tell whether
// another run of c4 changed any since a plan.
func (r *Roots) manifestDigests() map[string]string {
	ps, _ := filepath.Glob(filepath.Join(r.manifestDir(), "*.json"))
	out := map[string]string{}
	for _, p := range ps {
		if b, err := os.ReadFile(p); err == nil {
			out[p] = digest(b)
		}
	}
	return out
}

// versionShape is what a version looks like for each application.
var versionShape = map[string]*regexp.Regexp{
	"blender": bareMM,
	"unreal":  bareMM,
	"houdini": bareMM,
	"maya":    regexp.MustCompile(`^\d{4}$`),
	"nuke":    bareMM,
	"katana":  bareMM,
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
	shape := versionShape[f.Name]
	if shape == nil {
		return Change{}, fmt.Errorf("C4 has no integration for %q", f.Name)
	}
	if !shape.MatchString(f.Version) {
		return Change{}, fmt.Errorf("%q isn't a %s version", f.Version, titles[f.Name])
	}
	if f.Title == "" {
		f.Title = titles[f.Name] + " " + f.Version
	}
	if r.Home == "" || !isDir(r.Home) {
		return Change{}, fmt.Errorf("no home folder to install into (%q)", r.Home)
	}
	l, err := r.layout(f, on)
	if err != nil {
		return Change{}, err
	}
	c := Change{roots: r, on: on, mpath: r.manifestPath(f.Name, f.Version), seen: r.manifestDigests()}
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
	var touched []string
	for p := range l.files {
		touched = append(touched, p)
	}
	for _, ed := range l.edits {
		touched = append(touched, ed.path)
	}
	if own != nil {
		for _, e := range own.Files {
			touched = append(touched, e.Path)
		}
	}
	c.findStale(touched)
	sort.Strings(c.Writes)
	return c, nil
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// fileState reads a file: its contents, and whether it exists.
func fileState(p string) ([]byte, bool, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// resolveLink is the file a person's start-up file really is: p, or what
// p links to. A link to nothing is refused rather than read as no file.
func resolveLink(p string) (string, bool, error) {
	st, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return p, false, nil
	}
	if err != nil {
		return "", false, err
	}
	if st.Mode()&fs.ModeSymlink == 0 {
		return p, true, nil
	}
	t, err := filepath.EvalSymlinks(p)
	if err != nil {
		dest, _ := os.Readlink(p)
		return "", false, fmt.Errorf("%s is a link to %s, which isn't there; c4 won't replace the link. Restore the file it points to, or remove the link, and run this again", p, dest)
	}
	return t, true, nil
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

// recordedLine is the entry a manifest has for c4's line in path.
func recordedLine(ms []*manifest, path, marker string) *lineEntry {
	for _, m := range ms {
		if m == nil {
			continue
		}
		for i, le := range m.Lines {
			if le.Path == path && markerOf(le.Line) == marker {
				return &m.Lines[i]
			}
		}
	}
	return nil
}

func (c *Change) planOn(f Found, l layout, own *manifest, others []*manifest) error {
	all := append([]*manifest{own}, others...)
	known := knownDigests(all...)
	final := &manifest{Name: f.Name, Version: f.Version, Release: release, Complete: true}
	var prev []fileEntry // what the intent manifest records beyond final
	wanted := map[string]bool{}
	keep := func(e fileEntry) {
		wanted[e.Path] = true
		final.Files = append(final.Files, fileEntry{Path: e.Path, SHA256: e.SHA256})
		prev = append(prev, e)
	}

	paths := make([]string, 0, len(l.files))
	for p := range l.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		data := l.files[p]
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
		keep(e)
	}

	// The person's own files: one marked line each.
	for _, ed := range l.edits {
		target, exists, err := resolveLink(ed.path)
		if err != nil {
			return fmt.Errorf("can't add C4 to %s: %w", l.title, err)
		}
		var cur []byte
		if exists {
			if cur, err = os.ReadFile(target); err != nil {
				return err
			}
		}
		out, addedNL := addLine(cur, ed.line, ed.marker)
		entry := lineEntry{Path: ed.path, Line: ed.line, Created: !exists, AddedNewline: addedNL}
		if rec := recordedLine(all, ed.path, ed.marker); rec != nil { // keep how the file was before c4 first changed it
			entry.Created, entry.AddedNewline = rec.Created, rec.AddedNewline || addedNL
		}
		final.Lines = append(final.Lines, entry)

		// The backup is of the file before c4 first changed it, and is
		// never replaced once made.
		backup := ed.path + ".c4-backup"
		bcur, bexists, err := fileState(backup)
		if err != nil {
			return err
		}
		if bexists {
			if !known[backup][digest(bcur)] {
				return fmt.Errorf("can't add C4 to %s: %s is already there and isn't c4's; set it aside and run this again", l.title, backup)
			}
			keep(fileEntry{Path: backup, SHA256: digest(bcur)})
		}
		if exists && bytes.Equal(out, cur) {
			continue // the line is already there
		}
		lc := lineChange{path: target, data: out, existed: exists}
		if exists {
			lc.expect = digest(cur)
			if !bexists {
				c.writes = append(c.writes, fileWrite{path: backup, data: cur})
				keep(fileEntry{Path: backup, SHA256: digest(cur)})
			}
		}
		c.edits = append(c.edits, lc)
		c.Writes = append(c.Writes, target)
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

	if own != nil && len(c.writes) == 0 && len(c.edits) == 0 && len(c.removes) == 0 && sameManifest(own, final) {
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
	if lock := filepath.Join(c.roots.manifestDir(), lockName); !fileExists(lock) {
		c.Writes = append(c.Writes, lock) // the lock Apply holds, kept while c4 has files here
	}
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

// addLine is cur with c4's line in it: unchanged if the line is there; an
// earlier release's marked line replaced where it stands; else the line
// added at the end. addedNL says it ended cur's last line first.
func addLine(cur []byte, line, marker string) ([]byte, bool) {
	var out []byte
	placed := false
	for _, l := range bytes.SplitAfter(cur, []byte("\n")) {
		body := bytes.TrimRight(l, "\r\n")
		if len(l) == 0 || !bytes.HasSuffix(body, []byte(marker)) {
			out = append(out, l...)
			continue
		}
		if placed {
			continue // a second marked line
		}
		placed = true
		out = append(out, line...)
		out = append(out, l[len(body):]...) // its own line ending
	}
	if placed {
		return out, false
	}
	addedNL := len(out) > 0 && out[len(out)-1] != '\n'
	if addedNL {
		out = append(out, '\n')
	}
	out = append(out, line...)
	return append(out, '\n'), addedNL
}

// removeLine takes out every line ending with marker. last says one of
// them was the file's last line.
func removeLine(cur []byte, marker string) (out []byte, last bool) {
	parts := bytes.SplitAfter(cur, []byte("\n"))
	for len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	for i, l := range parts {
		if bytes.HasSuffix(bytes.TrimRight(l, "\r\n"), []byte(marker)) {
			last = last || i == len(parts)-1
			continue
		}
		out = append(out, l...)
	}
	return out, last
}

func (c *Change) planOff(f Found, l layout, own *manifest, others []*manifest) error {
	if own == nil {
		c.Line = "C4 isn't in " + f.Title + "; nothing to take out."
		return nil
	}
	shared := sharedFiles(others)
	known := knownDigests(own)
	for p, data := range l.files {
		if known[p] == nil {
			known[p] = map[string]bool{}
		}
		known[p][digest(data)] = true
	}

	// While another version of the application has C4 on, the files the
	// versions share (the panel under ~/.c4/integrations/<name>, the
	// per-user start-up file, c4's line and its backup) stay, and their
	// entries are handed to that version's manifest, whose off removes them.
	var heir *manifest
	for _, m := range others {
		if m.Name == f.Name {
			h := *m
			h.Files = append([]fileEntry{}, m.Files...)
			h.Lines = append([]lineEntry{}, m.Lines...)
			h.Dirs = append([]string{}, m.Dirs...)
			heir = &h
			break
		}
	}
	payload := c.roots.payloadDir(f.Name) + string(filepath.Separator)
	backups := map[string]bool{}
	for _, le := range own.Lines {
		backups[le.Path+".c4-backup"] = true
	}
	handed := false
	var kept []string
	keepForHeir := func(e fileEntry) bool {
		if heir == nil || !(strings.HasPrefix(e.Path, payload) || l.shared[e.Path] || backups[e.Path]) {
			return false
		}
		kept = append(kept, e.Path)
		for i := range heir.Files {
			if heir.Files[i].Path == e.Path {
				if heir.Files[i].SHA256 != e.SHA256 && heir.Files[i].Prev == "" {
					heir.Files[i].Prev, handed = e.SHA256, true
				}
				return true
			}
		}
		heir.Files = append(heir.Files, fileEntry{Path: e.Path, SHA256: e.SHA256})
		handed = true
		return true
	}

	for _, le := range own.Lines {
		marker := markerOf(le.Line)
		if marker == "" || recordedLine(others, le.Path, marker) != nil {
			continue // another version's line too
		}
		if heir != nil {
			heir.Lines = append(heir.Lines, le)
			handed = true
			continue
		}
		target, exists, err := resolveLink(le.Path)
		if err != nil {
			return fmt.Errorf("can't take C4 out of %s: %w", l.title, err)
		}
		if !exists {
			continue
		}
		cur, err := os.ReadFile(target)
		if err != nil {
			return err
		}
		out, last := removeLine(cur, marker)
		if bytes.Equal(out, cur) {
			continue
		}
		if le.AddedNewline && last { // give back the person's last line as it ended
			if bytes.HasSuffix(out, []byte("\r\n")) {
				out = out[:len(out)-2]
			} else if bytes.HasSuffix(out, []byte("\n")) {
				out = out[:len(out)-1]
			}
		}
		lc := lineChange{path: target, expect: digest(cur), data: out, existed: true}
		if le.Created && len(out) == 0 && target == le.Path {
			lc.remove = true
			c.Removes = append(c.Removes, target)
		} else {
			c.Writes = append(c.Writes, target)
		}
		c.edits = append(c.edits, lc)
	}

	dirs := map[string]bool{}
	for _, e := range own.Files {
		if shared[e.Path] || keepForHeir(e) {
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
	for _, d := range own.Dirs {
		if shared[d] {
			continue
		}
		holds := false
		for _, p := range kept {
			if strings.HasPrefix(p, d+string(filepath.Separator)) {
				holds = true
				break
			}
		}
		if holds { // a folder the kept files are in: the heir removes it
			heir.Dirs = append(heir.Dirs, d)
			handed = true
			continue
		}
		dirs[d] = true
	}
	for _, p := range l.derived {
		if _, exists, _ := fileState(p); exists {
			c.removes = append(c.removes, fileRemove{path: p})
			c.Removes = append(c.Removes, p)
		}
	}
	if handed {
		sort.Strings(heir.Dirs)
		c.heir = heir
		c.Writes = append(c.Writes, c.roots.manifestPath(heir.Name, heir.Version))
		c.Notes = append(c.Notes, fmt.Sprintf("C4's files that every %s version shares stay for %s %s, which still has C4 on.", titles[f.Name], titles[f.Name], heir.Version))
	}
	c.removes = append(c.removes, fileRemove{path: c.mpath})
	c.Removes = append(c.Removes, c.mpath)
	if lock := filepath.Join(c.roots.manifestDir(), lockName); len(others) == 0 && fileExists(lock) {
		c.Removes = append(c.Removes, lock) // with the last integration, the lock Apply holds goes too
	}
	for d := range dirs {
		c.rmdirs = append(c.rmdirs, d)
	}
	sortDeepestFirst(c.rmdirs)
	for _, d := range c.rmdirs {
		c.Removes = append(c.Removes, d+string(filepath.Separator))
	}
	c.Line = l.off
	return nil
}

func sortDeepestFirst(dirs []string) {
	sort.Slice(dirs, func(i, j int) bool {
		a, b := strings.Count(dirs[i], string(filepath.Separator)), strings.Count(dirs[j], string(filepath.Separator))
		if a != b {
			return a > b
		}
		return dirs[i] > dirs[j]
	})
}

// findStale lists c4's own temporary files and folders, left by a run that
// was killed, in the folders this integration's files are in.
func (c *Change) findStale(touched []string) {
	dirs := map[string]bool{c.roots.manifestDir(): true}
	nearest := func(p string) {
		d := filepath.Dir(p)
		for !isDir(d) && d != filepath.Dir(d) {
			d = filepath.Dir(d)
		}
		dirs[d] = true
	}
	for _, p := range touched {
		nearest(p)
	}
	for _, w := range c.writes {
		nearest(w.path)
	}
	for _, lc := range c.edits {
		nearest(lc.path)
	}
	for _, rm := range c.removes {
		nearest(rm.path)
	}
	for d := range dirs {
		ms, _ := filepath.Glob(filepath.Join(d, ".*"+tmpMark+"*"))
		c.stale = append(c.stale, ms...)
	}
	sort.Strings(c.stale)
	c.Removes = append(c.Removes, c.stale...)
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
// manifest's files removed. It holds a lock in ~/.c4/integrations while it
// works, so two runs of c4 never interleave. If anything on disk changed
// since Plan (another run's manifests included), it stops and says so.
func Apply(c Change) error {
	if c.roots == nil {
		return errors.New("apply a Change that Plan made")
	}
	if c.final == nil && len(c.edits) == 0 && len(c.removes) == 0 && len(c.stale) == 0 {
		return nil // nothing to change
	}
	made := c.roots.missingDirs(c.roots.manifestDir())
	lk, err := c.roots.lock()
	if err != nil {
		return err
	}
	cleanup, err := c.applyLocked()
	cleanup = append(cleanup, made...)
	removeEmptyDirs(cleanup)
	lk.release()
	// The manifest folder and those above it go only once the lock file
	// in it has.
	removeEmptyDirs(cleanup)
	return err
}

func (c *Change) applyLocked() ([]string, error) {
	if !maps.Equal(c.roots.manifestDigests(), c.seen) {
		return nil, errors.New("another run of c4 changed C4's integrations since the plan; plan again")
	}
	for _, w := range c.writes {
		if err := expectState(w.path, w.expect); err != nil {
			return nil, err
		}
	}
	for _, lc := range c.edits {
		if err := expectState(lc.path, lc.expect); err != nil {
			return nil, err
		}
	}
	for _, rm := range c.removes {
		if rm.expect == "" {
			continue
		}
		if err := expectState(rm.path, rm.expect); err != nil && !errors.Is(err, errGone) {
			return nil, err
		}
	}
	for _, p := range c.stale {
		if strings.Contains(filepath.Base(p), tmpMark) {
			if err := os.RemoveAll(p); err != nil {
				return nil, err
			}
		}
	}
	if c.on {
		return c.applyOn()
	}
	return c.rmdirs, c.applyOff()
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

func (c *Change) applyOn() ([]string, error) {
	if c.final == nil {
		return nil, nil // only stale temporary files to clear
	}
	// The manifest is written first, naming everything this may write, so
	// even a crash part-way leaves nothing of c4's unrecorded; off removes
	// only files whose contents are c4's. If a write fails, the manifest
	// is then cut down to what is really there.
	if err := writeManifest(c.mpath, c.intent); err != nil {
		return nil, err
	}
	err := c.writeAll()
	if err == nil {
		err = writeManifest(c.mpath, c.final)
	}
	if err != nil {
		return c.settle(), err
	}
	return nil, nil
}

// settle rewrites an interrupted install's manifest to name only what is
// on disk and c4's, or removes it when nothing is, and returns the folders
// to remove if they are now empty.
func (c *Change) settle() []string {
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
		return nil
	}
	if err := os.Remove(c.mpath); err != nil {
		return nil
	}
	sortDeepestFirst(m.Dirs)
	return m.Dirs
}

// removeEmptyDirs removes each folder that is empty, deepest first.
func removeEmptyDirs(dirs []string) {
	dirs = append([]string{}, dirs...)
	sortDeepestFirst(dirs)
	for _, d := range dirs {
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
		if err := writeEdit(lc); err != nil {
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

// writeEdit writes a person's start-up file whole, keeping its mode. A
// linked file's target is written, in the target's own folder, so the
// link stays a link.
func writeEdit(lc lineChange) error {
	if lc.remove {
		if err := os.Remove(lc.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(lc.path), 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if st, err := os.Stat(lc.path); err == nil {
		mode = st.Mode().Perm()
	}
	return writeAtomic(lc.path, lc.data, mode)
}

func (c *Change) applyOff() error {
	for _, lc := range c.edits {
		if err := writeEdit(lc); err != nil {
			return err
		}
	}
	// The other version's manifest takes the shared files before this one
	// lets them go; this manifest goes last, so an interrupted off can be
	// run again.
	if c.heir != nil {
		if err := writeManifest(c.roots.manifestPath(c.heir.Name, c.heir.Version), c.heir); err != nil {
			return err
		}
	}
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
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+tmpMark+"*")
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
	tmp, err := os.MkdirTemp(filepath.Dir(top), "."+filepath.Base(top)+tmpMark+"*")
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
