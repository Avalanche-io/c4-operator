package checkout

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/capture"
	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// The same scripts, on workspaces served over a real NFS loopback mount.
// They mount, so they run only on macOS with C4_NFS_TESTS=1.
func TestNFSConformance(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("C4_NFS_TESTS") == "" {
		t.Skip("needs macOS and C4_NFS_TESTS=1 (it mounts an NFS export)")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	dir := t.TempDir()
	conformance(t, func(ls *layer.Store) Backend {
		n := &NFS{Layers: ls, State: filepath.Join(dir, "uppers"), MountPoint: filepath.Join(dir, "mnt"), Port: port}
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := n.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		})
		return n
	})
}

// A view of a git repository: the workspace's index is the sidecar,
// refreshed against the times the backend serves, so git finds no
// stat-dirty entry and re-hashes nothing; and the sidecar is never part
// of what the workspace captures.
func TestNFSIndexSidecar(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("C4_NFS_TESTS") == "" {
		t.Skip("needs macOS and C4_NFS_TESTS=1 (it mounts an NFS export)")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	writeBase(t, src)
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(GitEnv(os.Environ()), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@studio.example", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@studio.example")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git(src, "init", "-q")
	git(src, "add", "-A")
	git(src, "commit", "-q", "-m", "base")
	v, err := view.ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	set, _ := objcache.OpenSet(filepath.Join(root, "cache"), objcache.Options{Mode: 0o644, ExecMode: 0o755, Budget: 1 << 30})
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
	layers, _ := layer.Open(cache)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	n := &NFS{Layers: layers, State: filepath.Join(root, "uppers"), MountPoint: filepath.Join(root, "mnt"), Port: port}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		ws, err := n.Mount(ctx, v, filepath.Join(root, "ws"+string(rune('0'+i))))
		if err != nil {
			t.Fatal(err)
		}
		if dirty := git(ws.Target, "diff-files", "--name-only"); dirty != "" {
			t.Fatalf("workspace %d: stat-dirty entries before any refresh:\n%s", i, dirty)
		}
		res, err := n.Capture(ctx, ws, capture.Input{Project: "show", Cache: cache, Sink: store, Workers: 2,
			Records: view.RecordFunc(func(_ context.Context, id c4.ID) ([]byte, error) { return cache.ReadAll(id) })})
		if err != nil {
			t.Fatal(err)
		}
		if res.Output != v.ID || res.Changeset != (c4.ID{}) {
			t.Fatalf("workspace %d: an untouched workspace captured %s (changeset %s)", i, res.Output, res.Changeset)
		}
		if err := n.Unmount(ctx, ws); err != nil {
			t.Fatal(err)
		}
	}
}
