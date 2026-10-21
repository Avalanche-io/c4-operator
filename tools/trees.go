package tools

import (
	"io/fs"
	"os"
	"path/filepath"

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

// ReleaseTrees are the prebuilt tools' trees by C4 ID, each mapped to
// release: the IDs c4 farm up's folders have when it publishes them. A
// tree's ID is final only once its folder carries the tool.lock the
// release wrote, so each folder is judged on its own: one without a lock
// is left out, and the others still count.
func ReleaseTrees(release string) (map[string]string, error) {
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

func releaseTrees(release string, folders map[string]fs.FS) (map[string]string, error) {
	tmp, err := os.MkdirTemp("", "c4-release-tools-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out := map[string]string{}
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
		out[v.ID.String()] = release
	}
	return out, nil
}
