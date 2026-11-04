package checkout

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// The mount says whether it folds case as the host folder under the
// workspaces does (pathconf _PC_CASE_SENSITIVE, 11 on darwin).
func TestNFSReportsTheHostsFolding(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("C4_NFS_TESTS") == "" {
		t.Skip("needs macOS and C4_NFS_TESTS=1 (it mounts an NFS export)")
	}
	root := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	n := &NFS{State: filepath.Join(root, "uppers"), MountPoint: filepath.Join(root, "mnt"), Port: port}
	must(t, n.Start())
	defer n.Close()
	host, err := unix.Pathconf(n.State, 11)
	must(t, err)
	mount, err := unix.Pathconf(n.MountPoint, 11)
	must(t, err)
	if host != mount {
		t.Fatalf("the host folder says case-sensitive=%d, the mount %d", host, mount)
	}
}
