package farmbook

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// Segment is one sealed segment as a reader checked it.
type Segment struct {
	// ID is the C4 ID of the segment as signed (its bundle).
	ID       string
	Signed   []byte
	SignedAt time.Time
	schema.BookSegmentPredicate
}

// Sealed is one entry as a reader has it: sealed at its position by the
// segment numbered Segment, at SealedAt; or, in a View, pending, its
// position provisional and its time the reader's now.
type Sealed struct {
	ID       string
	Raw      []byte
	Entry    *Entry
	Segment  int64
	SealedAt time.Time
	// Time is when the entry counts from: its seal, or for an entry a book
	// adopted, the time its own index booked it.
	Time    time.Time
	Pending bool
}

// LastHead is the last head a reader accepted. A client keeps it between
// reads (K4's CLI on disk) and gives it back (Reader.Keep), so a head that
// goes back to an older segment the queue signed, or forks from it, is
// refused (ErrRollback): signing alone shows a rewrite, not a rollback.
type LastHead struct {
	Number   int64  `json:"number"`
	Segment  string `json:"segment,omitempty"`
	Position int64  `json:"position"`
	Entry    string `json:"entry,omitempty"`
}

// After checks that h may follow the last head: not an older segment, and
// the same segment where the number is the same.
func (l LastHead) After(h Head) error {
	switch {
	case h.Number < l.Number:
		return fmt.Errorf("%w: segment %d, where segment %d was seen", ErrRollback, h.Number, l.Number)
	case h.Number == l.Number && h.Segment != l.Segment:
		return fmt.Errorf("%w: segment %d is %.16s, where %.16s was seen: the book was rewritten", ErrRollback, h.Number, h.Segment, l.Segment)
	}
	return nil
}

// checkSegment checks one signed segment against the one before it (nil
// for the first) and the book's last entry before it: it verifies against
// the farm's roots, is the queue's (CheckSigner), is this farm's, follows
// prev by number, ID and position, and its subject is the book's last
// entry as it leaves it.
func checkSegment(farm string, roots *x509.CertPool, id string, signed []byte, prev *Segment, lastEntry string) (*Segment, error) {
	if got := c4id(signed); got != id {
		return nil, fmt.Errorf("%w: segment %.16s hashes to %.16s", ErrBook, id, got)
	}
	v, err := provenance.VerifyBundle(signed, roots)
	if err != nil {
		return nil, fmt.Errorf("%w: segment %.16s: %v", ErrBook, id, err)
	}
	if v.PredicateType != provenance.PredicateBook {
		return nil, fmt.Errorf("%w: segment %.16s is a %s record", ErrBook, id, v.PredicateType)
	}
	if err := provenance.CheckSigner(v); err != nil {
		return nil, fmt.Errorf("%w: segment %.16s: %v", ErrBook, id, err)
	}
	st, errs := schema.ParseBookSegment(v.Payload)
	if errs != nil {
		return nil, fmt.Errorf("%w: segment %.16s: %s: %s", ErrBook, id, errs[0].Path, errs[0].Message)
	}
	p := st.Predicate
	s := &Segment{ID: id, Signed: signed, SignedAt: v.SignedAt, BookSegmentPredicate: p}
	wantNumber, wantPrev, wantFirst := int64(1), "", int64(1)
	if prev != nil {
		wantNumber, wantPrev, wantFirst = prev.Number+1, prev.ID, prev.Last+1
	}
	switch {
	case v.Farm != farm || p.Farm != farm:
		return nil, fmt.Errorf("%w: segment %.16s is farm %.16s's, signed under %.16s", ErrBook, id, p.Farm, v.Farm)
	case p.Number != wantNumber:
		return nil, fmt.Errorf("%w: segment %.16s is numbered %d, after segment %d", ErrBook, id, p.Number, wantNumber-1)
	case p.Number > 1 && p.Prev == "":
		return nil, fmt.Errorf("%w: segment %d names no segment before it", ErrBook, p.Number)
	case p.Prev != wantPrev:
		return nil, fmt.Errorf("%w: segment %d names %.16s before it, not %.16s: the book was rewritten", ErrBook, p.Number, p.Prev, wantPrev)
	case p.First != wantFirst || p.Last-p.First+1 != int64(len(p.Entries)):
		return nil, fmt.Errorf("%w: segment %d seals %d to %d with %d entries, after position %d", ErrBook, p.Number, p.First, p.Last, len(p.Entries), wantFirst-1)
	case p.Adopted != nil && p.Number != 1:
		return nil, fmt.Errorf("%w: segment %d adopts an index; only the first may", ErrBook, p.Number)
	case prev != nil && v.SignedAt.Before(prev.SignedAt):
		return nil, fmt.Errorf("%w: segment %d is sealed before the segment it follows", ErrBook, p.Number)
	}
	subject := st.Subject[0]
	if n := len(p.Entries); n > 0 {
		if subject.Name != "book:"+farm+"@"+strconv.FormatInt(p.Last, 10) || subject.Digest["c4"] != p.Entries[n-1] {
			return nil, fmt.Errorf("%w: segment %d's subject is %s, not its last entry", ErrBook, p.Number, subject.Name)
		}
	} else if prev != nil && prev.Last > 0 {
		if subject.Name != "book:"+farm+"@"+strconv.FormatInt(prev.Last, 10) || subject.Digest["c4"] != lastEntry {
			return nil, fmt.Errorf("%w: segment %d's subject is %s, not the book's last entry", ErrBook, p.Number, subject.Name)
		}
	}
	return s, nil
}

// Book is the sealed book as a reader checked it, from its first segment.
type Book struct {
	Farm     string
	segments []*Segment
	entries  []*Sealed
	first    map[string]int64
}

// Head is the book's sealed head.
func (b *Book) Head() Head {
	if len(b.segments) == 0 {
		return Head{}
	}
	s := b.segments[len(b.segments)-1]
	h := Head{Segment: s.ID, Number: s.Number, Position: s.Last}
	if n := len(b.entries); n > 0 {
		h.Entry = b.entries[n-1].ID
	}
	return h
}

// Last is the book's head as a LastHead, for a reader to keep.
func (b *Book) Last() LastHead {
	h := b.Head()
	return LastHead{Number: h.Number, Segment: h.Segment, Position: h.Position, Entry: h.Entry}
}

// Segments are the segments sealing positions after since, and always the
// head segment, oldest first.
func (b *Book) Segments(since int64) []*Segment {
	var out []*Segment
	for k, s := range b.segments {
		if s.Last > since || k == len(b.segments)-1 {
			out = append(out, s)
		}
	}
	return out
}

// Entries are the sealed entries after position since.
func (b *Book) Entries(since int64) []*Sealed {
	since = max(0, min(since, int64(len(b.entries))))
	return append([]*Sealed(nil), b.entries[since:]...)
}

// PositionOf is the first sealed position of an entry booking record: the
// position the record counts from (DESIGN 5.4).
func (b *Book) PositionOf(record string) (int64, bool) {
	p, ok := b.first[record]
	return p, ok
}

// SealedAt is when the entry at pos was sealed: the time its position is
// judged at.
func (b *Book) SealedAt(pos int64) (time.Time, error) {
	if pos < 1 || pos > int64(len(b.entries)) {
		return time.Time{}, fmt.Errorf("%w: position %d, the book is sealed to %d", ErrNotSealed, pos, len(b.entries))
	}
	return b.entries[pos-1].SealedAt, nil
}

// Flags are the sealed entries whose record's signedAt fell outside the
// window their position allows (DESIGN 5.4): flagged, not refused. The
// violations list reads them (slice 2's S2c).
func (b *Book) Flags() []schema.BookFlag {
	var out []schema.BookFlag
	for _, s := range b.segments {
		out = append(out, s.Flags...)
	}
	return out
}

// Refusals are the entries the queue refused at the seal, never sealed.
func (b *Book) Refusals() []schema.BookRefusal {
	var out []schema.BookRefusal
	for _, s := range b.segments {
		out = append(out, s.Refused...)
	}
	return out
}

// add checks a segment after the book's last and the entries it seals,
// and adds them. Each entry is in the store under the ID the segment
// lists, canonical, this farm's, at its position, naming the entry before
// it, in the book's order (or the issuer's, in an adopted segment).
func (b *Book) add(ctx context.Context, st Store, roots *x509.CertPool, id string, signed []byte) error {
	var prev *Segment
	if n := len(b.segments); n > 0 {
		prev = b.segments[n-1]
	}
	last := ""
	if n := len(b.entries); n > 0 {
		last = b.entries[n-1].ID
	}
	s, err := checkSegment(b.Farm, roots, id, signed, prev, last)
	if err != nil {
		return err
	}
	var news []*Sealed
	for k, eid := range s.Entries {
		raw, err := st.Get(ctx, eid)
		if err != nil {
			return fmt.Errorf("%w: entry %.16s: %v", ErrBook, eid, err)
		}
		e, err := checkEntry(b.Farm, eid, raw, s.First+int64(k), last, s.Adopted != nil)
		if err != nil {
			return err
		}
		t := s.SignedAt
		if s.Adopted != nil {
			// An adopted entry counts from when its own index booked it, so a
			// device that lapsed before the book doesn't come back.
			t, _ = time.Parse(time.RFC3339Nano, e.At)
		}
		news = append(news, &Sealed{ID: eid, Raw: raw, Entry: e, Segment: s.Number, SealedAt: s.SignedAt, Time: t})
		last = eid
	}
	b.segments = append(b.segments, s)
	if b.first == nil {
		b.first = map[string]int64{}
	}
	for _, n := range news {
		b.entries = append(b.entries, n)
		if r := n.Entry.Record; r != "" {
			if _, ok := b.first[r]; !ok {
				b.first[r] = n.Entry.Position
			}
		}
	}
	return nil
}

// checkEntry checks one sealed entry is what its segment says.
func checkEntry(farm, id string, raw []byte, pos int64, prev string, adopted bool) (*Entry, error) {
	if got := c4id(raw); got != id {
		return nil, fmt.Errorf("%w: entry %.16s hashes to %.16s", ErrBook, id, got)
	}
	if canon, err := schema.Canonical(raw); err != nil || !bytes.Equal(canon, raw) {
		return nil, fmt.Errorf("%w: entry %.16s is not canonical JSON", ErrBook, id)
	}
	e, err := ParseEntry(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: entry %.16s: %v", ErrBook, id, err)
	}
	order := OrderBook
	if adopted {
		order = OrderIssuer
	}
	switch {
	case e.Farm != farm:
		return nil, fmt.Errorf("%w: entry %.16s is farm %.16s's", ErrBook, id, e.Farm)
	case e.Position != pos || e.Prev != prev:
		return nil, fmt.Errorf("%w: entry %.16s is at %d after %.16s, where its segment seals %d after %.16s", ErrBook, id, e.Position, e.Prev, pos, prev)
	case e.Order != order:
		return nil, fmt.Errorf("%w: entry %.16s is in order %q, not %q", ErrBook, id, e.Order, order)
	}
	return e, nil
}

// ReadBook reads and checks the whole sealed book, from its first segment
// to head.
func ReadBook(ctx context.Context, farm string, roots *x509.CertPool, st Store, head Head) (*Book, error) {
	b := &Book{Farm: farm}
	return b, b.extend(ctx, st, roots, head)
}

// extend reads the segments from the book's last to head, walking back by
// prev, then checks and adds them oldest first.
func (b *Book) extend(ctx context.Context, st Store, roots *x509.CertPool, head Head) error {
	have := b.Head()
	if head.Number == have.Number {
		if head.Segment != have.Segment {
			return fmt.Errorf("%w: segment %d is %.16s, where %.16s was read: the book was rewritten", ErrBook, head.Number, head.Segment, have.Segment)
		}
		return nil
	}
	if head.Number < have.Number {
		return fmt.Errorf("%w: segment %d, where %d was read", ErrRollback, head.Number, have.Number)
	}
	var ids []string
	var raws [][]byte
	for id, n := head.Segment, head.Number; n > have.Number; n-- {
		if id == "" {
			return fmt.Errorf("%w: segment %d is not named by the one after it", ErrBook, n)
		}
		raw, err := st.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: segment %.16s: %v", ErrBook, id, err)
		}
		ids, raws = append(ids, id), append(raws, raw)
		v, err := provenance.VerifyBundle(raw, roots)
		if err != nil {
			return fmt.Errorf("%w: segment %.16s: %v", ErrBook, id, err)
		}
		sg, errs := schema.ParseBookSegment(v.Payload)
		if errs != nil {
			return fmt.Errorf("%w: segment %.16s: %s: %s", ErrBook, id, errs[0].Path, errs[0].Message)
		}
		if sg.Predicate.Number != n {
			return fmt.Errorf("%w: segment %.16s is numbered %d, where segment %d was linked: the book was rewritten", ErrBook, id, sg.Predicate.Number, n)
		}
		id = sg.Predicate.Prev
	}
	for k := len(ids) - 1; k >= 0; k-- {
		if err := b.add(ctx, st, roots, ids[k], raws[k]); err != nil {
			return err
		}
	}
	if got := b.Head(); got.Position != head.Position || got.Entry != head.Entry {
		return fmt.Errorf("%w: the head says position %d, entry %.16s; its segment seals %d, entry %.16s", ErrBook, head.Position, head.Entry, got.Position, got.Entry)
	}
	return nil
}

// Reader reads the sealed book from the system store and keeps the last
// head it accepted, refusing one that goes back (LastHead).
type Reader struct {
	Farm    string
	Roots   *x509.CertPool
	Store   Store
	Pointer Pointer
	// Own, set in the manager, is the sealer's own head (SealerState): the
	// reader reads the sealed book to it, never to a head the State names
	// that the sealer didn't seal, and so keeps the last head across its
	// own restarts.
	Own SealerStore

	mu   sync.Mutex
	book *Book
	last LastHead
}

// Keep gives the reader a head kept from before, as a client that kept
// it gives it back: the next Sync refuses a head older than it.
func (r *Reader) Keep(last LastHead) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = last
}

// Last is the last head the reader accepted.
func (r *Reader) Last() LastHead {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Sync reads the State and the segments sealed since the reader last
// read, checking each, and returns the sealed book and the State. A head
// before the last one accepted, or another segment in its place, is
// ErrRollback, and the reader keeps its last head.
func (r *Reader) Sync(ctx context.Context) (*Book, State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.Pointer.Read(ctx)
	if err != nil {
		return nil, State{}, err
	}
	if r.Own != nil {
		own, err := r.Own.Read(ctx)
		if err != nil {
			return nil, State{}, err
		}
		if own.Head.Number < st.Head.Number {
			return nil, State{}, fmt.Errorf("%w: the State names segment %d, and the sealer sealed to %d", ErrBook, st.Head.Number, own.Head.Number)
		}
		st.Head = own.Head
	}
	if err := r.last.After(st.Head); err != nil {
		return nil, State{}, err
	}
	b := r.book
	if b == nil || st.Head.Number < b.Head().Number {
		b = &Book{Farm: r.Farm}
	}
	nb := &Book{Farm: b.Farm, segments: b.segments, entries: b.entries, first: b.first}
	if err := nb.extend(ctx, r.Store, r.Roots, st.Head); err != nil {
		return nil, State{}, err
	}
	// A head kept from before must be the segment of that number here.
	if l := r.last; l.Number > 0 && (int64(len(nb.segments)) < l.Number || nb.segments[l.Number-1].ID != l.Segment) {
		return nil, State{}, fmt.Errorf("%w: segment %d is not %.16s, the one last seen", ErrRollback, l.Number, l.Segment)
	}
	r.book, r.last = nb, nb.Last()
	return nb, st, nil
}

// View reads the sealed book and the entries pending its seal: what the
// HUD and the people's issuer judge a request against before an entry is
// sealed. Pending entries get provisional positions after the sealed head,
// in the order written; one the store lacks, or that isn't an entry, is
// left out.
func (r *Reader) View(ctx context.Context, now time.Time) (*View, error) {
	b, st, err := r.Sync(ctx)
	if err != nil {
		return nil, err
	}
	v := &View{Book: b, State: st, Now: now}
	pos := b.Head().Position
	for _, id := range st.Pending {
		raw, err := r.Store.Get(ctx, id)
		if err != nil || c4id(raw) != id {
			continue
		}
		e, err := ParseEntry(raw)
		if err != nil || e.Order != OrderPending {
			continue
		}
		pos++
		e.Position = pos
		v.pending = append(v.pending, &Sealed{ID: id, Raw: raw, Entry: e, SealedAt: now, Time: now, Pending: true})
	}
	return v, nil
}

// View is the sealed book and the entries pending its seal, as of Now.
type View struct {
	*Book
	State   State
	Now     time.Time
	pending []*Sealed
}

// All are the sealed entries and those pending the seal, in book order.
func (v *View) All() []*Sealed { return append(v.Book.Entries(0), v.pending...) }

// Pending are the entries waiting for the seal, at provisional positions.
func (v *View) Pending() []*Sealed { return append([]*Sealed(nil), v.pending...) }

// Position is the last position the view holds, pending entries counted.
func (v *View) Position() int64 {
	if n := len(v.pending); n > 0 {
		return v.pending[n-1].Entry.Position
	}
	return v.Book.Head().Position
}

// What "in force" is asked of (DESIGN 5.4).
const (
	// ForceGrant is a person's submit grant to a tracker: opened by a grant
	// control record, closed by its revoke, and ending at its until.
	ForceGrant = "grant"
	// ForceBinding is a deployment binding, by its subject
	// (BindingSubject): opened by a bind control record, closed by an
	// end-binding or by a bind that follows it (its Ends), and ending at
	// its end.
	ForceBinding = "binding"
	// ForceMembership is a membership, by its subject:
	//   - a tracker login confirmed as a person, the people map: opened by
	//     people-confirm, closed by people-revoke;
	//   - a Party's member, by name (MemberKey) or by a group of the
	//     studio's own (GroupKey): opened by add-member, opened again with
	//     other purposes by set-purposes, closed by end-member. Making a
	//     Party opens no one's: an organisation's first make-party names
	//     its first admin, whose membership opens only with their own
	//     accept-admin, the party's first, of that person (ORGANIZATION 2,
	//     PEOPLE-KEYS-2 10.4 item 27 R4). A name is never reused, so a
	//     second make-party names no one. The purposes are the opening
	//     record's change.
	ForceMembership = "membership"
	// ForceParty is a Party, by PartyKey: opened by its first make-party.
	// A party never ends.
	ForceParty = "party"
	// ForceDevice is a person's device key, by its device ID: opened by
	// its device record, closed by a removal of it or of all its person's
	// devices enrolled before, and lapsing DeviceLapse after its last
	// renewal was sealed. A touch key's rules beside its device key are
	// the people's issuer's (people.Index.DevicesAt).
	ForceDevice = "device"
	// ForceAgentGrant is an agent's grant (PEOPLE-KEYS-2 5.2), by its ID:
	// opened by a person's confirmed grant naming the agent and its key,
	// closed by its end (the integration removed, or its lapse booked), and
	// lapsing AgentLapse after the agent's last booked use of it, or the
	// grant itself where it has none (DESIGN 10's exception, P5).
	ForceAgentGrant = "agent-grant"
)

// The people's issuer's entries for an agent's grant (PEOPLE-KEYS-2 5.2,
// 10.4 item 14): the grant, an agent's act under it, and its end.
const (
	// KindAgentGrant books a person's grant act: the grant's ID is its
	// record's C4 ID.
	KindAgentGrant = "agent-grant"
	// KindAgentUse books an act the agent made under the grant, at most one
	// a day (AgentUseEvery): what keeps an agent in use from lapsing.
	KindAgentUse = "agent-act"
	// KindGrantEnd books a grant's end: the person's revoke when the
	// integration is removed, or its lapse after AgentLapse unused.
	KindGrantEnd = "grant-end"
)

// AgentLapse is how long an agent's grant lasts without the agent making
// an act under it (P5): an abandoned agent's grant lapses, one in use
// keeps it, and nothing else renews it.
const AgentLapse = 90 * 24 * time.Hour

// AgentUseEvery is how often an agent's use of its grant is booked: often
// enough to judge AgentLapse by, rarely enough to keep the book small.
const AgentUseEvery = 24 * time.Hour

// DeviceLapse is how long a device may go without renewing before it
// lapses (PEOPLE-KEYS-2 7).
const DeviceLapse = 90 * 24 * time.Hour

// opens and closes are the control acts that open and close each kind.
var (
	opens = map[string][]string{ForceGrant: {"grant"}, ForceBinding: {"bind"},
		ForceMembership: {"people-confirm", "add-member", "set-purposes"}, ForceParty: {"make-party"}}
	closes = map[string][]string{ForceGrant: {"revoke"}, ForceBinding: {"end-binding"}, ForceMembership: {"people-revoke", "end-member"}}
)

// The keys a Party's entries are booked by: the party, a member by name
// and a member by a group of the studio's own. A party's name has no
// slash, so a group's name, whatever it holds, is read after the first.
func PartyKey(party string) string          { return "party:" + party }
func MemberKey(party, person string) string { return PartyKey(party) + "/person:" + person }
func GroupKey(party, group string) string   { return PartyKey(party) + "/group:" + group }

// memberOf reads a member's key: the party and the person.
func memberOf(subject string) (string, string, bool) {
	rest, ok := strings.CutPrefix(subject, "party:")
	p, person, found := strings.Cut(rest, "/person:")
	if !ok || !found || strings.Contains(p, "/") || person == "" {
		return "", "", false
	}
	return p, person, true
}

// AdminAccepted is the entry by which party's first admin accepted, among
// es in book order, or nil: the first accept-admin, after the party's
// first make-party, of the person it named, as the in-force rule takes it.
func AdminAccepted(es []*Sealed, party string) *Sealed {
	person, made := "", false
	for _, s := range es {
		e := s.Entry
		switch {
		case e.Kind != KindControl:
		case e.Act == "make-party" && e.Subject == PartyKey(party) && !made:
			made, person = true, e.Person
		case e.Act == "accept-admin" && person != "" && e.Subject == MemberKey(party, person) && e.Person == person:
			return s
		}
	}
	return nil
}

func partyOf(subject string) (string, bool) {
	p, ok := strings.CutPrefix(subject, "party:")
	return p, ok && !strings.Contains(p, "/")
}

// InForce is the entry that put key in force at sealed position pos, or
// nil if it wasn't in force there: what a verifier judges a record by,
// at the position the record counts from. The time at a position is when
// it was sealed, never the record's own signedAt. A position past the
// sealed head is ErrNotSealed.
func (b *Book) InForce(kind, key string, pos int64) (*Sealed, error) {
	at, err := b.SealedAt(pos)
	if err != nil {
		return nil, err
	}
	return b.InForceAt(kind, key, pos, at)
}

// InForceAt is InForce judged at time at: what put key in force by the
// entries sealed to pos, and still is at at. A verifier passes the time
// pos was sealed (InForce does); a live decision passes the clock, so a
// time-based end (a device's lapse, a grant's until) counts at once on a
// quiet book, not only after the next seal.
func (b *Book) InForceAt(kind, key string, pos int64, at time.Time) (*Sealed, error) {
	if pos < 0 || pos > int64(len(b.entries)) {
		return nil, fmt.Errorf("%w: position %d, the book is sealed to %d", ErrNotSealed, pos, len(b.entries))
	}
	return InForceAt(b.entries[:pos], kind, key, at)
}

// InForce is the entry that put key in force at pos, pending entries
// counted, as of the view's Now for a pending position: what the HUD and
// the people's issuer judge a request by before its entry is sealed.
func (v *View) InForce(kind, key string, pos int64) (*Sealed, error) {
	sealed := v.Book.Head().Position
	if pos <= sealed {
		return v.Book.InForce(kind, key, pos)
	}
	return v.InForceAt(kind, key, pos, v.Now)
}

// InForceAt is InForce judged at time at, pending entries counted.
func (v *View) InForceAt(kind, key string, pos int64, at time.Time) (*Sealed, error) {
	es := append(append([]*Sealed(nil), v.Book.entries...), v.pending...)
	return InForceAt(es[:max(0, min(pos, int64(len(es))))], kind, key, at)
}

// InForceAt is the one "in force" rule, for every kind: the entry among
// es (in book order) that put key in force, and still is at time at, or
// nil. Each time-based end is judged at at, which the caller chooses: a
// verifier the time its position was sealed, a live decision its clock.
func InForceAt(es []*Sealed, kind, key string, at time.Time) (*Sealed, error) {
	switch kind {
	case ForceDevice:
		return deviceInForce(es, key, at), nil
	case ForceAgentGrant:
		g := agentGrant(es, key)
		if !GrantInForce(g, at) {
			return nil, nil
		}
		return es[g.Ref], nil
	}
	open, ok := opens[kind]
	if !ok {
		return nil, fmt.Errorf("in force is asked of %s, %s, %s, %s, %s or %s, not %q", ForceGrant, ForceBinding, ForceMembership, ForceParty, ForceDevice, ForceAgentGrant, kind)
	}
	var on *Sealed
	firstAdmin := map[string]string{} // party -> the person its first make-party names
	made, accepted := map[string]bool{}, map[string]bool{}
	for _, s := range es {
		e := s.Entry
		if kind == ForceBinding && e.Kind == KindControl && e.Act == "bind" && e.Ends == key {
			// a binding that follows it ends it at the changeover
			on = nil
			continue
		}
		if e.Kind != KindControl {
			continue
		}
		if e.Act == "make-party" {
			// Only a party's first make-party counts: a name is never
			// reused, so a second can neither make it again nor name
			// another first admin.
			p, isParty := partyOf(e.Subject)
			if made[e.Subject] || !isParty {
				continue
			}
			made[e.Subject] = true
			firstAdmin[p] = e.Person
			if kind == ForceParty && e.Subject == key {
				on = s
			}
			continue
		}
		if e.Act == "accept-admin" {
			// The first admin's own acceptance, once: of the person the
			// party's first make-party named.
			p, person, ok := memberOf(e.Subject)
			if !ok || accepted[p] || firstAdmin[p] == "" || firstAdmin[p] != person || e.Person != person {
				continue
			}
			accepted[p] = true
			if kind == ForceMembership && e.Subject == key {
				on = s
			}
			continue
		}
		if e.Subject != key {
			continue
		}
		switch {
		case slices.Contains(open, e.Act):
			on = s
		case slices.Contains(closes[kind], e.Act):
			on = nil
		}
	}
	if on != nil && on.Entry.Expires != "" && at.After(on.Entry.expires()) {
		return nil, nil
	}
	return on, nil
}

// agentGrant is grant's state as entries es book it, by the rule the
// people's index uses too (GrantStates); nil where es book no such grant.
func agentGrant(es []*Sealed, grant string) *GrantState {
	var evs []GrantEvent
	for k, s := range es {
		if e := s.Entry; e.Grant == grant {
			evs = append(evs, GrantEvent{Kind: e.Kind, Grant: e.Grant, Time: s.Time, Ref: k})
		}
	}
	return GrantStates(evs)[grant]
}

// GrantLapses is when an agent's grant lapses if the agent makes no act,
// as the entries sealed to pos leave it: AgentLapse after its last use.
// The zero time for a grant not booked, or ended.
func (b *Book) GrantLapses(grant string, pos int64) (time.Time, error) {
	if pos < 0 || pos > int64(len(b.entries)) {
		return time.Time{}, fmt.Errorf("%w: position %d, the book is sealed to %d", ErrNotSealed, pos, len(b.entries))
	}
	return grantLapses(agentGrant(b.entries[:pos], grant)), nil
}

// GrantLapses is when an agent's grant lapses unused, as the entries to
// pos leave it, pending entries counted.
func (v *View) GrantLapses(grant string, pos int64) (time.Time, error) {
	es := append(append([]*Sealed(nil), v.Book.entries...), v.pending...)
	return grantLapses(agentGrant(es[:max(0, min(pos, int64(len(es))))], grant)), nil
}

func grantLapses(g *GrantState) time.Time {
	if g == nil || g.Ended {
		return time.Time{}
	}
	return g.Last.Add(AgentLapse)
}

// deviceInForce is the device's latest entry in force at time at, by the
// rule the people's index uses too (DevicesInForce).
func deviceInForce(es []*Sealed, device string, at time.Time) *Sealed {
	var evs []DeviceEvent
	for k, s := range es {
		e := s.Entry
		switch e.Kind {
		case "device":
			evs = append(evs, DeviceEvent{Device: e.Device, Person: e.Person, TouchFor: e.TouchFor, Position: e.Position, Time: s.Time, Ref: k})
		case "removal":
			evs = append(evs, DeviceEvent{Removal: true, Device: e.Device, Person: e.Person, Position: e.Position, Ref: k})
		}
	}
	d := DevicesInForce(DeviceStates(evs), at)[device]
	if d == nil {
		return nil
	}
	return es[d.Ref]
}
