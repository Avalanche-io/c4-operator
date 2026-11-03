//go:build unix

package keystore

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
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

// A FIFO planted where the key file belongs is refused at once, never
// opened: opening it would block the MCP server until someone writes a key
// into it.
func TestKeyFileFIFORefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "workload-claude-code.key"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Store{Dir: dir, FileOnly: true}.Key(Workload("claude-code"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was used as the key file")
		}
	case <-time.After(3 * time.Second):
		t.Error("the server blocked opening a FIFO planted as its key file")
	}
}
