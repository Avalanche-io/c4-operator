package people_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/schema"
)

// reread is a fresh index over the rig's store and head, as another
// replica or a verifier reads it.
func (r *rig) reread() error {
	x := &people.Index{Farm: r.farm.ID, Roots: r.farm.Roots, Store: r.store, Head: r.head}
	return x.Sync(context.Background())
}

// forge books an entry by hand, past the issuer, as anyone with the
// store and the head could: canonical, at the next position, linked to
// the head.
func (r *rig) forge(e *people.Entry, record []byte, prev string, pos int64) {
	r.t.Helper()
	ctx := context.Background()
	if record != nil {
		e.Record = c4.Identify(strings.NewReader(string(record))).String()
		r.store.Put(ctx, e.Record, record)
	}
	h, _ := r.head.Read(ctx)
	e.Farm, e.Order, e.At, e.Prev, e.Position = r.farm.ID, people.OrderIssuer, r.now.Format(time.RFC3339), prev, pos
	raw, _ := json.Marshal(e)
	b, err := schema.Canonical(raw)
	if err != nil {
		r.t.Fatal(err)
	}
	id := c4.Identify(strings.NewReader(string(b))).String()
	r.store.Put(ctx, id, b)
	if err := r.head.Write(ctx, h, people.HeadState{ID: id, Position: pos, Signed: r.signedHead("issuer", id, pos)}); err != nil {
		r.t.Fatal(err)
	}
}

// signedHead is a head signed as component/<as>, as a forger holding that
// key would sign it.
func (r *rig) signedHead(as, id string, pos int64) []byte {
	r.t.Helper()
	ctx := context.Background()
	s, err := provenance.NewSigner(ctx, provenance.LocalIssuer(r.farm.CA, provenance.ComponentIdentity(as), time.Hour))
	if err != nil {
		r.t.Fatal(err)
	}
	b, err := provenance.SignStatement(ctx, s, people.PredicateIndexHead, []provenance.Subject{{Name: "people-index:" + r.farm.ID, Digest: map[string]string{"c4": id}}},
		map[string]any{"farm": r.farm.ID, "head": id, "position": pos}, r.now)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// The index's hash links are checked: an entry whose bytes no longer hash
// to its ID, and a head whose entry names another entry before it than
// the one a reader has, are refused, so no past entry changes unseen.
func TestIndexLinksChecked(t *testing.T) {
	r := studio(t)
	r.signIn(newDev(t), "dana", danaSub)
	if err := r.reread(); err != nil {
		t.Fatalf("an untouched index: %v", err)
	}
	es, ids, _ := r.index.Entries(context.Background(), 0)
	// Rewrite a past entry in place.
	orig, _ := r.store.Get(context.Background(), ids[1])
	changed := *es[1]
	changed.KeyID = "another-key"
	raw, _ := json.Marshal(&changed)
	b, _ := schema.Canonical(raw)
	r.store.Put(context.Background(), ids[1], b)
	if err := r.reread(); !errors.Is(err, people.ErrIndex) || !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("a rewritten entry: %v", err)
	}
	r.store.Put(context.Background(), ids[1], orig)
	// Fork the index: a new head after the second entry, past the last.
	last := len(es) - 1
	r.forge(&people.Entry{Kind: people.KindFlag, Flag: people.FlagTwoPlaces, Device: es[last].Device}, nil, ids[1], int64(len(es))+1)
	if err := r.index.Sync(context.Background()); !errors.Is(err, people.ErrIndex) || !strings.Contains(err.Error(), "rewritten") {
		t.Errorf("a forked head: %v", err)
	}
}

// A device record not signed by the people's issuer (component/issuer)
// is refused when the index is read, though it chains to the farm and its
// shape and proof are right; the same record signed by the issuer passes.
func TestDeviceRecordNotByIssuerRefused(t *testing.T) {
	for _, signer := range []string{"hud", "issuer"} {
		r := onePerson(t)
		d := newDev(t)
		e := r.enrolment(d, "noor", "", "mac")
		p := schema.DevicePredicate{Farm: r.farm.ID, Person: "noor", Key: e.Request.Key, Protection: e.Protection, Kind: e.Kind,
			Evidence: schema.EvidenceInstaller, Nonce: e.Request.Nonce, RequestedAt: e.Request.RequestedAt, Proof: e.Proof}
		s, err := provenance.NewSigner(context.Background(), provenance.LocalIssuer(r.farm.CA, provenance.ComponentIdentity(signer), time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		rec, err := provenance.SignStatement(context.Background(), s, provenance.PredicateDevice,
			[]provenance.Subject{{Name: "device:" + d.id, Digest: map[string]string{"c4": d.id}}}, p, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		r.forge(&people.Entry{Kind: people.KindDevice, Device: d.id, Person: "noor"}, rec, "", 1)
		err = r.reread()
		switch signer {
		case "hud":
			if !errors.Is(err, people.ErrIndex) || !strings.Contains(err.Error(), "component/issuer") {
				t.Errorf("a device record signed by the HUD: %v", err)
			}
		case "issuer":
			if err != nil {
				t.Errorf("a device record signed by the issuer: %v", err)
			}
		}
	}
}

// A head at the position this process has read, naming another entry (a
// fork, or a head rewritten in place), is reported as a rewritten index,
// never a panic (review probe P3).
func TestForkedHeadAtTheSamePosition(t *testing.T) {
	r := studio(t)
	r.signIn(newDev(t), "dana", danaSub)
	ctx := context.Background()
	if err := r.index.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	h, _ := r.head.Read(ctx)
	_, ids, _ := r.index.Entries(ctx, 0)
	if err := r.head.Write(ctx, h, people.HeadState{ID: ids[0], Position: h.Position, Signed: r.signedHead("issuer", ids[0], h.Position)}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Sync panicked: %v", p)
		}
	}()
	if err := r.index.Sync(ctx); !errors.Is(err, people.ErrIndex) || !strings.Contains(err.Error(), "rewritten") {
		t.Errorf("a forked head: %v", err)
	}
}

// Each entry is checked against the record it books when the index is
// read, and an entry that says what its record doesn't is refused (review
// probe P4). Each forgery below cites an act signed by dana and put in the
// store, but never booked: an approval citing a removal; the approval
// naming another person, another key, or a longer life; a removal naming
// another device than its act. The faithful entry passes; and an act
// booked twice is refused.
func TestEntriesAgreeWithTheirRecords(t *testing.T) {
	type acts struct {
		r               *rig
		approve, remove string
		newKey, junk    string
		head            string
		pos             int64
	}
	prepare := func(t *testing.T) acts {
		r := studio(t)
		laptop, touch, junk := newDev(t), newDev(t), newDev(t)
		k := r.key(laptop, r.signIn(laptop, "dana", danaSub))
		r.signIn(junk, "dana", danaSub)
		e := r.enrolment(touch, "dana", laptop.id, "mac")
		e.IDToken = r.sso.Token(r.now, danaSub, touch.id, map[string]any{"auth_time": r.now.Unix(), "amr": []string{"pwd", "otp"}})
		var tc people.Certified
		if code, body := r.call("POST", "/v1/people/enroll", "", e, &tc); code != http.StatusOK {
			t.Fatalf("touch: %d %s", code, body)
		}
		k.Touch = r.key(touch, &tc)
		newKey := newDev(t).id
		put := func(b []byte) string {
			id := c4.Identify(bytes.NewReader(b)).String()
			r.store.Put(context.Background(), id, b)
			return id
		}
		a := acts{r: r, newKey: newKey, junk: junk.id,
			approve: put(r.act(k, "dana", "approve-machine", deviceSubject(newKey), map[string]string{}, true)),
			remove:  put(r.act(k, "dana", "remove-device", deviceSubject(junk.id), nil, false))}
		h, _ := r.head.Read(context.Background())
		a.head, a.pos = h.ID, h.Position
		return a
	}
	approval := func(a acts) *people.Entry {
		return &people.Entry{Kind: people.KindApproval, Record: a.approve, Device: a.newKey, Person: "dana", By: "dana",
			Expires: a.r.now.Add(people.ApprovalTTL).UTC().Format(time.RFC3339Nano)}
	}
	removal := func(a acts) *people.Entry {
		return &people.Entry{Kind: people.KindRemoval, Record: a.remove, Device: a.junk, Person: "dana", By: "dana"}
	}
	for name, c := range map[string]struct {
		forge func(a acts) *people.Entry
		ok    bool
	}{
		"the faithful approval": {approval, true},
		"the faithful removal":  {removal, true},
		"an approval citing a removal": {func(a acts) *people.Entry {
			e := approval(a)
			e.Record = a.remove
			return e
		}, false},
		"an approval for another person": {func(a acts) *people.Entry {
			e := approval(a)
			e.Person, e.By = "noor", "noor"
			return e
		}, false},
		"an approval for another key": {func(a acts) *people.Entry {
			e := approval(a)
			e.Device = newDev(t).id
			return e
		}, false},
		"an approval that lasts longer": {func(a acts) *people.Entry {
			e := approval(a)
			e.Expires = a.r.now.Add(48 * time.Hour).UTC().Format(time.RFC3339Nano)
			return e
		}, false},
		"a removal of another device": {func(a acts) *people.Entry {
			e := removal(a)
			e.Device = a.newKey
			return e
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			a := prepare(t)
			a.r.forge(c.forge(a), nil, a.head, a.pos+1)
			err := a.r.reread()
			if c.ok != (err == nil) || err != nil && !errors.Is(err, people.ErrIndex) {
				t.Errorf("read: %v", err)
			}
		})
	}
	t.Run("an act booked twice", func(t *testing.T) {
		a := prepare(t)
		a.r.forge(approval(a), nil, a.head, a.pos+1)
		h, _ := a.r.head.Read(context.Background())
		a.r.forge(approval(a), nil, h.ID, h.Position+1)
		if err := a.r.reread(); !errors.Is(err, people.ErrIndex) || !strings.Contains(err.Error(), "twice") {
			t.Errorf("read: %v", err)
		}
	})
}

// The head is signed by the people's issuer, and covers every entry
// through their links: a head written with only the head Secret's
// credentials, unsigned, signed by another component, or carrying the
// issuer's signature over another head, is refused.
func TestHeadSignedByTheIssuer(t *testing.T) {
	r := studio(t)
	r.signIn(newDev(t), "dana", danaSub)
	ctx := context.Background()
	h, _ := r.head.Read(ctx)
	_, ids, _ := r.index.Entries(ctx, 0)
	if err := r.reread(); err != nil {
		t.Fatalf("the issuer's own head: %v", err)
	}
	for name, forged := range map[string]people.HeadState{
		"unsigned":                     {ID: h.ID, Position: h.Position},
		"signed by the HUD":            {ID: h.ID, Position: h.Position, Signed: r.signedHead("hud", h.ID, h.Position)},
		"another head's signature":     {ID: ids[1], Position: 2, Signed: h.Signed},
		"a signature at another place": {ID: h.ID, Position: h.Position, Signed: r.signedHead("issuer", h.ID, h.Position+1)},
	} {
		cur, _ := r.head.Read(ctx)
		if err := r.head.Write(ctx, cur, forged); err != nil {
			t.Fatal(err)
		}
		if err := r.reread(); !errors.Is(err, people.ErrIndex) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
