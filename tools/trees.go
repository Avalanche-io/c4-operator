package tools

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// CopyTo writes one prebuilt tool's folder under dir, as c4 farm up does
// before it publishes the folder, and returns where.
func CopyTo(dir, name string) (string, error) {
	f, err := Folder(name)
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, name)
	return out, os.CopyFS(out, f)
}

// tested is the prebuilt tools whose own tests the release ran against
// its images (make tool-tests), comma-separated, set when the release
// links the operator, whose HUD books them (.goreleaser.yaml, from make
// tested-tools). A development build has none.
var tested string

// Tested says the release ran the prebuilt tool's own test against its
// images, and it passed: the release stops when one fails.
func Tested(name string) bool {
	for _, t := range strings.Split(tested, ",") {
		if strings.TrimSpace(t) == name {
			return true
		}
	}
	return false
}

// ReleaseTree is one of the release's own tool trees: its tool, the release that
// built it, and whether that release ran its test.
type ReleaseTree struct {
	Name, Release string
	Tested        bool
}

// ReleaseTrees are the prebuilt tools' trees by C4 ID, each mapped to
// release: the IDs c4 farm up's folders have when it publishes them. A
// tree's ID is final only once its folder carries the tool.lock the
// release wrote, so each folder is judged on its own: one without a lock
// is left out, and the others still count.
func ReleaseTrees(release string) (map[string]string, error) {
	trees, err := Trees(release)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(trees))
	for id, t := range trees {
		out[id] = t.Release
	}
	return out, nil
}

// Trees are ReleaseTrees with each tree's tool and whether the release
// tested it, so the farm books as tested only the trees whose tests ran.
func Trees(release string) (map[string]ReleaseTree, error) {
	folders := map[string]fs.FS{}
	for _, name := range Names {
		f, err := Folder(name)
		if err != nil {
			return nil, err
		}
		folders[name] = f
	}
	return releaseTrees(release, folders)
}

func releaseTrees(release string, folders map[string]fs.FS) (map[string]ReleaseTree, error) {
	tmp, err := os.MkdirTemp("", "c4-release-tools-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out := map[string]ReleaseTree{}
	for name, f := range folders {
		if _, err := fs.Stat(f, "tool.lock"); err != nil {
			continue
		}
		dir := filepath.Join(tmp, name)
		if err := os.CopyFS(dir, f); err != nil {
			return nil, err
		}
		v, err := view.ScanDir(dir)
		if err != nil {
			return nil, err
		}
		out[v.ID.String()] = ReleaseTree{Name: name, Release: release, Tested: Tested(name)}
	}
	return out, nil
}
