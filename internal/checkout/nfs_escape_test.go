package checkout

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/absfs/absfs"
	"github.com/absfs/basefs"
	"golang.org/x/sys/unix"

	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// A symlink in a workspace can point out of it. The client resolves
// symlinks itself, so an ordinary path through one never reaches the
// server; but a client holding a handle on a directory that is then
// replaced by such a symlink asks the server to look a name up under that
// handle, and the server must not follow the symlink out of the workspace.
//
// Targets are relative and chosen so the two sides resolve them to
// different places: on the server, to a secret beside the workspace's
// folders; on the client, to nothing. Reading the secret therefore means
// the server followed the symlink.
func TestNFSSymlinkStaysInTheWorkspace(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("C4_NFS_TESTS") == "" {
		t.Skip("needs macOS and C4_NFS_TESTS=1 (it mounts an NFS export)")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	must(t, os.MkdirAll(filepath.Join(src, "d"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "d", "x.txt"), []byte("x\n"), 0o644))
	// The lower is <root>/cache/.../layers/<key>; this reaches <root>/cache/<…>/escape-probe on the server.
	must(t, os.Symlink("../escape-probe", filepath.Join(src, "out")))
	must(t, os.Symlink("d/x.txt", filepath.Join(src, "ok")))
	v, err := view.ScanDir(src)
	must(t, err)
	set, err := objcache.OpenSet(filepath.Join(root, "cache"), objcache.Options{Mode: 0o644, ExecMode: 0o755, Budget: 1 << 30})
	must(t, err)
	cache, _ := set.Partition("p-show")
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
	}
	layers, err := layer.Open(cache)
	must(t, err)
	m, err := layers.BuildFull(v)
	must(t, err)
	secret := func(dir string) {
		must(t, os.MkdirAll(dir, 0o755))
		must(t, os.WriteFile(filepath.Join(dir, "secret"), []byte("outside the workspace\n"), 0o644))
	}
	secret(filepath.Join(filepath.Dir(layers.Dir(m.Key)), "escape-probe"))
	// Uppers are <root>/s/uppers/ws-*, one level deeper than the mount's
	// <root>/mnt/wN: "../../escape-probe" from an upper's root is
	// <root>/s/escape-probe on the server and <root>/escape-probe, which
	// doesn't exist, on the client.
	secret(filepath.Join(root, "s", "escape-probe"))

	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	n := &NFS{Layers: layers, State: filepath.Join(root, "s", "uppers"), MountPoint: filepath.Join(root, "mnt"), Port: port}
	must(t, n.Start())
	defer n.Close()
	ctx := context.Background()

	// serverRefuses asks the export's own filesystem, the one every NFS
	// procedure resolves through, for the secret under p in a workspace.
	// Whatever handle a client holds, the server reaches only what this
	// returns, so it covers every procedure without a raw client.
	serverRefuses := func(t *testing.T, ws *Workspace, p string) {
		t.Helper()
		full := n.names[ws.Target].name + p
		if data, err := absfsRead(n, full); err == nil {
			t.Errorf("the server's filesystem read %q at %s, outside the workspace", data, full)
		}
	}

	// swap holds a handle on the directory d, replaces d with the symlink
	// at from, and reads "secret" under the held handle, as a real client.
	swap := func(t *testing.T, ws *Workspace, from string) {
		t.Helper()
		dir := ws.Target
		if data, err := os.ReadFile(filepath.Join(dir, from, "secret")); err == nil {
			t.Fatalf("an ordinary path through the symlink read %q", data)
		}
		d, err := os.Open(filepath.Join(dir, "d"))
		must(t, err)
		defer d.Close()
		must(t, os.RemoveAll(filepath.Join(dir, "d")))
		if err := os.Rename(filepath.Join(dir, from), filepath.Join(dir, "d")); err != nil {
			t.Logf("refused at the move: %v", err)
			return
		}
		// Aimed right: followed on the host, the moved symlink reaches the secret.
		if _, err := os.ReadFile(filepath.Join(ws.Upper, "d", "secret")); err != nil {
			t.Fatalf("the probe is mis-aimed: %v", err)
		}
		serverRefuses(t, ws, "/d/secret")
		fd, err := unix.Openat(int(d.Fd()), "secret", unix.O_RDONLY, 0)
		if err != nil {
			t.Logf("refused at the lookup: %v", err)
			return
		}
		f := os.NewFile(uintptr(fd), "secret")
		defer f.Close()
		buf := make([]byte, 64)
		k, _ := f.Read(buf)
		t.Fatalf("a held directory handle, replaced by a symlink, read %q from outside the workspace", buf[:k])
	}

	// A symlink in the view (the lower) moves only by being copied up,
	// which makes it anew in the upper, where an escaping target is refused.
	t.Run("the view's own symlink", func(t *testing.T) {
		ws, err := n.Mount(ctx, v, filepath.Join(root, "ws1"))
		must(t, err)
		defer n.Unmount(ctx, ws)
		// Every layer is confined to its folder on this host.
		for i, fs := range n.names[ws.Target].fss {
			if !basefs.Confined(fs.(absfs.FileSystem)) {
				t.Errorf("layer %d isn't confined to its folder", i)
			}
		}
		// Aimed right: followed on the host, the symlink reaches the secret.
		if _, err := os.ReadFile(filepath.Join(layers.Dir(m.Key), "out", "secret")); err != nil {
			t.Fatalf("the probe is mis-aimed: %v", err)
		}
		serverRefuses(t, ws, "/out/secret")
		// The control: a view's symlink that stays inside moves.
		must(t, os.Rename(filepath.Join(ws.Target, "ok"), filepath.Join(ws.Target, "ok2")))
		swap(t, ws, "out")
	})

	// A symlink's target is checked where it is made, not where it is
	// moved to, so an agent can leave an escaping symlink in its upper;
	// the union still doesn't resolve a path through it.
	t.Run("a symlink made inside the root, then moved", func(t *testing.T) {
		ws, err := n.Mount(ctx, v, filepath.Join(root, "ws2"))
		must(t, err)
		defer n.Unmount(ctx, ws)
		deep := filepath.Join(ws.Target, "a", "b")
		must(t, os.MkdirAll(deep, 0o755))
		// From /a/b this is /escape-probe, inside the root, so it may be made.
		if err := os.Symlink("../../escape-probe", filepath.Join(deep, "e")); err != nil {
			t.Logf("refused at the symlink: %v", err)
			return
		}
		swap(t, ws, "a/b/e")
	})
}

func absfsRead(n *NFS, p string) ([]byte, error) {
	f, err := n.sw.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, 64)
	k, err := f.Read(buf)
	return buf[:k], err
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
