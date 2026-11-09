//go:build darwin

package objcache

import (
	"bytes"
	"os"

	"golang.org/x/sys/unix"
)

// cloneOpenFile clones an open regular file to dst, which must not exist,
// with fclonefileat(2): APFS shares the blocks until either side writes.
// The clone is the cache's own file, so it keeps none of the source's
// flags (a locked file could be neither chmodded nor removed) and none of
// its extended attributes (every layer linking the object would show
// them). ACLs are not copied unless asked for.
func cloneOpenFile(f *os.File, dst string) error {
	if err := unix.Fclonefileat(int(f.Fd()), unix.AT_FDCWD, dst, 0); err != nil {
		return err
	}
	if err := unix.Chflags(dst, 0); err != nil {
		return err
	}
	size, err := unix.Listxattr(dst, nil)
	if err != nil || size == 0 {
		return err
	}
	buf := make([]byte, size)
	if size, err = unix.Listxattr(dst, buf); err != nil {
		return err
	}
	for _, name := range bytes.Split(buf[:size], []byte{0}) {
		if len(name) == 0 {
			continue
		}
		if err := unix.Removexattr(dst, string(name)); err != nil {
			return err
		}
	}
	return nil
}
