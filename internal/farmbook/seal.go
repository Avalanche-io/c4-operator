package farmbook

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// The seal interval (condition 3 of the slice 2 review): a record counts
// from the first segment that seals it, so the interval sets how fine "in
// force" is. It is seconds, never minutes: DefaultSealEvery unless the
// install says otherwise (--farm-book-seal), and never more than
// MaxSealEvery.
const (
	DefaultSealEvery = 2 * time.Second
	MaxSealEvery     = 30 * time.Second
)

// CheckEvery checks a seal interval is within its bound.
func CheckEvery(d time.Duration) error {
	if d <= 0 || d > MaxSealEvery {
		return fmt.Errorf("the farm book seals at most every %s, and at least once in a while: %s", MaxSealEvery, d)
	}
	return nil
}

// Checker checks the entries of some kinds against the records they cite
// (condition 1 of the slice 2 review): the record verifies, passes the
// signer policy (provenance.CheckSigner), and says the same thing as the
// entry. A checker keeps what the entries before need: the sealer gives
// it every sealed entry in order when it starts, then each pending one.
type Checker interface {
	// Kinds are the entry kinds it checks.
	Kinds() []string
	// Admit checks e, at its position, against the entries admitted before
	// it and the record it cites, raw being the entry as written. An entry
	// it admits counts for those after it. It returns when the record says
	// it was signed, or the zero time for a record that says nothing of it
	// (a provider's key).
	Admit(ctx context.Context, e *Entry, raw []byte) (time.Time, error)
}

// Adoption is a people's index kept before the book (PEOPLE-KEYS-2 10.4
// item 11): its entries as booked, in order, each naming the one before
// it, and the records they book by C4 ID. The book takes them unchanged,
// once, as its first entries.
type Adoption struct {
	Head     string
	Position int64
	Entries  [][]byte
	Records  map[string][]byte
}

// Sealer seals the book: the leader-elected queue runs it (one sealer per
// farm), signing each segment as component/queue.
type Sealer struct {
	Farm    string
	Roots   *x509.CertPool
	Store   Store
	Pointer Pointer
	Signer  *provenance.Signer
	// Checkers makes a fresh set of checkers. An entry of a kind none
	// checks is refused.
	Checkers func() []Checker
	// Adopt, on a farm whose people's issuer kept its own index first,
	// reads that index; the first seal takes its entries.
	Adopt func(ctx context.Context) (*Adoption, error)
	// Every is the seal interval (DefaultSealEvery).
	Every time.Duration
	// Delay is how long a record may wait between being signed and being
	// written for the seal: a record's window starts at the seal before its
	// own less Delay (provenance.ActSkew by default).
	Delay time.Duration
	Log   logr.Logger
	Now   func() time.Time

	book     *Book
	checkers map[string]Checker
}

// Run seals every Every until ctx ends.
func (s *Sealer) Run(ctx context.Context) {
	every := s.Every
	if every <= 0 {
		every = DefaultSealEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := s.Seal(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error(err, "seal the farm book")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Sealer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// load reads the sealed book and replays it into a fresh set of checkers.
// A sealed entry a checker no longer admits (its roots changed) is logged
// and kept: it is sealed.
func (s *Sealer) load(ctx context.Context, head Head) error {
	b, err := ReadBook(ctx, s.Farm, s.Roots, s.Store, head)
	if err != nil {
		return err
	}
	s.checkers = map[string]Checker{}
	if s.Checkers != nil {
		for _, c := range s.Checkers() {
			for _, k := range c.Kinds() {
				s.checkers[k] = c
			}
		}
	}
	for _, e := range b.entries {
		c := s.checkers[e.Entry.Kind]
		if c == nil {
			continue
		}
		if _, err := c.Admit(ctx, e.Entry, e.Raw); err != nil {
			s.Log.Info("a sealed entry no longer checks", "position", e.Entry.Position, "kind", e.Entry.Kind, "error", err.Error())
		}
	}
	s.book = b
	return nil
}

// SealResult is what one seal did.
type SealResult struct {
	Segment *Segment
	Refused []schema.BookRefusal
}

// Seal seals the entries pending now into one segment, refusing those that
// don't check, and returns it; nil when nothing was pending. Refusals are
// logged and listed in the segment, and never sealed.
func (s *Sealer) Seal(ctx context.Context) (*SealResult, error) {
	st, err := s.Pointer.Read(ctx)
	if err != nil {
		return nil, err
	}
	if s.book == nil || s.book.Head() != st.Head {
		if err := s.load(ctx, st.Head); err != nil {
			s.book = nil
			return nil, err
		}
	}
	now := s.now().UTC()
	b := s.book
	head := b.Head()
	pos, prev := head.Position, head.Entry
	var ids []string
	var adopted *schema.BookAdopted
	if head.Number == 0 && s.Adopt != nil {
		a, err := s.Adopt(ctx)
		if err != nil {
			s.book = nil
			return nil, fmt.Errorf("adopt the people's index: %w", err)
		}
		if a != nil && len(a.Entries) > 0 {
			if ids, err = s.adopt(ctx, a); err != nil {
				s.book = nil
				return nil, fmt.Errorf("adopt the people's index: %w", err)
			}
			pos, prev = int64(len(ids)), ids[len(ids)-1]
			adopted = &schema.BookAdopted{Head: a.Head, Position: a.Position, Order: OrderIssuer}
		}
	}
	// A record's window: signed no earlier than the seal before less
	// Delay, and no later than this seal plus the clocks' skew.
	delay := s.Delay
	if delay <= 0 {
		delay = provenance.ActSkew
	}
	var from time.Time
	if n := len(b.segments); n > 0 {
		from = b.segments[n-1].SignedAt.Add(-delay)
	}
	to := now.Add(provenance.ActSkew)
	var refused []schema.BookRefusal
	var flags []schema.BookFlag
	done := map[string]bool{}
	for _, sub := range st.Pending {
		done[sub] = true
		e, raw, signedAt, err := s.check(ctx, sub, pos+1)
		if err != nil {
			r := schema.BookRefusal{Submission: sub, Why: err.Error()}
			if e != nil {
				r.Kind, r.Record = e.Kind, e.Record
			}
			refused = append(refused, r)
			s.Log.Info("the farm book refused an entry", "submission", sub, "kind", r.Kind, "record", r.Record, "why", r.Why)
			continue
		}
		b, id, err := place(raw, s.Farm, pos+1, prev)
		if err == nil {
			err = s.Store.Put(ctx, id, b)
		}
		if err != nil {
			s.book = nil
			return nil, err
		}
		pos, prev = pos+1, id
		ids = append(ids, id)
		if !signedAt.IsZero() && ((!from.IsZero() && signedAt.Before(from)) || signedAt.After(to)) {
			// Before the first seal nothing bounds a window from below: its
			// from is the zero time.
			flags = append(flags, schema.BookFlag{Position: pos, Record: e.Record, SignedAt: signedAt.UTC().Format(time.RFC3339Nano),
				From: from.UTC().Format(time.RFC3339Nano), To: to.Format(time.RFC3339Nano)})
		}
	}
	if len(ids) == 0 && len(refused) == 0 {
		return nil, nil
	}
	p := schema.BookSegmentPredicate{Farm: s.Farm, Number: head.Number + 1, Prev: head.Segment, First: head.Position + 1, Last: pos,
		Entries: ids, Refused: refused, Flags: flags, Adopted: adopted}
	if p.Entries == nil {
		p.Entries = []string{}
	}
	subject := provenance.Subject{Name: "farm:" + s.Farm, Digest: map[string]string{"c4": s.Farm}}
	if pos > 0 {
		subject = provenance.Subject{Name: "book:" + s.Farm + "@" + strconv.FormatInt(pos, 10), Digest: map[string]string{"c4": prev}}
	}
	if s.Signer == nil {
		s.book = nil
		return nil, errors.New("the queue has no signer: the farm book's segments are signed")
	}
	signed, err := provenance.SignStatement(ctx, s.Signer, provenance.PredicateBook, []provenance.Subject{subject}, p, now)
	if err != nil {
		s.book = nil
		return nil, err
	}
	id := c4id(signed)
	if err := s.Store.Put(ctx, id, signed); err != nil {
		s.book = nil
		return nil, err
	}
	next := Head{Segment: id, Number: p.Number, Position: pos, Entry: prev}
	for try := 0; ; try++ {
		var left []string
		for _, sub := range st.Pending {
			if !done[sub] {
				left = append(left, sub)
			}
		}
		err = s.Pointer.Write(ctx, st, State{Head: next, Pending: left})
		if err == nil {
			break
		}
		s.book = nil
		if !errors.Is(err, ErrMoved) || try >= 8 {
			return nil, err
		}
		// A writer listed another entry meanwhile: keep it pending. Another
		// sealer moving the head means this one isn't the leader any more.
		if st, err = s.Pointer.Read(ctx); err != nil {
			return nil, err
		}
		if st.Head != head {
			return nil, fmt.Errorf("another sealer moved the farm book's head to segment %d", st.Head.Number)
		}
	}
	if s.book == nil {
		// The write went through after a retry: read it back as any reader.
		if err := s.load(ctx, next); err != nil {
			return nil, err
		}
	} else if err := s.book.add(ctx, s.Store, s.Roots, id, signed); err != nil {
		s.book = nil
		return nil, err
	}
	seg := s.book.segments[len(s.book.segments)-1]
	return &SealResult{Segment: seg, Refused: refused}, nil
}

// check reads one pending entry and has its kind's checker admit it at
// pos. The entry is in the store under its ID, pending, this farm's, and
// names no position or entry before it.
func (s *Sealer) check(ctx context.Context, sub string, pos int64) (*Entry, []byte, time.Time, error) {
	raw, err := s.Store.Get(ctx, sub)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("the entry is not in the book's store: %v", err)
	}
	if got := c4id(raw); got != sub {
		return nil, nil, time.Time{}, fmt.Errorf("the entry hashes to %.16s", got)
	}
	e, err := ParseEntry(raw)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	switch {
	case e.Order != OrderPending || e.Position != 0 || e.Prev != "":
		return e, nil, time.Time{}, fmt.Errorf("an entry is written pending, with no position: this one is in order %q at %d", e.Order, e.Position)
	case e.Farm != "" && e.Farm != s.Farm:
		return e, nil, time.Time{}, fmt.Errorf("the entry is farm %.16s's", e.Farm)
	}
	c := s.checkers[e.Kind]
	if c == nil {
		return e, nil, time.Time{}, fmt.Errorf("the book takes no %q entries", e.Kind)
	}
	e.Position = pos
	signedAt, err := c.Admit(ctx, e, raw)
	if err != nil {
		return e, nil, time.Time{}, err
	}
	return e, raw, signedAt, nil
}

// adopt checks an adopted index's entries (the people's issuer's order,
// each naming the one before it from position 1) and admits each, copies
// them and their records to the book's store unchanged, and returns their
// IDs.
func (s *Sealer) adopt(ctx context.Context, a *Adoption) ([]string, error) {
	for id, rec := range a.Records {
		if c4id(rec) != id {
			return nil, fmt.Errorf("record %.16s hashes to %.16s", id, c4id(rec))
		}
		if err := s.Store.Put(ctx, id, rec); err != nil {
			return nil, err
		}
	}
	var ids []string
	prev := ""
	for k, raw := range a.Entries {
		id := c4id(raw)
		e, err := checkEntry(s.Farm, id, raw, int64(k+1), prev, true)
		if err != nil {
			return nil, err
		}
		c := s.checkers[e.Kind]
		if c == nil {
			return nil, fmt.Errorf("entry %d is a %q entry, which the book takes no checker for", e.Position, e.Kind)
		}
		if _, err := c.Admit(ctx, e, raw); err != nil {
			return nil, fmt.Errorf("entry %d: %w", e.Position, err)
		}
		if err := s.Store.Put(ctx, id, raw); err != nil {
			return nil, err
		}
		ids, prev = append(ids, id), id
	}
	if prev != a.Head || int64(len(ids)) != a.Position {
		return nil, fmt.Errorf("the index's head names %.16s at %d; its entries end at %.16s, %d", a.Head, a.Position, prev, len(ids))
	}
	return ids, nil
}
