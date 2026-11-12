package view

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	freshTries(t)
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
	freshTries(t)
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
	freshTries(t)
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

// A bounded read refuses an object past its limit, having read no more
// than one byte past it, and takes one at the limit, checked as ever.
func TestABoundedReadStopsAtItsLimit(t *testing.T) {
	freshTries(t)
	st := objStore{}
	at := bytes.Repeat([]byte("x"), 100)
	id := c4.Identify(bytes.NewReader(at))
	st[id] = at
	if b, err := ReadObjectAtMost(st, id, 100); err != nil || len(b) != 100 {
		t.Fatalf("an object at the limit: %d bytes, %v", len(b), err)
	}
	if b, err := ReadObjectAtMost(st, id, 99); err == nil || b != nil {
		t.Fatalf("an object past the limit: %d bytes, %v", len(b), err)
	}
	was := OnMismatch
	t.Cleanup(func() { OnMismatch = was })
	OnMismatch = func(string, c4.ID, c4.ID, error) {}
	st[id] = bytes.Repeat([]byte("y"), 100)
	if _, err := ReadObjectAtMost(st, id, 100); !errors.Is(err, ErrNotItsID) {
		t.Fatalf("other bytes at the limit: %v", err)
	}
}

// freshTries forgets the re-files tried, for a test that reads the same
// wrong objects as another.
func freshTries(t *testing.T) {
	t.Helper()
	triedMu.Lock()
	tried = map[triedKey]time.Time{}
	triedMu.Unlock()
}

// A wrong object a re-file cannot move is not re-filed, or told, again
// within the minute: every read is still not present, and only the first
// makes a log line and a count.
func TestAWrongObjectThatCannotMoveIsToldOnceAMinute(t *testing.T) {
	freshTries(t)
	st := objStore{}
	asked := c4.Identify(strings.NewReader("the record, again"))
	st[asked] = []byte("not it")
	told := 0
	was := OnMismatch
	t.Cleanup(func() { OnMismatch = was })
	OnMismatch = func(string, c4.ID, c4.ID, error) { told++ }
	for i := 0; i < 3; i++ {
		if _, err := ReadObject(st, asked); !errors.Is(err, ErrNotItsID) {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if told != 1 {
		t.Fatalf("told %d times, want once", told)
	}
}

// namedStore is an objStore with a name, for OnMismatch.
type namedStore struct {
	objStore
	name string
}

func (n namedStore) Name() string { return n.name }

// The once-a-minute rule is per store: the same wrong content under the
// same ID in two projects' stores is two objects, and the second is told
// though the first was a moment before.
func TestTheOnceAMinuteRuleIsPerStore(t *testing.T) {
	freshTries(t)
	asked := c4.Identify(strings.NewReader("shared content"))
	a := namedStore{objStore{asked: []byte("not it")}, "project-a"}
	b := namedStore{objStore{asked: []byte("not it")}, "project-b"}
	var told []string
	was := OnMismatch
	t.Cleanup(func() { OnMismatch = was })
	OnMismatch = func(store string, _, _ c4.ID, _ error) { told = append(told, store) }
	ReadObject(a, asked)
	ReadObject(b, asked)
	ReadObject(a, asked)
	if len(told) != 2 || told[0] != "project-a" || told[1] != "project-b" {
		t.Fatalf("told %v, want project-a then project-b, once each", told)
	}
}

// The once-a-minute rule tells two projects' stores apart even when both
// name their one shard "s0" (the independent reader of this branch): it is
// keyed by the store behind the shard, not the shard's name. Each store
// here holds the same wrong bytes under the same ID, and is read-only, so
// its re-file cannot be done and the rule applies.
func TestTheOnceAMinuteRuleTellsShardsNamedAlikeApart(t *testing.T) {
	freshTries(t)
	asked := c4.Identify(strings.NewReader("shared content"))
	open := func() *storeurl.Sharded {
		t.Helper()
		dir := t.TempDir()
		ts, err := store.NewTreeStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		w, err := ts.Create(asked)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("not it"))
		w.Close()
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(p, 0o555)
			}
			return nil
		})
		t.Cleanup(func() {
			filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
				if err == nil && d.IsDir() {
					os.Chmod(p, 0o755)
				}
				return nil
			})
		})
		s, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: dir}}, storeurl.Credentials{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := open(), open()
	var told []error
	was := OnMismatch
	t.Cleanup(func() { OnMismatch = was })
	OnMismatch = func(_ string, _, _ c4.ID, refiled error) { told = append(told, refiled) }
	for _, st := range []*storeurl.Sharded{a, b, a} {
		if _, err := ReadObject(st, asked); !errors.Is(err, ErrNotItsID) {
			t.Fatalf("a read of the wrong bytes: %v", err)
		}
	}
	if len(told) != 2 || told[0] == nil || told[1] == nil {
		t.Fatalf("told %v: want each store's failed re-file once", told)
	}
}
