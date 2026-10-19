//go:build linux

package capture

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/overlay"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// These tests mount real overlays. They run as root on Linux with
// C4_PRIVILEGED_TESTS=1 (make test-linux runs them in a privileged
// container), and skip elsewhere.
func privileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("C4_PRIVILEGED_TESTS") == "" {
		t.Skip("needs root on Linux and C4_PRIVILEGED_TESTS=1")
	}
}

// memSink is a store in memory that also answers record reads.
type memSink struct {
	mu   sync.Mutex
	objs map[c4.ID][]byte
}

func (m *memSink) PutFile(_ context.Context, _ string, id c4.ID, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return m.PutBytes(context.Background(), "", id, data)
}

func (m *memSink) PutBytes(_ context.Context, _ string, id c4.ID, data []byte) error {
	if c4.Identify(bytes.NewReader(data)) != id {
		return errors.New("sink: bytes do not hash to the ID")
	}
	m.mu.Lock()
	m.objs[id] = append([]byte(nil), data...)
	m.mu.Unlock()
	return nil
}

func (m *memSink) Open(id c4.ID) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.objs[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
}

type fixture struct {
	cache  *objcache.Cache
	layers *layer.Store
	sink   *memSink
	base   *view.View
	root   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	set, err := objcache.OpenSet(filepath.Join(root, "cache"), objcache.Options{Mode: 0o666, ExecMode: 0o777, Budget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := set.Partition("p-show")
	src := filepath.Join(root, "src")
	write(t, src, map[string]string{
		"shots/sh010/plates/sh010.1001.exr": "plate 1001",
		"shots/sh010/plates/sh010.1002.exr": "plate 1002",
		"shots/sh010/plates/sh010.1003.exr": "plate 1003",
		"shots/sh010/notes.txt":             "notes",
		"shots/sh020/plates/sh020.1001.exr": "plate 1002",
		"tools/render.sh":                   "#!/bin/sh\necho render\n",
		"weights/denoise.bin":               "weights",
	})
	v, err := view.ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{cache: cache, sink: &memSink{objs: map[c4.ID][]byte{}}, base: v, root: root}
	for _, file := range v.Files {
		data, _ := os.ReadFile(filepath.Join(src, file.Path))
		cache.Put(file.ID, data)
	}
	for _, r := range v.Records() {
		cache.Put(r.ID, r.Data)
		f.sink.PutBytes(context.Background(), "", r.ID, r.Data)
	}
	f.layers, err = layer.Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) records() view.RecordSource {
	return view.RecordFunc(func(_ context.Context, id c4.ID) ([]byte, error) { return f.cache.ReadAll(id) })
}

func TestCaptureMatchesTheMountedTree(t *testing.T) {
	privileged(t)
	f := newFixture(t)
	full, err := f.layers.BuildFull(f.base)
	if err != nil {
		t.Fatal(err)
	}
	mnt := filepath.Join(f.root, "mnt")
	upper := filepath.Join(f.root, "vol", "upper")
	work := filepath.Join(f.root, "vol", "work")
	os.MkdirAll(upper, 0o777)
	os.MkdirAll(work, 0o700)
	os.Chmod(upper, 0o777)
	if err := overlay.Mount(mnt, []string{f.layers.Dir(full.Key)}, upper, work); err != nil {
		t.Fatal(err)
	}
	mounted := true
	defer func() {
		if mounted {
			overlay.Unmount(mnt)
		}
	}()

	// The merged view behaves like an emptyDir: writable by anyone.
	fi, _ := os.Stat(filepath.Join(mnt, "shots", "sh010"))
	if fi.Mode().Perm() != 0o777 {
		t.Fatalf("directory mode %v", fi.Mode().Perm())
	}
	if before, _ := view.ScanID(mnt); before != f.base.ID {
		t.Fatalf("mounted tree %s, view %s", before, f.base.ID)
	}

	// A render: new frames, a modified file, a deleted frame, a deleted
	// directory, a replaced directory, an exec bit, a symlink, and a file
	// rewritten with identical bytes (a copy-up that is not a change).
	m := func(p string) string { return filepath.Join(mnt, p) }
	os.MkdirAll(m("shots/sh010/render"), 0o777)
	os.WriteFile(m("shots/sh010/render/sh010.1001.exr"), []byte("rendered 1001"), 0o644)
	os.WriteFile(m("shots/sh010/render/sh010.1002.exr"), []byte("rendered 1002"), 0o644)
	os.WriteFile(m("shots/sh010/notes.txt"), []byte("notes, revised"), 0o644)
	os.Remove(m("shots/sh010/plates/sh010.1003.exr"))
	os.RemoveAll(m("shots/sh020"))
	os.MkdirAll(m("shots/sh020"), 0o777)
	os.WriteFile(m("shots/sh020/replacement.txt"), []byte("new sh020"), 0o644)
	os.WriteFile(m("weights/denoise.bin"), []byte("weights"), 0o644)
	os.Chmod(m("shots/sh010/plates/sh010.1001.exr"), 0o755)
	os.Symlink("../shots/sh010/render", m("tools/latest"))

	want, err := view.ScanDir(mnt)
	if err != nil {
		t.Fatal(err)
	}
	// The unit's control folder (its progress line) is written after the
	// tree is taken, and must not reach the output.
	os.MkdirAll(m(".c4"), 0o777)
	os.WriteFile(m(".c4/progress"), []byte("0.5\n"), 0o644)
	if err := overlay.Unmount(mnt); err != nil {
		t.Fatal(err)
	}
	mounted = false

	res, err := Run(context.Background(), Input{
		Project: "show", Parent: f.base.ID, Upper: upper,
		Cache: f.cache, Records: f.records(), Sink: f.sink, Workers: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != want.ID {
		t.Fatalf("captured %s, the mounted tree was %s", res.Output, want.ID)
	}
	if res.Reused != 1 {
		t.Fatalf("rewritten-but-unchanged files: %d, want 1", res.Reused)
	}

	// The changeset resolves, through the store alone, to the same view,
	// with the base as its parent.
	r, err := view.FromID(res.Changeset, f.sink, view.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if r.View().ID != want.ID || r.Chain[0].ID != f.base.ID {
		t.Fatalf("changeset resolves to %s", r.View().ID)
	}

	// Running capture again writes nothing new and gives the same view.
	again, err := Run(context.Background(), Input{
		Project: "show", Parent: f.base.ID, Upper: upper,
		Cache: f.cache, Records: f.records(), Sink: f.sink,
	})
	if err != nil || again.Output != res.Output || again.Changeset != res.Changeset {
		t.Fatalf("capture is not idempotent: %v", err)
	}

	// The output mounts as a delta over the base layer and shows exactly
	// the captured tree: whiteouts, the opaque directory and the new files.
	changes, _, err := view.Diff(context.Background(), f.base.ID, res.Output, f.records())
	if err != nil {
		t.Fatal(err)
	}
	delta, err := f.layers.BuildDelta(f.base.ID, res.Output, changes)
	if err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(f.root, "ro")
	if err := overlay.Mount(ro, []string{f.layers.Dir(delta.Key), f.layers.Dir(full.Key)}, "", ""); err != nil {
		t.Fatal(err)
	}
	defer overlay.Unmount(ro)
	got, err := view.ScanID(ro)
	if err != nil {
		t.Fatal(err)
	}
	if got != want.ID {
		t.Fatalf("delta stack shows %s, want %s", got, want.ID)
	}
	if err := os.WriteFile(filepath.Join(ro, "x"), []byte("x"), 0o644); err == nil {
		t.Fatal("a read-only stack accepted a write")
	}
}

func TestLayerObjectsAreSharedByInode(t *testing.T) {
	privileged(t)
	f := newFixture(t)
	full, err := f.layers.BuildFull(f.base)
	if err != nil {
		t.Fatal(err)
	}
	var mounts []string
	for i := range 3 {
		mnt := filepath.Join(f.root, "pod", string(rune('a'+i)))
		upper := filepath.Join(f.root, "vol"+string(rune('a'+i)), "upper")
		work := filepath.Join(f.root, "vol"+string(rune('a'+i)), "work")
		os.MkdirAll(upper, 0o777)
		os.MkdirAll(work, 0o700)
		if err := overlay.Mount(mnt, []string{f.layers.Dir(full.Key)}, upper, work); err != nil {
			t.Fatal(err)
		}
		mounts = append(mounts, mnt)
	}
	defer func() {
		for _, m := range mounts {
			overlay.Unmount(m)
		}
	}()
	// Three pods see one copy of each object: the layer's files are
	// hardlinks of the cache's.
	var id c4.ID
	for _, file := range f.base.Files {
		if file.Path == "shots/sh010/plates/sh010.1001.exr" {
			id = file.ID
		}
	}
	obj, _ := os.Stat(f.cache.Path(id))
	lay, _ := os.Stat(filepath.Join(f.layers.Dir(full.Key), "shots/sh010/plates/sh010.1001.exr"))
	if !os.SameFile(obj, lay) {
		t.Fatal("the layer copied an object instead of linking it")
	}
	// A write in one pod is invisible to the others and to the cache.
	if err := os.WriteFile(filepath.Join(mounts[0], "shots/sh010/plates/sh010.1001.exr"), []byte("scribble"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, m := range mounts[1:] {
		data, _ := os.ReadFile(filepath.Join(m, "shots/sh010/plates/sh010.1001.exr"))
		if string(data) != "plate 1001" {
			t.Fatalf("a write in one pod leaked into %s", m)
		}
	}
	data, _ := os.ReadFile(f.cache.Path(id))
	if string(data) != "plate 1001" {
		t.Fatal("a write through overlay reached the cached object")
	}
}
