package dcc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// lockName is the lock file in ~/.c4/integrations that Apply holds.
const lockName = ".lock"

type held struct {
	f    *os.File
	path string
}

// lock takes the exclusive lock in the manifest folder, making the folder
// if it has to. The file can be removed by the holder before it lets go
// (when no integration is left), so a lock taken on a file no longer at
// that path is let go and taken again.
func (r *Roots) lock() (*held, error) {
	dir := r.manifestDir()
	p := filepath.Join(dir, lockName)
	for i := 0; i < 100; i++ {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
		if errors.Is(err, fs.ErrNotExist) {
			continue // the folder was removed between the two steps
		}
		if err != nil {
			return nil, err
		}
		if err := lockFile(f); err != nil {
			f.Close()
			return nil, err
		}
		a, errA := f.Stat()
		b, errB := os.Stat(p)
		if errA == nil && errB == nil && os.SameFile(a, b) {
			return &held{f: f, path: p}, nil
		}
		unlockFile(f)
		f.Close()
	}
	return nil, errors.New("couldn't take the lock in " + dir)
}

// release lets the lock go, removing the lock file when it is all that is
// left in the folder, so turning every integration off leaves nothing.
func (h *held) release() {
	ents, err := os.ReadDir(filepath.Dir(h.path))
	alone := err == nil && len(ents) == 1 && ents[0].Name() == lockName
	if alone && removeLockWhileHeld {
		os.Remove(h.path)
	}
	unlockFile(h.f)
	h.f.Close()
	if alone && !removeLockWhileHeld {
		os.Remove(h.path) // fails, harmlessly, while another run has it open
	}
}
