package gitimport

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"
)

// A c4git store's lookup that meets an error other than absence returns it,
// so an import fails rather than recording the file as missing, with a null
// size (c4gitObject reads os.ErrNotExist as missing); and an absent object
// is os.ErrNotExist, as before.
func TestATreeSourceTellsAbsentFromUnknown(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	dir := t.TempDir()
	ts, err := store.NewTreeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("a c4git object")
	id, err := ts.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	src := &TreeSource{TS: ts}
	sink := &TreeSink{TS: ts}
	absent := c4.Identify(bytes.NewReader([]byte("never stored")))
	if _, err := src.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an absent object: %v", err)
	}
	if n, err := src.Stat(id); err != nil || n != int64(len(data)) {
		t.Fatalf("a present object: %d, %v", n, err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if _, err := src.Stat(id); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat with the store unreadable: %v, want the lookup's error", err)
	}
	if _, present, err := sink.Size(nil, id); err == nil || present {
		t.Errorf("Size with the store unreadable: present %v, %v, want the lookup's error", present, err)
	}
}
