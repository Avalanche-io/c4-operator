//go:build unix

package mcpserver

import (
	"os"
	"syscall"
	"testing"
)

// otherOwner is a file's details as another user owns it.
type otherOwner struct{ os.FileInfo }

func (otherOwner) Sys() any { return &syscall.Stat_t{Uid: uint32(os.Getuid() + 1)} }

// A key file or directory another user owns is refused: planted for this
// user to sign with. This user's own passes.
func TestKeyOwnedByAnotherUserRefused(t *testing.T) {
	dir := t.TempDir()
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ownedByMe(dir, fi); err != nil {
		t.Errorf("this user's own: %v", err)
	}
	if err := ownedByMe(dir, otherOwner{fi}); err == nil {
		t.Error("another user's is taken")
	}
}
