//go:build !unix

package gitimport

import "os"

// lockFile is a no-op where flock is unavailable; run one import at a time.
func lockFile(*os.File) error { return nil }
