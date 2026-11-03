//go:build darwin

package checkout

import "golang.org/x/sys/unix"

// cloneDir clones the tree at src to dst, which must not exist, with one
// clonefile(2): APFS shares every block until either side writes, and
// keeps each file's mode and times.
func cloneDir(src, dst string) error { return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) }
