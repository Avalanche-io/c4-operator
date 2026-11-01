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

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// The index (PEOPLE-KEYS-2 10.4 item 11): the farm's device records, the
// provider keys their ID tokens were signed with, and the acts that change
// who may sign (removals, the sign-in provider setting, a one-person
// farm's browser codes and countersignatures, a second device's
// approval), in the order the issuer booked them. Slice 2's farm book
// takes it unchanged; until then its order is the issuer's own, and every
// entry says so (order: issuer), not yet the queue-sealed book's.
//
// Each entry is canonical JSON (RFC 8785), kept in the Store by its C4 ID,
// and names the entry before it by its ID, so changing any past entry
// changes every ID after it. The Head is the one mutable pointer: the last
// entry's ID and position. The records an entry books (a device record, an
// act, a provider's key) are in the same Store by their own C4 IDs.

// OrderIssuer is every entry's order until the farm book adopts the
// index: the people's issuer's own, not yet the queue-sealed book's.
const OrderIssuer = "issuer"

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
	// KindFlag: something the issuer saw that the person is told of: a
	// device renewing from two places at once.
	KindFlag = "flag"
)

// FlagTwoPlaces is a device seen renewing from two network addresses
// within TwoPlaces of each other.
const FlagTwoPlaces = "two-places"

// Grades of a sign-in provider setting: install, the install's preset
// (the cluster administrator's word, as installing the farm is); signed,
// an admin's signed act with no touch, until the touch is built (K9);
// confirmed, signed and touched.
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
}

func (e *Entry) at() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, e.At)
	return t
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

// HeadState is the index's head: the last entry's C4 ID and position, and
// the version a Write must be given to move it.
type HeadState struct {
	ID       string
	Position int64
	Version  string
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

	mu      sync.Mutex
	entries []*Entry
	ids     []string
	// devices are the device records the index books, by record ID.
	devices map[string]*schema.Device
	// raw are the device records' bundles, by record ID.
	raw map[string][]byte
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
	h, err := x.Head.Read(ctx)
	if err != nil {
		return err
	}
	have := int64(len(x.entries))
	if h.Position == have && (have == 0 || h.ID == x.ids[have-1]) {
		return nil
	}
	if h.Position < have {
		return fmt.Errorf("%w: the head is at position %d, before %d this process read", ErrIndex, h.Position, have)
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
		if err := x.load(ctx, news[k]); err != nil {
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

// load checks the record an entry books, and keeps a device record.
func (x *Index) load(ctx context.Context, e *Entry) error {
	if e.Record == "" {
		if e.Kind == KindFlag || e.Kind == KindSetting && e.Grade == GradeInstall {
			return nil
		}
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
		if x.devices == nil {
			x.devices, x.raw = map[string]*schema.Device{}, map[string][]byte{}
		}
		x.devices[e.Record], x.raw[e.Record] = d, b
	case KindProviderKey:
		if _, err := parseJWK(b); err != nil {
			return err
		}
	default:
		// An act the issuer took: still the act it was, signed as it was.
		v, err := provenance.VerifyBundle(b, x.Roots)
		if err != nil {
			return err
		}
		if v.PredicateType != provenance.PredicateAct {
			return fmt.Errorf("a %s entry books a %s record, not an act", e.Kind, v.PredicateType)
		}
	}
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
// head, so the head never names what the store lacks.
func (x *Index) append(ctx context.Context, now time.Time, build func() ([]pending, error)) ([]*Entry, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
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
		if from.Position != int64(len(x.entries)) {
			continue
		}
		prev, pos := from.ID, from.Position
		var out []*Entry
		var ids []string
		for _, p := range ps {
			e := p.entry
			if p.record != nil {
				e.Record = c4id(p.record)
				if err := x.Store.Put(ctx, e.Record, p.record); err != nil {
					return nil, err
				}
			}
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
		err = x.Head.Write(ctx, from, HeadState{ID: prev, Position: pos})
		if errors.Is(err, ErrMoved) && try < maxAppendTries {
			continue
		}
		if err != nil {
			return nil, err
		}
		for k, e := range out {
			if err := x.load(ctx, e); err != nil {
				return nil, err
			}
			x.entries = append(x.entries, e)
			x.ids = append(x.ids, ids[k])
		}
		return out, nil
	}
}

// Position is the index's position now: its last entry's, 0 when empty.
func (x *Index) Position(ctx context.Context) (int64, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return 0, err
	}
	return int64(len(x.entries)), nil
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
}

func (d *device) person() string { return d.rec.Predicate.Person }

// Lapse is how long a device may go without renewing before it lapses
// (PEOPLE-KEYS-2 7): it makes a new key on its next sign-in.
const Lapse = 90 * 24 * time.Hour

// at is every device the index books up to position pos, as of then:
// removed from the removal's position on, a person-wide removal reaching
// only the devices enrolled before it.
func (x *Index) at(pos int64) map[string]*device {
	devs := map[string]*device{}
	for _, e := range x.entries[:min(pos, int64(len(x.entries)))] {
		switch e.Kind {
		case KindDevice:
			d := devs[e.Device]
			if d == nil {
				d = &device{id: e.Device, first: e.Position}
				devs[e.Device] = d
			}
			if d.removed {
				continue
			}
			d.record, d.rec, d.last, d.lastAt, d.addr = e.Record, x.devices[e.Record], e.Position, e.at(), e.Addr
		case KindRemoval:
			for _, d := range devs {
				if d.id == e.Device || e.Device == "" && d.person() == e.Person {
					d.removed = true
				}
			}
		case KindCountersign:
			if d := devs[e.Device]; d != nil && d.record == e.Of {
				d.countersigns++
			}
		}
	}
	return devs
}

// inForce is the devices in force at position pos and time at: not
// removed, renewed within Lapse of at, and, for a touch key, its device
// key in force too.
func (x *Index) inForce(pos int64, at time.Time) map[string]*device {
	devs := x.at(pos)
	out := map[string]*device{}
	for id, d := range devs {
		if !d.removed && at.Sub(d.lastAt) <= Lapse {
			out[id] = d
		}
	}
	for id, d := range out {
		if t := d.rec.Predicate.TouchFor; t != "" && out[t] == nil {
			delete(out, id)
		}
	}
	return out
}

// DevicesAt is the device records in force at position pos and time at,
// the latest of each device: what CheckTouchKey reads, and what a verifier
// checks a person's act against at the act's position.
func (x *Index) DevicesAt(ctx context.Context, pos int64, at time.Time) ([]*schema.Device, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return nil, err
	}
	var out []*schema.Device
	for _, d := range x.inForce(pos, at) {
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
// An act that isn't a person's passes.
func (x *Index) CheckAct(ctx context.Context, v *provenance.Verified, pos int64) error {
	devs, err := x.DevicesAt(ctx, pos, v.SignedAt)
	if err != nil {
		return err
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
		return fmt.Errorf("device %.16s is a touch key: it signs beside its device key, never first", v.Device)
	}
	return provenance.CheckTouchKey(v, devs)
}

// Entries returns the entries after position from, with their IDs.
func (x *Index) Entries(ctx context.Context, from int64) ([]*Entry, []string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx); err != nil {
		return nil, nil, err
	}
	from = max(0, min(from, int64(len(x.entries))))
	return append([]*Entry(nil), x.entries[from:]...), append([]string(nil), x.ids[from:]...), nil
}

func c4id(b []byte) string { return c4.Identify(bytes.NewReader(b)).String() }
