//go:build !darwin

package objcache

import "os"

// syncData is the platform's fsync, on every platform but macOS. On Linux
// it asks the filesystem to flush the device, so what is synced survives a
// power cut as well as a crash where the filesystem sends that flush and
// the device honours it.
func syncData(f *os.File) error { return f.Sync() }
