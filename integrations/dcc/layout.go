package dcc

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// layout is what turning C4 on in one application puts where.
type layout struct {
	title   string
	on, off string            // Change.Line for on and for off
	files   map[string][]byte // whole files c4 owns, by absolute path
	edits   []lineEdit        // single marked lines in the person's own files (Nuke, Katana)
	notes   []string
	// refuse says why an existing file at one of files' paths, which no
	// manifest of c4's records, can't be replaced ("" for the default).
	refuse map[string]string
	// shared are per-user files every version of the application reads
	// (Unreal's start-up script); off leaves them while another version
	// has C4 on.
	shared map[string]bool
	// derived are files the integration itself makes later, inside a
	// folder c4 owns, which off takes out too (Blender's turned-on-once
	// mark).
	derived []string
}

// lineEdit is one line c4 adds to a file the person owns.
type lineEdit struct {
	path, line, marker string
}

// supported are the versions each panel is made for: below min, Plan
// refuses; above max, it says the version is newer than C4 was checked
// with. A max without a dot compares the major version only.
var supported = map[string][2]string{
	"blender": {"4.2", ""},
	"unreal":  {"5.4", "5.6"},
	"houdini": {"20.5", "21.0"},
	"maya":    {"2024", "2026"},
	"nuke":    {"14.0", "16"},
	"katana":  {"7.0", "8"},
}

func newerThan(v, max string) bool {
	if max == "" {
		return false
	}
	if !strings.Contains(max, ".") {
		major, _, _ := strings.Cut(v, ".")
		return versionLess(max, major)
	}
	return versionLess(max, v)
}

// payloadDir is where an application's panel files are kept:
// ~/.c4/integrations/<name>.
func (r *Roots) payloadDir(name string) string {
	return filepath.Join(r.Home, ".c4", "integrations", name)
}

func (r *Roots) manifestDir() string { return filepath.Join(r.Home, ".c4", "integrations") }

func (r *Roots) manifestPath(name, version string) string {
	return filepath.Join(r.manifestDir(), name+"-"+version+".json")
}

// copyTree adds every embedded file under src to files, at dst plus its
// path under src.
func copyTree(files map[string][]byte, src, dst string) {
	for _, rel := range embedded(src) {
		files[filepath.Join(dst, filepath.FromSlash(rel))] = embeddedFile(path.Join(src, rel))
	}
}

// payload is the panel's folders (each kept at the same place under the
// payload as under integrations/dcc, so each panel's start-up script finds
// c4submit beside it as in the repository), c4submit and the licence.
func (r *Roots) payload(name string, folders ...string) map[string][]byte {
	dir := r.payloadDir(name)
	files := map[string][]byte{filepath.Join(dir, "LICENSE"): embeddedFile("LICENSE")}
	for _, f := range folders {
		copyTree(files, f, filepath.Join(dir, filepath.FromSlash(f)))
	}
	copyTree(files, "c4submit/src/c4submit", filepath.Join(dir, "c4submit", "src", "c4submit"))
	return files
}

// pyStr is s as a Python string literal.
func pyStr(s string) (string, error) {
	if strings.ContainsAny(s, "\n\r\x00") {
		return "", fmt.Errorf("%q can't be written into a Python file", s)
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'", nil
}

// signIn is the note every panel shares.
const signIn = "The panel uses the farm `c4 farm` uses (~/.c4/farm.yaml): run `c4 farm login` first if you haven't."

// layout is where turning C4 on in f puts each file. Only on refuses a
// version older than the panel supports; off takes out whatever is there.
func (r *Roots) layout(f Found, on bool) (layout, error) {
	if lim, ok := supported[f.Name]; ok && on && lim[0] != "" && versionLess(f.Version, lim[0]) {
		return layout{}, fmt.Errorf("%s is older than C4's panel supports (%s and later)", f.Title, lim[0])
	}
	var l layout
	var err error
	switch f.Name {
	case "blender":
		l, err = r.blenderLayout(f)
	case "unreal":
		l, err = r.unrealLayout(f)
	case "houdini":
		l, err = r.houdiniLayout(f)
	case "maya":
		l, err = r.mayaLayout(f)
	case "nuke":
		l, err = r.nukeLayout(f)
	case "katana":
		l, err = r.katanaLayout(f)
	default:
		return layout{}, fmt.Errorf("C4 has no integration for %q", f.Name)
	}
	if err != nil {
		return layout{}, err
	}
	l.title = f.Title
	if lim := supported[f.Name]; newerThan(f.Version, lim[1]) {
		l.notes = append(l.notes, fmt.Sprintf("%s is newer than C4's panel has been checked with (up to %s); if the panel doesn't appear, say so.", f.Title, lim[1]))
	}
	l.notes = append(l.notes, signIn)
	return l, nil
}

// blenderScripts is Blender's per-user scripts folder for a version.
func (r *Roots) blenderScripts(v string) string {
	switch r.OS {
	case "darwin":
		return filepath.Join(r.Home, "Library", "Application Support", "Blender", v, "scripts")
	case "windows":
		return filepath.Join(r.Config, "Blender Foundation", "Blender", v, "scripts")
	}
	return filepath.Join(r.Config, "blender", v, "scripts")
}

// blenderEnable is the start-up module that turns the add-on on once.
// Blender imports each module in scripts/startup at start-up and calls its
// register().
const blenderEnable = `"""Added by ` + "`c4 farm integrate blender on`" + `: turns on the C4 Farm add-on
(scripts/addons/c4_blender) the first time Blender starts after it was
installed, and not again, so turning it off in Preferences > Add-ons
sticks. ` + "`c4 farm integrate blender off`" + ` removes this file and the add-on.
"""

import os

import bpy

_DONE = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                     "addons", "c4_blender", ".c4-enabled")


def _enable_once():
    if os.path.exists(_DONE):
        return None
    import addon_utils
    try:
        if not addon_utils.check("c4_blender")[1]:
            addon_utils.enable("c4_blender", default_set=True, persistent=True)
        prefs = bpy.context.preferences
        prefs.is_dirty = True  # so Blender saves the add-on as on when it quits
        if not prefs.use_preferences_save:
            return None  # preferences aren't saved: turn it on again next time
    except Exception as e:  # noqa: BLE001 - never stop Blender starting
        print(f"C4: could not turn on the C4 Farm add-on: {e}")
        return None
    try:
        with open(_DONE, "w", encoding="utf-8") as f:
            f.write("turned on once by c4 farm integrate blender\n")
    except OSError:
        pass
    return None


def register():
    bpy.app.timers.register(_enable_once, first_interval=0.5)


def unregister():
    if bpy.app.timers.is_registered(_enable_once):
        bpy.app.timers.unregister(_enable_once)
`

func (r *Roots) blenderLayout(f Found) (layout, error) {
	scripts := r.blenderScripts(f.Version)
	addon := filepath.Join(scripts, "addons", "c4_blender")
	files := map[string][]byte{filepath.Join(addon, "LICENSE"): embeddedFile("LICENSE")}
	copyTree(files, "blender/addons/c4_blender", addon)
	copyTree(files, "c4submit/src/c4submit", filepath.Join(addon, "vendor", "c4submit"))
	files[filepath.Join(scripts, "startup", "c4_farm_enable.py")] = []byte(blenderEnable)
	return layout{
		on:      fmt.Sprintf("Adds the C4 Farm add-on to %s's add-ons (a C4 Farm panel in Render Properties) and turns it on the next time Blender starts.", f.Title),
		off:     fmt.Sprintf("Takes the C4 Farm add-on out of %s.", f.Title),
		files:   files,
		derived: []string{filepath.Join(addon, ".c4-enabled")},
		notes:   []string{"Restart Blender if it's open."},
	}, nil
}

const unrealMark = "# Added by c4 farm integrate unreal."

func (r *Roots) unrealLayout(f Found) (layout, error) {
	dir := filepath.Join(r.Home, "Documents", "UnrealEngine", "Python")
	init := filepath.Join(dir, "init_unreal.py")
	files := r.payload("unreal", "unreal/python")
	target, err := pyStr(filepath.Join(r.payloadDir("unreal"), "unreal", "python", "init_unreal.py"))
	if err != nil {
		return layout{}, err
	}
	files[init] = []byte(unrealMark + ` Unreal runs this at editor start-up; it loads C4's
# Movie Render Queue executor (C4FarmExecutor) from ~/.c4/integrations/unreal.
# ` + "`c4 farm integrate unreal off`" + ` removes this file.
import runpy as _c4_runpy

_c4_runpy.run_path(` + target + `, run_name="c4_init_unreal")
del _c4_runpy
`)
	return layout{
		on:     fmt.Sprintf("Adds a start-up script to Unreal's per-user Python folder, which makes C4FarmExecutor (Render (Remote) in the Movie Render Queue) available in %s and every other Unreal Engine you open.", f.Title),
		off:    fmt.Sprintf("Takes C4's start-up script out of Unreal's per-user Python folder, so %s no longer loads C4FarmExecutor.", f.Title),
		files:  files,
		shared: map[string]bool{init: true},
		refuse: map[string]string{init: "you already have an init_unreal.py in " + dir +
			", and c4 won't overwrite it. Add `import runpy; runpy.run_path(" + target + ")` to it yourself, " +
			"or set it aside and run this again"},
		notes: []string{
			"In each project, enable the Python Editor Script Plugin, Editor Scripting Utilities and Movie Render Queue plug-ins (Edit > Plugins), then restart the editor.",
			"In each project, set Project Settings > Plugins > Movie Render Pipeline > Default Remote Executor to C4FarmExecutor.",
		},
	}, nil
}

func (r *Roots) houdiniPrefs(v string) string {
	switch r.OS {
	case "darwin":
		return filepath.Join(r.Home, "Library", "Preferences", "houdini", v)
	case "windows":
		return filepath.Join(r.Home, "Documents", "houdini"+v)
	}
	return filepath.Join(r.Home, "houdini"+v)
}

func (r *Roots) houdiniLayout(f Found) (layout, error) {
	files := r.payload("houdini", "houdini/python", "houdini/toolbar")
	pkg := map[string]any{
		"env": []any{
			map[string]any{"C4_DCC": filepath.ToSlash(r.payloadDir("houdini"))},
			map[string]any{"PYTHONPATH": map[string]string{"value": "$C4_DCC/houdini/python", "method": "prepend"}},
			map[string]any{"PYTHONPATH": map[string]string{"value": "$C4_DCC/c4submit/src", "method": "prepend"}},
		},
		"hpath": "$C4_DCC/houdini",
	}
	b, err := json.MarshalIndent(pkg, "", "    ")
	if err != nil {
		return layout{}, err
	}
	at := filepath.Join(r.houdiniPrefs(f.Version), "packages", "c4.json")
	files[at] = append(b, '\n')
	return layout{
		on:     fmt.Sprintf("Adds a C4 package to %s's packages, which puts the C4 Render LOP, the C4 Submit ROP and the C4 shelf in Houdini.", f.Title),
		off:    fmt.Sprintf("Takes the C4 package out of %s.", f.Title),
		files:  files,
		refuse: map[string]string{at: "there is already a c4.json in " + filepath.Dir(at) + " that c4 didn't put there (perhaps a copy of the repository's); set it aside and run this again"},
		notes:  []string{"Restart Houdini if it's open."},
	}, nil
}

func (r *Roots) mayaModules(year string) string {
	switch r.OS {
	case "darwin":
		return filepath.Join(r.Home, "Library", "Preferences", "Autodesk", "maya", year, "modules")
	case "windows":
		return filepath.Join(r.Home, "Documents", "maya", year, "modules")
	}
	return filepath.Join(r.Home, "maya", year, "modules")
}

func (r *Roots) mayaLayout(f Found) (layout, error) {
	files := r.payload("maya", "maya/scripts")
	at := filepath.Join(r.mayaModules(f.Version), "c4.mod")
	files[at] = []byte("+ c4 1.0 " + filepath.ToSlash(filepath.Join(r.payloadDir("maya"), "maya")) + "\nPYTHONPATH +:= ../c4submit/src\n")
	return layout{
		on:     fmt.Sprintf("Adds a C4 module to %s's modules, which puts a C4 menu in Maya's main window.", f.Title),
		off:    fmt.Sprintf("Takes the C4 module out of %s.", f.Title),
		files:  files,
		refuse: map[string]string{at: "there is already a c4.mod in " + filepath.Dir(at) + " that c4 didn't put there; set it aside and run this again"},
		notes:  []string{"Restart Maya if it's open."},
	}, nil
}

// lineMark ends the line c4 adds to Nuke's and Katana's start-up files.
func lineMark(name string) string { return "# added by c4 farm integrate " + name }

func (r *Roots) nukeLayout(f Found) (layout, error) {
	dir, err := pyStr(filepath.Join(r.payloadDir("nuke"), "nuke"))
	if err != nil {
		return layout{}, err
	}
	init := filepath.Join(r.Home, ".nuke", "init.py")
	mark := lineMark("nuke")
	return layout{
		on:    fmt.Sprintf("Adds one marked line to your Nuke start-up file (%s), which puts a C4 menu in %s; the file is backed up first.", init, f.Title),
		off:   fmt.Sprintf("Takes C4's line out of your Nuke start-up file (%s); the rest of it is left as it is.", init),
		files: r.payload("nuke", "nuke"),
		edits: []lineEdit{{path: init, marker: mark, line: "import nuke; nuke.pluginAddPath(" + dir + ")  " + mark}},
		notes: []string{"Every Nuke version reads " + init + ", so C4 is in each one until the last is turned off. Restart Nuke if it's open."},
	}, nil
}

// katanaLayout uses ~/.katana/init.py, which Katana runs at start-up when
// the person's resources are in the default place (~/.katana, unless
// KATANA_USER_RESOURCE_DIRECTORY moves them).
func (r *Roots) katanaLayout(f Found) (layout, error) {
	script, err := pyStr(filepath.Join(r.payloadDir("katana"), "katana", "Startup", "init.py"))
	if err != nil {
		return layout{}, err
	}
	init := filepath.Join(r.Home, ".katana", "init.py")
	mark := lineMark("katana")
	return layout{
		on:    fmt.Sprintf("Adds one marked line to your Katana start-up file (%s), which puts C4 on %s's farm menus; the file is backed up first.", init, f.Title),
		off:   fmt.Sprintf("Takes C4's line out of your Katana start-up file (%s); the rest of it is left as it is.", init),
		files: r.payload("katana", "katana/Startup", "katana/python"),
		edits: []lineEdit{{path: init, marker: mark,
			line: "import runpy; runpy.run_path(" + script + ", run_name='c4_katana_startup')  " + mark}},
		notes: []string{
			"Every Katana version reads " + init + ", so C4 is in each one until the last is turned off.",
			"If KATANA_USER_RESOURCE_DIRECTORY is set, Katana reads that folder instead of ~/.katana, and won't see this line.",
		},
	}, nil
}
