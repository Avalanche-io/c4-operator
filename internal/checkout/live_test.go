package checkout

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/capture"
	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// downStore is a memStore that refuses every file while down is set.
type downStore struct {
	*memStore
	down atomic.Bool
}

func (s *downStore) PutFile(ctx context.Context, p string, id c4.ID, path string) error {
	if s.down.Load() {
		return errors.New("store unavailable")
	}
	return s.memStore.PutFile(ctx, p, id, path)
}

// A clone workspace is the agent's own folder, and the agent goes on
// working in it after a capture. So a capture must leave it alone: the
// script it just wrote still runs and keeps its date, an edit made
// afterwards doesn't reach what the capture stored, and a capture that
// failed and is run again still records the script as executable.
func TestCaptureLeavesACloneWorkspaceAlone(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("clone workspaces need APFS")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	writeBase(t, src)
	v, err := view.ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	set, err := objcache.OpenSet(filepath.Join(root, "cache"), objcache.Options{Mode: 0o644, ExecMode: 0o755, Budget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := set.Partition("p-show")
	store := &downStore{memStore: &memStore{objs: map[c4.ID][]byte{}}}
	for _, f := range v.Files {
		data, _ := os.ReadFile(filepath.Join(src, f.Path))
		cache.Put(f.ID, data)
	}
	for _, r := range v.Records() {
		cache.Put(r.ID, r.Data)
		store.PutBytes(context.Background(), "", r.ID, r.Data)
	}
	layers, err := layer.Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	b := &Clone{Layers: layers}
	in := capture.Input{
		Project: "show", Cache: cache, Sink: store, Workers: 2,
		Records: view.RecordFunc(func(_ context.Context, id c4.ID) ([]byte, error) { return cache.ReadAll(id) }),
	}
	ctx := context.Background()
	target := filepath.Join(root, "ws", "live")
	os.MkdirAll(filepath.Dir(target), 0o755)
	ws, err := b.Mount(ctx, v, target)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Unmount(ctx, ws)

	// The same edits on a plain copy say what each capture should give.
	ref := filepath.Join(root, "ref")
	copyTree(t, src, ref)
	script := func(dir, body string) string {
		p := filepath.Join(dir, "new.sh")
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	wantID := func() c4.ID {
		w, err := view.ScanDir(ref)
		if err != nil {
			t.Fatal(err)
		}
		return w.ID
	}
	const one, two = "#!/bin/sh\necho one\n", "#!/bin/sh\necho two\n"
	script(ref, one)
	mine := script(ws.Target, one)
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	os.Chtimes(mine, when, when)

	// The store is down for the first try.
	store.down.Store(true)
	if _, err := b.Capture(ctx, ws, in); err == nil {
		t.Fatal("a capture with the store down succeeded")
	}
	store.down.Store(false)
	res, err := b.Capture(ctx, ws, in)
	if err != nil {
		t.Fatal(err)
	}
	if want := wantID(); res.Output != want {
		t.Fatalf("the retried capture gave %s, the agent's tree is %s", res.Output, want)
	}
	fi, err := os.Stat(mine)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 || !fi.ModTime().Equal(when) {
		t.Fatalf("after the capture the agent's script is mode %v, dated %s", fi.Mode().Perm(), fi.ModTime().UTC().Format(time.RFC3339))
	}
	id1 := c4.Identify(bytes.NewReader([]byte(one)))
	obj, err := os.Stat(cache.Path(id1))
	if err != nil {
		t.Fatalf("the script isn't in the cache: %v", err)
	}
	if os.SameFile(fi, obj) {
		t.Fatal("the agent's script and the cache's object are one inode")
	}

	// The agent edits the script in place, and captures again.
	w, err := os.OpenFile(mine, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(two)
	w.Close()
	script(ref, two)
	if data, _ := cache.ReadAll(id1); !bytes.Equal(data, []byte(one)) {
		t.Fatalf("an edit in the workspace changed the object the first capture stored: it reads %q", data)
	}
	res2, err := b.Capture(ctx, ws, in)
	if err != nil {
		t.Fatal(err)
	}
	if want := wantID(); res2.Output != want {
		t.Fatalf("the second capture gave %s, the agent's tree is %s", res2.Output, want)
	}
	// The first capture's view still resolves to what it captured.
	r, err := view.FromID(res.Changeset, store, view.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if r.View().ID != res.Output {
		t.Fatalf("the first changeset now resolves to %s, not %s", r.View().ID, res.Output)
	}
	if data, ok := store.objs[id1]; !ok || !bytes.Equal(data, []byte(one)) {
		t.Fatalf("the store's copy of the first script reads %q", data)
	}
}
