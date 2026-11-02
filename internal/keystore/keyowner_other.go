//go:build !unix

package keystore

import "os"

// ownedByMe is the owner check where file owners are Unix user IDs; on
// other systems the key directory's own access rules stand, and K4's
// keystore replaces these files.
func ownedByMe(string, os.FileInfo) error { return nil }
