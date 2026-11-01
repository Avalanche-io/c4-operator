package people_test

import (
	"context"
	"encoding/json"
	"errors"
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
	if err := r.head.Write(ctx, h, people.HeadState{ID: id, Position: pos}); err != nil {
		r.t.Fatal(err)
	}
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
	// Fork the index: a new head after the second entry, past the third.
	r.forge(&people.Entry{Kind: people.KindFlag, Flag: people.FlagTwoPlaces, Device: es[2].Device}, nil, ids[1], 4)
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
	if err := r.head.Write(ctx, h, people.HeadState{ID: ids[0], Position: h.Position}); err != nil {
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
