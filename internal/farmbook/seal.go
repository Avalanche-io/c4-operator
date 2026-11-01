package farmbook

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
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

// Placement is where the sealer puts an entry: this farm, its position,
// the C4 ID of the entry before it, and its order (OrderBook; OrderIssuer
// for an adopted entry).
type Placement struct {
	Farm     string
	Position int64
	Prev     string
	Order    string
	// Time is when the entry counts from: the seal it is in, or for an
	// adopted entry its own time.
	Time time.Time
}

// Checker checks the entries of some kinds (condition 1 of the slice 2
// review). It reads an entry strictly (Strict), checks it is its writer's
// and says what the record it cites says, and builds the entry the sealer
// seals from what it checked, never from the bytes written. A checker
// keeps what the entries after need: the sealer gives it every sealed
// entry in order when it starts, then each pending one.
type Checker interface {
	// Kinds are the entry kinds it checks.
	Kinds() []string
	// Admit reads raw as an entry of its kind, written pending (or, given
	// a sealed entry again, already placed at p), checks it against the
	// entries admitted before and the record it cites, and returns it
	// placed at p as canonical JSON, with when its record or its writer's
	// signature says it was signed (the zero time for neither). An entry
	// it refuses changes nothing; one it admits counts for those after.
	// Its errors carry their reason (Refuse).
	Admit(ctx context.Context, raw []byte, p Placement) (sealed []byte, signedAt time.Time, err error)
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
	// Own is where the sealer keeps its own last head (SealerState), out
	// of reach of whoever writes the State: it seals only after it, so it
	// never signs a fork.
	Own    SealerStore
	Signer *provenance.Signer
	// Checkers makes a fresh set of checkers. An entry of a kind none
	// checks is refused.
	Checkers func() []Checker
	// Adopt, on a farm whose people's issuer kept its own index first,
	// reads that index; the first seal ever takes its entries.
	Adopt func(ctx context.Context) (*Adoption, error)
	// Every is the seal interval (DefaultSealEvery).
	Every time.Duration
	// Delay is how long a record may wait between being signed and being
	// written for the seal (provenance.ActSkew by default): a record's
	// window opens Delay before the sealer first saw its entry pending.
	Delay time.Duration
	Log   logr.Logger
	Now   func() time.Time

	book     *Book
	own      SealerState
	checkers map[string]Checker
	// firstSeen is when this sealer first saw each pending entry: the
	// anchor of its window. A new leader starts afresh, which only narrows
	// a window.
	firstSeen map[string]time.Time
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

// load reads the sealed book from the sealer's own head and replays it
// into a fresh set of checkers. A sealed entry a checker no longer admits
// (the farm's roots changed) is kept, since it is sealed, and counts for
// nothing: the people's index does the same (Unchecked).
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
		p := Placement{Farm: s.Farm, Position: e.Entry.Position, Prev: e.Entry.Prev, Order: e.Entry.Order, Time: e.Time}
		again, _, err := c.Admit(ctx, e.Raw, p)
		if err == nil && !bytes.Equal(again, e.Raw) {
			err = errors.New("it checks as another entry")
		}
		if err != nil {
			s.Log.Info("a sealed entry no longer checks, and counts for nothing", "position", e.Entry.Position, "kind", e.Entry.Kind, "error", err.Error())
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

// Seal seals the entries pending now into one segment after the sealer's
// own last head, refusing those that don't check, and returns it; nil
// when nothing was pending.
//
// The sealer seals only after its own head. A State whose head went back,
// forked or was emptied is put back at the sealer's head and sealed after
// it; one naming a segment the sealer never sealed is refused. Its own
// head moves first, then the State's.
func (s *Sealer) Seal(ctx context.Context) (*SealResult, error) {
	if s.Own == nil {
		return nil, errors.New("the sealer keeps its own head (Own)")
	}
	own, err := s.Own.Read(ctx)
	if err != nil {
		return nil, err
	}
	if s.book != nil && own.Head.Number < s.book.Head().Number {
		s.book = nil
		return nil, fmt.Errorf("%w: the sealer's own head is at segment %d, where it sealed %d", ErrRollback, own.Head.Number, s.own.Head.Number)
	}
	if s.book == nil || s.book.Head() != own.Head {
		if err := s.load(ctx, own.Head); err != nil {
			s.book = nil
			return nil, err
		}
	}
	s.own = own
	st, err := s.Pointer.Read(ctx)
	if err != nil {
		return nil, err
	}
	if st.Head != own.Head {
		if st.Head.Number > own.Head.Number {
			return nil, fmt.Errorf("%w: the State names segment %d, and this farm's sealer sealed to %d: it won't seal over segments it didn't seal", ErrBook, st.Head.Number, own.Head.Number)
		}
		s.Log.Info("the farm book's State went back, forked or was emptied: putting it back at the sealer's head",
			"state", st.Head.Number, "sealer", own.Head.Number)
		if err := s.Pointer.Write(ctx, st, State{Head: own.Head, Pending: st.Pending}); err != nil {
			return nil, err
		}
		if st, err = s.Pointer.Read(ctx); err != nil {
			return nil, err
		}
		if st.Head != own.Head {
			return nil, fmt.Errorf("the farm book's State moved again while it was put right")
		}
	}
	b := s.book
	head := b.Head()
	now := s.now().UTC()
	if n := len(b.segments); n > 0 && now.Before(b.segments[n-1].SignedAt) {
		// A segment is never sealed before the one it follows.
		now = b.segments[n-1].SignedAt
	}
	pos, prev := head.Position, head.Entry
	var ids []string
	var flags []schema.BookFlag
	var adopted *schema.BookAdopted
	if head.Number == 0 && !own.Adopted && s.Adopt != nil {
		a, err := s.Adopt(ctx)
		if err != nil {
			s.book = nil
			return nil, fmt.Errorf("adopt the people's index: %w", err)
		}
		if a != nil && len(a.Entries) > 0 {
			if ids, flags, err = s.adopt(ctx, a); err != nil {
				s.book = nil
				return nil, fmt.Errorf("adopt the people's index: %w", err)
			}
			pos, prev = int64(len(ids)), ids[len(ids)-1]
			adopted = &schema.BookAdopted{Head: a.Head, Position: a.Position, Order: OrderIssuer}
		}
	}
	delay := s.Delay
	if delay <= 0 {
		delay = provenance.ActSkew
	}
	batch, done := s.batch(st.Pending)
	raws := s.fetch(ctx, batch)
	if s.firstSeen == nil {
		s.firstSeen = map[string]time.Time{}
	}
	var refused []schema.BookRefusal
	for k, sub := range batch {
		seen, ok := s.firstSeen[sub]
		if !ok {
			seen = now
		}
		sealed, id, signedAt, err := s.check(ctx, sub, raws[k], pos+1, prev, now)
		if err != nil {
			refused = append(refused, schema.BookRefusal{Submission: sub, Reason: ReasonOf(err)})
			s.Log.Info("the farm book refused an entry", "submission", sub, "reason", ReasonOf(err), "why", err.Error())
			continue
		}
		if err := s.Store.Put(ctx, id, sealed); err != nil {
			s.book = nil
			return nil, err
		}
		pos, prev = pos+1, id
		ids = append(ids, id)
		// The window is anchored on when the sealer first saw the entry
		// pending: signed no earlier than Delay before, and no later than
		// the clocks' skew after.
		from, to := seen.Add(-delay), seen.Add(provenance.ActSkew)
		switch {
		case signedAt.IsZero():
			flags = append(flags, schema.BookFlag{Position: pos, Reason: FlagNoTime})
		case signedAt.Before(from) || signedAt.After(to):
			flags = append(flags, schema.BookFlag{Position: pos, Reason: FlagWindow, SignedAt: signedAt.UTC().Format(time.RFC3339Nano),
				From: from.UTC().Format(time.RFC3339Nano), To: to.UTC().Format(time.RFC3339Nano)})
		}
	}
	// What is left pending keeps its first sight.
	for _, sub := range st.Pending {
		if !done[sub] {
			if _, ok := s.firstSeen[sub]; !ok {
				s.firstSeen[sub] = now
			}
		}
	}
	for sub := range done {
		delete(s.firstSeen, sub)
	}
	if len(ids) == 0 && len(refused) == 0 {
		if len(done) > 0 {
			// Only dropped input: take it off the State.
			return nil, s.writeState(ctx, st, head, done)
		}
		return nil, nil
	}
	p := schema.BookSegmentPredicate{Farm: s.Farm, Number: head.Number + 1, Prev: head.Segment, First: head.Position + 1, Last: pos,
		Entries: ids, Refused: refused, Flags: flags, Adopted: adopted, SignedAt: now.Format(time.RFC3339Nano)}
	if p.Entries == nil {
		p.Entries = []string{}
	}
	subject := provenance.Subject{Name: "farm:" + s.Farm, Digest: map[string]string{"c4": s.Farm}}
	if pos > 0 {
		subject = provenance.Subject{Name: "book:" + s.Farm + "@" + strconv.FormatInt(pos, 10), Digest: map[string]string{"c4": prev}}
	}
	// The segment is checked against book/v1 before it is signed, and
	// again, signed, before the head moves: nothing written for the seal
	// can make one that doesn't hold together.
	if err := validSegment(subject, p); err != nil {
		s.book = nil
		return nil, err
	}
	if s.Signer == nil {
		s.book = nil
		return nil, errors.New("the queue has no signer: the farm book's segments are signed")
	}
	p.SignedAt = ""
	signed, err := provenance.SignStatement(ctx, s.Signer, provenance.PredicateBook, []provenance.Subject{subject}, p, now)
	if err != nil {
		s.book = nil
		return nil, err
	}
	id := c4id(signed)
	var last *Segment
	if n := len(b.segments); n > 0 {
		last = b.segments[n-1]
	}
	if _, err := checkSegment(s.Farm, s.Roots, id, signed, last, head.Entry); err != nil {
		s.book = nil
		return nil, fmt.Errorf("the segment the sealer signed doesn't check: %w", err)
	}
	if err := s.Store.Put(ctx, id, signed); err != nil {
		s.book = nil
		return nil, err
	}
	next := Head{Segment: id, Number: p.Number, Position: pos, Entry: prev}
	mine := SealerState{Head: next, Adopted: own.Adopted || adopted != nil}
	if err := s.Own.Write(ctx, own, mine); err != nil {
		s.book = nil
		return nil, fmt.Errorf("the sealer's own head: %w", err)
	}
	if s.own, err = s.Own.Read(ctx); err != nil {
		s.book = nil
		return nil, err
	}
	if err := s.book.add(ctx, s.Store, s.Roots, id, signed); err != nil {
		s.book = nil
		return nil, err
	}
	if err := s.writeState(ctx, st, next, done); err != nil {
		// The sealer's own head has moved: the next seal puts the State
		// right.
		s.Log.Error(err, "move the farm book's State; the next seal puts it right")
	}
	seg := s.book.segments[len(s.book.segments)-1]
	return &SealResult{Segment: seg, Refused: refused}, nil
}

// writeState moves the State to head, without the entries done, keeping
// any a writer listed meanwhile.
func (s *Sealer) writeState(ctx context.Context, st State, head Head, done map[string]bool) error {
	for try := 0; ; try++ {
		var left []string
		for _, sub := range st.Pending {
			if !done[sub] {
				left = append(left, sub)
			}
		}
		err := s.Pointer.Write(ctx, st, State{Head: head, Pending: left})
		if !errors.Is(err, ErrMoved) || try >= 8 {
			return err
		}
		if st, err = s.Pointer.Read(ctx); err != nil {
			return err
		}
	}
}

// batch is the pending entries this seal takes, in the order written, and
// every listing it takes off the State: those it takes, and those it
// drops unread. A listing that isn't a C4 ID, one listed again, and any
// past MaxPending are dropped: they are logged, and nothing of them goes
// into a segment. Past MaxSeal, the rest wait for the next seal.
func (s *Sealer) batch(pending []string) ([]string, map[string]bool) {
	done := map[string]bool{}
	var out []string
	seen := map[string]bool{}
	for k, sub := range pending {
		switch {
		case k >= MaxPending:
			done[sub] = true
			s.Log.Info("the farm book dropped a listing past its limit", "limit", MaxPending)
		case !IsC4ID(sub):
			done[sub] = true
			s.Log.Info("the farm book dropped a listing that isn't a C4 ID", "listing", fmt.Sprintf("%.100q", sub))
		case seen[sub]:
			done[sub] = true
		case len(out) < MaxSeal:
			seen[sub] = true
			done[sub] = true
			out = append(out, sub)
		}
	}
	return out, done
}

// fetch reads the batch's entries from the store, a few at a time; one
// the store lacks is nil.
func (s *Sealer) fetch(ctx context.Context, batch []string) [][]byte {
	out := make([][]byte, len(batch))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for k, sub := range batch {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if b, err := s.Store.Get(ctx, sub); err == nil {
				out[k] = b
			}
		}()
	}
	wg.Wait()
	return out
}

// check has the kind's checker admit one pending entry at pos, and returns
// the entry it built, as sealed, and its C4 ID.
func (s *Sealer) check(ctx context.Context, sub string, raw []byte, pos int64, prev string, now time.Time) ([]byte, string, time.Time, error) {
	if raw == nil {
		return nil, "", time.Time{}, Refuse(ReasonMissing, errors.New("the entry is not in the book's store"))
	}
	if got := c4id(raw); got != sub {
		return nil, "", time.Time{}, Refuse(ReasonMalformed, fmt.Errorf("the entry hashes to %.16s", got))
	}
	var h struct {
		Farm     string `json:"farm"`
		Position int64  `json:"position"`
		Prev     string `json:"prev"`
		Order    string `json:"order"`
		Kind     string `json:"kind"`
	}
	if err := exactFields(raw, &h); err != nil {
		return nil, "", time.Time{}, Refuse(ReasonMalformed, err)
	}
	switch {
	case h.Order != OrderPending || h.Position != 0 || h.Prev != "":
		return nil, "", time.Time{}, Refuse(ReasonNotPending, errors.New("an entry is written pending, with no position"))
	case h.Farm != "" && h.Farm != s.Farm:
		return nil, "", time.Time{}, Refuse(ReasonOtherFarm, errors.New("the entry is another farm's"))
	}
	c := s.checkers[h.Kind]
	if c == nil {
		return nil, "", time.Time{}, Refuse(ReasonUnknownKind, errors.New("the book takes no entries of its kind"))
	}
	sealed, signedAt, err := c.Admit(ctx, raw, Placement{Farm: s.Farm, Position: pos, Prev: prev, Order: OrderBook, Time: now})
	if err != nil {
		return nil, "", time.Time{}, err
	}
	id := c4id(sealed)
	e, err := checkEntry(s.Farm, id, sealed, pos, prev, false)
	if err == nil && e.Kind != h.Kind {
		err = errors.New("the checker built an entry of another kind")
	}
	if err != nil {
		return nil, "", time.Time{}, Refuse(ReasonRejected, err)
	}
	return sealed, id, signedAt, nil
}

// adopt checks an adopted index's entries (the people's issuer's order,
// each naming the one before it from position 1), copies them and their
// records to the book's store unchanged, and returns their IDs. An entry
// today's checks refuse is adopted all the same, flagged unchecked, and
// counts for nothing: adoption never stops the book.
func (s *Sealer) adopt(ctx context.Context, a *Adoption) ([]string, []schema.BookFlag, error) {
	for id, rec := range a.Records {
		if c4id(rec) != id {
			return nil, nil, fmt.Errorf("record %.16s hashes to %.16s", id, c4id(rec))
		}
		if err := s.Store.Put(ctx, id, rec); err != nil {
			return nil, nil, err
		}
	}
	var ids []string
	var flags []schema.BookFlag
	prev := ""
	for k, raw := range a.Entries {
		id := c4id(raw)
		pos := int64(k + 1)
		e, err := checkEntry(s.Farm, id, raw, pos, prev, true)
		if err != nil {
			return nil, nil, err
		}
		var again []byte
		if c := s.checkers[e.Kind]; c == nil {
			err = errors.New("the book takes no entries of its kind")
		} else {
			at, _ := time.Parse(time.RFC3339Nano, e.At)
			again, _, err = c.Admit(ctx, raw, Placement{Farm: s.Farm, Position: pos, Prev: prev, Order: OrderIssuer, Time: at})
		}
		if err == nil && !bytes.Equal(again, raw) {
			err = errors.New("it checks as another entry")
		}
		if err != nil {
			s.Log.Info("an adopted entry doesn't check today: adopted, flagged, and counting for nothing", "position", pos, "error", err.Error())
			flags = append(flags, schema.BookFlag{Position: pos, Reason: FlagUnchecked})
		}
		if err := s.Store.Put(ctx, id, raw); err != nil {
			return nil, nil, err
		}
		ids, prev = append(ids, id), id
	}
	if prev != a.Head || int64(len(ids)) != a.Position {
		return nil, nil, fmt.Errorf("the index's head names %.16s at %d; its entries end at %.16s, %d", a.Head, a.Position, prev, len(ids))
	}
	return ids, flags, nil
}

// validSegment checks a segment's statement against book/v1, as it will
// be signed.
func validSegment(subject provenance.Subject, p schema.BookSegmentPredicate) error {
	st := struct {
		Type          string                      `json:"_type"`
		Subject       []provenance.Subject        `json:"subject"`
		PredicateType string                      `json:"predicateType"`
		Predicate     schema.BookSegmentPredicate `json:"predicate"`
	}{schema.StatementType, []provenance.Subject{subject}, provenance.PredicateBook, p}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if _, errs := schema.ParseBookSegment(raw); errs != nil {
		return fmt.Errorf("the segment isn't book/v1: %s: %s", errs[0].Path, errs[0].Message)
	}
	return nil
}
