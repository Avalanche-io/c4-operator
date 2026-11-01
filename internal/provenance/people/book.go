package people

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// The index kept in the farm book (S2a, PEOPLE-KEYS-2 10.4 item 11). The
// issuer writes its entries to the book, pending, as it booked them to
// its own head before; the queue checks each against its record with
// BookChecker, the same checks this process makes, and seals it. The
// entries keep their shape: only their order and position are the book's.

// peopleKinds are the kinds of entry the index books, in the book beside
// the HUD's control entries.
var peopleKinds = map[string]bool{KindDevice: true, KindProviderKey: true, KindSetting: true, KindRemoval: true,
	KindCode: true, KindCountersign: true, KindApproval: true, KindSignIns: true, KindFlag: true}

// all is every entry read, sealed then pending.
func (x *Index) all() []*Entry {
	if len(x.pend) == 0 {
		return x.entries
	}
	return append(append([]*Entry(nil), x.entries...), x.pend...)
}

// upTo is every entry read at or before position pos.
func (x *Index) upTo(pos int64) []*Entry {
	var out []*Entry
	for _, e := range x.all() {
		if e.Position > pos {
			break
		}
		out = append(out, e)
	}
	return out
}

// Order is the order the index's positions are in: the farm book's, or the
// issuer's own.
func (x *Index) Order() string {
	if x.Book != nil {
		return OrderBook
	}
	return OrderIssuer
}

// errAdopting means the farm book is new and hasn't yet taken the index
// kept before it: the issuer waits for the queue's first seal.
var errAdopting = refuse(http.StatusServiceUnavailable, "the farm book hasn't adopted the people's index yet: the queue does at its first seal, within seconds")

// syncBook reads the entries sealed since this process last read the book,
// checking each as sync does, then the pending ones, at provisional
// positions. A pending entry this process's checks refuse is left out:
// the queue refuses it at the seal too.
func (x *Index) syncBook(ctx context.Context) error {
	v, err := x.Book.View(ctx, time.Now())
	if err != nil {
		return err
	}
	if v.Head().Number == 0 && x.Head != nil {
		h, err := x.Head.Read(ctx)
		if err != nil {
			return err
		}
		if h.Position > 0 {
			return errAdopting
		}
	}
	x.pend, x.pendIDs, x.pendActs = nil, nil, nil
	for _, s := range v.Entries(x.sealed) {
		if !peopleKinds[s.Entry.Kind] {
			continue
		}
		var e Entry
		if err := json.Unmarshal(s.Raw, &e); err != nil {
			return fmt.Errorf("%w: entry %d: %v", ErrIndex, s.Entry.Position, err)
		}
		if err := x.load(ctx, &e, false); err != nil {
			return fmt.Errorf("%w: entry %d: %v", ErrIndex, e.Position, err)
		}
		x.entries, x.ids = append(x.entries, &e), append(x.ids, s.ID)
	}
	x.sealed, x.pos = v.Head().Position, v.Head().Position
	if segs := v.Segments(x.sealed); len(segs) > 0 {
		x.head = segs[len(segs)-1]
	}
	for _, s := range v.Pending() {
		x.pos = s.Entry.Position
		if !peopleKinds[s.Entry.Kind] {
			continue
		}
		var e Entry
		if json.Unmarshal(s.Raw, &e) != nil {
			continue
		}
		e.Position = s.Entry.Position
		if x.load(ctx, &e, true) != nil {
			continue
		}
		x.pend, x.pendIDs = append(x.pend, &e), append(x.pendIDs, s.ID)
	}
	x.state = v.State
	return nil
}

// appendBook writes the entries build makes to the book, pending, from the
// State this process last read: when another writer or the queue moved it
// first, it reads the book again and builds again, so build's checks see
// every entry before, pending ones included. Records go to the store
// before the entries, and the entries before the State.
func (x *Index) appendBook(ctx context.Context, now time.Time, build func() ([]pending, error)) ([]*Entry, error) {
	for try := 0; ; try++ {
		if err := x.syncBook(ctx); err != nil {
			return nil, err
		}
		ps, err := build()
		if err != nil || len(ps) == 0 {
			return nil, err
		}
		var subs []farmbook.Submission
		for _, p := range ps {
			e := p.entry
			if p.record != nil {
				e.Record = c4id(p.record)
				if err := x.Store.Put(ctx, e.Record, p.record); err != nil {
					return nil, err
				}
			}
			e.Farm, e.Position, e.Prev, e.Order, e.At = x.Farm, 0, "", OrderPending, now.UTC().Format(time.RFC3339Nano)
			raw, err := json.Marshal(e)
			if err != nil {
				return nil, err
			}
			subs = append(subs, farmbook.Submission{Entry: json.RawMessage(raw)})
		}
		// Each entry is checked at its provisional position, as the queue
		// will check it, before it is written: one this process refuses is
		// never left pending. The next read drops what this one added.
		var out []*Entry
		for k, p := range ps {
			e := p.entry
			e.Position = x.pos + int64(k) + 1
			if err := x.load(ctx, e, true); err != nil {
				return nil, err
			}
			x.pend = append(x.pend, e)
			out = append(out, e)
		}
		ids, err := farmbook.Submit(ctx, x.Book.Store, x.Book.Pointer, x.state, subs)
		if errors.Is(err, farmbook.ErrMoved) && try < maxAppendTries {
			continue
		}
		if err != nil {
			return nil, err
		}
		x.pos += int64(len(out))
		x.pendIDs = append(x.pendIDs, ids...)
		return out, nil
	}
}

// Adoption is the index kept before the farm book, from this Index's own
// Store and Head (one without a Book): every entry, checked as Sync checks
// them, as booked, and the records they book. The book's first seal takes
// them unchanged, once, as its first entries (farmbook.Sealer.Adopt);
// after that the issuer writes to the book and the old head is left as it
// was. An empty index adopts nothing.
func (x *Index) Adoption(ctx context.Context) (*farmbook.Adoption, error) {
	if x.Book != nil {
		return nil, errors.New("the index kept before the book is read without a book")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return nil, err
	}
	if len(x.entries) == 0 {
		return nil, nil
	}
	a := &farmbook.Adoption{Head: x.ids[len(x.ids)-1], Position: int64(len(x.entries)), Records: map[string][]byte{}}
	for k, id := range x.ids {
		raw, err := x.Store.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		a.Entries = append(a.Entries, raw)
		if r := x.entries[k].Record; r != "" {
			if a.Records[r], err = x.Store.Get(ctx, r); err != nil {
				return nil, err
			}
		}
	}
	return a, nil
}

// BookChecker checks the people's entries for the farm book's sealer, with
// the checks Sync makes (load): a device record verifies, is the issuer's
// and names the entry's device and person; an act is the entry's verb,
// signer, subject and params, booked once; a provider key is its JWK. It
// keeps an index of the entries it admitted, in book order.
type BookChecker struct{ x *Index }

// NewBookChecker is a checker for the farm's entries, reading the records
// they cite from store, the book's.
func NewBookChecker(farm string, roots *x509.CertPool, store Store) *BookChecker {
	return &BookChecker{x: &Index{Farm: farm, Roots: roots, Store: store}}
}

func (c *BookChecker) Kinds() []string {
	var out []string
	for k := range peopleKinds {
		out = append(out, k)
	}
	return out
}

// Admit checks an entry at its position against the entries admitted
// before it, and returns when its record says it was signed: a device
// record's or an act's signedAt; nothing for a provider's key or the
// sign-ins, which aren't signed records.
func (c *BookChecker) Admit(ctx context.Context, fe *farmbook.Entry, raw []byte) (time.Time, error) {
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return time.Time{}, err
	}
	if e.Farm != "" && e.Farm != c.x.Farm {
		return time.Time{}, fmt.Errorf("the entry is farm %.16s's", e.Farm)
	}
	e.Position = fe.Position
	x := c.x
	if err := x.load(ctx, &e, false); err != nil {
		return time.Time{}, err
	}
	x.entries = append(x.entries, &e)
	x.sealed, x.pos = e.Position, e.Position
	switch e.Kind {
	case KindProviderKey, KindSignIns, KindSetting, KindFlag:
		return time.Time{}, nil
	}
	b, err := x.Store.Get(ctx, e.Record)
	if err != nil {
		return time.Time{}, err
	}
	v, err := provenance.VerifyBundle(b, x.Roots)
	if err != nil {
		return time.Time{}, err
	}
	return v.SignedAt, nil
}
