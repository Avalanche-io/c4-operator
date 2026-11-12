package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// A tool's tool.lock pins its images, and is published and run as read, so
// it is checked as it is read: bytes under its ID that are not that lock
// (here, a lock pinning another image) are not present to readFile. Once,
// they were read as they came.
func TestAToolLockIsReadChecked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: dir}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	lock := []byte("images:\n  - ghcr.io/studio/comp@sha256:1111\n")
	id, err := st.PutBytes(ctx, lock)
	if err != nil {
		t.Fatal(err)
	}
	m := c4m.NewManifest()
	m.AddEntry(&c4m.Entry{Name: lockFile, Size: int64(len(lock)), Timestamp: c4m.NullTimestamp(), C4ID: id})
	b, err := c4m.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := c4view.NewFilter(nil)
	res, err := c4view.FromBytes(b, st, all)
	if err != nil {
		t.Fatal(err)
	}
	v := res.View()
	if got, ok, err := readFile(ctx, st, v, lockFile, 1<<20); err != nil || !ok || string(got) != string(lock) {
		t.Fatalf("its own lock: %q, %v, %v", got, ok, err)
	}

	ts, err := store.NewTreeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := ts.ContentPath(id)
	if !ok {
		t.Fatal("no file for the lock")
	}
	os.Chmod(p, 0o644)
	if err := os.WriteFile(p, []byte("images:\n  - ghcr.io/elsewhere/miner@sha256:2222\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	was := c4view.OnMismatch
	t.Cleanup(func() { c4view.OnMismatch = was })
	var told c4.ID
	c4view.OnMismatch = func(_ string, asked, _ c4.ID, _ error) { told = asked }
	got, ok, err := readFile(ctx, st, v, lockFile, 1<<20)
	if !errors.Is(err, c4view.ErrNotItsID) || got != nil || !ok || told != id {
		t.Fatalf("another lock under its ID: %q, %v, %v, told %s", got, ok, err, told)
	}
}

// A control record is booked by the ID of its bytes, so its bytes are
// checked against the ID asked for first: another record's bytes under it
// (here, a membership that makes kai an admin) are refused, and nothing is
// booked. Once, the other record was booked as if it were this one.
func TestAControlRecordIsBookedOnlyIfItIsItsID(t *testing.T) {
	ctx := context.Background()
	p := newPartyRig(t)
	sign := func(purposes []string) []byte {
		t.Helper()
		rec, err := provenance.SignStatement(ctx, p.s.Signer, provenance.PredicateControl,
			[]provenance.Subject{{Name: farmbook.MemberKey("kestrel", "kai"), Digest: map[string]string{"sha256": "00"}}},
			controlPredicate{Actor: "noor", Action: "add-member", Target: farmbook.MemberKey("kestrel", "kai"),
				Change: map[string]any{"party": "kestrel", "member": "kai", "purposes": purposes}}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	asked := sign([]string{"member"})
	other := sign([]string{"member", "admin"})
	id := c4.Identify(bytes.NewReader(asked)).String()
	if err := p.store.Put(ctx, id, other); err != nil {
		t.Fatal(err)
	}
	if err := p.s.bookChecked(ctx, id, nil); !errors.Is(err, storeurl.ErrNotItsID) {
		t.Fatalf("another record's bytes under the ID: %v", err)
	}
	v, err := p.s.FarmBookReader.View(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(v.State.Pending) != 0 {
		t.Fatalf("booked: %d entries pending the seal", len(v.State.Pending))
	}
	// a farm-level act's record, booked after the act, the same
	p.s.bookFarm(ctx, id)
	if v, err = p.s.FarmBookReader.View(ctx, time.Now()); err != nil || len(v.State.Pending) != 0 {
		t.Fatalf("bookFarm booked another record's bytes: %v, %d pending", err, len(v.State.Pending))
	}
}

// readFile's limit holds on the bytes read, not on the size a description
// states (null here, or stated small): a tool file that names a 3 MiB
// object is refused, never read whole into the HUD (the reader of this
// branch, which had briefly checked the stated size alone).
func TestAToolFilesLimitIsOnWhatIsRead(t *testing.T) {
	ctx := context.Background()
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), 3<<20)
	id, err := st.PutBytes(ctx, big)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{-1, 10} {
		m := c4m.NewManifest()
		m.AddEntry(&c4m.Entry{Name: lockFile, Size: size, Timestamp: c4m.NullTimestamp(), C4ID: id})
		b, err := c4m.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		all, _ := c4view.NewFilter(nil)
		res, err := c4view.FromBytes(b, st, all)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		got, ok, err := readFile(ctx, st, res.View(), lockFile, 1<<20)
		if err == nil || got != nil || !ok {
			t.Errorf("a stated size of %d over a 3 MiB object: %d bytes, %v, %v", size, len(got), ok, err)
		}
	}
}
