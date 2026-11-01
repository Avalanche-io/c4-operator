package farmbook_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
)

// rig is a farm with a HUD and a queue that sign as themselves, the
// book's store and State, and a sealer.
type rig struct {
	t      *testing.T
	ctx    context.Context
	farm   *provtest.Farm
	now    time.Time
	hud    *provenance.Signer
	queue  *provenance.Signer
	store  *farmbook.MemoryStore
	ptr    *farmbook.MemoryPointer
	sealer *farmbook.Sealer
	writer *farmbook.Writer
}

// signer signs as identity on farm f, with a leaf issued two hours before
// now and valid two days, so a test may sign at any time near now.
func signer(t *testing.T, f *provtest.Farm, identity string) *provenance.Signer {
	t.Helper()
	s, err := provenance.NewSigner(context.Background(), func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		c, err := f.CA.Issue(pub, identity, 48*time.Hour, f.Now)
		if err != nil {
			return nil, err
		}
		return []*x509.Certificate{c, f.CA.Cert}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newRig(t *testing.T) *rig {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	r := &rig{t: t, ctx: context.Background(), farm: provtest.NewFarm(t, now.Add(-2*time.Hour)), now: now,
		store: &farmbook.MemoryStore{}, ptr: &farmbook.MemoryPointer{}}
	r.hud = signer(t, r.farm, provenance.ComponentIdentity("hud"))
	r.queue = signer(t, r.farm, provenance.ComponentIdentity("queue"))
	r.writer = &farmbook.Writer{Store: r.store, Pointer: r.ptr}
	r.sealer = &farmbook.Sealer{Farm: r.farm.ID, Roots: r.farm.Roots, Store: r.store, Pointer: r.ptr, Signer: r.queue, Log: logr.Discard(),
		Now: func() time.Time { return r.now },
		Checkers: func() []farmbook.Checker {
			return []farmbook.Checker{&farmbook.ControlChecker{Farm: r.farm.ID, Roots: r.farm.Roots, Store: r.store}, &deviceChecker{}}
		}}
	return r
}

// control signs a control record as s at at.
func (r *rig) control(s *provenance.Signer, action, target string, change map[string]any, at time.Time) []byte {
	r.t.Helper()
	pred := map[string]any{"actor": "dana", "authenticated": "SSO proxy", "via": "hud", "authorization": "SubjectAccessReview",
		"action": action, "target": target, "change": change}
	rec, err := provenance.SignStatement(r.ctx, s, provenance.PredicateControl, []provenance.Subject{{Name: target, Digest: map[string]string{"sha256": strings.Repeat("0", 64)}}}, pred, at)
	if err != nil {
		r.t.Fatal(err)
	}
	return rec
}

// book writes the entry a control record makes, edited by edit, for the
// seal.
func (r *rig) book(rec []byte, edit func(*farmbook.Entry)) string {
	r.t.Helper()
	e, err := farmbook.ControlEntry(rec, r.now)
	if err != nil {
		r.t.Fatal(err)
	}
	if edit != nil {
		edit(e)
	}
	ids, err := r.writer.Append(r.ctx, farmbook.Submission{Entry: e, Record: rec})
	if err != nil {
		r.t.Fatal(err)
	}
	return ids[0]
}

func (r *rig) seal() *farmbook.SealResult {
	r.t.Helper()
	res, err := r.sealer.Seal(r.ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *rig) reader() *farmbook.Reader {
	return &farmbook.Reader{Farm: r.farm.ID, Roots: r.farm.Roots, Store: r.store, Pointer: r.ptr}
}

func (r *rig) read() *farmbook.Book {
	r.t.Helper()
	b, _, err := r.reader().Sync(r.ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func grantChange(person string, until time.Time) map[string]any {
	return map[string]any{"person": person, "tracker": "flow", "pools": []string{"render"}, "until": until}
}

// deviceChecker stands in for the people's issuer's checker: it admits
// device and removal entries as written.
type deviceChecker struct{}

func (deviceChecker) Kinds() []string { return []string{"device", "removal"} }
func (deviceChecker) Admit(context.Context, *farmbook.Entry, []byte) (time.Time, error) {
	return time.Time{}, nil
}

// An entry whose record says something else is refused at the seal, never
// sealed, and the refusal is listed in the segment; the entry beside it
// that agrees is sealed.
func TestEntryDisagreeingWithItsRecordRefused(t *testing.T) {
	r := newRig(t)
	until := r.now.Add(30 * 24 * time.Hour)
	r.book(r.control(r.hud, "grant", "submitgrant:saltmarsh/dana-flow", grantChange("dana", until), r.now), nil)
	for what, edit := range map[string]func(*farmbook.Entry){
		"the person":  func(e *farmbook.Entry) { e.Person = "noor" },
		"the act":     func(e *farmbook.Entry) { e.Act = "revoke" },
		"the subject": func(e *farmbook.Entry) { e.Subject = "submitgrant:saltmarsh/noor-flow" },
		"the expiry":  func(e *farmbook.Entry) { e.Expires = until.Add(365 * 24 * time.Hour).Format(time.RFC3339Nano) },
		"the change":  func(e *farmbook.Entry) { e.Settings = provtest.ID("another change") },
		"who acted":   func(e *farmbook.Entry) { e.By = "noor" },
		"the device":  func(e *farmbook.Entry) { e.Device = provtest.ID("a device") },
	} {
		c := grantChange("dana", until)
		c["note"] = what // a record of its own each time
		r.book(r.control(r.hud, "grant", "submitgrant:saltmarsh/dana-flow", c, r.now), edit)
		res := r.seal()
		if len(res.Refused) != 1 || !strings.Contains(res.Refused[0].Why, "disagree on "+what) {
			t.Errorf("an entry disagreeing on %s: refused %+v", what, res.Refused)
		}
	}
	b := r.read()
	if n := len(b.Entries(0)); n != 1 {
		t.Fatalf("sealed %d entries, want only the one that agrees", n)
	}
	if b.Entries(0)[0].Entry.Record == "" || len(b.Refusals()) != 7 {
		t.Errorf("refusals %d, want 7", len(b.Refusals()))
	}
	st, _ := r.ptr.Read(r.ctx)
	if len(st.Pending) != 0 {
		t.Errorf("refused entries still pending: %v", st.Pending)
	}
}

// An entry citing a record that doesn't verify against the farm's roots,
// or that isn't the HUD's, is refused.
func TestEntryCitingAnUnverifiedRecordRefused(t *testing.T) {
	r := newRig(t)
	other := provtest.NewFarm(t, r.farm.Now)
	until := r.now.Add(time.Hour)
	r.book(r.control(signer(t, other, provenance.ComponentIdentity("hud")), "grant", "submitgrant:saltmarsh/a", grantChange("dana", until), r.now), nil)
	r.book(r.control(r.queue, "grant", "submitgrant:saltmarsh/b", grantChange("dana", until), r.now), nil)
	// The HUD writes only farm-level acts; one written around it is refused.
	decide := r.control(r.hud, "decide", "workjob:saltmarsh/sm0140", map[string]any{"do": "approve"}, r.now)
	if _, err := r.writer.Append(r.ctx, farmbook.Submission{Record: decide, Entry: &farmbook.Entry{Kind: farmbook.KindControl, Order: farmbook.OrderPending,
		At: r.now.Format(time.RFC3339Nano), Record: provtest.ID(string(decide)), Act: "decide", Subject: "workjob:saltmarsh/sm0140", By: "dana"}}); err != nil {
		t.Fatal(err)
	}
	res := r.seal()
	whys := []string{"doesn't verify", "the policy wants https://cccc.io/id/component/hud", "not a farm-level act"}
	if len(res.Refused) != len(whys) {
		t.Fatalf("refused %+v", res.Refused)
	}
	for k, why := range whys {
		if !strings.Contains(res.Refused[k].Why, why) {
			t.Errorf("refusal %d: %q, want %q", k, res.Refused[k].Why, why)
		}
	}
	if n := len(r.read().Entries(0)); n != 0 {
		t.Errorf("sealed %d entries", n)
	}
}

// A control entry can't be booked twice.
func TestRecordBookedOnce(t *testing.T) {
	r := newRig(t)
	rec := r.control(r.hud, "bind", "deploymentbinding:farm", map[string]any{"owner": "noor", "to": "studio/*"}, r.now)
	r.book(rec, nil)
	r.seal()
	r.book(rec, nil)
	if res := r.seal(); len(res.Refused) != 1 || !strings.Contains(res.Refused[0].Why, "booked already") {
		t.Fatalf("booked twice: %+v", res)
	}
}

// Each segment names the one before it: a segment the queue signed that
// names another, or skips a number, is refused by every reader.
func TestSegmentLinkChecked(t *testing.T) {
	r := newRig(t)
	for _, name := range []string{"a", "b"} {
		r.book(r.control(r.hud, "bind", "deploymentbinding:"+name, map[string]any{"owner": "noor"}, r.now), nil)
		r.seal()
	}
	b := r.read()
	segs := b.Segments(0)
	if len(segs) != 2 || segs[1].Prev != segs[0].ID {
		t.Fatalf("segments %+v", segs)
	}
	// A third segment, signed by the queue, naming the first as before it.
	forged := segs[1].BookSegmentPredicate
	forged.Number, forged.Prev, forged.First, forged.Entries = 3, segs[0].ID, forged.Last+1, []string{}
	signed, err := provenance.SignStatement(r.ctx, r.queue, provenance.PredicateBook,
		[]provenance.Subject{{Name: "book:" + r.farm.ID + "@2", Digest: map[string]string{"c4": b.Head().Entry}}}, forged, r.now)
	if err != nil {
		t.Fatal(err)
	}
	id := provtest.ID(string(signed))
	r.store.Put(r.ctx, id, signed)
	st, _ := r.ptr.Read(r.ctx)
	r.ptr.Set(farmbook.State{Head: farmbook.Head{Segment: id, Number: 3, Position: 2, Entry: b.Head().Entry}})
	if _, _, err := r.reader().Sync(r.ctx); !errors.Is(err, farmbook.ErrBook) || !strings.Contains(err.Error(), "rewritten") {
		t.Errorf("a segment naming the wrong one before it: %v", err)
	}
	// The sealer refuses to seal after it too.
	if _, err := r.sealer.Seal(r.ctx); err == nil {
		t.Error("the sealer sealed after a forged segment")
	}
	r.ptr.Set(st)
	if _, _, err := r.reader().Sync(r.ctx); err != nil {
		t.Errorf("the real head: %v", err)
	}
}

// A segment signed by anyone but the queue is refused.
func TestSegmentSignedByTheQueueOnly(t *testing.T) {
	r := newRig(t)
	r.sealer.Signer = r.hud
	r.book(r.control(r.hud, "bind", "deploymentbinding:farm", map[string]any{"owner": "noor"}, r.now), nil)
	if _, err := r.sealer.Seal(r.ctx); err == nil {
		t.Error("the sealer kept a segment it signed as the HUD")
	}
	if _, _, err := r.reader().Sync(r.ctx); !errors.Is(err, farmbook.ErrBook) || !strings.Contains(err.Error(), "component/queue") {
		t.Errorf("a segment the HUD signed: %v", err)
	}
}

// A head rolled back to an older segment the queue signed verifies on its
// own; a reader holding the last head it saw refuses it, whether it read
// the newer head in this process or was given it back.
func TestRollbackDetectedByTheLastHead(t *testing.T) {
	r := newRig(t)
	r.book(r.control(r.hud, "grant", "submitgrant:saltmarsh/dana-flow", grantChange("dana", r.now.Add(time.Hour)), r.now), nil)
	r.seal()
	older, _ := r.ptr.Read(r.ctx)
	r.book(r.control(r.hud, "revoke", "submitgrant:saltmarsh/dana-flow", map[string]any{"person": "dana"}, r.now), nil)
	r.seal()
	rd := r.reader()
	if _, _, err := rd.Sync(r.ctx); err != nil {
		t.Fatal(err)
	}
	kept := rd.Last()
	if kept.Number != 2 || kept.Position != 2 {
		t.Fatalf("last head %+v", kept)
	}
	r.ptr.Set(older)
	if _, err := farmbook.ReadBook(r.ctx, r.farm.ID, r.farm.Roots, r.store, older.Head); err != nil {
		t.Fatalf("the older head verifies by itself: %v", err)
	}
	if _, _, err := rd.Sync(r.ctx); !errors.Is(err, farmbook.ErrRollback) {
		t.Errorf("a rolled-back head: %v", err)
	}
	if rd.Last() != kept {
		t.Errorf("the reader dropped the head it kept: %+v", rd.Last())
	}
	fresh := r.reader()
	fresh.Keep(kept)
	if _, _, err := fresh.Sync(r.ctx); !errors.Is(err, farmbook.ErrRollback) {
		t.Errorf("a client given back its last head: %v", err)
	}
}

// The seal interval is seconds, never minutes.
func TestSealIntervalBounded(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second, 31 * time.Second, 5 * time.Minute} {
		if err := farmbook.CheckEvery(d); err == nil {
			t.Errorf("a seal interval of %s accepted", d)
		}
	}
	for _, d := range []time.Duration{time.Second, farmbook.DefaultSealEvery, farmbook.MaxSealEvery} {
		if err := farmbook.CheckEvery(d); err != nil {
			t.Errorf("a seal interval of %s: %v", d, err)
		}
	}
	if farmbook.MaxSealEvery > time.Minute {
		t.Errorf("the bound is %s, minutes", farmbook.MaxSealEvery)
	}
}

// Before its entry is sealed, a revocation counts for the HUD, which reads
// the pending entries; a verifier judges by sealed position only, where
// the grant is still in force, and a pending position isn't sealed.
func TestPendingCountsForTheHUDNotAVerifier(t *testing.T) {
	r := newRig(t)
	key := "submitgrant:saltmarsh/dana-flow"
	r.book(r.control(r.hud, "grant", key, grantChange("dana", r.now.Add(time.Hour)), r.now), nil)
	r.seal()
	r.book(r.control(r.hud, "revoke", key, map[string]any{"person": "dana"}, r.now), nil)
	v, err := r.reader().View(r.ctx, r.now)
	if err != nil {
		t.Fatal(err)
	}
	if v.Position() != 2 || len(v.Pending()) != 1 || v.Head().Position != 1 {
		t.Fatalf("view at %d, %d pending, sealed to %d", v.Position(), len(v.Pending()), v.Head().Position)
	}
	if on, err := v.InForce(farmbook.ForceGrant, key, v.Position()); err != nil || on != nil {
		t.Errorf("the HUD, the revocation pending: %v, %v", on, err)
	}
	if on, err := v.Book.InForce(farmbook.ForceGrant, key, v.Head().Position); err != nil || on == nil {
		t.Errorf("a verifier at the sealed head: %v, %v", on, err)
	}
	if _, err := v.Book.InForce(farmbook.ForceGrant, key, 2); !errors.Is(err, farmbook.ErrNotSealed) {
		t.Errorf("a verifier at the pending position: %v", err)
	}
	r.seal()
	if on, err := r.read().InForce(farmbook.ForceGrant, key, 2); err != nil || on != nil {
		t.Errorf("a verifier once the revocation is sealed: %v, %v", on, err)
	}
}

// In force at a position is right across a grant's revocation and its
// until, and a device's removal: in force before, not from the removal's
// position on, whatever the clock says. The time at a position is its
// seal's.
func TestInForceAcrossARemoval(t *testing.T) {
	r := newRig(t)
	key := "submitgrant:saltmarsh/dana-flow"
	device := provtest.ID("dana's laptop key")
	other := provtest.ID("dana's desk key")
	write := func(e map[string]any) {
		e["order"], e["at"] = farmbook.OrderPending, r.now.Format(time.RFC3339Nano)
		if _, err := r.writer.Append(r.ctx, farmbook.Submission{Entry: e}); err != nil {
			t.Fatal(err)
		}
		r.seal()
		r.now = r.now.Add(time.Second)
	}
	r.book(r.control(r.hud, "grant", key, grantChange("dana", r.now.Add(10*time.Second)), r.now), nil)
	r.seal()                                                                              // 1: the grant
	write(map[string]any{"kind": "device", "device": device, "person": "dana"})           // 2
	write(map[string]any{"kind": "device", "device": other, "person": "dana"})            // 3
	write(map[string]any{"kind": "removal", "device": device, "person": "dana"})          // 4
	r.book(r.control(r.hud, "revoke", key, map[string]any{"person": "dana"}, r.now), nil) // 5
	r.seal()
	b := r.read()
	for _, c := range []struct {
		kind, key string
		pos       int64
		in        bool
	}{
		{farmbook.ForceGrant, key, 1, true}, {farmbook.ForceGrant, key, 4, true}, {farmbook.ForceGrant, key, 5, false},
		{farmbook.ForceDevice, device, 1, false}, {farmbook.ForceDevice, device, 2, true}, {farmbook.ForceDevice, device, 3, true},
		{farmbook.ForceDevice, device, 4, false}, {farmbook.ForceDevice, device, 5, false},
		{farmbook.ForceDevice, other, 4, true}, {farmbook.ForceDevice, other, 5, true},
	} {
		on, err := b.InForce(c.kind, c.key, c.pos)
		if err != nil || (on != nil) != c.in {
			t.Errorf("%s %.12s at %d: in force %v (%v), want %v", c.kind, c.key, c.pos, on != nil, err, c.in)
		}
	}
	// A grant whose until passed by its position's seal is out of force,
	// though nothing revoked it.
	r2 := newRig(t)
	r2.book(r2.control(r2.hud, "grant", key, grantChange("dana", r2.now.Add(time.Minute)), r2.now), nil)
	r2.seal()
	r2.now = r2.now.Add(2 * time.Minute)
	r2.book(r2.control(r2.hud, "bind", "deploymentbinding:farm", map[string]any{"owner": "noor"}, r2.now), nil)
	r2.seal()
	b2 := r2.read()
	if on, _ := b2.InForce(farmbook.ForceGrant, key, 1); on == nil {
		t.Error("the grant at its own position")
	}
	if on, _ := b2.InForce(farmbook.ForceGrant, key, 2); on != nil {
		t.Error("the grant past its until")
	}
	if p, ok := b2.PositionOf(b2.Entries(1)[0].Entry.Record); !ok || p != 2 {
		t.Errorf("the binding's record counts from %d, %v", p, ok)
	}
}

// A record whose signedAt falls before the window its position allows (a
// backdated record, booked long after) or after it (signed in the future)
// is flagged in its segment, sealed, not refused.
func TestSignedAtOutsideItsWindowFlagged(t *testing.T) {
	r := newRig(t)
	r.book(r.control(r.hud, "bind", "deploymentbinding:farm", map[string]any{"owner": "noor"}, r.now), nil)
	r.seal()
	first := r.now
	r.now = r.now.Add(10 * time.Second)
	key := "submitgrant:saltmarsh/dana-flow"
	backdated := r.control(r.hud, "grant", key, grantChange("dana", first.Add(time.Hour)), first.Add(-time.Hour))
	ahead := r.control(r.hud, "grant", "submitgrant:saltmarsh/noor-flow", grantChange("noor", first.Add(time.Hour)), r.now.Add(time.Hour))
	fresh := r.control(r.hud, "grant", "submitgrant:saltmarsh/lee-flow", grantChange("lee", first.Add(time.Hour)), r.now.Add(-time.Second))
	for _, rec := range [][]byte{backdated, ahead, fresh} {
		r.book(rec, nil)
	}
	res := r.seal()
	if len(res.Refused) != 0 || res.Segment.Last != 4 {
		t.Fatalf("refused %+v, sealed to %d", res.Refused, res.Segment.Last)
	}
	flags := r.read().Flags()
	if len(flags) != 2 || flags[0].Position != 2 || flags[1].Position != 3 {
		t.Fatalf("flags %+v", flags)
	}
	from, _ := time.Parse(time.RFC3339Nano, flags[0].From)
	if want := first.Add(-provenance.ActSkew); !from.Equal(want) {
		t.Errorf("the window opens at %s, want the seal before less the skew, %s", from, want)
	}
	if on, _ := r.read().InForce(farmbook.ForceGrant, key, 2); on == nil {
		t.Error("a flagged grant is still booked, and in force at its position")
	}
}

// Entries are written pending, and the sealer gives each its position and
// the entry before it, so the entries chain as the segments do.
func TestEntriesChain(t *testing.T) {
	r := newRig(t)
	for _, n := range []string{"a", "b", "c"} {
		r.book(r.control(r.hud, "bind", "deploymentbinding:"+n, map[string]any{"owner": "noor"}, r.now), nil)
	}
	r.seal()
	es := r.read().Entries(0)
	for k, e := range es {
		var m map[string]any
		json.Unmarshal(e.Raw, &m)
		want := ""
		if k > 0 {
			want = es[k-1].ID
		}
		if e.Entry.Position != int64(k+1) || e.Entry.Prev != want || m["order"] != farmbook.OrderBook {
			t.Errorf("entry %d: %+v", k, e.Entry)
		}
	}
	// An entry written with a position of its own is refused.
	e, _ := farmbook.ControlEntry(r.control(r.hud, "bind", "deploymentbinding:d", map[string]any{"owner": "noor"}, r.now), r.now)
	e.Order = farmbook.OrderBook
	if _, err := r.writer.Append(r.ctx, farmbook.Submission{Entry: e}); err == nil {
		t.Error("an entry written in the book's order")
	}
}

// page is the book as the HUD's route serves it after since, read by a
// fresh reader, as a HUD that restarted (or whoever controls it) would.
func (r *rig) page(since int64) *farmbook.Page {
	r.t.Helper()
	v, err := r.reader().View(r.ctx, r.now)
	if err != nil {
		r.t.Fatal(err)
	}
	return farmbook.NewPage(v, since, 1000)
}

// A client checks each page against the last head it kept: a page that
// goes on from it is taken; one served from a rolled-back head, or with an
// entry changed, is refused, though every segment on it verifies.
func TestClientRefusesARolledBackPage(t *testing.T) {
	r := newRig(t)
	key := "submitgrant:saltmarsh/dana-flow"
	r.book(r.control(r.hud, "grant", key, grantChange("dana", r.now.Add(time.Hour)), r.now), nil)
	r.seal()
	older, _ := r.ptr.Read(r.ctx)
	r.book(r.control(r.hud, "revoke", key, map[string]any{"person": "dana"}, r.now), nil)
	r.seal()
	var last farmbook.LastHead
	last, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, last, r.page(0))
	if err != nil || last.Number != 2 || last.Position != 2 {
		t.Fatalf("the first read: %+v, %v", last, err)
	}
	// Nothing new: the same head comes back, and is taken.
	if again, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, last, r.page(last.Position)); err != nil || again != last {
		t.Errorf("a read with nothing new: %+v, %v", again, err)
	}
	// The head rolled back to before the revocation.
	newest, _ := r.ptr.Read(r.ctx)
	r.ptr.Set(older)
	if _, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, last, r.page(last.Position)); !errors.Is(err, farmbook.ErrRollback) {
		t.Errorf("a rolled-back page: %v", err)
	}
	r.ptr.Set(newest)
	// The next read goes on from the kept head.
	r.book(r.control(r.hud, "bind", "deploymentbinding:farm", map[string]any{"owner": "noor"}, r.now), nil)
	r.seal()
	p := r.page(last.Position)
	next, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, last, p)
	if err != nil || next.Number != 3 || len(p.Entries) != 1 {
		t.Fatalf("the next read: %+v, %v", next, err)
	}
	// An entry changed on the way is refused.
	p.Entries[0].Entry = []byte(strings.Replace(string(p.Entries[0].Entry), "noor", "dana", 1))
	if _, err := farmbook.CheckPage(r.farm.ID, r.farm.Roots, last, p); !errors.Is(err, farmbook.ErrBook) {
		t.Errorf("a changed entry: %v", err)
	}
}
