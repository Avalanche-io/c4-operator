//go:build windows

package dcc

import (
	"os"

	"golang.org/x/sys/windows"
)

// removeLockWhileHeld: Windows can't remove a file another process has
// open, so the lock file is removed after it is let go, or left.
const removeLockWhileHeld = false

func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped))
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
