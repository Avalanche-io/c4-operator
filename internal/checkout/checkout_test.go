package checkout

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/capture"
	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// memStore is a store in memory that checks every ID it is given.
type memStore struct {
	mu   sync.Mutex
	objs map[c4.ID][]byte
}

func (m *memStore) PutFile(ctx context.Context, p string, id c4.ID, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return m.PutBytes(ctx, p, id, data)
}

func (m *memStore) PutBytes(_ context.Context, _ string, id c4.ID, data []byte) error {
	if c4.Identify(bytes.NewReader(data)) != id {
		return errors.New("store: bytes do not hash to the ID")
	}
	m.mu.Lock()
	m.objs[id] = append([]byte(nil), data...)
	m.mu.Unlock()
	return nil
}

func (m *memStore) Open(id c4.ID) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.objs[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}

// The base every script starts from: nested directories, an executable,
// a symlink, a file in two places, and an empty directory.
func writeBase(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"README.md":         "readme\n",
		"go.mod":            "module example.com/w\n",
		"cmd/w/main.go":     "package main\n\nfunc main() {}\n",
		"internal/a/a.go":   "package a\n",
		"internal/a/b.go":   "package a // b\n",
		"internal/old/x.go": "package old\n",
		"internal/old/y.go": "package old // y\n",
		"docs/same.txt":     "same\n",
		"docs/copy.txt":     "same\n",
		"bin/build.sh":      "#!/bin/sh\ngo build ./...\n",
	}
	for p, content := range files {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(dir, "empty"), 0o755)
	if err := os.Symlink("cmd/w/main.go", filepath.Join(dir, "main.go")); err != nil {
		t.Fatal(err)
	}
}

// copyTree copies src to dst with plain reads and writes: the reference
// a workspace is checked against, made with no backend at all.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(to, 0o755)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Each script is what an agent might do to its workspace, written against
// the workspace's root.
var scripts = []struct {
	name   string
	change bool
	run    func(root string) error
}{
	{"nothing", false, func(string) error { return nil }},
	{"add a file in a new directory", true, func(r string) error {
		os.MkdirAll(filepath.Join(r, "internal/n"), 0o755)
		return os.WriteFile(filepath.Join(r, "internal/n/n.go"), []byte("package n\n"), 0o644)
	}},
	{"modify a file to a new size", true, func(r string) error {
		return os.WriteFile(filepath.Join(r, "internal/a/a.go"), []byte("package a\n\nvar X = 1\n"), 0o644)
	}},
	{"modify a file, same size", true, func(r string) error {
		return os.WriteFile(filepath.Join(r, "README.md"), []byte("README\n"), 0o644)
	}},
	{"rewrite a file unchanged", false, func(r string) error {
		return os.WriteFile(filepath.Join(r, "go.mod"), []byte("module example.com/w\n"), 0o644)
	}},
	{"touch a file", false, func(r string) error {
		now := time.Now()
		return os.Chtimes(filepath.Join(r, "docs/same.txt"), now, now)
	}},
	{"delete a file", true, func(r string) error { return os.Remove(filepath.Join(r, "internal/a/b.go")) }},
	{"delete a directory of the view", true, func(r string) error { return os.RemoveAll(filepath.Join(r, "internal/old")) }},
	{"replace a directory with a file", true, func(r string) error {
		os.RemoveAll(filepath.Join(r, "internal/old"))
		return os.WriteFile(filepath.Join(r, "internal/old"), []byte("was a directory\n"), 0o644)
	}},
	{"replace a directory with a new one", true, func(r string) error {
		os.RemoveAll(filepath.Join(r, "internal/old"))
		os.MkdirAll(filepath.Join(r, "internal/old"), 0o755)
		return os.WriteFile(filepath.Join(r, "internal/old/z.go"), []byte("package old // z\n"), 0o644)
	}},
	{"replace a file with a directory", true, func(r string) error {
		os.Remove(filepath.Join(r, "docs/copy.txt"))
		os.MkdirAll(filepath.Join(r, "docs/copy.txt"), 0o755)
		return os.WriteFile(filepath.Join(r, "docs/copy.txt/inside"), []byte("inside\n"), 0o644)
	}},
	{"add a symlink", true, func(r string) error { return os.Symlink("../go.mod", filepath.Join(r, "docs/mod")) }},
	{"retarget a symlink", true, func(r string) error {
		os.Remove(filepath.Join(r, "main.go"))
		return os.Symlink("internal/a/a.go", filepath.Join(r, "main.go"))
	}},
	{"delete a symlink", true, func(r string) error { return os.Remove(filepath.Join(r, "main.go")) }},
	{"make a file executable", true, func(r string) error { return os.Chmod(filepath.Join(r, "go.mod"), 0o755) }},
	{"add an empty directory", true, func(r string) error { return os.MkdirAll(filepath.Join(r, "tmp/cache"), 0o755) }},
	{"delete the empty directory", true, func(r string) error { return os.Remove(filepath.Join(r, "empty")) }},
	{"write only the control folder", false, func(r string) error {
		os.MkdirAll(filepath.Join(r, ".c4"), 0o755)
		return os.WriteFile(filepath.Join(r, ".c4/progress"), []byte("0.5\n"), 0o644)
	}},
}

// Every script, on a clone workspace, captures to exactly the view of the
// same script run on a plain copy, and its changeset resolves to that view
// over the base through the store alone.
func TestCloneConformance(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("clone workspaces need APFS")
	}
	conformance(t, func(l *layer.Store) Backend { return &Clone{Layers: l} })
}

// conformance runs every script on workspaces from one backend and checks
// each against the same script on a plain copy.
func conformance(t *testing.T, backend func(*layer.Store) Backend) {
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
	store := &memStore{objs: map[c4.ID][]byte{}}
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
	b := backend(layers)
	in := capture.Input{
		Project: "show", Cache: cache, Sink: store, Workers: 2,
		Records: view.RecordFunc(func(_ context.Context, id c4.ID) ([]byte, error) { return cache.ReadAll(id) }),
	}
	ctx := context.Background()

	for i, s := range scripts {
		t.Run(s.name, func(t *testing.T) {
			target := filepath.Join(root, "ws", strings.ReplaceAll(s.name, " ", "-"))
			os.MkdirAll(filepath.Dir(target), 0o755)
			ws, err := b.Mount(ctx, v, target)
			if err != nil {
				t.Fatal(err)
			}
			// A target may be a symlink into a mount (the NFS backend).
			dir, err := filepath.EvalSymlinks(ws.Target)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := view.ScanID(dir); err != nil || got != v.ID {
				t.Fatalf("a new workspace is %s, the view %s (%v)", got, v.ID, err)
			}
			if _, isClone := b.(*Clone); i == 0 && isClone {
				// Nothing changed: nothing is reported, so nothing is hashed.
				if entries, err := WalkClone(ws.Target, v); err != nil || len(entries) != 0 {
					t.Fatalf("an untouched clone reports %d entries (%v)", len(entries), err)
				}
			}
			ref := filepath.Join(root, "ref", strings.ReplaceAll(s.name, " ", "-"))
			copyTree(t, src, ref)
			if err := s.run(ref); err != nil {
				t.Fatal(err)
			}
			if err := s.run(ws.Target); err != nil {
				t.Fatal(err)
			}
			os.RemoveAll(filepath.Join(ref, ".c4"))
			want, err := view.ScanDir(ref)
			if err != nil {
				t.Fatal(err)
			}
			res, err := b.Capture(ctx, ws, in)
			if err != nil {
				t.Fatal(err)
			}
			if res.Output != want.ID {
				t.Fatalf("captured %s, the agent's tree is %s", res.Output, want.ID)
			}
			if s.change != (res.Changeset != c4.ID{}) {
				t.Fatalf("change %v, changeset %v", s.change, res.Changeset)
			}
			if s.change {
				r, err := view.FromID(res.Changeset, store, view.Filter{})
				if err != nil {
					t.Fatal(err)
				}
				if r.View().ID != want.ID || r.Chain[0].ID != v.ID {
					t.Fatalf("changeset resolves to %s over %s", r.View().ID, r.Chain[0].ID)
				}
			}
			if err := b.Unmount(ctx, ws); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("workspace left behind: %v", err)
			}
		})
	}
	// The layer the clones came from is untouched by all of it.
	if got, err := view.ScanID(layers.Dir(layer.FullKey(v.ID))); err != nil || got != v.ID {
		t.Fatalf("the layer changed: %s (%v)", got, err)
	}
}

// A workspace's git settings travel in its environment, after any the
// caller already passes the same way.
func TestGitEnv(t *testing.T) {
	got := GitEnv([]string{"HOME=/h", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=agent"})
	want := []string{
		"HOME=/h", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=agent",
		"GIT_CONFIG_KEY_1=core.checkStat", "GIT_CONFIG_VALUE_1=minimal",
		"GIT_CONFIG_KEY_2=core.trustctime", "GIT_CONFIG_VALUE_2=false",
		"GIT_CONFIG_KEY_3=gc.auto", "GIT_CONFIG_VALUE_3=0",
		"GIT_CONFIG_COUNT=4",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
