package people

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// The private part of a booked entry (a device's addresses) is served to
// its person as stored, checked: bytes under its ID that are not that part
// (another person's addresses, say) are not served. A read of the store
// made after the index loaded is checked again, since the store does not
// rehash. Once, Private served what the store held.
func TestAPrivatePartIsServedOnlyIfItIsItsID(t *testing.T) {
	ctx := context.Background()
	st := &MemoryStore{}
	part := []byte(`{"addr":"192.0.2.10"}`)
	id := c4id(part)
	if err := st.Put(ctx, id, part); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(Entry{Kind: KindDevice, Person: "noor", Private: id})
	sees := func(string) bool { return true }
	if got, red, err := Private(ctx, st, raw, sees); err != nil || red != nil || string(got) != string(part) {
		t.Fatalf("its own part: %s, %v, %v", got, red, err)
	}
	if err := st.Put(ctx, id, []byte(`{"addr":"198.51.100.7"}`)); err != nil {
		t.Fatal(err)
	}
	if got, _, err := Private(ctx, st, raw, sees); !errors.Is(err, storeurl.ErrNotItsID) || got != nil {
		t.Fatalf("another part under its ID: %s, %v", got, err)
	}
}
