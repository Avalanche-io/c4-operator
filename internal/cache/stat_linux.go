package cache

import (
	"os"
	"syscall"
	"time"
)

func linkInfo(p string) (uint64, time.Time, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return 0, time.Time{}, err
	}
	st := fi.Sys().(*syscall.Stat_t)
	return uint64(st.Nlink), time.Unix(st.Atim.Sec, st.Atim.Nsec), nil
}
