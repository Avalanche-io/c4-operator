//go:build !linux

package overlay

import "errors"

var errNotLinux = errors.New("overlay mounts need Linux")

// Options is the fixed overlay feature set (see the Linux implementation).
const Options = "index=off,metacopy=off,redirect_dir=off,xino=off"

// Mount needs Linux.
func Mount(string, []string, string, string) error { return errNotLinux }

// Unmount needs Linux.
func Unmount(string) error { return errNotLinux }

// IsMounted needs Linux.
func IsMounted(string) (bool, error) { return false, errNotLinux }

// Walk needs Linux.
func Walk(string) ([]Entry, error) { return nil, errNotLinux }
