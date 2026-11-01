package people_test

// The S2a review's probes of the people's index in the farm book, each kept
// as a test of what is refused. The attacker writes the book's State and
// its store, never the issuer's or the queue's key.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// listRaw lists raw bytes as a pending entry, as a writer of the store and
// the State can.
func (b *booked) listRaw(raw []byte) string {
	id := provtest.ID(string(raw))
	b.store.Put(context.Background(), id, raw)
	b.relist(id)
	return id
}

// relist lists an ID as pending again.
func (b *booked) relist(id string) {
	st, _ := b.ptr.Read(context.Background())
	if err := b.ptr.Write(context.Background(), st, farmbook.State{Head: st.Head, Pending: append(st.Pending, id)}); err != nil {
		b.t.Fatal(err)
	}
}

// fbook is the sealed book, read as the manager reads it.
func (b *booked) fbook() *farmbook.Book {
	b.t.Helper()
	rd := &farmbook.Reader{Farm: b.farm.ID, Roots: b.farm.Roots, Store: b.store, Pointer: b.ptr, Own: b.own}
	bk, _, err := rd.Sync(context.Background())
	if err != nil {
		b.t.Fatal(err)
	}
	return bk
}

// pendingOf is the pending entry of a kind, as listed.
func (b *booked) pendingOf(kind string) string {
	st, _ := b.ptr.Read(context.Background())
	for _, id := range st.Pending {
		raw, _ := b.store.Get(context.Background(), id)
		if strings.Contains(string(raw), `"kind":"`+kind+`"`) {
			return id
		}
	}
	return ""
}

// A setting entry no issuer signed is refused as unsigned: a writer of
// the store and the State can't set the sign-in provider (review item 4,
// probe a).
func TestUnsignedSettingEntryRefused(t *testing.T) {
	b := inBook(studio(t), nil)
	b.signIn(newDev(t), "dana", danaSub)
	b.seal()
	at := b.now.Format(time.RFC3339Nano)
	b.listRaw([]byte(`{"at":"` + at + `","clientID":"evil","farm":"` + b.farm.ID + `","grade":"install","issuer":"https://sso.evil.example","kind":"setting","order":"pending","position":0}`))
	res := b.seal()
	if len(res.Refused) != 1 || res.Refused[0].Reason != farmbook.ReasonUnsigned || len(res.Segment.Entries) != 0 {
		t.Fatalf("a forged setting: refused %+v, sealed %d", res.Refused, len(res.Segment.Entries))
	}
}

// A device entry listed again is refused as booked: it doesn't renew the
// device, whose lapse runs from its own seal (review item 4, probe b).
func TestRelistedDeviceEntryRefusedAsBooked(t *testing.T) {
	b := inBook(studio(t), nil)
	d := newDev(t)
	b.signIn(d, "dana", danaSub)
	dev := b.pendingOf("device")
	b.seal()
	before, _ := b.fbook().InForce(farmbook.ForceDevice, d.id, b.sealedAt())
	b.now = b.now.Add(24 * time.Hour)
	b.relist(dev)
	res := b.seal()
	if len(res.Refused) != 1 || res.Refused[0].Reason != farmbook.ReasonBooked || len(res.Segment.Entries) != 0 {
		t.Fatalf("a re-listed device entry: refused %+v, sealed %d", res.Refused, len(res.Segment.Entries))
	}
	bk := b.fbook()
	after, _ := bk.InForce(farmbook.ForceDevice, d.id, bk.Head().Position)
	if before == nil || after == nil || after.Segment != before.Segment {
		t.Errorf("the device's entry moved from segment %v to %v", before, after)
	}
}

// An entry with its kind written twice in two cases is refused as
// malformed: it never becomes a device with no record for a person of the
// writer's choosing, and the issuer reads the book on (review item 1,
// probe c).
func TestCaseFoldedKindRefused(t *testing.T) {
	b := inBook(studio(t), nil)
	b.signIn(newDev(t), "dana", danaSub)
	b.seal()
	at := b.now.Format(time.RFC3339Nano)
	fake := provtest.ID("a device of mallory's")
	b.listRaw([]byte(`{"at":"` + at + `","clientID":"x","device":"` + fake + `","farm":"` + b.farm.ID + `","grade":"install","issuer":"https://sso.example","kind":"device","KIND":"setting","order":"pending","person":"mallory","position":0}`))
	res := b.seal()
	if len(res.Refused) != 1 || res.Refused[0].Reason != farmbook.ReasonMalformed {
		t.Fatalf("a kind written twice: refused %+v", res.Refused)
	}
	bk := b.fbook()
	if on, _ := bk.InForce(farmbook.ForceDevice, fake, bk.Head().Position); on != nil {
		t.Error("a forged device is in force")
	}
	if _, err := b.index.Sealed(context.Background()); err != nil {
		t.Errorf("the issuer after the kind-flip: %v", err)
	}
}

// A device entry's address is in its private part, salted, so neither the
// entry nor its ID gives it away: a /16 searched against the ID finds
// nothing (review item 6, probe).
func TestRedactedAddressNotRecoverableFromTheID(t *testing.T) {
	b := inBook(studio(t), nil)
	b.addr = "198.51.100.77"
	b.signIn(newDev(t), "dana", danaSub)
	b.seal()
	found := false
	for _, s := range b.fbook().Entries(0) {
		if s.Entry.Kind != "device" {
			continue
		}
		found = true
		if strings.Contains(string(s.Raw), "198.51") {
			t.Fatalf("the entry holds the address: %s", s.Raw)
		}
		var m map[string]any
		json.Unmarshal(s.Raw, &m)
		for x := 0; x < 65536; x++ {
			m["addr"] = fmt.Sprintf("198.51.%d.%d", x>>8, x&255)
			j, _ := json.Marshal(m)
			c, _ := schema.Canonical(j)
			if provtest.ID(string(c)) == s.ID {
				t.Fatalf("recovered %s from the entry's ID", m["addr"])
			}
		}
	}
	if !found {
		t.Fatal("no device entry")
	}
}

// An entry the book adopted keeps its own time for lapse: a device that
// lapsed before the book doesn't come back for another 90 days, and the
// farm book and the people's index say the same of it (review item 8).
func TestAdoptedEntryKeepsItsOwnTime(t *testing.T) {
	r := studio(t)
	old, fresh := newDev(t), newDev(t)
	r.signIn(old, "dana", danaSub)
	r.now = r.now.Add(people.Lapse + time.Hour)
	r.signIn(fresh, "dana", danaSub)
	legacy := r.index
	b := inBook(r, legacy)
	b.seal()
	bk := b.fbook()
	pos := bk.Head().Position
	if on, _ := bk.InForce(farmbook.ForceDevice, old.id, pos); on != nil {
		t.Error("the farm book: a device that lapsed before the book is in force")
	}
	if on, _ := bk.InForce(farmbook.ForceDevice, fresh.id, pos); on == nil {
		t.Error("the farm book: the fresh device isn't in force")
	}
	devs, err := b.index.DevicesAt(context.Background(), pos, b.now)
	if err != nil || len(devs) != 1 {
		t.Errorf("the people's index: %d devices in force, %v", len(devs), err)
	}
}

// A sealed entry that no longer checks (here, its record is gone from the
// store) is kept in the book and counts for nothing, by one policy at the
// issuer and at the queue's sealer: neither stops (review item 13).
func TestSealedEntryThatNoLongerChecksCountsForNothing(t *testing.T) {
	b := inBook(studio(t), nil)
	d := newDev(t)
	c := b.signIn(d, "dana", danaSub)
	b.seal()
	b.store.Put(context.Background(), c.Record, []byte("gone"))
	fresh := &people.Index{Farm: b.farm.ID, Roots: b.farm.Roots, Store: b.store, Head: b.head,
		Book: &farmbook.Reader{Farm: b.farm.ID, Roots: b.farm.Roots, Store: b.store, Pointer: b.ptr, Own: b.own}}
	pos, err := fresh.Sealed(context.Background())
	if err != nil {
		t.Fatalf("the issuer stopped: %v", err)
	}
	if devs, _ := fresh.DevicesAt(context.Background(), pos, b.now); len(devs) != 0 {
		t.Errorf("a device whose record no longer checks is in force: %d", len(devs))
	}
	o := b.sealer
	sealer := &farmbook.Sealer{Farm: o.Farm, Roots: o.Roots, Store: o.Store, Pointer: o.Pointer, Own: o.Own, Signer: o.Signer,
		Checkers: o.Checkers, Log: o.Log, Now: o.Now}
	b.relist(provtest.ID("another listing"))
	if _, err := sealer.Seal(context.Background()); err != nil {
		t.Errorf("a new sealer stopped: %v", err)
	}
}
