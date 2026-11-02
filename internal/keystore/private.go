package keystore

import (
	"fmt"
	"io"
	"os"
)

// A store's directory and files are the person's own and only theirs (10.4
// item 15): the directory 0700, each key 0600, both owned by this user and
// neither a symlink. A key file planted there, a symlink, or one others
// can read is refused, never used: an agent's sandbox denies the directory
// to the agent (P6), and a key it could swap would sign as someone else.

// KeyRemade says a workload's key file didn't read as a key and a new key
// was made in its place: the grant named the old key, so a person grants
// the new one (c4 farm integrate) before the workload signs again. Key
// returns the new key with it.
type KeyRemade struct {
	Path, Aside string
	Cause       error
}

func (e *KeyRemade) Error() string {
	return fmt.Sprintf("the key file %s didn't read as a key (%v): it is kept as %s, and a new key was made, which needs a new grant: run c4 farm integrate", e.Path, e.Cause, e.Aside)
}

// private checks a key directory, or a key file, is the person's own and
// only theirs: not a symlink, owned by this user, mode 0700 (a directory)
// or 0600 (a file).
func private(path string, dir bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	want, kind, what := os.FileMode(0o600), "key file", "regular file"
	if dir {
		want, kind, what = 0o700, "key directory", "directory"
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		what += ", but a symlink"
	}
	switch {
	case dir != fi.IsDir() || !dir && !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("the %s %s is not a %s: refused; a key lives in a file of the person's own", kind, path, what)
	case fi.Mode().Perm() != want:
		return fmt.Errorf("the %s %s is mode %v, not %v: refused, since others may have read it (chmod %o, or remove the key and enrol a new one)", kind, path, fi.Mode().Perm(), want, want)
	}
	return ownedByMe(path, fi)
}

// readPrivate reads a key file after checking it is private. Its
// directory is checked first (Key): the person's own and 0700, so no one
// else can put another file in its place between the check and the read.
func readPrivate(path string) ([]byte, error) {
	if err := private(path, false); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 64<<10))
}
