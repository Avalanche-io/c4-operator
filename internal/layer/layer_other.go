//go:build !linux

package layer

import "errors"

var errNotLinux = errors.New("overlay layers need Linux")

func whiteout(string) error { return errNotLinux }

func setOpaque(string) error { return errNotLinux }
