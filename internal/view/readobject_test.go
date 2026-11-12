package view

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// Bytes that are not the ID asked for are not present (Joshua,
// 2026-11-11): ReadObject returns none of them, with an error every
// caller already takes for a missing object, and tells OnMismatch both
// IDs. The right bytes read as ever, and tell nothing.
func TestBytesThatAreNotTheIDAskedForAreNotPresent(t *testing.T) {
	st := objStore{}
	asked := c4.Identify(strings.NewReader("the record"))
	other := []byte("not the record")
	got := c4.Identify(bytes.NewReader(other))
	st[asked] = other
	var told [][2]c4.ID
	was := OnMismatch
	t.Cleanup(func() { OnMismatch = was })
	OnMismatch = func(_ string, a, g c4.ID, _ error) { told = append(told, [2]c4.ID{a, g}) }

	data, err := ReadObject(st, asked)
	if data != nil || err == nil {
		t.Fatalf("a read of another object's bytes: %q, %v", data, err)
	}
	for _, missing := range []error{ErrNotItsID, ErrIncomplete, storeurl.ErrNotFound, fs.ErrNotExist} {
		if !errors.Is(err, missing) {
			t.Errorf("%v is not %v: a caller that degrades on that would not", err, missing)
		}
	}
	if !strings.Contains(err.Error(), asked.String()) || !strings.Contains(err.Error(), got.String()) {
		t.Errorf("the error names neither or one of the IDs: %v", err)
	}
	if len(told) != 1 || told[0] != [2]c4.ID{asked, got} {
		t.Errorf("told %v, want [%s %s]", told, asked, got)
	}

	right := []byte("the record")
	st[asked] = right
	told = nil
	if data, err := ReadObject(st, asked); err != nil || !bytes.Equal(data, right) || len(told) != 0 {
		t.Errorf("a read of the right bytes: %q, %v, told %v", data, err, told)
	}
}

// A view one of whose records is another object's bytes resolves as
// incomplete, never as another tree.
func TestAViewWithASwappedRecordIsIncomplete(t *testing.T) {
	st := objStore{}
	all, _ := NewFilter(nil)
	m := c4m.NewManifest()
	m.AddEntry(&c4m.Entry{Name: "shots/", Size: -1, Timestamp: c4m.NullTimestamp()})
	m.AddEntry(&c4m.Entry{Name: "a.exr", Depth: 1, Size: 1, Timestamp: c4m.NullTimestamp(), C4ID: c4.Identify(strings.NewReader("a"))})
	b, err := c4m.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	res, err := FromBytes(b, st, all)
	if err != nil {
		t.Fatal(err)
	}
	v := res.View()
	recs, _ := v.Records()
	for _, r := range recs {
		st[r.ID] = r.Data
	}
	sub := v.Root.Dirs["shots/"].Entry.C4ID
	was := OnMismatch
	t.Cleanup(func() { OnMismatch = was })
	OnMismatch = func(string, c4.ID, c4.ID, error) {}
	st[sub] = st[v.ID] // the root's record under the subdirectory's ID
	if r, err := FromID(v.ID, st, all); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("a view whose record is another's bytes: %v, %v", r, err)
	}
}

// Through a store that can (storeurl.Sharded), the read that finds a wrong
// object re-files it: the bytes are under their own ID, and gone from under
// the one asked for, and the read says so (ErrRefiled), as does
// OnMismatch.
func TestTheReadThatFindsAWrongObjectRefilesIt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: dir}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	asked, err := st.PutBytes(ctx, []byte("the record"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := []byte("not the record")
	got := c4.Identify(bytes.NewReader(wrong))
	ts, _ := store.NewTreeStore(dir)
	p, _ := ts.ContentPath(asked)
	os.Chmod(p, 0o644)
	if err := os.WriteFile(p, wrong, 0o644); err != nil {
		t.Fatal(err)
	}
	var refiled error = errors.New("not told")
	was := OnMismatch
	t.Cleanup(func() { OnMismatch = was })
	OnMismatch = func(_ string, _, _ c4.ID, r error) { refiled = r }
	if _, err := ReadObject(st, asked); !errors.Is(err, ErrNotItsID) || !errors.Is(err, ErrRefiled) {
		t.Fatalf("the read: %v", err)
	}
	if refiled != nil {
		t.Fatalf("OnMismatch was told %v", refiled)
	}
	if b, err := ReadObject(st, got); err != nil || !bytes.Equal(b, wrong) {
		t.Fatalf("under their own ID: %q, %v", b, err)
	}
	if _, err := st.Head(ctx, asked); !errors.Is(err, storeurl.ErrNotFound) {
		t.Fatalf("still under the ID asked for: %v", err)
	}
}
