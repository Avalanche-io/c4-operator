package people_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

// An index page answers the position its entries reach, with its signed
// head: a page from past the end holds no entries and answers the true
// end, never the position asked for; a page from before the start holds
// every entry and answers the end too.
func TestIndexPageFromPastItsEnd(t *testing.T) {
	r := studio(t)
	ctx := context.Background()
	for {
		pos, err := r.index.Position(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if pos >= 12 {
			if pos != 12 {
				t.Fatalf("the index is at %d, not 12", pos)
			}
			break
		}
		r.signIn(newDev(t), "dana", danaSub)
	}
	for _, c := range []struct {
		from    string
		entries int
	}{{"100", 0}, {"12", 0}, {"-5", 12}, {"0", 12}, {"9", 3}} {
		var page people.IndexPage
		if code, body := r.call("GET", "/v1/people/index?from="+c.from, "", nil, &page); code != http.StatusOK {
			t.Fatalf("from %s: %d %s", c.from, code, body)
		}
		if page.Position != 12 || page.Head == "" || len(page.Signed) == 0 || len(page.Entries) != c.entries {
			t.Errorf("from %s: position %d, head %q, %d entries; want 12, the head, %d", c.from, page.Position, page.Head, len(page.Entries), c.entries)
		}
	}
}

// Kept in a farm book, a page answers the sealed head's position and
// segment, from wherever it is read.
func TestIndexPageInTheBookFromPastItsEnd(t *testing.T) {
	b := inBook(studio(t), nil)
	for k := 0; k < 3; k++ {
		b.signIn(newDev(t), "dana", danaSub)
	}
	b.seal()
	sealed := b.sealedAt()
	if sealed < 3 {
		t.Fatalf("sealed to %d", sealed)
	}
	all, _, _ := b.index.Entries(context.Background(), 0)
	for _, c := range []struct {
		from    string
		entries int
	}{{"100", 0}, {"-5", len(all)}, {"0", len(all)}} {
		var page people.IndexPage
		if code, body := b.call("GET", "/v1/people/index?from="+c.from, "", nil, &page); code != http.StatusOK {
			t.Fatalf("from %s: %d %s", c.from, code, body)
		}
		if page.Position != sealed || page.Head == "" || len(page.Signed) == 0 || len(page.Entries) != c.entries {
			t.Errorf("from %s: position %d, head %q, %d entries; want %d, the head, %d", c.from, page.Position, page.Head, len(page.Entries), sealed, c.entries)
		}
	}
}
