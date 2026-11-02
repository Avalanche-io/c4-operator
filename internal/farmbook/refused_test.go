package farmbook_test

// The S2a review's probes, each kept as a test of what the book refuses,
// and the tests of the sealer's own head, time and limits. The attacker
// throughout can write the book's State and its store, never the queue's
// key or the sealer's own head.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// listRaw is a writer of the store and the State listing raw bytes as a
// pending entry.
func (r *rig) listRaw(raw []byte) string {
	r.t.Helper()
	id := provtest.ID(string(raw))
	if err := r.store.Put(r.ctx, id, raw); err != nil {
		r.t.Fatal(err)
	}
	r.list(id)
	return id
}

// list is a writer of the State listing an ID as pending.
func (r *rig) list(ids ...string) {
	r.t.Helper()
	st, _ := r.ptr.Read(r.ctx)
	if err := r.ptr.Write(r.ctx, st, farmbook.State{Head: st.Head, Pending: append(st.Pending, ids...)}); err != nil {
		r.t.Fatal(err)
	}
}

// ownReader is a reader in the manager, which reads the sealer's own head.
func (r *rig) ownReader() *farmbook.Reader {
	rd := r.reader()
	rd.Own = r.own
	return rd
}

// A key written twice, in two cases ("act" and "ACT"), is refused at the
// seal as malformed: the queue never checks one entry and seals another,
// and a revoke stays a revoke (review item 1, probe 1).
func TestCaseFoldedKeyRefusedAtTheSeal(t *testing.T) {
	r := newRig(t)
	subj := "submitgrant:saltmarsh/dana-flow"
	r.book(r.control(r.hud, "grant", subj, grantChange("dana", r.now.Add(30*24*time.Hour)), r.now), nil)
	r.seal()
	rev := r.control(r.hud, "revoke", subj, map[string]any{"person": "dana"}, r.now)
	r.store.Put(r.ctx, provtest.ID(string(rev)), rev)
	e, err := farmbook.ControlEntry(rev, r.now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e)
	canon, _ := schema.Canonical(b)
	for _, raw := range []string{
		strings.Replace(string(canon), `"act":"revoke"`, `"act":"grant","ACT":"revoke"`, 1),
		strings.Replace(string(canon), `"act":"revoke"`, `"Act":"revoke"`, 1),
		strings.Replace(string(canon), `"act":"revoke"`, `"act":"revoke","unknown":"x"`, 1),
		strings.Replace(string(canon), `"act":"revoke",`, `"act" : "revoke",`, 1),
	} {
		r.listRaw([]byte(raw))
	}
	res := r.seal()
	if len(res.Refused) != 4 || len(res.Segment.Entries) != 0 {
		t.Fatalf("refused %+v, sealed %d", res.Refused, len(res.Segment.Entries))
	}
	for _, f := range res.Refused {
		if f.Reason != farmbook.ReasonMalformed {
			t.Errorf("refused as %q, want malformed", f.Reason)
		}
	}
	b2 := r.read()
	if on, _ := b2.InForce(farmbook.ForceGrant, subj, b2.Head().Position); on == nil {
		t.Error("the grant should still be in force: no revoke was sealed")
	}
	// The real revoke, as the HUD writes it, is sealed as a revoke.
	r.book(rev, nil)
	r.seal()
	b3 := r.read()
	if on, _ := b3.InForce(farmbook.ForceGrant, subj, b3.Head().Position); on != nil {
		t.Error("the grant is in force after its revoke was sealed")
	}
}

// A State rolled back past a revoke, or emptied, is put back at the
// sealer's own head and sealed after it: the sealer never signs a fork,
// never starts a second book, and adopts once (review item 2, probe 2).
func TestSealerPutsARolledBackStateRightInsteadOfForking(t *testing.T) {
	r := newRig(t)
	adoptions := 0
	r.sealer.Adopt = func(context.Context) (*farmbook.Adoption, error) {
		adoptions++
		return nil, nil
	}
	subj := "submitgrant:saltmarsh/dana-flow"
	r.book(r.control(r.hud, "grant", subj, grantChange("dana", r.now.Add(30*24*time.Hour)), r.now), nil)
	r.seal()
	st1, _ := r.ptr.Read(r.ctx)
	r.now = r.now.Add(3 * time.Second)
	r.book(r.control(r.hud, "revoke", subj, map[string]any{"person": "dana"}, r.now), nil)
	seg2 := r.seal().Segment
	r.ptr.Set(farmbook.State{Head: st1.Head})
	r.now = r.now.Add(3 * time.Second)
	r.book(r.control(r.hud, "disable", "node:n1", map[string]any{"unschedulable": true}, r.now), nil)
	res, err := r.sealer.Seal(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Segment.Number != 3 || res.Segment.Prev != seg2.ID {
		t.Fatalf("sealed segment %d after %.16s, want 3 after %.16s", res.Segment.Number, res.Segment.Prev, seg2.ID)
	}
	b := r.ownReader()
	book, _, err := b.Sync(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if on, _ := book.InForce(farmbook.ForceGrant, subj, book.Head().Position); on != nil {
		t.Error("the grant is in force: the revoke was dropped")
	}
	// The State emptied: no new book at segment 1, and no second adoption.
	r.ptr.Set(farmbook.State{})
	r.now = r.now.Add(3 * time.Second)
	r.book(r.control(r.hud, "enable", "node:n1", map[string]any{"unschedulable": false}, r.now), nil)
	res, err = r.sealer.Seal(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Segment.Number != 4 || res.Segment.First != 4 || adoptions != 1 {
		t.Errorf("after emptying the State: segment %d from %d, %d adoptions", res.Segment.Number, res.Segment.First, adoptions)
	}
	// A new sealer (another leader, a restart) reads its own head, and
	// does the same.
	fresh := *r.sealer
	r.ptr.Set(farmbook.State{Head: st1.Head})
	if _, err := fresh.Seal(r.ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.ptr.Read(r.ctx); st.Head.Number != 4 {
		t.Errorf("a new sealer left the State at segment %d", st.Head.Number)
	}
	// The sealer's own head lowered under it: it refuses.
	own, _ := r.own.Read(r.ctx)
	r.own.Write(r.ctx, own, farmbook.SealerState{Head: st1.Head})
	if _, err := r.sealer.Seal(r.ctx); !errors.Is(err, farmbook.ErrRollback) {
		t.Errorf("the sealer's own head went back: %v", err)
	}
}

// A page with an entry and no new segment is refused, never a panic
// (review item 7, probe 3).
func TestCheckPageRefusesAPageWithNoNewSegment(t *testing.T) {
	r := newRig(t)
	r.book(r.control(r.hud, "disable", "node:n1", map[string]any{"unschedulable": true}, r.now), nil)
	r.seal()
	p := r.page(0)
	last, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, farmbook.LastHead{}, p)
	if err != nil {
		t.Fatal(err)
	}
	evil := *p
	evil.Since = last.Position
	evil.Entries = []farmbook.PageEntry{{ID: "", Position: last.Position + 1, Entry: json.RawMessage(`{}`)}}
	if _, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, last, &evil); err == nil {
		t.Error("a page with an entry no segment seals was taken")
	}
	if _, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, last, nil); err == nil {
		t.Error("no page was taken")
	}
}

// An entry listed again after it was sealed is refused as booked
// (review item 4, probe 4).
func TestRelistedEntryRefusedAsBooked(t *testing.T) {
	r := newRig(t)
	id := r.book(r.control(r.hud, "disable", "node:n1", map[string]any{"unschedulable": true}, r.now), nil)
	r.seal()
	r.list(id, id)
	res := r.seal()
	if len(res.Refused) != 1 || res.Refused[0].Reason != farmbook.ReasonBooked || len(res.Segment.Entries) != 0 {
		t.Errorf("re-listed: refused %+v, sealed %d", res.Refused, len(res.Segment.Entries))
	}
}

// A listing that isn't a C4 ID is dropped, logged and never copied into a
// segment: the book goes on, and a fresh reader reads it (review item 3,
// probe 6).
func TestBadListingDroppedWithoutBrickingTheBook(t *testing.T) {
	r := newRig(t)
	r.book(r.control(r.hud, "disable", "node:n1", map[string]any{"unschedulable": true}, r.now), nil)
	r.seal()
	r.list("x", strings.Repeat("y", 5000))
	if _, err := r.sealer.Seal(r.ctx); err != nil {
		t.Fatalf("a bad listing: %v", err)
	}
	if st, _ := r.ptr.Read(r.ctx); len(st.Pending) != 0 {
		t.Errorf("bad listings left pending: %d", len(st.Pending))
	}
	r.now = r.now.Add(3 * time.Second)
	r.book(r.control(r.hud, "enable", "node:n1", map[string]any{"unschedulable": false}, r.now), nil)
	if _, err := r.sealer.Seal(r.ctx); err != nil {
		t.Fatalf("the next seal: %v", err)
	}
	if b, _, err := r.reader().Sync(r.ctx); err != nil || b.Head().Position != 2 {
		t.Errorf("a fresh reader: %v", err)
	}
}

// An entry whose kind is long, or unknown, is refused by reason alone:
// nothing it says goes into the segment, which stays book/v1 (review item
// 3, probe 7).
func TestLongKindRefusedWithoutBrickingTheBook(t *testing.T) {
	r := newRig(t)
	r.listRaw([]byte(`{"farm":"","kind":"` + strings.Repeat("k", 100) + `","order":"pending","position":0}`))
	res, err := r.sealer.Seal(r.ctx)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if len(res.Refused) != 1 || res.Refused[0].Reason != farmbook.ReasonUnknownKind {
		t.Errorf("refused %+v", res.Refused)
	}
	if _, _, err := r.reader().Sync(r.ctx); err != nil {
		t.Errorf("a fresh reader: %v", err)
	}
}

// An entry marked redacted is checked against its ID like any other: a
// revoke rewritten to a grant is refused (review item 5, probe 8).
func TestCheckPageChecksAnEntryMarkedRedacted(t *testing.T) {
	r := newRig(t)
	subj := "submitgrant:saltmarsh/dana-flow"
	r.book(r.control(r.hud, "revoke", subj, map[string]any{"person": "dana"}, r.now), nil)
	r.seal()
	for _, red := range [][]string{{"addr"}, {farmbook.RedactedPrivate}} {
		p := r.page(0)
		p.Entries[0].Entry = json.RawMessage(strings.Replace(string(p.Entries[0].Entry), `"act":"revoke"`, `"act":"grant"`, 1))
		p.Entries[0].Redacted = red
		if _, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, farmbook.LastHead{}, p); !errors.Is(err, farmbook.ErrBook) {
			t.Errorf("a rewritten entry marked redacted %v: %v", red, err)
		}
	}
}

// Only an adopted entry may be withheld whole. A later one withheld whole,
// a revoke a compromised HUD hides, is refused, so the grant it ended
// can't stay in force for the reader.
func TestOnlyAnAdoptedEntryMayBeWithheldWhole(t *testing.T) {
	r := newRig(t)
	e := farmbook.Entry{Farm: r.farm.ID, Position: 1, Order: farmbook.OrderIssuer, At: r.now.Format(time.RFC3339Nano),
		Kind: "device", Device: provtest.ID("a device"), Person: "dana"}
	b, _ := json.Marshal(&e)
	adopted, _ := schema.Canonical(b)
	r.sealer.Adopt = func(context.Context) (*farmbook.Adoption, error) {
		return &farmbook.Adoption{Head: provtest.ID(string(adopted)), Position: 1, Entries: [][]byte{adopted}}, nil
	}
	r.seal()
	r.book(r.control(r.hud, "revoke", "submitgrant:saltmarsh/dana-flow", map[string]any{"person": "dana"}, r.now), nil)
	r.seal()
	withhold := func(k int) *farmbook.Page {
		p := r.page(0)
		if len(p.Entries) != 2 {
			t.Fatalf("page holds %d entries, want the adopted one and the revoke", len(p.Entries))
		}
		p.Entries[k].Entry, p.Entries[k].Redacted = nil, []string{farmbook.RedactedEntry}
		return p
	}
	if _, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, farmbook.LastHead{}, withhold(0)); err != nil {
		t.Errorf("the adopted entry withheld whole: %v", err)
	}
	if _, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, farmbook.LastHead{}, withhold(1)); !errors.Is(err, farmbook.ErrBook) {
		t.Errorf("the revoke withheld whole: %v", err)
	}
	p := withhold(0)
	p.Entries[0].Entry = json.RawMessage(`{"act":"grant"}`)
	if _, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, farmbook.LastHead{}, p); err == nil {
		t.Error("an entry withheld and given")
	}
}

// A segment is never sealed before the one it follows: the sealer holds
// its clock to the last seal, and a reader refuses a segment that goes
// back (review item 8).
func TestSegmentTimeNeverGoesBack(t *testing.T) {
	r := newRig(t)
	r.book(r.control(r.hud, "disable", "node:n1", map[string]any{"unschedulable": true}, r.now), nil)
	first := r.seal().Segment
	r.now = r.now.Add(-time.Hour)
	r.book(r.control(r.hud, "enable", "node:n1", map[string]any{"unschedulable": false}, r.now), nil)
	second := r.seal().Segment
	if second.SignedAt.Before(first.SignedAt) {
		t.Fatalf("segment 2 sealed at %s, before segment 1 at %s", second.SignedAt, first.SignedAt)
	}
	// A segment planted after them, signed by the queue an hour earlier.
	b := r.read()
	p := schema.BookSegmentPredicate{Farm: r.farm.ID, Number: 3, Prev: second.ID, First: 3, Last: 2, Entries: []string{}}
	signed, err := provenance.SignStatement(r.ctx, r.queue, provenance.PredicateBook,
		[]provenance.Subject{{Name: "book:" + r.farm.ID + "@2", Digest: map[string]string{"c4": b.Head().Entry}}}, p, first.SignedAt.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	id := provtest.ID(string(signed))
	r.store.Put(r.ctx, id, signed)
	r.ptr.Set(farmbook.State{Head: farmbook.Head{Segment: id, Number: 3, Position: 2, Entry: b.Head().Entry}})
	if _, _, err := r.reader().Sync(r.ctx); !errors.Is(err, farmbook.ErrBook) || !strings.Contains(err.Error(), "before the segment it follows") {
		t.Errorf("a segment sealed before the one it follows: %v", err)
	}
}

// timeless admits entries whose record says nothing of when it was signed.
type timeless struct{}

func (timeless) Kinds() []string { return []string{"timeless"} }
func (timeless) Admit(ctx context.Context, raw []byte, p farmbook.Placement) ([]byte, time.Time, error) {
	b, _, err := deviceChecker{}.Admit(ctx, raw, p)
	return b, time.Time{}, err
}

// An entry whose check returns no time is flagged no-time; one written
// long after the sealer first saw it pending is still judged from that
// first sight (review item 9).
func TestEntryWithNoTimeFlagged(t *testing.T) {
	r := newRig(t)
	checkers := r.sealer.Checkers
	r.sealer.Checkers = func() []farmbook.Checker { return append(checkers(), timeless{}) }
	r.writer.Append(r.ctx, farmbook.Submission{Entry: &farmbook.Entry{Kind: "timeless", Order: farmbook.OrderPending, At: r.now.Format(time.RFC3339Nano)}})
	res := r.seal()
	if fl := res.Segment.Flags; len(fl) != 1 || fl[0].Reason != farmbook.FlagNoTime || fl[0].Position != 1 {
		t.Errorf("flags %+v", res.Segment.Flags)
	}
}

// A seal takes at most MaxSeal entries, leaving the rest pending, and
// drops any listed past MaxPending (review item 12).
func TestSealerBoundsWhatItTakes(t *testing.T) {
	r := newRig(t)
	var ids []string
	for k := 0; k < farmbook.MaxPending+5; k++ {
		ids = append(ids, provtest.ID("listing "+string(rune('a'+k%26))+time.Duration(k).String()))
	}
	r.list(ids...)
	res, err := r.sealer.Seal(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := r.ptr.Read(r.ctx)
	if len(res.Refused) != farmbook.MaxSeal || len(st.Pending) != farmbook.MaxPending-farmbook.MaxSeal {
		t.Errorf("refused %d, left %d pending; want %d and %d", len(res.Refused), len(st.Pending), farmbook.MaxSeal, farmbook.MaxPending-farmbook.MaxSeal)
	}
}

// An adopted entry today's checks refuse is adopted all the same, flagged
// unchecked, and never stops the seal (review item 11).
func TestAdoptedEntryThatNoLongerChecksFlagged(t *testing.T) {
	r := newRig(t)
	var raws [][]byte
	prev := ""
	for k, kind := range []string{"device", "mystery"} {
		e := farmbook.Entry{Farm: r.farm.ID, Position: int64(k + 1), Prev: prev, Order: farmbook.OrderIssuer, At: r.now.Format(time.RFC3339Nano),
			Kind: kind, Device: provtest.ID("a device"), Person: "dana"}
		b, _ := json.Marshal(&e)
		c, _ := schema.Canonical(b)
		raws, prev = append(raws, c), provtest.ID(string(c))
	}
	r.sealer.Adopt = func(context.Context) (*farmbook.Adoption, error) {
		return &farmbook.Adoption{Head: prev, Position: 2, Entries: raws}, nil
	}
	res := r.seal()
	if res.Segment.Adopted == nil || res.Segment.Last != 2 {
		t.Fatalf("adopted %+v", res.Segment.BookSegmentPredicate)
	}
	if fl := res.Segment.Flags; len(fl) != 1 || fl[0].Position != 2 || fl[0].Reason != farmbook.FlagUnchecked {
		t.Errorf("flags %+v", res.Segment.Flags)
	}
}

// A reader in the manager reads the sealer's own head: a State rolled back
// doesn't take it back, and a State naming a segment the sealer never
// sealed is refused (review item 2).
func TestReaderInTheManagerReadsTheSealersHead(t *testing.T) {
	r := newRig(t)
	r.book(r.control(r.hud, "disable", "node:n1", map[string]any{"unschedulable": true}, r.now), nil)
	r.seal()
	older, _ := r.ptr.Read(r.ctx)
	r.book(r.control(r.hud, "enable", "node:n1", map[string]any{"unschedulable": false}, r.now), nil)
	r.seal()
	r.ptr.Set(older)
	if b, _, err := r.ownReader().Sync(r.ctx); err != nil || b.Head().Number != 2 {
		t.Errorf("a manager reader after the State was rolled back: %v", err)
	}
	r.ptr.Set(farmbook.State{Head: farmbook.Head{Number: 9, Segment: provtest.ID("nothing")}})
	if _, _, err := r.ownReader().Sync(r.ctx); !errors.Is(err, farmbook.ErrBook) {
		t.Errorf("a State past the sealer's head: %v", err)
	}
}

// In force is judged at the time the caller passes: a verifier at the
// time a position was sealed sees a device in force that a live decision,
// at the clock, sees lapsed on a quiet book, and a grant past its until
// likewise.
func TestInForceAtTakesItsTime(t *testing.T) {
	r := newRig(t)
	device := provtest.ID("dana's laptop key")
	key := "submitgrant:saltmarsh/dana-flow"
	r.writer.Append(r.ctx, farmbook.Submission{Entry: &farmbook.Entry{Kind: "device", Device: device, Person: "dana", Order: farmbook.OrderPending, At: r.now.Format(time.RFC3339Nano)}})
	r.book(r.control(r.hud, "grant", key, grantChange("dana", r.now.Add(time.Hour)), r.now), nil)
	r.seal()
	b := r.read()
	pos := b.Head().Position
	later := r.now.Add(farmbook.DeviceLapse + time.Hour)
	for _, c := range []struct{ kind, key string }{{farmbook.ForceDevice, device}, {farmbook.ForceGrant, key}} {
		if on, _ := b.InForce(c.kind, c.key, pos); on == nil {
			t.Errorf("%s: a verifier at its seal sees it out of force", c.kind)
		}
		if on, err := b.InForceAt(c.kind, c.key, pos, later); err != nil || on != nil {
			t.Errorf("%s: a live check, past its end, sees it in force (%v)", c.kind, err)
		}
	}
}
