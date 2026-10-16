// Package tree assembles a view's directory tree from cached objects.
//
// Files are hardlinked from the cache, so a tree costs inodes, not
// bytes, and fifty pods reading the same plates read one copy. link(2)
// cannot cross filesystems or even two mounts of one filesystem
// (EXDEV); in that case the file is copied instead, and the copy is
// reported so the cost is visible.
//
// A tree is built in a temporary sibling directory and renamed into
// place, so a reader sees either no tree or a complete one.
package tree

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4-operator/internal/manifest"
)

// ObjectPath returns the local path of a cached object.
type ObjectPath func(c4.ID) (string, error)

// Stats reports what a build did.
type Stats struct {
	Linked      int64
	BytesLinked int64
	Copied      int64
	BytesCopied int64
}

// Build materializes v at dst, replacing whatever is there.
// mtime is applied to copied files so they match linked ones.
func Build(dst string, v *manifest.View, obj ObjectPath, mtime time.Time) (Stats, error) {
	var st Stats
	tmp := filepath.Join(filepath.Dir(dst), ".build-"+filepath.Base(dst)+"-"+randSuffix())
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return st, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()

	for _, d := range v.Dirs {
		if err := os.MkdirAll(filepath.Join(tmp, filepath.FromSlash(d)), 0o755); err != nil {
			return st, err
		}
	}
	for _, f := range v.Files {
		target := filepath.Join(tmp, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return st, err
		}
		n, copied, err := place(f.ID, target, obj, mtime)
		if err != nil {
			return st, fmt.Errorf("%s: %w", f.Path, err)
		}
		if copied {
			st.Copied++
			st.BytesCopied += n
			continue
		}
		st.Linked++
		st.BytesLinked += n
	}
	for _, s := range v.Symlinks {
		target := filepath.Join(tmp, filepath.FromSlash(s.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return st, err
		}
		if err := os.Symlink(s.Target, target); err != nil {
			return st, fmt.Errorf("%s: %w", s.Path, err)
		}
	}

	if err := replace(dst, tmp); err != nil {
		return st, err
	}
	ok = true
	return st, nil
}

// place links (or copies) one object to target and returns its size.
func place(id c4.ID, target string, obj ObjectPath, mtime time.Time) (int64, bool, error) {
	var lastErr error
	// The cache's TreeStore may split a directory between lookup and
	// link, moving the object; resolve again on ENOENT.
	for range 3 {
		src, err := obj(id)
		if err != nil {
			return 0, false, err
		}
		err = os.Link(src, target)
		if err == nil {
			fi, err := os.Lstat(target)
			if err != nil {
				return 0, false, err
			}
			return fi.Size(), false, nil
		}
		if errors.Is(err, syscall.EXDEV) {
			n, err := copyFile(src, target, mtime)
			return n, true, err
		}
		lastErr = err
		if !errors.Is(err, os.ErrNotExist) {
			break
		}
	}
	return 0, false, lastErr
}

func copyFile(src, dst string, mtime time.Time) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	return n, os.Chtimes(dst, time.Now(), mtime)
}

// replace publishes tmp at dst atomically. A previous tree is renamed
// aside, never deleted here: running pods bind-mount it, and deleting
// it would pull files out from under them. RetiredPrefix names it so
// the cache's collector can remove it once no pod uses the view.
func replace(dst, tmp string) error {
	if _, err := os.Lstat(dst); err == nil {
		old := filepath.Join(filepath.Dir(dst), RetiredPrefix+filepath.Base(dst)+"-"+randSuffix())
		if err := os.Rename(dst, old); err != nil {
			return err
		}
	}
	return os.Rename(tmp, dst)
}

// RetiredPrefix starts the name of a tree replaced by a rebuild:
// ".retired-<viewID>-<random>".
const RetiredPrefix = ".retired-"

// Check reports whether the tree at dst is still v as built from the
// cache: every file is the very inode the cache holds for its ID (so a
// refetched object forces a rebuild) and every link points where
// recorded. It returns "" when the tree is good.
func Check(dst string, v *manifest.View, obj ObjectPath) string {
	if _, err := os.Lstat(dst); err != nil {
		return "not built"
	}
	for _, f := range v.Files {
		fi, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(f.Path)))
		if err != nil {
			return f.Path + ": " + err.Error()
		}
		src, err := obj(f.ID)
		if err != nil {
			return f.Path + ": " + err.Error()
		}
		ci, err := os.Lstat(src)
		if err != nil {
			return f.Path + ": " + err.Error()
		}
		if !os.SameFile(fi, ci) {
			return f.Path + ": not linked to the cached object"
		}
	}
	for _, s := range v.Symlinks {
		t, err := os.Readlink(filepath.Join(dst, filepath.FromSlash(s.Path)))
		if err != nil || t != s.Target {
			return s.Path + ": symlink changed"
		}
	}
	return ""
}

func randSuffix() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
