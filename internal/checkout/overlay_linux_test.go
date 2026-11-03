//go:build linux

package checkout

import (
	"os"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/layer"
)

// The same scripts, on overlay workspaces. They mount real overlays, so
// they run as root on Linux with C4_PRIVILEGED_TESTS=1 (make test-linux
// runs them in a privileged container), and skip elsewhere.
func TestOverlayConformance(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("C4_PRIVILEGED_TESTS") == "" {
		t.Skip("needs root on Linux and C4_PRIVILEGED_TESTS=1")
	}
	state := t.TempDir()
	conformance(t, func(l *layer.Store) Backend { return &Overlay{Layers: l, State: state} })
}
