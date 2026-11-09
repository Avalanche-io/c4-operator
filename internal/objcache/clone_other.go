//go:build !darwin

package objcache

import "errors"

// cloneFile has no portable form; Snapshot copies where it fails.
func cloneFile(string, string) error { return errors.New("no clonefile on this platform") }
