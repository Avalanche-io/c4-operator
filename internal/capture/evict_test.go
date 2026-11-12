package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// checkingSink refuses bytes that are not the ID's as a store does, with
// storeurl.ErrNotItsID.
type checkingSink struct{ *storeSink }

func (s checkingSink) PutFile(ctx context.Context, p string, id c4.ID, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if c4.Identify(bytes.NewReader(data)) != id {
		return fmt.Errorf("%s: %w", id, storeurl.ErrNotItsID)
	}
	return s.storeSink.PutFile(ctx, p, id, path)
}

// A node cache holding other bytes under a captured file's ID (rot, a bad
// fill) kept them over the capture's own copy, and every capture sent
// them again and was refused. Now the refusal evicts them: that capture
// fails, and the next one caches and sends its own copy.
func TestAWrongCachedCopyIsEvictedOnRefusal(t *testing.T) {
	f := newLiveFixture(t)
	upper := filepath.Join(f.root, "upper")
	writeUpper(t, upper, "#!/bin/sh\necho one\n")
	id := c4.Identify(strings.NewReader("notes\n"))
	rot := filepath.Join(f.root, "rot")
	if err := os.WriteFile(rot, []byte("notez\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.cache.Commit(rot, id); err != nil {
		t.Fatal(err)
	}
	in := f.input(upper, false)
	in.Sink = checkingSink{f.sink.storeSink}

	if _, err := Run(context.Background(), in); !errors.Is(err, storeurl.ErrNotItsID) {
		t.Fatalf("the first capture: %v, want ErrNotItsID", err)
	}
	if f.cache.Has(id) {
		t.Fatal("the wrong cached copy is still cached")
	}
	if _, err := Run(context.Background(), in); err != nil {
		t.Fatalf("the next capture: %v", err)
	}
	if b, _ := f.cache.ReadAll(id); string(b) != "notes\n" {
		t.Fatalf("cached now: %q", b)
	}
	f.sink.mu.Lock()
	defer f.sink.mu.Unlock()
	if got := f.sink.objs[id]; string(got) != "notes\n" {
		t.Fatalf("stored: %q", got)
	}
}
