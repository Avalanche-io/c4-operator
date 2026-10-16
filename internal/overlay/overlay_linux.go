//go:build linux

package overlay

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Options is a fixed set of overlay features, independent of kernel
// defaults: copy-up copies whole files, and a renamed directory is copied
// rather than redirected, so the upper holds plain files, plain
// directories, whiteouts and opaque markers only.
const Options = "index=off,metacopy=off,redirect_dir=off,xino=off"

// Mount mounts lowers (top first) at target. With upper and work set, the
// mount is writable and every write lands in upper; otherwise it is
// read-only.
func Mount(target string, lowers []string, upper, work string) error {
	if len(lowers) == 0 {
		return errors.New("no layers to mount")
	}
	for _, l := range lowers {
		if strings.ContainsAny(l, ",:\\") {
			return fmt.Errorf("layer path %q has characters overlay options cannot carry", l)
		}
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if upper == "" && len(lowers) == 1 {
		return bindReadOnly(lowers[0], target)
	}
	opts := "lowerdir=" + strings.Join(lowers, ":")
	if upper != "" {
		opts += ",upperdir=" + upper + ",workdir=" + work
	}
	opts += "," + Options
	if len(opts) >= unix.Getpagesize() {
		return fmt.Errorf("overlay options are %d bytes; the kernel takes one page", len(opts))
	}
	var flags uintptr
	if upper == "" {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount("overlay", target, "overlay", flags, opts); err != nil {
		return fmt.Errorf("mount overlay at %s: %w", target, err)
	}
	return nil
}

func bindReadOnly(src, target string) error {
	if err := unix.Mount(src, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s: %w", target, err)
	}
	if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		unix.Unmount(target, 0)
		return fmt.Errorf("remount %s read-only: %w", target, err)
	}
	return nil
}

// Unmount unmounts target if it is mounted. A busy mount is detached, so
// an exiting container never wedges kubelet.
func Unmount(target string) error {
	mounted, err := IsMounted(target)
	if err != nil || !mounted {
		return err
	}
	err = unix.Unmount(target, 0)
	if errors.Is(err, unix.EBUSY) {
		err = unix.Unmount(target, unix.MNT_DETACH)
	}
	if err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	return nil
}

// IsMounted reports whether target is a mount point.
func IsMounted(target string) (bool, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		if unescape(fields[4]) == abs {
			return true, nil
		}
	}
	return false, sc.Err()
}

// unescape decodes mountinfo's octal escapes (\040 for space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Walk lists the upper directory, parents before children.
func Walk(upper string) ([]Entry, error) {
	var out []Entry
	err := filepath.WalkDir(upper, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(upper, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		e := Entry{Path: rel, Full: p, Size: st.Size}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			e.Kind = File
			e.Exec = st.Mode&0o111 != 0
		case unix.S_IFDIR:
			e.Kind = Dir
			e.Path += "/"
			e.Opaque = isOpaque(p)
		case unix.S_IFLNK:
			e.Kind = Symlink
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			e.Target = t
		case unix.S_IFCHR:
			if st.Rdev == 0 {
				e.Kind = Whiteout
				break
			}
			e.Kind = Other
			e.Unusual = "character device"
		default:
			e.Kind = Other
			e.Unusual = fmt.Sprintf("file type %o", st.Mode&unix.S_IFMT)
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

func isOpaque(p string) bool {
	buf := make([]byte, 8)
	n, err := unix.Lgetxattr(p, "trusted.overlay.opaque", buf)
	return err == nil && n > 0 && buf[0] == 'y'
}
