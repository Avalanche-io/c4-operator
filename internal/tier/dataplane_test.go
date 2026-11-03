package tier

import (
	"errors"
	"os"
	"os/exec"
	"sort"
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

// The other packages of this module the data plane may import. They are
// held to the same rule as the data plane. membership, the one storage
// piece that knows Kubernetes, is not among them: only the node daemon
// imports it.
var dataPlaneMayImport = []string{
	"internal/metrics",
}

// Packages the data plane must never reach, directly or through anything.
var dataPlaneNever = []string{
	"k8s.io/",
	"sigs.k8s.io/", // controller-runtime among them
}

// Every package of the data plane, on Linux and on macOS (overlay and
// layer are Linux-only), reaches only the data plane and its allowed
// packages of this module, and nothing of Kubernetes by any path.
func TestDataPlaneStaysApartFromTheFarm(t *testing.T) {
	checked := append(append([]string{}, dataPlane...), dataPlaneMayImport...)
	for _, goos := range []string{"linux", "darwin"} {
		args := []string{"list", "-deps", "-f", "{{.ImportPath}} {{join .Imports \" \"}}"}
		for _, p := range checked {
			args = append(args, "../../"+p)
		}
		cmd := exec.Command("go", args...)
		cmd.Env = append(os.Environ(), "GOOS="+goos)
		out, err := cmd.Output()
		if err != nil {
			// The guard must never pass because it could not look.
			var stderr []byte
			if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
				stderr = ee.Stderr
			}
			t.Fatalf("GOOS=%s: go list: %v\n%s", goos, err, stderr)
		}
		for _, v := range dataPlaneViolations(string(out), checked) {
			t.Errorf("GOOS=%s: %s", goos, v)
		}
	}
}

// dataPlaneViolations reads a `go list -deps` listing (each line a package
// then its imports) and names each way the checked packages leave the data
// plane: an import of this module outside the checked set, or any path to
// a dataPlaneNever package, named by the checked package's import that
// leads there.
func dataPlaneViolations(listing string, checked []string) []string {
	imports := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(listing), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 {
			imports[f[0]] = f[1:]
		}
	}
	allowed := map[string]bool{}
	for _, p := range checked {
		allowed[module+p] = true
	}
	var out []string
	for _, p := range checked {
		if _, ok := imports[module+p]; !ok {
			out = append(out, "go list did not list "+module+p)
		}
	}
	// reaches gives a never-package the import reaches, or "".
	memo := map[string]string{}
	var reaches func(p string) string
	reaches = func(p string) string {
		if r, ok := memo[p]; ok {
			return r
		}
		memo[p] = "" // Go has no import cycles; this only stops a malformed listing
		for _, n := range dataPlaneNever {
			if strings.HasPrefix(p, n) {
				memo[p] = p
				return p
			}
		}
		for _, imp := range imports[p] {
			if r := reaches(imp); r != "" {
				memo[p] = r
				return r
			}
		}
		return ""
	}
	for _, p := range checked {
		for _, imp := range imports[module+p] {
			switch {
			case strings.HasPrefix(imp, module) && !allowed[imp]:
				out = append(out, module+p+" imports "+imp)
			case strings.HasPrefix(imp, module):
				// checked in its own right
			default:
				if r := reaches(imp); r == imp {
					out = append(out, module+p+" imports "+imp)
				} else if r != "" {
					out = append(out, module+p+" imports "+imp+", which reaches "+r)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// The check itself, on listings made up to break each rule.
func TestDataPlaneViolations(t *testing.T) {
	m := module
	checked := []string{"internal/tier", "internal/metrics"}
	cases := []struct {
		name, listing string
		want          []string
	}{
		{"clean",
			m + "internal/tier " + m + "internal/metrics fmt\n" + m + "internal/metrics github.com/prometheus/client_golang/prometheus\ngithub.com/prometheus/client_golang/prometheus\nfmt",
			nil},
		{"the farm, from the data plane",
			m + "internal/tier " + m + "internal/hud\n" + m + "internal/metrics\n" + m + "internal/hud",
			[]string{m + "internal/tier imports " + m + "internal/hud"}},
		{"the farm, from an allowed package",
			m + "internal/tier " + m + "internal/metrics\n" + m + "internal/metrics " + m + "internal/hud\n" + m + "internal/hud",
			[]string{m + "internal/metrics imports " + m + "internal/hud"}},
		{"Kubernetes, directly",
			m + "internal/tier k8s.io/api/core/v1\n" + m + "internal/metrics\nk8s.io/api/core/v1",
			[]string{m + "internal/tier imports k8s.io/api/core/v1"}},
		{"Kubernetes, from an allowed package",
			m + "internal/tier " + m + "internal/metrics\n" + m + "internal/metrics sigs.k8s.io/controller-runtime\nsigs.k8s.io/controller-runtime",
			[]string{m + "internal/metrics imports sigs.k8s.io/controller-runtime"}},
		{"Kubernetes, through a third party",
			m + "internal/tier example.com/a\n" + m + "internal/metrics\nexample.com/a example.com/b\nexample.com/b k8s.io/client-go/kubernetes\nk8s.io/client-go/kubernetes",
			[]string{m + "internal/tier imports example.com/a, which reaches k8s.io/client-go/kubernetes"}},
		{"a checked package go list did not list",
			m + "internal/tier",
			[]string{"go list did not list " + m + "internal/metrics"}},
	}
	for _, c := range cases {
		got := dataPlaneViolations(c.listing, checked)
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
