//go:build darwin

package capture

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Avalanche-io/c4"
)

func xattrNames(t *testing.T, p string) []string {
	t.Helper()
	size, err := unix.Listxattr(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	if size, err = unix.Listxattr(p, buf); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range strings.Split(string(buf[:size]), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// A cached object keeps nothing of the workspace's file but its bytes: a
// clone would carry the file's extended attributes (tags, quarantine,
// where-from) into every layer that links the object, and its flags.
func TestCaptureKeepsOnlyTheBytes(t *testing.T) {
	f := newLiveFixture(t)
	upper := filepath.Join(f.root, "upper")
	writeUpper(t, upper, "#!/bin/sh\necho one\n")
	notes := filepath.Join(upper, "notes.txt")
	if err := unix.Setxattr(notes, "com.example.tag", []byte("private to this workspace"), 0); err != nil {
		t.Skipf("setxattr: %v", err)
	}
	// A file locked in Finder (the user-immutable flag) can still be captured.
	locked := filepath.Join(upper, "bin", "tool.sh")
	if err := syscall.Chflags(locked, 0x2); err != nil { // UF_IMMUTABLE
		t.Skipf("chflags: %v", err)
	}
	defer syscall.Chflags(locked, 0)

	if _, err := Run(context.Background(), f.input(upper, false)); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"notes.txt": "notes\n", "bin/tool.sh": "#!/bin/sh\necho one\n"} {
		obj := f.cache.Path(c4.Identify(bytes.NewReader([]byte(body))))
		var st syscall.Stat_t
		if err := syscall.Lstat(obj, &st); err != nil {
			t.Fatalf("%s isn't in the cache: %v", name, err)
		}
		if st.Flags != 0 {
			t.Errorf("the object for %s has flags %#x", name, st.Flags)
		}
		if names := xattrNames(t, obj); len(names) != 0 {
			t.Errorf("the object for %s carries extended attributes: %q", name, names)
		}
	}
	// The workspace's own files keep theirs.
	if names := xattrNames(t, notes); len(names) != 1 {
		t.Errorf("the workspace's file now has attributes %q", names)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(locked, &st); err != nil || st.Flags&0x2 == 0 {
		t.Errorf("the workspace's locked file lost its flag: %#x %v", st.Flags, err)
	}
	if left, _ := filepath.Glob(filepath.Join(f.cache.Dir(), "tmp", "*")); len(left) != 0 {
		for _, p := range left {
			syscall.Chflags(p, 0)
			os.Remove(p)
		}
		t.Errorf("left in the cache's tmp: %d", len(left))
	}
}
