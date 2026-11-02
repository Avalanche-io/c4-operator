//go:build unix

package mcpserver

import (
	"fmt"
	"os"
	"syscall"
)

// ownedByMe refuses a key file or directory another user owns: one
// planted for this user to sign with.
func ownedByMe(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("the agent's key %s: its owner can't be read", path)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("the agent's key %s is owned by user %d, not this user (%d): refused", path, st.Uid, os.Getuid())
	}
	return nil
}
