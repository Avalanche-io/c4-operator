package tier

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/Avalanche-io/c4-operator/"

// dataPlane is the storage code the operator's farm sits on: the content
// API and its cache, stores and hashing, views, layers, overlay and
// capture. It is meant to move into its own module one day (the storage
// project's stage 3), so it must stay free of the farm and of Kubernetes.
var dataPlane = []string{
	"internal/tier",
	"internal/objcache",
	"internal/fill",
	"internal/storeurl",
	"internal/rendezvous",
	"internal/view",
	"internal/layer",
	"internal/overlay",
	"internal/capture",
}

// The data plane may reach only itself and these packages of this module.
// membership is the one piece that knows Kubernetes, and only the node
// daemon imports it; no data-plane package may.
var dataPlaneMayImport = []string{
	"internal/metrics",
}

// Third-party packages the data plane must never reach.
var dataPlaneNever = []string{
	"k8s.io/",
	"sigs.k8s.io/", // controller-runtime among them
}

// Every package of the data plane, on Linux and on macOS (overlay and
// layer are Linux-only), reaches only the data plane and its allowed
// packages of this module, and nothing of Kubernetes.
func TestDataPlaneStaysApartFromTheFarm(t *testing.T) {
	allowed, inDataPlane := map[string]bool{}, map[string]bool{}
	for _, p := range dataPlane {
		inDataPlane[module+p] = true
	}
	for _, p := range append(dataPlane, dataPlaneMayImport...) {
		allowed[module+p] = true
	}
	for _, goos := range []string{"linux", "darwin"} {
		args := []string{"list", "-deps", "-f", "{{.ImportPath}} {{join .Imports \" \"}}"}
		for _, p := range dataPlane {
			args = append(args, "../../"+p)
		}
		cmd := exec.Command("go", args...)
		cmd.Env = append(os.Environ(), "GOOS="+goos)
		out, err := cmd.Output()
		if err != nil {
			t.Skipf("go list (GOOS=%s): %v", goos, err)
		}
		reached := 0
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			pkg, imports, _ := strings.Cut(line, " ")
			// Only the edges leaving the data plane: a forbidden package is
			// named where the data plane first imports it, not again for
			// every package beneath it.
			if !inDataPlane[pkg] {
				continue
			}
			reached++
			for _, imp := range strings.Fields(imports) {
				if forbidden(imp, allowed) {
					t.Errorf("GOOS=%s: %s imports %s", goos, pkg, imp)
				}
			}
		}
		if reached != len(dataPlane) {
			t.Fatalf("GOOS=%s: go list listed %d of the data plane's %d packages", goos, reached, len(dataPlane))
		}
	}
}

func forbidden(imp string, allowed map[string]bool) bool {
	if strings.HasPrefix(imp, module) {
		return !allowed[imp]
	}
	for _, p := range dataPlaneNever {
		if strings.HasPrefix(imp, p) {
			return true
		}
	}
	return false
}
