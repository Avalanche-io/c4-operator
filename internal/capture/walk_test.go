package capture

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/overlay"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// storeSink is a store in memory that checks every ID it is given.
type storeSink struct {
	mu   sync.Mutex
	objs map[c4.ID][]byte
}

func (s *storeSink) PutFile(ctx context.Context, p string, id c4.ID, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return s.PutBytes(ctx, p, id, data)
}

func (s *storeSink) PutBytes(_ context.Context, _ string, id c4.ID, data []byte) error {
	if c4.Identify(bytes.NewReader(data)) != id {
		return errors.New("sink: bytes do not hash to the ID")
	}
	s.mu.Lock()
	s.objs[id] = append([]byte(nil), data...)
	s.mu.Unlock()
	return nil
}

func (s *storeSink) Open(id c4.ID) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.objs[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A workspace that keeps its writes in some other form than overlayfs
// captures through its own Walk, and the result is the tree the agent
// left, as if it had been scanned. This runs on every platform: no mount.
func TestCaptureThroughAnyWalk(t *testing.T) {
	root := t.TempDir()
	set, err := objcache.OpenSet(filepath.Join(root, "cache"), objcache.Options{Mode: 0o666, ExecMode: 0o777, Budget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := set.Partition("p-show")
	sink := &storeSink{objs: map[c4.ID][]byte{}}
	base := map[string]string{
		"src/main.go":   "package main\n",
		"src/util.go":   "package main // util\n",
		"docs/a.md":     "a",
		"docs/old/b.md": "b",
		"docs/old/c.md": "c",
		"README.md":     "readme",
	}
	src := filepath.Join(root, "src")
	writeTree(t, src, base)
	v, err := view.ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Files {
		data, _ := os.ReadFile(filepath.Join(src, f.Path))
		cache.Put(f.ID, data)
	}
	recs, rerr := v.Records()
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, r := range recs {
		cache.Put(r.ID, r.Data)
		sink.PutBytes(context.Background(), "", r.ID, r.Data)
	}

	// What the agent left: one file changed, one added, one deleted, a
	// directory replaced by a new one, a file rewritten unchanged, and
	// its control folder.
	want := map[string]string{
		"src/main.go":   "package main\n\nfunc main() {}\n",
		"src/util.go":   "package main // util\n",
		"src/new.go":    "package main // new\n",
		"docs/old/d.md": "d",
		"README.md":     "readme",
	}
	wantDir := filepath.Join(root, "want")
	writeTree(t, wantDir, want)
	wantView, err := view.ScanDir(wantDir)
	if err != nil {
		t.Fatal(err)
	}

	// The upper holds only what was written, in no particular format;
	// this Walk reports it as entries.
	upper := filepath.Join(root, "upper")
	writeTree(t, upper, map[string]string{
		"src/main.go":   want["src/main.go"],
		"src/util.go":   want["src/util.go"],
		"src/new.go":    want["src/new.go"],
		"docs/old/d.md": "d",
		".c4/progress":  "0.5\n",
	})
	file := func(p string) overlay.Entry {
		fi, err := os.Stat(filepath.Join(upper, p))
		if err != nil {
			t.Fatal(err)
		}
		return overlay.Entry{Path: p, Full: filepath.Join(upper, p), Kind: overlay.File, Size: fi.Size()}
	}
	walk := func(dir string) ([]overlay.Entry, error) {
		if dir != upper {
			t.Fatalf("walked %s, not the upper", dir)
		}
		return []overlay.Entry{
			{Path: ".c4/", Kind: overlay.Dir},
			file(".c4/progress"),
			{Path: "docs/a.md", Kind: overlay.Whiteout},
			{Path: "docs/old/", Kind: overlay.Dir, Opaque: true},
			file("docs/old/d.md"),
			{Path: "src/", Kind: overlay.Dir},
			file("src/main.go"),
			file("src/new.go"),
			file("src/util.go"),
		}, nil
	}

	res, err := Run(context.Background(), Input{
		Project: "show", Parent: v.ID, Upper: upper, Walk: walk,
		Cache: cache, Sink: sink, Workers: 2,
		Records: view.RecordFunc(func(_ context.Context, id c4.ID) ([]byte, error) { return cache.ReadAll(id) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != wantView.ID {
		t.Fatalf("captured %s, the agent's tree is %s", res.Output, wantView.ID)
	}
	if res.Reused != 1 {
		t.Fatalf("rewritten-but-unchanged files: %d, want 1", res.Reused)
	}
	r, err := view.FromID(res.Changeset, sink, view.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if r.View().ID != wantView.ID || r.Chain[0].ID != v.ID {
		t.Fatalf("changeset resolves to %s over %s", r.View().ID, r.Chain[0].ID)
	}
}

// With no Walk, capture reads an overlayfs upper, as the node daemon's
// volumes have always had: off Linux that is refused, not guessed at.
func TestCaptureWithoutWalkReadsOverlay(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("the overlay path is covered by the privileged tests")
	}
	_, err := Run(context.Background(), Input{Upper: t.TempDir()})
	if err == nil {
		t.Fatal("captured an overlay upper off Linux")
	}
}

// A change of the exec bit alone, whose bytes stayed in a lower, is
// captured from the parent's own C4 ID: nothing is hashed or uploaded, and
// it works when this machine's cache never held those bytes.
func TestMetadataOnlyChangeNeedsNoBytes(t *testing.T) {
	root := t.TempDir()
	set, err := objcache.OpenSet(filepath.Join(root, "cache"), objcache.Options{Budget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := set.Partition("p-show")
	sink := &storeSink{objs: map[c4.ID][]byte{}}
	src := filepath.Join(root, "src")
	writeTree(t, src, map[string]string{"tool.sh": "#!/bin/sh\n", "README.md": "readme"})
	v, err := view.ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	// The content is in the store only; the records are readable.
	for _, f := range v.Files {
		data, _ := os.ReadFile(filepath.Join(src, f.Path))
		sink.PutBytes(context.Background(), "", f.ID, data)
	}
	recs, rerr := v.Records()
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, r := range recs {
		cache.Put(r.ID, r.Data)
	}
	var tool view.File
	for _, f := range v.Files {
		if f.Path == "tool.sh" {
			tool = f
		}
	}
	os.Chmod(filepath.Join(src, "tool.sh"), 0o755)
	want, _ := view.ScanDir(src)
	res, err := Run(context.Background(), Input{
		Project: "show", Parent: v.ID, Upper: t.TempDir(), Cache: cache, Sink: sink, Workers: 1,
		Walk: func(string) ([]overlay.Entry, error) {
			return []overlay.Entry{{Path: "tool.sh", Kind: overlay.File, Size: tool.Size, Exec: true, ID: tool.ID}}, nil
		},
		Records: view.RecordFunc(func(_ context.Context, id c4.ID) ([]byte, error) { return cache.ReadAll(id) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != want.ID || res.Modified != 1 || res.Hashed != 0 || res.Uploaded != 0 || res.Reused != 0 {
		t.Fatalf("output %s (want %s), modified %d, hashed %d, uploaded %d, reused %d", res.Output, want.ID, res.Modified, res.Hashed, res.Uploaded, res.Reused)
	}
}
