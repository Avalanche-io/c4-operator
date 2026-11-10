//go:build !darwin

package objcache

import "os"

// syncData is the platform's fsync. On Linux that flushes the device, so
// what is synced survives a power cut as well as a crash.
func syncData(f *os.File) error { return f.Sync() }
