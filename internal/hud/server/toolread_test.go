package server

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// A tool's tool.lock pins its images, and is published and run as read, so
// it is checked as it is read: bytes under its ID that are not that lock
// (here, a lock pinning another image) are not present to readFile. Once,
// they were read as they came.
func TestAToolLockIsReadChecked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: dir}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	lock := []byte("images:\n  - ghcr.io/studio/comp@sha256:1111\n")
	id, err := st.PutBytes(ctx, lock)
	if err != nil {
		t.Fatal(err)
	}
	m := c4m.NewManifest()
	m.AddEntry(&c4m.Entry{Name: lockFile, Size: int64(len(lock)), Timestamp: c4m.NullTimestamp(), C4ID: id})
	b, err := c4m.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := c4view.NewFilter(nil)
	res, err := c4view.FromBytes(b, st, all)
	if err != nil {
		t.Fatal(err)
	}
	v := res.View()
	if got, ok, err := readFile(ctx, st, v, lockFile, 1<<20); err != nil || !ok || string(got) != string(lock) {
		t.Fatalf("its own lock: %q, %v, %v", got, ok, err)
	}

	ts, err := store.NewTreeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := ts.ContentPath(id)
	if !ok {
		t.Fatal("no file for the lock")
	}
	os.Chmod(p, 0o644)
	if err := os.WriteFile(p, []byte("images:\n  - ghcr.io/elsewhere/miner@sha256:2222\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	was := c4view.OnMismatch
	t.Cleanup(func() { c4view.OnMismatch = was })
	var told c4.ID
	c4view.OnMismatch = func(_ string, asked, _ c4.ID, _ error) { told = asked }
	got, ok, err := readFile(ctx, st, v, lockFile, 1<<20)
	if !errors.Is(err, c4view.ErrNotItsID) || got != nil || !ok || told != id {
		t.Fatalf("another lock under its ID: %q, %v, %v, told %s", got, ok, err, told)
	}
}
