//go:build darwin

package objcache

import (
	"bytes"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// cloneOpenFile clones an open regular file to dst, which must not exist,
// with fclonefileat(2): APFS shares the blocks until either side writes.
// The clone is the cache's own file, so it keeps none of the source's
// extended attributes (every layer linking the object would show them).
// ACLs are not copied unless asked for.
//
// A file with any flag set is not cloned: the caller copies its bytes
// instead. A clone carries the flags, and they can't simply be cleared: a
// transparently compressed file (UF_COMPRESSED, as ditto, installers and
// toolchains write) keeps its bytes in an extended attribute, so the clone
// with the flag cleared reads as empty, and a locked clone can be neither
// chmodded nor removed. Reading through the descriptor gives the real
// bytes in every case.
func cloneOpenFile(f *os.File, dst string) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Flags != 0 {
		return errFlagged
	}
	if err := unix.Fclonefileat(int(f.Fd()), unix.AT_FDCWD, dst, 0); err != nil {
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

// errFlagged says a file has flags set and must be copied, not cloned.
var errFlagged = errors.New("the file has flags set")
