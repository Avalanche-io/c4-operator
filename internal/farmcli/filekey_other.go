//go:build !unix

package farmcli

import "os"

// fileKey is a file's identity where the platform gives one cheaply; on
// Windows a FileInfo holds none until os.SameFile asks, so identities
// compares with each instead.
type fileKey struct{}

func keyOf(os.FileInfo) (fileKey, bool) { return fileKey{}, false }
