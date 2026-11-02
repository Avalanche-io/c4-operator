package people

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// The index kept in the farm book (S2a, PEOPLE-KEYS-2 10.4 item 11). The
// issuer writes its entries to the book, pending, each signed as
// component/issuer (book-entry/v1); the queue checks each with
// BookChecker, the same checks this process makes, and seals the entry it
// checked. The entries keep their shape: only their order and position
// are the book's.

// peopleKinds are the kinds of entry the index books, in the book beside
// the HUD's control entries.
var peopleKinds = map[string]bool{KindDevice: true, KindProviderKey: true, KindSetting: true, KindRemoval: true,
	KindCode: true, KindCountersign: true, KindApproval: true, KindSignIns: true, KindFlag: true, KindTouchWindow: true,
	KindAgentGrant: true, KindAgentUse: true, KindGrantEnd: true}

// inBook reports whether an entry was written for a farm book: pending,
// or sealed in the book's order. The entries of the issuer's own index,
// and those a book adopted from it, are in the issuer's.
func inBook(e *Entry) bool { return e.Order == OrderPending || e.Order == OrderBook }

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

// timeAt is the time a verifier judges a position at: with a farm book,
// when the queue sealed it (a pending position, now); without one, the
// issuer's own order, at. A live decision passes its own clock instead.
func (x *Index) timeAt(pos int64, at time.Time) time.Time {
	if x.Book == nil {
		return at
	}
	if x.fbook != nil && pos <= x.sealed {
		if t, err := x.fbook.SealedAt(pos); err == nil {
			return t
		}
	}
	return time.Now()
}

// privatePart is what an entry keeps out of its hashed form: the
// addresses its device was seen at, with 16 random bytes of salt, so the
// entry, which names only the part's C4 ID, says nothing of them however
// few the addresses are.
type privatePart struct {
	Salt string   `json:"salt"`
	Addr string   `json:"addr,omitempty"`
	Seen []string `json:"seen,omitempty"`
}

// loadPrivate reads the private part an entry names.
func (x *Index) loadPrivate(ctx context.Context, e *Entry) error {
	if e.Private == "" {
		return nil
	}
	if e.Kind != KindDevice && e.Kind != KindFlag && e.Kind != KindTouchWindow {
		return fmt.Errorf("a %s entry has no private part", e.Kind)
	}
	b, err := x.Store.Get(ctx, e.Private)
	if err != nil {
		return fmt.Errorf("its private part: %w", err)
	}
	if c4id(b) != e.Private {
		return errors.New("its private part isn't the one it names")
	}
	var part privatePart
	if err := farmbook.Strict(b, &part); err != nil {
		return fmt.Errorf("its private part: %w", err)
	}
	if x.priv == nil {
		x.priv = map[string]privatePart{}
	}
	x.priv[e.Private] = part
	return nil
}

// prepare readies entries to write: each record to the store, named by its
// C4 ID; a device entry's touch key as its record names it; and any
// addresses into a private part, stored by its C4 ID.
func (x *Index) prepare(ctx context.Context, ps []pending) error {
	for _, p := range ps {
		e := p.entry
		if p.record != nil {
			e.Record = c4id(p.record)
			if err := x.Store.Put(ctx, e.Record, p.record); err != nil {
				return err
			}
			if e.Kind == KindDevice {
				if v, err := provenance.VerifyBundle(p.record, x.Roots); err == nil {
					if d, errs := schema.ParseDevice(v.Payload); errs == nil {
						e.TouchFor = d.Predicate.TouchFor
					}
				}
			}
		}
		if e.Addr == "" && len(e.Seen) == 0 {
			continue
		}
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return err
		}
		part := privatePart{Salt: base64.RawURLEncoding.EncodeToString(salt), Addr: e.Addr, Seen: e.Seen}
		raw, _ := json.Marshal(part)
		b, err := schema.Canonical(raw)
		if err != nil {
			return err
		}
		e.Private, e.Addr, e.Seen = c4id(b), "", nil
		if err := x.Store.Put(ctx, e.Private, b); err != nil {
			return err
		}
		if x.priv == nil {
			x.priv = map[string]privatePart{}
		}
		x.priv[e.Private] = part
	}
	return nil
}

// writtenForm is an entry as its writer wrote it, pending, without its
// signature, as canonical JSON, and its C4 ID: what the issuer signs, and
// what makes an entry booked once.
func (x *Index) writtenForm(e *Entry) ([]byte, string, error) {
	c := *e
	c.Farm, c.Position, c.Prev, c.Order, c.Sig = x.Farm, 0, "", OrderPending, nil
	raw, err := json.Marshal(&c)
	if err != nil {
		return nil, "", err
	}
	b, err := schema.Canonical(raw)
	if err != nil {
		return nil, "", err
	}
	return b, c4id(b), nil
}

// checkSig checks an entry written for a farm book is signed by the
// people's issuer as written, and returns its written ID and when it was
// signed.
func (x *Index) checkSig(e *Entry) (string, time.Time, error) {
	unsigned := func(err error) (string, time.Time, error) {
		return "", time.Time{}, farmbook.Refuse(farmbook.ReasonUnsigned, err)
	}
	if len(e.Sig) == 0 {
		return unsigned(errors.New("an entry for the farm book is signed by the people's issuer"))
	}
	_, id, err := x.writtenForm(e)
	if err != nil {
		return unsigned(err)
	}
	v, err := provenance.VerifyBundle(e.Sig, x.Roots)
	if err != nil {
		return unsigned(fmt.Errorf("its signature: %v", err))
	}
	if v.PredicateType != provenance.PredicateBookEntry {
		return unsigned(fmt.Errorf("its signature is a %s record", v.PredicateType))
	}
	if err := provenance.CheckSigner(v); err != nil {
		return unsigned(err)
	}
	if v.Farm != x.Farm || len(v.Subjects) != 1 || v.Subjects[0].Name != "book-entry:"+e.Kind || v.Subjects[0].Digest["c4"] != id {
		return unsigned(errors.New("its signature is over another entry"))
	}
	return id, v.SignedAt, nil
}

// sign signs an entry as written for the farm book.
func (x *Index) sign(ctx context.Context, sign func(context.Context) (*provenance.Signer, error), e *Entry, now time.Time) error {
	_, id, err := x.writtenForm(e)
	if err != nil {
		return err
	}
	s, err := sign(ctx)
	if err != nil {
		return err
	}
	sig, err := provenance.SignStatement(ctx, s, provenance.PredicateBookEntry,
		[]provenance.Subject{{Name: "book-entry:" + e.Kind, Digest: map[string]string{"c4": id}}}, map[string]string{"farm": x.Farm, "kind": e.Kind}, now)
	if err != nil {
		return err
	}
	b, err := schema.Canonical(sig)
	if err != nil {
		return err
	}
	e.Sig = b
	return nil
}

// errAdopting means the farm book is new and hasn't yet taken the index
// kept before it: the issuer waits for the queue's first seal.
var errAdopting = refuse(http.StatusServiceUnavailable, "the farm book hasn't adopted the people's index yet: the queue does at its first seal, within seconds")

// syncBook reads the entries sealed since this process last read the book,
// checking each as sync does, then the pending ones, at provisional
// positions. A sealed entry that no longer checks (the farm's roots
// changed) is kept in the book and counts for nothing, as the queue's
// sealer does too. A pending entry this process's checks refuse is left
// out: the queue refuses it at the seal too.
func (x *Index) syncBook(ctx context.Context) error {
	now := time.Now()
	if x.Now != nil {
		now = x.Now()
	}
	v, err := x.Book.View(ctx, now)
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
	x.pend, x.pendIDs, x.pendActs, x.pendWritten = nil, nil, nil, nil
	for _, s := range v.Entries(x.sealed) {
		if !peopleKinds[s.Entry.Kind] {
			continue
		}
		var e Entry
		err := farmbook.Strict(s.Raw, &e)
		if err == nil {
			e.counts = s.Time
			err = x.load(ctx, &e, false)
		}
		if err != nil {
			x.unchecked = append(x.unchecked, s.Entry.Position)
			continue
		}
		x.entries, x.ids = append(x.entries, &e), append(x.ids, s.ID)
	}
	x.sealed, x.pos, x.fbook = v.Head().Position, v.Head().Position, v.Book
	if segs := v.Segments(x.sealed); len(segs) > 0 {
		x.head = segs[len(segs)-1]
	}
	for _, s := range v.Pending() {
		x.pos = s.Entry.Position
		if !peopleKinds[s.Entry.Kind] {
			continue
		}
		var e Entry
		if farmbook.Strict(s.Raw, &e) != nil {
			continue
		}
		e.Position, e.counts = s.Entry.Position, now
		if x.load(ctx, &e, true) != nil {
			continue
		}
		x.pend, x.pendIDs = append(x.pend, &e), append(x.pendIDs, s.ID)
	}
	x.state, x.view = v.State, v
	return nil
}

// appendBook writes the entries build makes to the book, pending and
// signed, from the State this process last read: when another writer or
// the queue moved it first, it reads the book again and builds again, so
// build's checks see every entry before, pending ones included. Records
// and private parts go to the store before the entries, and the entries
// before the State.
func (x *Index) appendBook(ctx context.Context, now time.Time, sign func(context.Context) (*provenance.Signer, error), build func() ([]pending, error)) ([]*Entry, error) {
	for try := 0; ; try++ {
		if err := x.syncBook(ctx); err != nil {
			return nil, err
		}
		ps, err := build()
		if err != nil || len(ps) == 0 {
			return nil, err
		}
		if err := x.prepare(ctx, ps); err != nil {
			return nil, err
		}
		var subs []farmbook.Submission
		for _, p := range ps {
			e := p.entry
			e.Farm, e.Position, e.Prev, e.Order, e.At = x.Farm, 0, "", OrderPending, now.UTC().Format(time.RFC3339Nano)
			if err := x.sign(ctx, sign, e, now); err != nil {
				return nil, err
			}
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
			e.Position, e.counts = x.pos+int64(k)+1, now
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
// them, as booked, and the records and private parts they name. The
// book's first seal takes them unchanged, once, as its first entries
// (farmbook.Sealer.Adopt); after that the issuer writes to the book and
// the old head is left as it was. An empty index adopts nothing.
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
		for _, r := range []string{x.entries[k].Record, x.entries[k].Private} {
			if r == "" {
				continue
			}
			if a.Records[r], err = x.Store.Get(ctx, r); err != nil {
				return nil, err
			}
		}
	}
	return a, nil
}

// BookChecker checks the people's entries for the farm book's sealer, with
// the checks Sync makes (load): the issuer signed the entry as written; a
// device record verifies, is the issuer's and names the entry's device,
// person and touch key; an act is the entry's verb, signer, subject and
// params, signed by a device in force and its own touch key; a provider
// key is its JWK; and every entry is booked once. It keeps an index of
// the entries it admitted, in book order, and builds the entry it seals
// from the one it checked.
type BookChecker struct{ x *Index }

// NewBookChecker is a checker for the farm's entries, reading the records
// they cite from store, the book's.
func NewBookChecker(farm string, roots *x509.CertPool, store Store) *BookChecker {
	return &BookChecker{x: &Index{Farm: farm, Roots: roots, Store: store}}
}

// WithAdmin says who the farm's admins are, for the rules the queue holds
// an entry to as the issuer does: an admin may revoke anyone's agent grant.
func (c *BookChecker) WithAdmin(admin func(ctx context.Context, person string) bool) *BookChecker {
	c.x.Admin = admin
	return c
}

func (c *BookChecker) Kinds() []string {
	var out []string
	for k := range peopleKinds {
		out = append(out, k)
	}
	return out
}

// Admit reads an entry strictly, checks it at its position against the
// entries admitted before it, and returns it placed, with when it was
// signed: its record's signedAt for a device record or an act, its
// signature's for the rest (nothing for an adopted entry with neither).
func (c *BookChecker) Admit(ctx context.Context, raw []byte, p farmbook.Placement) ([]byte, time.Time, error) {
	var e Entry
	if err := farmbook.Strict(raw, &e); err != nil {
		return nil, time.Time{}, err
	}
	pending := e.Order == OrderPending && e.Position == 0 && e.Prev == "" && e.Farm == p.Farm
	placed := e.Order == p.Order && e.Position == p.Position && e.Prev == p.Prev && e.Farm == p.Farm
	if !pending && !placed {
		return nil, time.Time{}, farmbook.Refuse(farmbook.ReasonNotPending, errors.New("an entry is written pending, this farm's, with no position"))
	}
	x := c.x
	x.sealing = p.Time
	defer func() { x.sealing = time.Time{} }()
	e.Position, e.counts = p.Position, p.Time
	if err := x.load(ctx, &e, false); err != nil {
		reason := farmbook.ReasonOf(err)
		if reason == farmbook.ReasonRejected && (errors.Is(err, ErrNotInForce) || errors.Is(err, ErrTouchKey)) {
			reason = farmbook.ReasonNotInForce
		}
		return nil, time.Time{}, farmbook.Refuse(reason, err)
	}
	e.Farm, e.Prev, e.Order = p.Farm, p.Prev, p.Order
	sealed, err := json.Marshal(&e)
	if err == nil {
		sealed, err = schema.Canonical(sealed)
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	x.entries = append(x.entries, &e)
	x.sealed, x.pos = e.Position, e.Position
	signedAt := time.Time{}
	switch e.Kind {
	case KindDevice, KindRemoval, KindCode, KindCountersign, KindApproval:
		if b, err := x.Store.Get(ctx, e.Record); err == nil {
			if v, err := provenance.VerifyBundle(b, x.Roots); err == nil {
				signedAt = v.SignedAt
			}
		}
	default:
		if inBook(&e) {
			_, signedAt, _ = x.checkSig(&e)
		}
	}
	return sealed, signedAt, nil
}
