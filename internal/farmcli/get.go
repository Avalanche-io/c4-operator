package farmcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/fsname"
)

// getOS is the platform whose rules a get writes under; a test sets it.
var getOS = runtime.GOOS

// GetArgs are `c4 farm get`'s arguments.
type GetArgs struct {
	Project string
	Ref     string   // a job (ns/name or name) or a view's C4 ID
	Paths   []string // paths in the view; empty for all of it
	Dir     string
}

// outputView finds the view a ref names: a C4 ID as given, or a job's
// output view.
func outputView(ctx context.Context, c *Client, project, ref string) (string, string, error) {
	if id, err := c4.Parse(ref); err == nil {
		return project, id.String(), nil
	}
	j, err := findJob(ctx, c, project, ref)
	if err != nil {
		return "", "", err
	}
	if j.Output == nil || j.Output.View == "" {
		return "", "", fmt.Errorf("%s/%s has no output yet (%s)", j.Namespace, j.Name, strings.ToLower(j.Phase))
	}
	return j.Project, j.Output.View, nil
}

// Get writes a view's files (or some of them) under a folder, checking
// each file's C4 ID as it lands; files already there with the right ID
// are left alone.
func Get(ctx context.Context, c *Client, o Out, a GetArgs) error {
	project, vid, err := outputView(ctx, c, a.Project, a.Ref)
	if err != nil {
		return err
	}
	var (
		files []api.ViewFile
		dirs  []string
		links []api.ViewLink
	)
	prefixes := a.Paths
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	for _, p := range prefixes {
		var ls api.ViewListing
		q := "/views/" + project + "/" + vid + "/files"
		if p != "" {
			q += "?path=" + strings.TrimPrefix(filepath.ToSlash(p), "/")
		}
		if err := c.Do(ctx, "GET", q, nil, &ls); err != nil {
			return err
		}
		if ls.Truncated {
			return fmt.Errorf("the view has more files than one listing holds; get it in parts by path")
		}
		files = append(files, ls.Files...)
		dirs = append(dirs, ls.Dirs...)
		links = append(links, ls.Links...)
	}
	if len(files)+len(dirs)+len(links) == 0 {
		return fmt.Errorf("nothing at %s in %s", strings.Join(a.Paths, ", "), vid)
	}
	files, dirs, links, left := writable(getOS, files, dirs, links)
	lost, err := losers(a.Dir, files, dirs, links)
	if err != nil {
		return err
	}
	files, dirs, links, lostLeft := withoutLosers(lost, files, dirs, links)
	left = append(left, lostLeft...)
	var (
		next        atomic.Int64
		wrote, kept atomic.Int64
		bytes       atomic.Int64
		wg          sync.WaitGroup
		mu          sync.Mutex
		first       error
	)
	for range min(8, len(files)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(files) {
					return
				}
				f := files[i]
				fresh, err := fetch(ctx, c, project, a.Dir, f)
				if err != nil {
					mu.Lock()
					first = errors.Join(first, fmt.Errorf("%s: %w", f.Path, err))
					mu.Unlock()
					return
				}
				if fresh {
					wrote.Add(1)
					bytes.Add(f.Size)
				} else {
					kept.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return first
	}
	for _, d := range dirs {
		rel := filepath.FromSlash(strings.TrimSuffix(d, "/"))
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("%s: not a local path", d)
		}
		if err := os.MkdirAll(filepath.Join(a.Dir, rel), 0o755); err != nil {
			return err
		}
	}
	made, skipped, err := makeLinks(a.Dir, links)
	if err != nil {
		return err
	}
	// Every name the view has, in the folder's listing as it is spelled.
	var names []string
	for _, f := range files {
		names = append(names, f.Path)
	}
	for _, d := range dirs {
		names = append(names, d)
	}
	for _, l := range links {
		names = append(names, l.Path)
	}
	for _, p := range append([]string(nil), names...) {
		for d := path.Dir(strings.TrimSuffix(p, "/")); d != "." && d != "/"; d = path.Dir(d) {
			names = append(names, d)
		}
	}
	left = append(left, respell(a.Dir, names)...)
	line := fmt.Sprintf("%s · %s · every ID checked", plural(int(wrote.Load()), "file", "files"), Bytes(bytes.Load()))
	if k := kept.Load(); k > 0 {
		line += fmt.Sprintf(" · %d already here", k)
	}
	if made > 0 {
		line += " · " + plural(made, "link", "links")
	}
	o.printf("%s\n", line)
	for _, s := range append(left, skipped...) {
		o.printf("skipped   %s\n", s)
	}
	o.printf("view      %s\n", vid)
	o.printf("in        %s\n", a.Dir)
	return nil
}

// writable keeps of a listing what this platform can hold as it is, and
// says of the rest which and why. An entry whose name the platform cannot
// hold (fsname.Refusal: on Windows, git's rule) is left out, and so is
// everything under a directory left out; it is never renamed, mapped or
// joined into a path; so is a link whose target holds a name the platform
// cannot hold. The rest of the view is written. Names that differ
// only in case (or in Unicode form) on a folder that takes them for one
// are not looked for here.
func writable(goos string, files []api.ViewFile, dirs []string, links []api.ViewLink) ([]api.ViewFile, []string, []api.ViewLink, []string) {
	var left []string
	keep := func(p string) bool {
		p = strings.TrimSuffix(p, "/")
		seg, r := fsname.PathRefusal(goos, p)
		if r == "" {
			return true
		}
		left = append(left, fmt.Sprintf("%s: this platform cannot hold the name %q as it is: %s", p, seg, r))
		return false
	}
	var kf []api.ViewFile
	for _, f := range files {
		if keep(f.Path) {
			kf = append(kf, f)
		}
	}
	var kd []string
	for _, d := range dirs {
		if keep(d) {
			kd = append(kd, d)
		}
	}
	var kl []api.ViewLink
	for _, l := range links {
		if !keep(l.Path) {
			continue
		}
		// A link's target is a path Windows resolves, so there it is held
		// to the same rule: a backslash in it is a separator, and the link
		// would point at another entry than the view's. Elsewhere a target
		// is bytes the filesystem keeps as they are (APFS takes any,
		// measured), so it is not held to the name rule.
		if seg, r := fsname.TargetRefusal(goos, l.Target); r != "" {
			left = append(left, fmt.Sprintf("%s -> %s: this platform cannot hold the name %q in the link's target as it is: %s", l.Path, l.Target, seg, r))
			continue
		}
		kl = append(kl, l)
	}
	return kf, kd, kl, left
}

// fetch writes one file unless it is already there with its ID.
func fetch(ctx context.Context, c *Client, project, dir string, f api.ViewFile) (bool, error) {
	want, err := c4.Parse(f.ID)
	if err != nil {
		return false, err
	}
	rel := filepath.FromSlash(f.Path)
	if !filepath.IsLocal(rel) {
		return false, fmt.Errorf("not a local path")
	}
	dst := filepath.Join(dir, rel)
	if have, err := os.Open(dst); err == nil {
		got := c4.Identify(have)
		have.Close()
		if got == want {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	body, err := c.Get(ctx, "/objects/"+project+"/"+f.ID)
	if err != nil {
		return false, err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".c4-get-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return false, err
	}
	got := c4.Identify(tmp)
	tmp.Close()
	if got != want {
		return false, fmt.Errorf("arrived as %s, not %s", got, want)
	}
	mode := os.FileMode(0o644)
	if f.Exec {
		mode = 0o755
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), dst)
}

// makeLinks creates the view's symbolic links under dir, after its files.
// A link whose target leaves dir is skipped, so a view fetched into a
// folder never reaches past it, and so is one this machine can't make
// (Windows without the privilege); skipped says which and why.
func makeLinks(dir string, links []api.ViewLink) (made int, skipped []string, err error) {
	for _, l := range links {
		rel := filepath.FromSlash(l.Path)
		if !filepath.IsLocal(rel) {
			return made, skipped, fmt.Errorf("%s: not a local path", l.Path)
		}
		target := filepath.FromSlash(l.Target)
		if filepath.IsAbs(target) || !filepath.IsLocal(filepath.Join(filepath.Dir(rel), target)) {
			skipped = append(skipped, fmt.Sprintf("%s -> %s: the link points outside the folder", l.Path, l.Target))
			continue
		}
		dst := filepath.Join(dir, rel)
		if have, err := os.Readlink(dst); err == nil {
			if have == target {
				continue
			}
			if err := os.Remove(dst); err != nil {
				return made, skipped, err
			}
		} else if _, err := os.Lstat(dst); err == nil {
			return made, skipped, fmt.Errorf("%s: a file is where the view has a link", l.Path)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return made, skipped, err
		}
		if err := os.Symlink(target, dst); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s -> %s: %v", l.Path, l.Target, errors.Unwrap(err)))
			continue
		}
		made++
	}
	return made, skipped, nil
}
