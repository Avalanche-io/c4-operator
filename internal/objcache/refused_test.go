package objcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
)

// commitBytes caches data under id whatever it hashes to, as a fill taken
// on its source's word, or rot, leaves it.
func commitBytes(t *testing.T, c *Cache, id c4.ID, data string) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "tmp")
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(tmp, id); err != nil {
		t.Fatal(err)
	}
}

// Refused takes out the plain copy only if it is still the file that was
// sent, and the executable variant only if it is not id's either.
func TestRefusedTakesOutOnlyWhatWasSentAndWrong(t *testing.T) {
	s, err := OpenSet(t.TempDir(), Options{Mode: 0o666, ExecMode: 0o777, Budget: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Partition("p")
	id := c4.Identify(strings.NewReader("right"))

	// The wrong copy, and an executable variant made from it.
	commitBytes(t, c, id, "wrong")
	if _, err := c.ExecPath(id); err != nil {
		t.Fatal(err)
	}
	sent, _ := os.Lstat(c.Path(id))
	removed, err := c.Refused(id, sent)
	if err != nil || !removed {
		t.Fatalf("the refused copy: removed %v, %v", removed, err)
	}
	if c.Has(id) {
		t.Fatal("the refused copy is still cached")
	}
	if _, err := os.Stat(c.ExecPathOf(id)); err == nil {
		t.Fatal("the executable variant made from it is still there")
	}

	// A right copy committed after the wrong one was sent stays when the
	// refusal of the wrong one comes late.
	commitBytes(t, c, id, "wrong")
	stale, _ := os.Lstat(c.Path(id))
	if err := c.Remove(id); err != nil {
		t.Fatal(err)
	}
	commitBytes(t, c, id, "right")
	if removed, err := c.Refused(id, stale); err != nil || removed {
		t.Fatalf("a late refusal: removed %v, %v", removed, err)
	}
	if b, _ := c.ReadAll(id); string(b) != "right" {
		t.Fatalf("cached now: %q", b)
	}

	// A right executable variant stays when the plain copy is refused.
	if _, err := c.ExecPath(id); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(id); err != nil {
		t.Fatal(err)
	}
	commitBytes(t, c, id, "wrong")
	sent, _ = os.Lstat(c.Path(id))
	if _, err := c.Refused(id, sent); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(c.ExecPathOf(id)); err != nil || string(b) != "right" {
		t.Fatalf("the right executable variant: %q, %v", b, err)
	}
}
