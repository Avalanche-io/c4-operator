package farmcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

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
	var files []api.ViewFile
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
	}
	if len(files) == 0 {
		return fmt.Errorf("nothing at %s in %s", strings.Join(a.Paths, ", "), vid)
	}
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
	line := fmt.Sprintf("%s · %s · every ID checked", plural(int(wrote.Load()), "file", "files"), Bytes(bytes.Load()))
	if k := kept.Load(); k > 0 {
		line += fmt.Sprintf(" · %d already here", k)
	}
	o.printf("%s\n", line)
	o.printf("view      %s\n", vid)
	o.printf("in        %s\n", a.Dir)
	return nil
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
