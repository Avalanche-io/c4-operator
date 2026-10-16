//go:build unix

package gitimport

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f, released when f closes.
func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }
