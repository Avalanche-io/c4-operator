package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/absfs/absfs"
	"github.com/absfs/absnfs"
	"github.com/absfs/basefs"
	"github.com/absfs/memfs"
	"github.com/absfs/osfs"
	"github.com/absfs/switchfs"
	"github.com/absfs/unionfs"

	"github.com/Avalanche-io/c4-operator/internal/capture"
	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// ErrReservedNames means a view has names a union reads as its own
// markers (UnionReserved), so the NFS backend can't serve it; another
// backend can.
var ErrReservedNames = errors.New("the view has names a union can't carry")

// NFS serves every workspace from one NFSv3 export on this machine,
// mounted once at MountPoint, as the user: the export's root lists the
// workspaces, and each /<name>/ is a union of an upper of its own over its
// view's files. A new workspace is a route and an empty folder, whatever
// the tree's size; its target is a symlink into the one mount.
//
// The lower is the view's full layer (hard links into the object cache).
// It becomes a view read straight from the store, with nothing
// materialised, when the store reader can be used.
type NFS struct {
	Layers     *layer.Store
	State      string // uppers, in folders the backend names
	MountPoint string // where the export is mounted
	Port       int    // NFS and mount port, above 1023 (no root)

	mu      sync.Mutex
	host    absfs.SymlinkFileSystem
	root    absfs.FileSystem // the export's root listing
	sw      *switchfs.SwitchFS
	srv     *absnfs.Server
	names   map[string]string // target -> workspace name
	next    int
	started bool
}

// The index sidecar. A view keeps the developer's .git/index, whose stat
// data names the developer's file times; the layer serves a view's files
// at the time a null timestamp creates, so git would find every entry
// stat-dirty and re-hash the tree on a workspace's first status (31,651
// entries, 17 s on Kubernetes). The sidecar is that index refreshed once,
// per view and machine, against the times served here, in a read-only
// layer between a workspace's upper and the view. The view is unchanged.
// It is built in a scratch workspace that is then thrown away, so the
// refresh never appears as a change in an agent's capture; its file time
// is when it was built, later than every entry, as git's racy check needs.
func (n *NFS) indexSidecar(v *view.View, lower absfs.FileSystem) (absfs.FileSystem, error) {
	hasIndex := false
	for _, f := range v.Files {
		if f.Path == ".git/index" {
			hasIndex = true
			break
		}
	}
	if !hasIndex {
		return nil, nil
	}
	dir := filepath.Join(n.State, "index", v.ID.String())
	if _, err := os.Stat(filepath.Join(dir, ".git", "index")); err != nil {
		scratch, err := os.MkdirTemp(n.State, "index-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(scratch)
		up, err := basefs.NewFS(n.host, scratch)
		if err != nil {
			return nil, err
		}
		n.next++
		name := "/s" + strconv.Itoa(n.next)
		if err := n.root.Mkdir(name, 0o755); err != nil {
			return nil, err
		}
		u := unionfs.New(unionfs.WithWritableLayer(up), unionfs.WithReadOnlyLayer(lower))
		if err := n.sw.AddRoute(name, u, switchfs.WithRewriter(switchfs.StripPrefix(name))); err != nil {
			n.root.Remove(name)
			return nil, err
		}
		cmd := exec.Command("git", "update-index", "-q", "--refresh")
		cmd.Dir = filepath.Join(n.MountPoint, name)
		cmd.Env = GitEnv(os.Environ())
		cmd.Run() // exits 1 when an entry needs updating; the index is refreshed either way
		n.sw.RemoveRoute(name, switchfs.PatternPrefix)
		n.root.Remove(name)
		refreshed, err := os.ReadFile(filepath.Join(scratch, ".git", "index"))
		if err != nil {
			return nil, nil // git found nothing to refresh: the view's own index serves
		}
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			return nil, err
		}
		tmp := filepath.Join(dir, ".git", "index.tmp")
		if err := os.WriteFile(tmp, refreshed, 0o644); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, filepath.Join(dir, ".git", "index")); err != nil {
			return nil, err
		}
	}
	return basefs.NewFS(n.host, dir)
}

// MountOptions are the macOS client's options for the export (NFSv3):
// locks kept on the client (go build locks go.mod), no reserved source
// port (no root), no access-time writes (each would copy a file up), and
// no Finder or Spotlight on the mount.
const MountOptions = "vers=3,tcp,locallocks,noresvport,noatime,nobrowse"

// The server listens on the IPv4 loopback only, named here rather than
// left to the library's default. That keeps it off the network; it doesn't
// keep out other processes or users on this machine, which can speak NFS
// to it and reach every workspace it serves.
const loopback = "127.0.0.1"

func serverOptions(port int) absnfs.ServerOptions {
	return absnfs.ServerOptions{Hostname: loopback, Port: port, MountPort: port, UseRecordMarking: true}
}

func (n *NFS) Name() string { return "nfs" }

// Start serves the export and mounts it. Close undoes both.
func (n *NFS) Start() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return nil
	}
	host, err := osfs.NewFS()
	if err != nil {
		return err
	}
	root, err := memfs.NewFS()
	if err != nil {
		return err
	}
	sw, err := switchfs.New(switchfs.WithDefault(root))
	if err != nil {
		return err
	}
	handler, err := absnfs.New(sw, absnfs.ExportOptions{AllowAnySymlinkTarget: true})
	if err != nil {
		return err
	}
	srv, err := absnfs.NewServer(serverOptions(n.Port))
	if err != nil {
		return err
	}
	srv.SetHandler(handler)
	if err := srv.Listen(); err != nil {
		return fmt.Errorf("nfs: listen on %d: %w", n.Port, err)
	}
	if err := os.MkdirAll(n.MountPoint, 0o755); err != nil {
		srv.Stop()
		return err
	}
	port := strconv.Itoa(n.Port)
	opts := MountOptions + ",port=" + port + ",mountport=" + port
	if out, err := exec.Command("mount_nfs", "-o", opts, loopback+":/", n.MountPoint).CombinedOutput(); err != nil {
		srv.Stop()
		return fmt.Errorf("nfs: mount_nfs: %v: %s", err, out)
	}
	n.host, n.root, n.sw, n.srv = host, root, sw, srv
	n.names = map[string]string{}
	n.started = true
	return nil
}

// Close unmounts the export and stops serving it.
func (n *NFS) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.started {
		return nil
	}
	var err error
	for i := 0; i < 20; i++ {
		if err = exec.Command("umount", n.MountPoint).Run(); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	n.srv.Stop()
	n.started = false
	return err
}

func (n *NFS) Mount(_ context.Context, v *view.View, target string) (*Workspace, error) {
	if r := UnionReserved(v); len(r) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrReservedNames, r[0])
	}
	m, err := n.Layers.BuildFull(v)
	if err != nil {
		return nil, fmt.Errorf("nfs workspace of %s: %w", v.ID, err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.started {
		return nil, errors.New("nfs: not started")
	}
	lower, err := basefs.NewFS(n.host, n.Layers.Dir(m.Key))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(n.State, 0o755); err != nil {
		return nil, err
	}
	upperDir, err := os.MkdirTemp(n.State, "ws-")
	if err != nil {
		return nil, err
	}
	upper, err := basefs.NewFS(n.host, upperDir)
	if err != nil {
		os.RemoveAll(upperDir)
		return nil, err
	}
	// The union is also what keeps a workspace's paths inside it. basefs
	// follows a symlink already in its folder out of it, and checks a
	// symlink's target only where the symlink is made, so a view's own
	// symlink, or one an agent makes and then moves, can point out.
	// unionfs resolves a path one component at a time and won't go through
	// a symlink, so the export never reaches the far side. Don't serve a
	// basefs directly, or put anything that resolves whole paths above the
	// layers, until basefs refuses on its own; TestNFSSymlinkStaysInTheWorkspace
	// guards this.
	layers := []unionfs.Option{unionfs.WithWritableLayer(upper)}
	sidecar, err := n.indexSidecar(v, lower)
	if err != nil {
		os.RemoveAll(upperDir)
		return nil, fmt.Errorf("nfs workspace of %s: index sidecar: %w", v.ID, err)
	}
	if sidecar != nil {
		layers = append(layers, unionfs.WithReadOnlyLayer(sidecar))
	}
	layers = append(layers, unionfs.WithReadOnlyLayer(lower))
	n.next++
	name := "/w" + strconv.Itoa(n.next)
	u := unionfs.New(layers...)
	if err := n.root.Mkdir(name, 0o755); err != nil {
		os.RemoveAll(upperDir)
		return nil, err
	}
	if err := n.sw.AddRoute(name, u, switchfs.WithRewriter(switchfs.StripPrefix(name))); err != nil {
		n.root.Remove(name)
		os.RemoveAll(upperDir)
		return nil, err
	}
	if err := os.Symlink(filepath.Join(n.MountPoint, name), target); err != nil {
		n.sw.RemoveRoute(name, switchfs.PatternPrefix)
		n.root.Remove(name)
		os.RemoveAll(upperDir)
		return nil, err
	}
	n.names[target] = name
	return &Workspace{View: v, Target: target, Upper: upperDir, Env: GitEnv(nil)}, nil
}

// Capture reads the workspace's upper on disk, beside the mount, with the
// union's markers read against the view. The workspace stays mounted.
func (n *NFS) Capture(ctx context.Context, ws *Workspace, in capture.Input) (*capture.Result, error) {
	in.Parent, in.Upper, in.Walk = ws.View.ID, ws.Upper, WalkUnionOver(ws.View)
	return capture.Run(ctx, in)
}

func (n *NFS) Unmount(_ context.Context, ws *Workspace) error {
	n.mu.Lock()
	name, ok := n.names[ws.Target]
	delete(n.names, ws.Target)
	n.mu.Unlock()
	if ok {
		n.sw.RemoveRoute(name, switchfs.PatternPrefix)
		n.root.Remove(name)
	}
	if err := os.Remove(ws.Target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(ws.Upper)
}
