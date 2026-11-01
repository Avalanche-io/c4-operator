// Package farmbook is the farm book (accountability DESIGN 5.5, DESIGN.md
// section 9.2): the farm-wide, hash-linked log of farm-level acts, kept in
// the installation's system store rather than any project's.
//
// The HUD and the people's issuer write the entries; the leader-elected
// queue seals them, so one writer seals the book. A writer stores the
// record an entry cites, then the entry, then lists the entry as pending
// in the book's one mutable object (State). The queue, at a bounded
// interval (Sealer.Every), checks each pending entry against the record it
// cites, refuses any that doesn't verify, isn't signed by its signer or
// says something its record doesn't, and seals the rest: each gets its
// position and the C4 ID of the entry before it, and the run is signed as
// one segment (book/v1, component/queue) naming the segment before it.
//
// A record counts from the first sealed segment that names it (DESIGN
// 5.4), so the book's order, not anyone's clock, says whether a grant,
// binding, device or membership was in force when a record was made.
// Before an entry is sealed, the HUD and the people's issuer judge a
// request against the sealed book and the pending entries (View); once it
// is sealed, a verifier judges by sealed position only (Book).
//
// Signing shows a rewrite: any change to a past entry or segment changes
// every ID after it. It doesn't stop a rollback to an older head the queue
// signed, by whoever can write the State. A reader stops that by keeping
// the last head it saw (LastHead) and refusing one that goes back.
package farmbook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/schema"
)

// The orders an entry is in: book, sealed by the queue at its position;
// pending, written for the seal, its position not yet given; issuer, a
// people's index kept before the book and adopted unchanged.
const (
	OrderBook    = "book"
	OrderPending = "pending"
	OrderIssuer  = "issuer"
)

// KindControl is an entry booking one of the HUD's control records: a
// farm-level act a person or agent took through the HUD (ControlActs).
// The people's issuer's kinds (device, removal, setting and the rest) are
// its own (internal/provenance/people).
const KindControl = "control"

// Entry is a book entry's fields that every kind shares, and the fields
// of a control entry. An entry is canonical JSON (RFC 8785), kept in the
// store by its C4 ID; a kind's own fields are read by its checker from the
// entry as stored.
type Entry struct {
	Farm string `json:"farm"`
	// Position counts from 1, in the book's order; Prev is the C4 ID of
	// the entry before, and the first has none. A pending entry has
	// neither.
	Position int64  `json:"position"`
	Prev     string `json:"prev,omitempty"`
	Order    string `json:"order"`
	// At is the writer's clock when it wrote the entry, RFC 3339 in UTC:
	// its claim, as a record's signedAt is.
	At   string `json:"at"`
	Kind string `json:"kind"`
	// Record is the C4 ID of the record the entry books.
	Record string `json:"record,omitempty"`
	// Act, Subject, By and Settings are a control record's action, target,
	// actor, and the C4 ID of its change as canonical JSON.
	Act      string `json:"act,omitempty"`
	Subject  string `json:"subject,omitempty"`
	By       string `json:"by,omitempty"`
	Settings string `json:"settings,omitempty"`
	// Person and Device are whom the entry is about: a grant's person, a
	// binding's owner, a device's person and key; TouchFor is a touch
	// key's device, as its device record names it.
	Person   string `json:"person,omitempty"`
	Device   string `json:"device,omitempty"`
	TouchFor string `json:"touchFor,omitempty"`
	// Expires is when what the entry books stops being in force by itself:
	// a grant's until.
	Expires string `json:"expires,omitempty"`
}

func (e *Entry) expires() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, e.Expires)
	return t
}

// ParseEntry reads an entry's shared fields, each from the key that names
// it exactly: a key in another case is no field of it.
func ParseEntry(raw []byte) (*Entry, error) {
	var e Entry
	if err := exactFields(raw, &e); err != nil {
		return nil, fmt.Errorf("not a book entry: %w", err)
	}
	return &e, nil
}

// Errors a reader reports.
var (
	// ErrBook means the book doesn't hold together: a segment that doesn't
	// verify, isn't the queue's, or doesn't name the one before it, or an
	// entry that isn't what its segment says.
	ErrBook = errors.New("the farm book")
	// ErrRollback means the head went back, or forked, from the last head
	// the reader saw.
	ErrRollback = errors.New("the farm book's head went back")
	// ErrNotSealed means a position past the sealed head.
	ErrNotSealed = errors.New("not sealed yet")
	// ErrMoved means another writer moved the State first.
	ErrMoved = errors.New("the farm book's state moved")
	// ErrNotFound means a Store doesn't hold a record.
	ErrNotFound = errors.New("not in the farm book's store")
)

// Store keeps the book's segments, its entries and the records they cite,
// by C4 ID: the installation's system store (--system-store), readable
// only by the manager. MemoryStore is for tests.
type Store interface {
	Put(ctx context.Context, id string, data []byte) error
	// Get returns ErrNotFound for a record it doesn't hold.
	Get(ctx context.Context, id string) ([]byte, error)
}

// Head is the sealed head: the last segment, its number, and the last
// entry it seals.
type Head struct {
	Segment  string `json:"segment,omitempty"`
	Number   int64  `json:"number"`
	Position int64  `json:"position"`
	Entry    string `json:"entry,omitempty"`
}

// State is the book's one mutable object: the sealed head, the entries
// waiting for the seal by C4 ID in the order written, and the version a
// Write must be given to move it.
type State struct {
	Head    Head
	Pending []string
	Version string
}

// Pointer holds the State. The manager's is the Secret c4-farm-book in
// the signing namespace (SecretPointer), written by resourceVersion, so
// writers and the sealer move it one at a time.
type Pointer interface {
	Read(ctx context.Context) (State, error)
	// Write moves the State from from to to, or fails with ErrMoved.
	Write(ctx context.Context, from, to State) error
}

// MaxPending bounds the entries waiting for the seal: a writer past it is
// refused until the queue seals, and the queue drops any listed past it.
// MaxSeal is how many one seal takes; the rest wait for the next.
const (
	MaxPending = 4096
	MaxSeal    = 512
)

// SealerState is the sealer's own record of what it sealed: its last head,
// and whether it adopted a people's index. It is kept apart from the
// State, where a writer of the State can't reach it (the Secret
// c4-farm-book-sealer), so a State rolled back, forked or emptied is put
// right from it, never sealed over, and adoption happens once.
type SealerState struct {
	Head    Head   `json:"head"`
	Adopted bool   `json:"adopted,omitempty"`
	Version string `json:"-"`
}

// SealerStore keeps the SealerState.
type SealerStore interface {
	Read(ctx context.Context) (SealerState, error)
	// Write moves it from from to to, or fails with ErrMoved.
	Write(ctx context.Context, from, to SealerState) error
}

// MemorySealer is a SealerStore in memory.
type MemorySealer struct {
	mu sync.Mutex
	s  SealerState
	n  int
}

func (m *MemorySealer) Read(context.Context) (SealerState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s, nil
}

func (m *MemorySealer) Write(_ context.Context, from, to SealerState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if from.Version != m.s.Version {
		return ErrMoved
	}
	m.n++
	to.Version = fmt.Sprint(m.n)
	m.s = to
	return nil
}

// MemoryStore is a Store in memory.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *MemoryStore) Put(_ context.Context, id string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[id] = bytes.Clone(data)
	return nil
}

func (s *MemoryStore) Get(_ context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[id]
	if !ok {
		return nil, fmt.Errorf("%w: %.16s", ErrNotFound, id)
	}
	return bytes.Clone(b), nil
}

// MemoryPointer is a Pointer in memory.
type MemoryPointer struct {
	mu sync.Mutex
	s  State
	n  int
}

func (p *MemoryPointer) Read(context.Context) (State, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.s
	s.Pending = append([]string(nil), s.Pending...)
	return s, nil
}

func (p *MemoryPointer) Write(_ context.Context, from, to State) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if from.Version != p.s.Version {
		return ErrMoved
	}
	p.n++
	to.Version = fmt.Sprint(p.n)
	to.Pending = append([]string(nil), to.Pending...)
	p.s = to
	return nil
}

// Set replaces the State whatever its version: what whoever can write the
// manager's ConfigMap can do, for tests of a rollback.
func (p *MemoryPointer) Set(s State) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	s.Version = fmt.Sprint(p.n)
	p.s = s
}

func c4id(b []byte) string { return c4.Identify(bytes.NewReader(b)).String() }

// canonical is v as canonical JSON (RFC 8785), and its C4 ID.
func canonical(v any) ([]byte, string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	b, err := schema.Canonical(raw)
	if err != nil {
		return nil, "", err
	}
	return b, c4id(b), nil
}
