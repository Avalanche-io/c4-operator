package view

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/Avalanche-io/c4"
)

// HashFile computes the C4 ID of a file.
func HashFile(p string) (c4.ID, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return c4.ID{}, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return c4.ID{}, 0, err
	}
	return c4.Identify(f), st.Size(), nil
}

// ScanDir reads a directory into a View under the view projection,
// hashing every regular file. `c4-operator tree-id` uses it so a pod can
// check that its mount is exactly the view in its Materialization's
// status.
func ScanDir(dir string) (*View, error) { return ScanDirKeep(dir, nil) }

// ScanDirKeep is ScanDir over only what keep keeps: keep is asked of each
// path under dir (slash-separated, relative to it), and a directory it
// does not keep is not read. A nil keep keeps everything.
func ScanDirKeep(dir string, keep func(rel string, isDir bool) bool) (*View, error) {
	root := NewNode("")
	type job struct {
		path string
		node *Node
		name string
		exec bool
	}
	var jobs []job
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if keep != nil && !keep(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		parent, name := SplitPath(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			root.Dir(rel + "/")
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			root.Dir(parent).Files[name] = SymlinkEntry(name, target)
		case info.Mode().IsRegular():
			jobs = append(jobs, job{path: p, node: root.Dir(parent), name: name, exec: info.Mode()&0o111 != 0})
		default:
			return fmt.Errorf("%s: not a regular file, directory or symlink", rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		first error
		ch    = make(chan job)
	)
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				id, size, err := HashFile(j.path)
				mu.Lock()
				if err != nil && first == nil {
					first = err
				}
				j.node.Files[j.name] = FileEntry(j.name, size, id, j.exec)
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	if first != nil {
		return nil, first
	}
	return FromNode(root), nil
}

// ScanID is ScanDir's view ID.
func ScanID(dir string) (c4.ID, error) {
	v, err := ScanDir(dir)
	if err != nil {
		return c4.ID{}, err
	}
	return v.ID, nil
}
