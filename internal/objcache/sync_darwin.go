//go:build darwin

package objcache

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncData is fsync(2): the file's bytes are handed to the drive. That
// survives a crash of the process or of the operating system. It is not
// F_FULLFSYNC (what os.File.Sync does here), which also empties the
// drive's own cache and costs about 6 ms an object, where writing and
// naming a small object costs 8 to 20 µs more with fsync(2) than without;
// so after a power cut on a drive that reorders its writes, a name may
// exist over bytes that never landed.
func syncData(f *os.File) error { return unix.Fsync(int(f.Fd())) }
