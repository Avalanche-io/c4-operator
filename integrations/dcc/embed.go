// Package dcc puts C4's panels and plug-ins into the applications artists
// work in (Blender, Unreal, Houdini, Maya, Nuke and Katana) and takes them
// out again. It is what `c4 farm integrations` and `c4 farm integrate
// <name> on|off` call.
//
// Detect finds the applications installed on a machine by reading files
// only: it never launches an application, never runs a program and never
// uses the network. Plan says what turning C4 on or off in one of them
// would write and remove, and writes nothing. Apply does exactly that:
// every file is written whole (a temporary file renamed into place, a new
// folder built beside its place and renamed in), and what it wrote is
// recorded in a manifest, ~/.c4/integrations/<name>-<version>.json, so off
// removes exactly those files and nothing else.
//
// The panels' Python files and c4submit, the library under them, are
// embedded in this package, so an installed `c4 farm` needs no checkout
// and no Python of its own.
package dcc

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// payloadFS holds every file a panel needs inside its application: no
// tests, fixtures or tool folders. Each pattern names files, not folders,
// so `__init__.py` is kept and `__pycache__` and dotfiles never match.
//
//go:embed LICENSE
//go:embed c4submit/src/c4submit/*.py
//go:embed nuke/menu.py nuke/c4_nuke/*.py
//go:embed houdini/python/c4_houdini/*.py houdini/toolbar/*.shelf
//go:embed maya/scripts/*.py maya/scripts/c4_maya/*.py
//go:embed katana/Startup/*.py katana/python/c4_katana/*.py
//go:embed blender/addons/c4_blender/*.py
//go:embed unreal/python/*.py unreal/python/c4_unreal/*.py
var payloadFS embed.FS

// runtimeFolders are the folders, under integrations/dcc, whose files are
// embedded; a test checks every runtime file in them is.
var runtimeFolders = []string{
	"c4submit/src/c4submit",
	"nuke/c4_nuke",
	"houdini/python/c4_houdini",
	"houdini/toolbar",
	"maya/scripts",
	"katana/Startup",
	"katana/python/c4_katana",
	"blender/addons/c4_blender",
	"unreal/python",
}

// embedded lists the embedded files under dir (slash-separated), each as
// its path relative to dir.
func embedded(dir string) []string {
	var out []string
	_ = fs.WalkDir(payloadFS, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		out = append(out, strings.TrimPrefix(p, dir+"/"))
		return nil
	})
	sort.Strings(out)
	return out
}

func embeddedFile(p string) []byte {
	b, err := payloadFS.ReadFile(p)
	if err != nil {
		panic("dcc: embedded file missing: " + p) // the embed patterns and the layouts disagree
	}
	return b
}

// Release identifies the panels this build of c4 carries: a digest of
// every embedded file. A manifest recorded by a build with a different
// Release is an earlier (or later) c4 release's install.
func Release() string { return release }

var release = func() string {
	h := sha256.New()
	for _, p := range embedded(".") {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(embeddedFile(path.Clean(p)))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}()
