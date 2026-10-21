//go:build !unix && !windows

package dcc

import "os"

// No file locks on this system: runs are not kept apart.
const removeLockWhileHeld = true

func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
