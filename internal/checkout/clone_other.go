//go:build !darwin

package checkout

import "errors"

// cloneDir needs APFS. On Linux a workspace is an overlay (or, later, a
// reflink on XFS or Btrfs).
func cloneDir(string, string) error {
	return errors.New("clone workspaces need macOS (APFS clonefile)")
}
