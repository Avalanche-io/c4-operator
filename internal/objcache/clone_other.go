//go:build !darwin

package objcache

import (
	"errors"
	"os"
)

// cloneOpenFile has no portable form; Snapshot copies where it fails.
func cloneOpenFile(*os.File, string) error { return errors.New("no clonefile on this platform") }
