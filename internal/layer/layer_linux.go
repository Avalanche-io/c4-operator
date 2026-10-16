//go:build linux

package layer

import "golang.org/x/sys/unix"

// whiteout creates an overlay whiteout: a character device 0/0.
func whiteout(p string) error {
	return unix.Mknod(p, unix.S_IFCHR, int(unix.Mkdev(0, 0)))
}

// setOpaque marks a directory so nothing below it in lower layers shows.
func setOpaque(p string) error {
	return unix.Setxattr(p, "trusted.overlay.opaque", []byte("y"), 0)
}
