//go:build linux

package objcache

import (
	"os"
	"syscall"
	"time"
)

func accessTime(fi os.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atim.Sec, st.Atim.Nsec)
	}
	return fi.ModTime()
}
