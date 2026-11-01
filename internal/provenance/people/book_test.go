package people_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// booked is a rig whose index is kept in a farm book: the book's store and
// State, and the queue's sealer with the people's checker beside the HUD's.
type booked struct {
	*rig
	store  *farmbook.MemoryStore
	ptr    *farmbook.MemoryPointer
	own    *farmbook.MemorySealer
	sealer *farmbook.Sealer
}

// inBook keeps r's index in a new farm book. The index kept before it, if
// any, is r's own Head, which the book adopts when legacy is set.
func inBook(r *rig, legacy *people.Index) *booked {
	r.t.Helper()
	b := &booked{rig: r, store: &farmbook.MemoryStore{}, ptr: &farmbook.MemoryPointer{}, own: &farmbook.MemorySealer{}}
	queue, err := provenance.NewSigner(context.Background(), func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		c, err := r.farm.CA.Issue(pub, provenance.ComponentIdentity("queue"), 48*time.Hour, r.now)
		return []*x509.Certificate{c, r.farm.CA.Cert}, err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.index = &people.Index{Farm: r.farm.ID, Roots: r.farm.Roots, Store: b.store, Head: r.head,
		Book: &farmbook.Reader{Farm: r.farm.ID, Roots: r.farm.Roots, Store: b.store, Pointer: b.ptr, Own: b.own}}
	r.iss.Index = r.index
	b.sealer = &farmbook.Sealer{Farm: r.farm.ID, Roots: r.farm.Roots, Store: b.store, Pointer: b.ptr, Own: b.own, Signer: queue, Log: logr.Discard(),
		Now: func() time.Time { return r.now },
		Checkers: func() []farmbook.Checker {
			return []farmbook.Checker{people.NewBookChecker(r.farm.ID, r.farm.Roots, b.store),
				&farmbook.ControlChecker{Farm: r.farm.ID, Roots: r.farm.Roots, Store: b.store}}
		}}
	if legacy != nil {
		b.sealer.Adopt = legacy.Adoption
	}
	return b
}

func (b *booked) seal() *farmbook.SealResult {
	b.t.Helper()
	res, err := b.sealer.Seal(context.Background())
	if err != nil {
		b.t.Fatal(err)
	}
	return res
}

func (b *booked) sealedAt() int64 {
	b.t.Helper()
	p, err := b.index.Sealed(context.Background())
	if err != nil {
		b.t.Fatal(err)
	}
	return p
}

// With a farm book, the issuer writes its entries pending and certifies
// at once; a verifier judges by sealed position only, so the new device
// counts for it once the queue seals, at the book's position, in the
// book's order.
func TestIndexKeptInTheBook(t *testing.T) {
	b := inBook(studio(t), nil)
	d := newDev(t)
	c := b.signIn(d, "dana", danaSub)
	if c.Order != people.OrderPending || c.Position == 0 || !c.Provisional {
		t.Fatalf("certified at %d in order %q", c.Position, c.Order)
	}
	if _, err := b.index.DevicesAt(context.Background(), c.Position, b.now); !errors.Is(err, farmbook.ErrNotSealed) {
		t.Errorf("a verifier at a pending position: %v", err)
	}
	b.seal()
	devs, err := b.index.DevicesAt(context.Background(), b.sealedAt(), b.now)
	if err != nil || countPerson(devs, "dana") != 1 {
		t.Fatalf("dana's devices once sealed: %d, %v", countPerson(devs, "dana"), err)
	}
	es, _, _ := b.index.Entries(context.Background(), 0)
	for _, e := range es {
		if e.Order != people.OrderBook {
			t.Errorf("entry %d is in order %q", e.Position, e.Order)
		}
	}
	var cfg people.Config
	b.caller = "dana"
	if code, body := b.call("GET", "/v1/people/config", "", nil, &cfg); code != http.StatusOK || cfg.Order != people.OrderBook {
		t.Errorf("config: %d %s", code, body)
	}
	var page people.IndexPage
	if code, body := b.call("GET", "/v1/people/index", "", nil, &page); code != http.StatusOK || page.Order != people.OrderBook || len(page.Signed) == 0 {
		t.Errorf("the index page names the book's signed head: %d %s", code, body)
	}
	// The issuer renews on the book as on its own index.
	b.now = b.now.Add(6 * time.Hour)
	if code, body, _ := b.renew(d, "dana", danaSub); code != http.StatusOK {
		t.Errorf("renew: %d %s", code, body)
	}
}

// A removal pending the seal counts for the issuer, which refuses the
// removed device's next act at once; a verifier still judges at the
// sealed position, where the device is in force, until the queue seals
// the removal. In force at a position is right on both sides of it.
func TestPendingRemovalCountsForTheIssuerNotAVerifier(t *testing.T) {
	b := inBook(studio(t), nil)
	d, other := newDev(t), newDev(t)
	k := b.key(d, b.signIn(d, "noor", noorSub))
	ko := b.key(other, b.signIn(other, "noor", noorSub))
	b.seal()
	before := b.sealedAt()
	approve := b.act(k, "noor", "approve", provenance.Subject{}, nil, false)
	if err := b.checkAt(approve, before); err != nil {
		t.Fatalf("before the removal: %v", err)
	}
	code, body, rm := b.book(b.act(ko, "noor", "remove-device", deviceSubject(d.id), nil, false), "")
	if code != http.StatusOK || rm.Entry.Order != people.OrderPending {
		t.Fatalf("remove: %d %s", code, body)
	}
	// The issuer: d is out of force now.
	if code, body, _ := b.book(b.act(k, "noor", "remove-device", deviceSubject(other.id), nil, false), ""); code != http.StatusForbidden || !strings.Contains(body, "no device record in force") {
		t.Errorf("the removed device acted while its removal was pending: %d %s", code, body)
	}
	// A verifier: still in force at the sealed position.
	if err := b.checkAt(approve, b.sealedAt()); err != nil {
		t.Errorf("a verifier before the removal is sealed: %v", err)
	}
	b.seal()
	if err := b.checkAt(approve, b.sealedAt()); !errors.Is(err, people.ErrNotInForce) {
		t.Errorf("a verifier once the removal is sealed: %v", err)
	}
	if err := b.checkAt(approve, before); err != nil {
		t.Errorf("a verifier at a position before the removal: %v", err)
	}
}

// An index the issuer kept before the book is adopted at the book's first
// seal: its entries unchanged, at the same positions, under their own prev
// links and the issuer's order; the issuer waits until then; and what was
// in force stays in force.
func TestIndexAdoptedWithItsOrder(t *testing.T) {
	r := studio(t)
	d, other := newDev(t), newDev(t)
	r.signIn(d, "dana", danaSub)
	ko := r.key(other, r.signIn(other, "noor", noorSub))
	if code, body, _ := r.book(r.act(ko, "noor", "remove-device", deviceSubject(other.id), nil, false), ""); code != http.StatusOK {
		t.Fatalf("remove: %d %s", code, body)
	}
	legacy := r.index
	old, oldIDs, err := legacy.Entries(context.Background(), 0)
	if err != nil || len(old) < 5 {
		t.Fatalf("the index before the book: %d entries, %v", len(old), err)
	}
	was, _ := legacy.DevicesAt(context.Background(), int64(len(old)), r.now)
	b := inBook(r, legacy)
	if code, body := b.call("GET", "/v1/people/devices", "", nil, nil); code != http.StatusServiceUnavailable || !strings.Contains(body, "adopted") {
		t.Errorf("the issuer before the adoption: %d %s", code, body)
	}
	res := b.seal()
	if res.Segment.Adopted == nil || res.Segment.Adopted.Head != oldIDs[len(oldIDs)-1] || res.Segment.Last != int64(len(old)) {
		t.Fatalf("the first segment: %+v", res.Segment.BookSegmentPredicate)
	}
	es, ids, err := b.index.Entries(context.Background(), 0)
	if err != nil || len(es) != len(old) {
		t.Fatalf("adopted %d entries, %v", len(es), err)
	}
	for k := range es {
		if ids[k] != oldIDs[k] || es[k].Position != int64(k+1) || es[k].Order != people.OrderIssuer {
			t.Errorf("entry %d: %.16s at %d in order %q, was %.16s", k, ids[k], es[k].Position, es[k].Order, oldIDs[k])
		}
	}
	now, _ := b.index.DevicesAt(context.Background(), b.sealedAt(), b.now)
	if countPerson(now, "dana") != countPerson(was, "dana") || countPerson(now, "noor") != 0 {
		t.Errorf("in force after the adoption: dana %d, noor %d", countPerson(now, "dana"), countPerson(now, "noor"))
	}
	// The next entry follows the adopted ones, in the book's order.
	b.signIn(newDev(t), "noor", noorSub)
	b.seal()
	after, _, _ := b.index.Entries(context.Background(), int64(len(old)))
	if len(after) == 0 || after[0].Position != int64(len(old))+1 || after[0].Prev != oldIDs[len(oldIDs)-1] || after[0].Order != people.OrderBook {
		t.Fatalf("the entry after the adopted ones: %+v", after)
	}
}

// A confirmed act is confirmed only by the signer's own touch key: one
// whose record names the device that signed (touchFor), the same person's.
// Another enrolled device of the person's, beside the signer, is refused at
// the issuer and, written into the book around it, at the seal; the same
// act confirmed by the real touch key passes both.
func TestTouchKeyMustNameTheSigner(t *testing.T) {
	b := inBook(studio(t), nil)
	laptop, desk, laptopTouch := newDev(t), newDev(t), newDev(t)
	k := b.key(laptop, b.signIn(laptop, "dana", danaSub))
	deskKey := b.key(desk, b.signIn(desk, "dana", danaSub))
	e := b.enrolment(laptopTouch, "dana", laptop.id, "mac")
	e.IDToken = b.sso.Token(b.now, danaSub, laptopTouch.id, map[string]any{"auth_time": b.now.Unix(), "amr": []string{"hwk", "pin"}})
	var tc people.Certified
	if code, body := b.call("POST", "/v1/people/enroll", "", e, &tc); code != http.StatusOK {
		t.Fatalf("touch: %d %s", code, body)
	}
	b.seal()
	target := newDev(t)
	k.Touch = deskKey
	wrong := b.act(k, "dana", "approve-machine", deviceSubject(target.id), map[string]string{}, true)
	if code, body, _ := b.book(wrong, ""); code != http.StatusForbidden || !strings.Contains(body, "not a touch key for device") {
		t.Errorf("the issuer took an act confirmed by another of dana's devices: %d %s", code, body)
	}
	if st, _ := b.ptr.Read(context.Background()); len(st.Pending) != 0 {
		t.Errorf("an act the issuer refused was left pending: %v", st.Pending)
	}
	// Written into the book around the issuer, the queue refuses it.
	// Written into the book around the issuer, but signed with its key, as
	// a writer holding it could: the queue still checks what it says.
	approval := func(act []byte, device string) farmbook.Submission {
		return farmbook.Submission{Record: act, Entry: b.asIssuer(&people.Entry{Kind: people.KindApproval, Device: device, Person: "dana", By: "dana",
			Order: people.OrderPending, Farm: b.farm.ID, At: b.now.Format(time.RFC3339Nano), Expires: b.now.Add(people.ApprovalTTL).Format(time.RFC3339Nano),
			Record: provtest.ID(string(act))})}
	}
	if _, err := (&farmbook.Writer{Store: b.store, Pointer: b.ptr}).Append(context.Background(), approval(wrong, target.id)); err != nil {
		t.Fatal(err)
	}
	res := b.seal()
	if len(res.Refused) != 1 || res.Refused[0].Reason != farmbook.ReasonNotInForce || res.Segment.Last != res.Segment.First-1 {
		t.Fatalf("the seal: refused %+v, sealed %d to %d", res.Refused, res.Segment.First, res.Segment.Last)
	}
	// The real touch key passes both.
	k.Touch = b.key(laptopTouch, &tc)
	b.now = b.now.Add(time.Second)
	if code, body, _ := b.book(b.act(k, "dana", "approve-machine", deviceSubject(target.id), map[string]string{}, true), ""); code != http.StatusOK {
		t.Fatalf("the issuer, confirmed by dana's touch key: %d %s", code, body)
	}
	other := newDev(t)
	b.now = b.now.Add(time.Second)
	right := b.act(k, "dana", "approve-machine", deviceSubject(other.id), map[string]string{}, true)
	sub := approval(right, other.id)
	if _, err := (&farmbook.Writer{Store: b.store, Pointer: b.ptr}).Append(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if res := b.seal(); len(res.Refused) != 0 || res.Segment.Last != res.Segment.First+1 {
		t.Fatalf("the seal, confirmed by dana's touch key: refused %+v, sealed %d to %d", res.Refused, res.Segment.First, res.Segment.Last)
	}
}

// asIssuer signs an entry as the people's issuer signs one it writes for
// the farm book (book-entry/v1), over the entry as written, pending.
func (b *booked) asIssuer(e *people.Entry) *people.Entry {
	b.t.Helper()
	s, err := provenance.NewSigner(context.Background(), provenance.LocalIssuer(b.farm.CA, provenance.ComponentIdentity("issuer"), time.Hour))
	if err != nil {
		b.t.Fatal(err)
	}
	c := *e
	c.Sig, c.Position, c.Prev, c.Order, c.Farm = nil, 0, "", people.OrderPending, b.farm.ID
	raw, _ := json.Marshal(&c)
	canon, _ := schema.Canonical(raw)
	sig, err := provenance.SignStatement(context.Background(), s, provenance.PredicateBookEntry,
		[]provenance.Subject{{Name: "book-entry:" + e.Kind, Digest: map[string]string{"c4": provtest.ID(string(canon))}}}, map[string]string{"farm": b.farm.ID, "kind": e.Kind}, b.now)
	if err != nil {
		b.t.Fatal(err)
	}
	e.Sig, _ = schema.Canonical(sig)
	return e
}
