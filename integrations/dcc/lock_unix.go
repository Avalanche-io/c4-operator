//go:build unix

package dcc

import (
	"os"
	"syscall"
)

// removeLockWhileHeld: on Unix a run waiting on the file checks, once it
// has the lock, that the file is still the one at the path.
const removeLockWhileHeld = true

func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
