//go:build darwin

package objcache

import "golang.org/x/sys/unix"

// cloneFile clones one file with clonefile(2): APFS shares its blocks until
// either side writes. dst must not exist.
func cloneFile(src, dst string) error { return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) }
