//go:build unix

package farmcli

import (
	"os"
	"syscall"
)

// fileKey is a file's identity: its device and inode.
type fileKey struct{ dev, ino uint64 }

func keyOf(fi os.FileInfo) (fileKey, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}, false
	}
	return fileKey{uint64(st.Dev), uint64(st.Ino)}, true
}
