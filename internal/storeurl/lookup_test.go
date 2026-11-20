package storeurl

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Avalanche-io/c4"
)

// A tree store's lookup has three answers (TreeStore.Lookup): there, absent
// at every level, or unknown because a lookup met another error. Head and
// Get say ErrNotFound only for absent; a lookup that meets a permission
// error returns that error, never "not found", since a reader that took an
// unknown for absent would fetch elsewhere, fill, or record a null.
func TestATreeShardTellsAbsentFromUnknown(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	ctx := context.Background()
	dir := t.TempDir()
	sh, err := OpenWith(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("an object")
	id := c4.Identify(bytes.NewReader(data))
	if err := sh.Put(ctx, id, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	absent := c4.Identify(bytes.NewReader([]byte("never stored")))
	if _, err := sh.Head(ctx, absent); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an absent object: %v", err)
	}
	if n, err := sh.Head(ctx, id); err != nil || n != int64(len(data)) {
		t.Fatalf("a present object: %d, %v", n, err)
	}
	// The store root unreadable: whether either object is there is unknown.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	for name, check := range map[string]c4.ID{"present": id, "absent": absent} {
		if _, err := sh.Head(ctx, check); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Head of the %s object with the store unreadable: %v, want the lookup's error", name, err)
		}
		if _, _, _, err := sh.Get(ctx, check, 0); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Get of the %s object with the store unreadable: %v, want the lookup's error", name, err)
		}
	}
}
