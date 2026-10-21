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
// release: the IDs c4 farm up's folders have when it publishes them. They
// are final only in a release build, whose folders carry the tool.lock the
// release wrote; with any folder lacking one there are none.
func ReleaseTrees(release string) (map[string]string, error) {
	tmp, err := os.MkdirTemp("", "c4-release-tools-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out := map[string]string{}
	for _, name := range Names {
		f, err := Folder(name)
		if err != nil {
			return nil, err
		}
		if _, err := fs.Stat(f, "tool.lock"); err != nil {
			return nil, nil
		}
		dir, err := CopyTo(tmp, name)
		if err != nil {
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
