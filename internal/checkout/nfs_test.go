package checkout

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/layer"
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
