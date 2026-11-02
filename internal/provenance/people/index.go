package people

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// The index (PEOPLE-KEYS-2 10.4 item 11): the farm's device records, the
// provider keys their ID tokens were signed with, and the acts that change
// who may sign (removals, the sign-in provider setting, a one-person
// farm's browser codes and countersignatures, a second device's
// approval), in order.
//
// Each entry is canonical JSON (RFC 8785), kept in the Store by its C4 ID,
// and names the entry before it by its ID, so changing any past entry
// changes every ID after it. The records an entry books (a device record,
// an act, a provider's key) are in the same Store by their own C4 IDs.
//
// On a farm that keeps a farm book (internal/farmbook, Index.Book), the
// index is kept in the book: the issuer writes its entries there, pending,
// the queue checks each against its record and seals it, and a position is
// the book's. "In force" is book order. Before an entry is sealed the
// issuer judges against the sealed book and the pending entries; a
// verifier (DevicesAt, CheckAct) judges by sealed position only. The first
// seal adopts an index kept before the book (Adoption), unchanged.
//
// On a farm without one, the index is the issuer's own: its Head is the
// one mutable pointer, the last entry's ID and position, and every entry
// says its order is the issuer's (order: issuer), not the queue-sealed
// book's.

// OrderIssuer is an entry's order on a farm without a farm book, and of
// the entries a book adopted: the people's issuer's own. OrderBook is a
// sealed entry's in the book, OrderPending one written for its seal.
const (
	OrderIssuer  = farmbook.OrderIssuer
	OrderBook    = farmbook.OrderBook
	OrderPending = farmbook.OrderPending
)

// What an entry books.
const (
	// KindDevice: a device record (device/v1), an enrolment or a renewal.
	KindDevice = "device"
	// KindProviderKey: a sign-in provider's key (its JWK, canonical), the
	// first time a device record names it.
	KindProviderKey = "provider-key"
	// KindSetting: the farm's sign-in provider, from a signed act or the
	// install's preset.
	KindSetting = "setting"
	// KindRemoval: a signed act removing a device, or all of a person's.
	KindRemoval = "removal"
	// KindCode: a one-person farm's one-time code, by its SHA-256, that
	// the owner's key opened the HUD with.
	KindCode = "code"
	// KindCountersign: the owner's key countersigning the browser key its
	// code enrolled.
	KindCountersign = "countersign"
	// KindApproval: a confirmed act approving a new device or touch key
	// from another of the person's devices.
	KindApproval = "approval"
	// KindSignIns: the people's sign-ins, canonical, when they changed
	// since last booked.
	KindSignIns = "sign-ins"
	// KindFlag: something the issuer saw that the person is told of: a
	// device renewing from two places at once.
	KindFlag = "flag"
	// KindTouchWindow: a one-person farm's owner, with no touch key,
	// reopened the installer token's window for their first one (c4 farm
	// touch-key --reopen). Books no record.
	KindTouchWindow = "touch-window"
)

// FlagTwoPlaces is a device seen renewing from two network addresses
// within TwoPlaces of each other.
const FlagTwoPlaces = "two-places"

// Grades of a sign-in provider setting: install, the install's (the
// cluster administrator's word, as installing the farm is), the only one
// until a touch can confirm an admin's act (K9); then confirmed. Signed is
// an act's grade with no touch, which a setting never has.
const (
	GradeInstall   = "install"
	GradeSigned    = schema.GradeSigned
	GradeConfirmed = schema.GradeConfirmed
)

// Entry is one entry of the index.
type Entry struct {
	Farm string `json:"farm"`
	// Position counts from 1, in the issuer's order.
	Position int64 `json:"position"`
	// Prev is the C4 ID of the entry before; the first has none.
	Prev  string `json:"prev,omitempty"`
	Order string `json:"order"`
	// At is the issuer's clock when it booked the entry, RFC 3339 in UTC.
	At   string `json:"at"`
	Kind string `json:"kind"`
	// Record is the C4 ID of what the entry books: a device record, an
	// act's bundle or a provider's canonical JWK. A setting from the
	// install has none.
	Record string `json:"record,omitempty"`
	// Device and Person are whose device the entry is about; By is the
	// person whose act it books.
	Device string `json:"device,omitempty"`
	Person string `json:"person,omitempty"`
	By     string `json:"by,omitempty"`
	// Addr is where a device's enrolment or renewal came from, as the
	// issuer saw it (KindDevice).
	Addr string `json:"addr,omitempty"`
	// Issuer, KeyID: a provider key's (KindProviderKey) or the setting's
	// provider (KindSetting), with ClientID and Grade.
	Issuer   string `json:"issuer,omitempty"`
	KeyID    string `json:"kid,omitempty"`
	ClientID string `json:"clientID,omitempty"`
	Grade    string `json:"grade,omitempty"`
	// From is a removal's start, when it reaches into the past.
	From string `json:"from,omitempty"`
	// Code is a one-time code's SHA-256 (KindCode); Of the device record a
	// countersignature is of.
	Code string `json:"code,omitempty"`
	Of   string `json:"of,omitempty"`
	// Expires is when a code or an approval stops working.
	Expires string `json:"expires,omitempty"`
	// Flag and Seen: what was flagged, and what was seen (KindFlag).
	Flag string   `json:"flag,omitempty"`
	Seen []string `json:"seen,omitempty"`
	// TouchFor is a touch key's device, as its device record names it
	// (KindDevice).
	TouchFor string `json:"touchFor,omitempty"`
	// Private is the C4 ID of the entry's private part (privatePart): the
	// addresses a device was seen at, kept out of the entry with a random
	// salt, so the entry's ID says nothing of them. Only the device's
	// person and the farm's admins read it (10.4 item 19). Addr and Seen
	// are written into an entry only by an index from before private
	// parts.
	Private string `json:"private,omitempty"`
	// Sig is the people's issuer's signature over the entry as written
	// pending (book-entry/v1, component/issuer), on a farm with a farm
	// book: the queue seals only what the issuer wrote.
	Sig json.RawMessage `json:"sig,omitempty"`

	// counts is when the entry counts from: its seal, or for an entry a
	// book adopted, or one in the issuer's own order, its own at.
	counts time.Time
}

func (e *Entry) at() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, e.At)
	return t
}

// time is when the entry counts from.
func (e *Entry) time() time.Time {
	if !e.counts.IsZero() {
		return e.counts
	}
	return e.at()
}

func (e *Entry) expires() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, e.Expires)
	return t
}

// ErrNotFound means a Store does not hold a record.
var ErrNotFound = errors.New("not in the people's store")

// Store keeps the issuer's records and the index's entries by C4 ID. The
// manager's is an object store (--people-store); MemoryStore is for tests.
type Store interface {
	Put(ctx context.Context, id string, data []byte) error
	// Get returns ErrNotFound for a record it doesn't hold.
	Get(ctx context.Context, id string) ([]byte, error)
}

// HeadState is the index's head: the last entry's C4 ID and position, the
// issuer's signature over them, and the version a Write must be given to
// move it.
type HeadState struct {
	ID       string
	Position int64
	// Signed is a bundle the people's issuer signed (component/issuer),
	// naming the farm, the head and its position (PredicateIndexHead).
	Signed  []byte
	Version string
}

// PredicateIndexHead is the people's index's head as the issuer signed
// it. Every entry is linked to the head by its C4 ID, so the signature
// covers them all: whoever holds only the store's or the head Secret's
// credentials can't add, change or drop an entry. It doesn't stop a
// rollback to an older head the issuer signed, or a compromised manager,
// which holds the issuer's key (DESIGN.md section 9.2).
const PredicateIndexHead = "https://cccc.io/provenance/people-index/v1"

// headPredicate is what a signed head says.
type headPredicate struct {
	Farm     string `json:"farm"`
	Head     string `json:"head"`
	Position int64  `json:"position"`
}

// signHead signs a head as the issuer.
func (x *Index) signHead(ctx context.Context, sign func(context.Context) (*provenance.Signer, error), id string, pos int64, now time.Time) ([]byte, error) {
	s, err := sign(ctx)
	if err != nil {
		return nil, err
	}
	return provenance.SignStatement(ctx, s, PredicateIndexHead, []provenance.Subject{{Name: "people-index:" + x.Farm, Digest: map[string]string{"c4": id}}},
		headPredicate{Farm: x.Farm, Head: id, Position: pos}, now)
}

// checkHead checks a head is the one the issuer signed for this farm.
func (x *Index) checkHead(h HeadState) error {
	if h.Position == 0 && h.ID == "" {
		return nil
	}
	if len(h.Signed) == 0 {
		return fmt.Errorf("%w: the head is not signed by the people's issuer", ErrIndex)
	}
	v, err := provenance.VerifyBundle(h.Signed, x.Roots)
	if err != nil {
		return fmt.Errorf("%w: the head's signature: %v", ErrIndex, err)
	}
	var p headPredicate
	json.Unmarshal(v.Predicate, &p)
	switch {
	case v.PredicateType != PredicateIndexHead || v.Identity != provenance.ComponentIdentity("issuer"):
		return fmt.Errorf("%w: the head is signed by %s, not the people's issuer", ErrIndex, v.Identity)
	case v.Farm != x.Farm || p.Farm != x.Farm || p.Head != h.ID || p.Position != h.Position:
		return fmt.Errorf("%w: the head names entry %.16s at %d, and its signature %.16s at %d", ErrIndex, h.ID, h.Position, p.Head, p.Position)
	}
	return nil
}

// ErrMoved means another writer moved the head first.
var ErrMoved = errors.New("the index's head moved")

// Head is the index's one mutable pointer. The manager's is a Secret in
// the signing namespace, written with its resourceVersion, so replicas
// append one at a time.
type Head interface {
	Read(ctx context.Context) (HeadState, error)
	// Write moves the head from from to to, or fails with ErrMoved.
	Write(ctx context.Context, from, to HeadState) error
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

// MemoryHead is a Head in memory.
type MemoryHead struct {
	mu sync.Mutex
	h  HeadState
	n  int
}

func (h *MemoryHead) Read(context.Context) (HeadState, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.h, nil
}

func (h *MemoryHead) Write(_ context.Context, from, to HeadState) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if from.Version != h.h.Version {
		return ErrMoved
	}
	h.n++
	to.Version = fmt.Sprint(h.n)
	h.h = to
	return nil
}

// Index is the farm's index as this process has read it, from its Store
// and Head. Every read catches up with the head first.
type Index struct {
	Farm  string
	Roots *x509.CertPool
	Store Store
	Head  Head
	// Book is the farm book the index is kept in, or nil for the issuer's
	// own. With a book, Store is the book's store too, so the queue finds
	// the records the entries cite; Head is the index kept before the book,
	// read only to wait for its adoption.
	Book *farmbook.Reader

	mu sync.Mutex
	// entries are the entries read, sealed in the book's order with a book,
	// with their IDs; pend are the pending ones at provisional positions.
	entries []*Entry
	ids     []string
	pend    []*Entry
	pendIDs []string
	// sealed is the last sealed position, and pos the last position read,
	// pending entries (any writer's) counted. Without a book both are the
	// last entry's.
	sealed, pos int64
	// state is the book's State as last read: an append moves it.
	state farmbook.State
	// head is the book's head segment as last read.
	head *farmbook.Segment
	// pendActs are the payload hashes of the acts pending entries book.
	pendActs map[string]bool
	// written are the IDs of the entries read, as their writer wrote them
	// (writtenID), sealed and pending: an entry is booked once.
	written, pendWritten map[string]bool
	// priv are the private parts the entries name, by C4 ID.
	priv map[string]privatePart
	// unchecked are the positions of sealed entries that no longer check
	// (the farm's roots changed): kept in the book, counting for nothing,
	// as the queue's sealer does too.
	unchecked []int64
	// fbook is the sealed farm book as last read, for the time each
	// position was sealed; sealing, set while the queue's checker admits
	// an entry, is the time of the seal it goes into.
	fbook   *farmbook.Book
	sealing time.Time
	// devices are the device records the index books, by record ID.
	devices map[string]*schema.Device
	// raw are the device records' bundles, by record ID.
	raw map[string][]byte
	// acts are the payload hashes of the acts the index books: an act is
	// booked once, whichever replica it reaches and however long after.
	acts map[string]bool
}

// ErrIndex means the index doesn't hold together: an entry that doesn't
// hash to its ID, a broken link, or a record that isn't what its entry
// says.
var ErrIndex = errors.New("the people's index")

// Sync reads the entries booked since this process last read the index,
// checking each: it hashes to its ID, is canonical, names the entry before
// it and its position, is this farm's in the issuer's order, and the
// record it books is that record. A device record must verify against the
// farm's roots and be signed by the people's issuer (component/issuer).
func (x *Index) Sync(ctx context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.sync(ctx)
}

func (x *Index) sync(ctx context.Context) error {
	if x.Book != nil {
		return x.syncBook(ctx)
	}
	defer func() { x.sealed, x.pos = int64(len(x.entries)), int64(len(x.entries)) }()
	h, err := x.Head.Read(ctx)
	if err != nil {
		return err
	}
	if err := x.checkHead(h); err != nil {
		return err
	}
	have := int64(len(x.entries))
	if h.Position == have && (have == 0 || h.ID == x.ids[have-1]) {
		return nil
	}
	switch {
	case h.Position < have:
		return fmt.Errorf("%w: the head is at position %d, before %d this process read: the index was rewritten", ErrIndex, h.Position, have)
	case h.Position == have:
		return fmt.Errorf("%w: the head names entry %.16s at position %d, where this process read %.16s: the index was rewritten", ErrIndex, h.ID, have, x.ids[have-1])
	}
	// Walk back from the head to the last entry already read.
	var news []*Entry
	var newIDs []string
	for id, pos := h.ID, h.Position; pos > have; pos-- {
		e, err := x.entry(ctx, id)
		if err != nil {
			return err
		}
		if e.Position != pos {
			return fmt.Errorf("%w: entry %.16s is at position %d, where %d was linked", ErrIndex, id, e.Position, pos)
		}
		news = append(news, e)
		newIDs = append(newIDs, id)
		id = e.Prev
	}
	last := ""
	if have > 0 {
		last = x.ids[have-1]
	}
	if oldest := news[len(news)-1]; oldest.Prev != last {
		return fmt.Errorf("%w: entry %d names %.16s before it, not %.16s: the index was rewritten", ErrIndex, oldest.Position, oldest.Prev, last)
	}
	for k := len(news) - 1; k >= 0; k-- {
		if err := x.load(ctx, news[k], false); err != nil {
			return fmt.Errorf("%w: entry %d: %v", ErrIndex, news[k].Position, err)
		}
		x.entries = append(x.entries, news[k])
		x.ids = append(x.ids, newIDs[k])
	}
	return nil
}

// entry reads one entry and checks it is what its ID says.
func (x *Index) entry(ctx context.Context, id string) (*Entry, error) {
	b, err := x.Store.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%w: entry %.16s: %v", ErrIndex, id, err)
	}
	if got := c4id(b); got != id {
		return nil, fmt.Errorf("%w: entry %.16s hashes to %.16s", ErrIndex, id, got)
	}
	if canon, err := schema.Canonical(b); err != nil || !bytes.Equal(canon, b) {
		return nil, fmt.Errorf("%w: entry %.16s is not canonical JSON", ErrIndex, id)
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("%w: entry %.16s: %v", ErrIndex, id, err)
	}
	switch {
	case e.Farm != x.Farm:
		return nil, fmt.Errorf("%w: entry %.16s is farm %.16s's", ErrIndex, id, e.Farm)
	case e.Order != OrderIssuer:
		return nil, fmt.Errorf("%w: entry %.16s is in order %q", ErrIndex, id, e.Order)
	case e.Position < 1 || (e.Position == 1) != (e.Prev == ""):
		return nil, fmt.Errorf("%w: entry %.16s: position %d, prev %q", ErrIndex, id, e.Position, e.Prev)
	}
	return &e, nil
}

// load checks an entry against the record it books, and keeps a device
// record. An entry says nothing its record doesn't: its device and person
// are the device record's; a provider key's kid is its JWK's; an act's
// verb is the entry's kind, its signer the entry's by, and its subject
// and params the entry's device, person, code, record and from; an
// approval is confirmed; a code's or an approval's expiry is its TTL from
// when it was booked, and it was booked within ActSkew of being signed.
// A setting is the install's, and books no record. Everything before the
// entry is read at its position less one: the entries sealed before it,
// and, for a pending entry, the pending ones before it.
func (x *Index) load(ctx context.Context, e *Entry, pending bool) error {
	// An entry written for a farm book is the issuer's, signed, and booked
	// once, whatever its kind; its addresses are in its private part.
	written := ""
	if inBook(e) {
		id, _, err := x.checkSig(e)
		if err != nil {
			return err
		}
		if x.written[id] || x.pendWritten[id] {
			return farmbook.Refuse(farmbook.ReasonBooked, fmt.Errorf("entry %.16s is booked already", id))
		}
		if e.Addr != "" || len(e.Seen) > 0 {
			return farmbook.Refuse(farmbook.ReasonMalformed, errors.New("an entry keeps its addresses in its private part"))
		}
		written = id
	}
	if err := x.loadPrivate(ctx, e); err != nil {
		return err
	}
	if err := x.loadKind(ctx, e, pending); err != nil {
		return err
	}
	if written != "" {
		set := &x.written
		if pending {
			set = &x.pendWritten
		}
		if *set == nil {
			*set = map[string]bool{}
		}
		(*set)[written] = true
	}
	return nil
}

// loadKind checks an entry of its kind against what it books.
func (x *Index) loadKind(ctx context.Context, e *Entry, pending bool) error {
	switch e.Kind {
	case KindFlag:
		if e.Record != "" || e.Flag != FlagTwoPlaces || x.at(e.Position - 1)[e.Device] == nil {
			return fmt.Errorf("a flag is %s, of a device the index books, and books no record", FlagTwoPlaces)
		}
		return nil
	case KindTouchWindow:
		if e.Record != "" || e.Person == "" || e.Device == "" || x.at(int64(len(x.entries)))[e.Device] == nil {
			return errors.New("a touch window is a person's, for an owner key the index books, and books no record")
		}
		return nil
	case KindSetting:
		if e.Record != "" || e.Grade != GradeInstall || e.Issuer == "" || e.ClientID == "" {
			return errors.New("a sign-in provider setting is the install's, with an issuer and a client ID, and books no record")
		}
		return nil
	}
	if e.Record == "" {
		return fmt.Errorf("a %s entry books no record", e.Kind)
	}
	b, err := x.Store.Get(ctx, e.Record)
	if err != nil {
		return err
	}
	if got := c4id(b); got != e.Record {
		return fmt.Errorf("record %.16s hashes to %.16s", e.Record, got)
	}
	switch e.Kind {
	case KindDevice:
		d, err := x.checkDeviceRecord(b)
		if err != nil {
			return err
		}
		spki, _ := d.Predicate.PublicKey()
		if schema.DeviceID(spki) != e.Device || d.Predicate.Person != e.Person {
			return fmt.Errorf("the entry names device %.16s of %s, its record %.16s of %s", e.Device, e.Person, schema.DeviceID(spki), d.Predicate.Person)
		}
		if (e.TouchFor != "" || inBook(e)) && e.TouchFor != d.Predicate.TouchFor {
			return fmt.Errorf("the entry names touchFor %.16s, its record %.16s", e.TouchFor, d.Predicate.TouchFor)
		}
		if x.devices == nil {
			x.devices, x.raw = map[string]*schema.Device{}, map[string][]byte{}
		}
		x.devices[e.Record], x.raw[e.Record] = d, b
	case KindSignIns:
		if _, err := parseSignIns(b); err != nil {
			return err
		}
	case KindProviderKey:
		k, err := parseJWK(b)
		if err != nil {
			return err
		}
		if k.KeyID != e.KeyID || e.Issuer == "" {
			return fmt.Errorf("the entry names key %q of %q, its JWK %q", e.KeyID, e.Issuer, k.KeyID)
		}
	default:
		return x.loadAct(e, b, pending)
	}
	return nil
}

// actKinds is the verb each act entry books.
var actKinds = map[string]string{KindRemoval: ActRemoveDevice, KindCode: ActBrowserCode, KindCountersign: ActCountersign, KindApproval: ActApprove}

// loadAct checks an act entry against its act: still the act it was,
// signed as it was, by a device in force at the position before the entry
// and, for a confirmed act, beside that device's own touch key there
// (checkAct), and saying what the entry says. The queue makes the same
// check at the seal (BookChecker), so a confirmed grade is never sealed
// on a touch key nobody checked.
func (x *Index) loadAct(e *Entry, b []byte, pending bool) error {
	verb, ok := actKinds[e.Kind]
	if !ok {
		return fmt.Errorf("an entry of kind %q", e.Kind)
	}
	v, err := provenance.VerifyBundle(b, x.Roots)
	if err != nil {
		return err
	}
	if v.PredicateType != provenance.PredicateAct {
		return fmt.Errorf("a %s entry books a %s record, not an act", e.Kind, v.PredicateType)
	}
	// A booked act is judged at the time its entry counts from (in the
	// issuer's own order, the act's signedAt).
	at := v.SignedAt
	if inBook(e) || x.Book != nil || !x.sealing.IsZero() {
		at = e.time()
	}
	if err := x.checkAct(v, e.Position-1, at); err != nil {
		return err
	}
	a, errs := schema.ParseAct(v.Payload)
	if errs != nil {
		return fmt.Errorf("not an act: %s: %s", errs[0].Path, errs[0].Message)
	}
	p := a.Predicate
	var params map[string]string
	json.Unmarshal(p.Params, &params)
	subject := a.Subject[0]
	device := ""
	if id := subject.Digest["c4"]; subject.Name == "device:"+id {
		device = id
	}
	onFarm := subject.Name == "farm:"+x.Farm && subject.Digest["c4"] == x.Farm
	lives := e.expires().Sub(e.at())
	says := func(ok bool, what string) error {
		if ok {
			return nil
		}
		return fmt.Errorf("the %s entry and its %s act disagree on %s", e.Kind, p.Act, what)
	}
	if err := says(p.Act == verb, "the verb"); err != nil {
		return err
	}
	if err := says(p.Chain.By.Person != "" && e.By == p.Chain.By.Person, "who made it"); err != nil {
		return err
	}
	if d := e.at().Sub(v.SignedAt); d > provenance.ActSkew || d < -provenance.ActSkew {
		return fmt.Errorf("the %s entry was booked %s from when its act was signed", e.Kind, d.Round(time.Second))
	}
	switch e.Kind {
	case KindRemoval:
		if device != "" {
			d := x.at(e.Position - 1)[device]
			err = says(e.Device == device && params["person"] == "" && d != nil && d.person() == e.Person, "the device and whose it is")
		} else {
			err = says(onFarm && e.Device == "" && params["person"] == e.Person, "whose devices")
		}
		if err == nil {
			err = says(e.From == params["from"], "from")
		}
	case KindCode:
		err = says(onFarm && e.Code == params["code"] && e.Person == e.By && lives == CodeTTL, "the code, whose it is and when it lapses")
	case KindCountersign:
		err = says(device != "" && e.Device == device && e.Of == params["record"] && e.Person == e.By, "the device and the record")
	case KindApproval:
		err = says(device != "" && e.Device == device && e.Person == e.By && v.Confirmed() && lives == ApprovalTTL, "the device, whose it is, the touch and when it lapses")
	}
	if err != nil {
		return err
	}
	hash := provenance.PayloadHash(v.Payload)
	if x.acts[hash] || x.pendActs[hash] {
		return fmt.Errorf("act %.16s is booked twice", hash)
	}
	acts := &x.acts
	if pending {
		acts = &x.pendActs
	}
	if *acts == nil {
		*acts = map[string]bool{}
	}
	(*acts)[hash] = true
	return nil
}

// checkDeviceRecord verifies a device record: its bundle against the
// farm's roots, the signer policy (the people's issuer, component/issuer),
// its shape, and its own proof.
func (x *Index) checkDeviceRecord(b []byte) (*schema.Device, error) {
	v, err := provenance.VerifyBundle(b, x.Roots)
	if err != nil {
		return nil, err
	}
	if v.PredicateType != provenance.PredicateDevice {
		return nil, fmt.Errorf("a %s record, not a device record", v.PredicateType)
	}
	if err := provenance.CheckSigner(v); err != nil {
		return nil, fmt.Errorf("device record: %w", err)
	}
	d, errs := schema.ParseDevice(v.Payload)
	if errs != nil {
		return nil, fmt.Errorf("device record: %s: %s", errs[0].Path, errs[0].Message)
	}
	if d.Predicate.Farm != x.Farm || v.Farm != x.Farm {
		return nil, fmt.Errorf("device record of farm %.16s, signed under %.16s", d.Predicate.Farm, v.Farm)
	}
	if err := checkProof(&d.Predicate); err != nil {
		return nil, err
	}
	return d, nil
}

// pending is an entry to book, and the record it books.
type pending struct {
	entry  *Entry
	record []byte
}

// maxAppendTries bounds how often an append is rebuilt after another
// writer moved the head.
const maxAppendTries = 8

// append books the entries build makes from the index as it stands. When
// another writer moves the head first, it reads the index again and
// builds again, so build's checks always see what came before. The
// records go to the store before the entries, and the entries before the
// head, so the head never names what the store lacks. With a farm book the
// entries go to the book instead (appendBook).
func (x *Index) append(ctx context.Context, now time.Time, sign func(context.Context) (*provenance.Signer, error), build func() ([]pending, error)) ([]*Entry, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.Book != nil {
		return x.appendBook(ctx, now, sign, build)
	}
	for try := 0; ; try++ {
		if err := x.sync(ctx); err != nil {
			return nil, err
		}
		ps, err := build()
		if err != nil || len(ps) == 0 {
			return nil, err
		}
		from, err := x.Head.Read(ctx)
		if err != nil {
			return nil, err
		}
		if from.Position != x.pos {
			continue
		}
		prev, pos := from.ID, from.Position
		if err := x.prepare(ctx, ps); err != nil {
			return nil, err
		}
		var out []*Entry
		var ids []string
		for _, p := range ps {
			e := p.entry
			pos++
			e.Farm, e.Position, e.Prev, e.Order, e.At = x.Farm, pos, prev, OrderIssuer, now.UTC().Format(time.RFC3339Nano)
			raw, err := json.Marshal(e)
			if err != nil {
				return nil, err
			}
			b, err := schema.Canonical(raw)
			if err != nil {
				return nil, err
			}
			prev = c4id(b)
			if err := x.Store.Put(ctx, prev, b); err != nil {
				return nil, err
			}
			out, ids = append(out, e), append(ids, prev)
		}
		signed, err := x.signHead(ctx, sign, prev, pos, now)
		if err != nil {
			return nil, err
		}
		err = x.Head.Write(ctx, from, HeadState{ID: prev, Position: pos, Signed: signed})
		if errors.Is(err, ErrMoved) && try < maxAppendTries {
			continue
		}
		if err != nil {
			return nil, err
		}
		for k, e := range out {
			if err := x.load(ctx, e, false); err != nil {
				return nil, err
			}
			x.entries = append(x.entries, e)
			x.ids = append(x.ids, ids[k])
		}
		x.sealed, x.pos = int64(len(x.entries)), int64(len(x.entries))
		return out, nil
	}
}

// Position is the index's position now: its last entry's, 0 when empty.
// With a farm book it counts the entries pending the seal, at provisional
// positions; Sealed is the last sealed position.
func (x *Index) Position(ctx context.Context) (int64, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return 0, err
	}
	return x.pos, nil
}

// Sealed is the last position the queue sealed: what a verifier judges at.
// Without a farm book, every entry the issuer booked is its own order's,
// and Sealed is Position.
func (x *Index) Sealed(ctx context.Context) (int64, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return 0, err
	}
	return x.sealed, nil
}

// device is one device as the index has it at a position: its latest
// record, and the positions it was enrolled and last renewed at.
type device struct {
	id, record   string
	rec          *schema.Device
	first, last  int64
	lastAt       time.Time
	addr         string
	removed      bool
	countersigns int
	state        *farmbook.DeviceState
}

func (d *device) person() string { return d.rec.Predicate.Person }

// Lapse is how long a device may go without renewing before it lapses
// (PEOPLE-KEYS-2 7): it makes a new key on its next sign-in.
const Lapse = farmbook.DeviceLapse

// at is every device the index books up to position pos, as of then, by
// the rule the farm book uses too (farmbook.DeviceStates): removed from
// the removal's position on, a person-wide removal reaching only the
// devices enrolled before it.
func (x *Index) at(pos int64) map[string]*device {
	es := x.upTo(pos)
	var evs []farmbook.DeviceEvent
	for k, e := range es {
		switch e.Kind {
		case KindDevice:
			rec := x.devices[e.Record]
			if rec == nil {
				continue
			}
			evs = append(evs, farmbook.DeviceEvent{Device: e.Device, Person: rec.Predicate.Person, TouchFor: rec.Predicate.TouchFor,
				Position: e.Position, Time: e.time(), Ref: k})
		case KindRemoval:
			evs = append(evs, farmbook.DeviceEvent{Removal: true, Device: e.Device, Person: e.Person, Position: e.Position, Ref: k})
		}
	}
	devs := map[string]*device{}
	for id, st := range farmbook.DeviceStates(evs) {
		e := es[st.Ref]
		devs[id] = &device{id: id, record: e.Record, rec: x.devices[e.Record], first: st.First, last: st.Last, lastAt: st.LastTime,
			addr: x.addrOf(e), removed: st.Removed, state: st}
	}
	for _, e := range es {
		if d := devs[e.Device]; e.Kind == KindCountersign && d != nil && d.record == e.Of {
			d.countersigns++
		}
	}
	return devs
}

// inForce is the devices in force at position pos and time at: not
// removed, renewed within Lapse of at, and, for a touch key, its device
// key in force too (farmbook.DevicesInForce).
func (x *Index) inForce(pos int64, at time.Time) map[string]*device {
	devs := x.at(pos)
	states := map[string]*farmbook.DeviceState{}
	for id, d := range devs {
		states[id] = d.state
	}
	out := map[string]*device{}
	for id := range farmbook.DevicesInForce(states, at) {
		out[id] = devs[id]
	}
	return out
}

// addrOf is where an entry says its device was seen: its private part's
// address, or an entry's own from before private parts.
func (x *Index) addrOf(e *Entry) string {
	if e.Addr != "" {
		return e.Addr
	}
	return x.priv[e.Private].Addr
}

// seenOf is what a flag entry says was seen.
func (x *Index) seenOf(e *Entry) []string {
	if len(e.Seen) > 0 {
		return e.Seen
	}
	return x.priv[e.Private].Seen
}

// DevicesAt is the device records in force at position pos and time at,
// the latest of each device: what CheckTouchKey reads, and what a verifier
// checks a person's act against at the act's position. With a farm book,
// pos is a book position, judged by sealed entries only, at the time pos
// was sealed (at is the issuer's own order's): a position past the sealed
// head is farmbook.ErrNotSealed.
func (x *Index) DevicesAt(ctx context.Context, pos int64, at time.Time) ([]*schema.Device, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return nil, err
	}
	if err := x.verifiable(pos); err != nil {
		return nil, err
	}
	var out []*schema.Device
	for _, d := range x.inForce(pos, x.timeAt(pos, at)) {
		out = append(out, d.rec)
	}
	return out, nil
}

// ErrNotInForce means a person's act is signed by a device that isn't in
// force at its position: never enrolled, removed or lapsed. A certificate
// for user/<name> with no device record behind it is a forgery
// (PEOPLE-KEYS-2 3.1).
var ErrNotInForce = errors.New("no device record in force")

// CheckAct checks a person's act against the device records in force at
// position pos, as of the act's signedAt: its device key's record is the
// signer's, on this farm, and is no touch key; and a confirmed act's
// touch key is that person's touch key for that device (CheckTouchKey).
// An act that isn't a person's passes. With a farm book, as DevicesAt,
// only sealed entries count.
func (x *Index) CheckAct(ctx context.Context, v *provenance.Verified, pos int64) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return err
	}
	if err := x.verifiable(pos); err != nil {
		return err
	}
	return x.checkAct(v, pos, x.timeAt(pos, v.SignedAt))
}

// CheckActNow is CheckAct as a live decision makes it: against every entry
// read, pending ones too, at the clock, so a removal or a lapse counts at
// once. The HUD takes a person's act by it.
func (x *Index) CheckActNow(ctx context.Context, v *provenance.Verified) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return err
	}
	return x.checkAct(v, x.pos, time.Now())
}

// verifiable refuses a verifier's position past the sealed head, where
// pending entries would count: a verifier judges by sealed position only.
func (x *Index) verifiable(pos int64) error {
	if x.Book != nil && pos > x.sealed {
		return fmt.Errorf("%w: position %d, the farm book is sealed to %d", farmbook.ErrNotSealed, pos, x.sealed)
	}
	return nil
}

// checkAct is CheckAct on the index as this process has read it.
//
// at is the time time-based ends are judged at: a verifier's, the time pos
// was sealed (CheckAct); a live decision's, the issuer's clock (takeAct).
func (x *Index) checkAct(v *provenance.Verified, pos int64, at time.Time) error {
	var devs []*schema.Device
	for _, d := range x.inForce(pos, at) {
		devs = append(devs, d.rec)
	}
	person, ok := strings.CutPrefix(v.Identity, provenance.IdentityBase+"user/")
	if !ok {
		return nil
	}
	if p, err := url.PathUnescape(person); err == nil {
		person = p
	}
	var d *schema.Device
	for _, r := range devs {
		if spki, _ := r.Predicate.PublicKey(); schema.DeviceID(spki) == v.Device {
			d = r
		}
	}
	switch {
	case d == nil:
		return fmt.Errorf("%w: %s's certificate names device %.16s, and no device record behind it is in force at position %d: a forgery, or a device removed or lapsed", ErrNotInForce, person, v.Device, pos)
	case d.Predicate.Person != person || d.Predicate.Farm != v.Farm:
		return fmt.Errorf("%w: device %.16s is %s's at farm %.16s, the act %s's at %.16s", ErrNotInForce, v.Device, d.Predicate.Person, d.Predicate.Farm, person, v.Farm)
	case d.Predicate.TouchFor != "":
		return fmt.Errorf("%w: device %.16s is a touch key: it signs beside its device key, never first", ErrTouchKey, v.Device)
	}
	if err := provenance.CheckTouchKey(v, devs); err != nil {
		return fmt.Errorf("%w: %v", ErrTouchKey, err)
	}
	return nil
}

// ErrTouchKey means a confirmed act's touch key isn't, by the device
// records in force at its position, the signer's touch key for the device
// that signed it: its record doesn't name that device (touchFor), or is
// another person's or farm's. Or a touch key signed first.
var ErrTouchKey = errors.New("not the signer's touch key")

// Entries returns the entries after position from, with their IDs: with a
// farm book, the sealed ones, at their book positions.
func (x *Index) Entries(ctx context.Context, from int64) ([]*Entry, []string, error) {
	p, err := x.page(ctx, from)
	return p.entries, p.ids, err
}

// indexPage is the entries after a position, read at once with the
// position they reach and, with a farm book, its head segment.
type indexPage struct {
	entries []*Entry
	ids     []string
	// end is the position the entries reach: the last entry's, sealed
	// only with a farm book.
	end  int64
	head *farmbook.Segment
}

// page is Entries with its end and head, all read under one lock, so a
// page never names a position other than the one its entries reach: a
// page from past the end holds none and answers the end, as one from
// before the start holds all and does (from clamped to [0, end]).
func (x *Index) page(ctx context.Context, from int64) (indexPage, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return indexPage{}, err
	}
	p := indexPage{end: x.sealed, head: x.head}
	for k, e := range x.entries {
		if e.Position > from {
			p.entries, p.ids = append(p.entries, e), append(p.ids, x.ids[k])
		}
	}
	return p, nil
}

// entryByID is the entry with this C4 ID, sealed or pending.
func (x *Index) entryByID(id string) (*Entry, bool) {
	for k, e := range x.entries {
		if x.ids[k] == id {
			return e, true
		}
	}
	for k, e := range x.pend {
		if x.pendIDs[k] == id {
			return e, true
		}
	}
	return nil, false
}

// books reports whether an entry books the record with this C4 ID.
func (x *Index) books(id string) bool {
	for _, e := range x.all() {
		if e.Record == id {
			return true
		}
	}
	return false
}

func c4id(b []byte) string { return c4.Identify(bytes.NewReader(b)).String() }
