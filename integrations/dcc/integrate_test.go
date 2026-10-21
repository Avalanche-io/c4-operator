package dcc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"
)

// Every test here runs against fake application folders and a fake home
// in t.TempDir(); none reads the real home or the real applications.

func write(t *testing.T, p string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func xmlPlist(version string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDocumentTypes</key>
	<array><dict><key>CFBundleShortVersionString</key><string>9.9</string></dict></array>
	<key>CFBundleIdentifier</key>
	<string>org.blenderfoundation.blender</string>
	<key>CFBundleShortVersionString</key>
	<string>` + version + `</string>
</dict>
</plist>
`
}

// bplist writes a binary plist ("bplist00") holding one dictionary of
// strings; values starting with "u:" are written as UTF-16.
func bplist(kv [][2]string) []byte {
	var objs [][]byte
	str := func(s string) []byte {
		if u, ok := strings.CutPrefix(s, "u:"); ok {
			cs := utf16.Encode([]rune(u))
			b := lenMarker(0x60, len(cs))
			for _, c := range cs {
				b = binary.BigEndian.AppendUint16(b, c)
			}
			return b
		}
		return append(lenMarker(0x50, len(s)), s...)
	}
	n := len(kv)
	dict := lenMarker(0xD0, n)
	for i := range kv {
		dict = append(dict, byte(1+i))
	}
	for i := range kv {
		dict = append(dict, byte(1+n+i))
	}
	objs = append(objs, dict)
	for _, p := range kv {
		objs = append(objs, str(p[0]))
	}
	for _, p := range kv {
		objs = append(objs, str(p[1]))
	}
	out := []byte("bplist00")
	var offsets []int
	for _, o := range objs {
		offsets = append(offsets, len(out))
		out = append(out, o...)
	}
	table := len(out)
	for _, o := range offsets {
		out = binary.BigEndian.AppendUint16(out, uint16(o))
	}
	out = append(out, 0, 0, 0, 0, 0, 0, 2, 1)
	out = binary.BigEndian.AppendUint64(out, uint64(len(objs)))
	out = binary.BigEndian.AppendUint64(out, 0)
	return binary.BigEndian.AppendUint64(out, uint64(table))
}

func lenMarker(kind byte, n int) []byte {
	if n < 15 {
		return []byte{kind | byte(n)}
	}
	return []byte{kind | 0x0F, 0x10, byte(n)}
}

func TestPlistStrings(t *testing.T) {
	dir := t.TempDir()
	x := filepath.Join(dir, "x.plist")
	write(t, x, xmlPlist("4.2.3"))
	if v, err := plistString(x, "CFBundleShortVersionString"); err != nil || v != "4.2.3" {
		t.Errorf("xml: %q %v (a nested dict's key must not count)", v, err)
	}
	b := filepath.Join(dir, "b.plist")
	write(t, b, string(bplist([][2]string{
		{"CFBundleIdentifier", "org.blenderfoundation.blender"},
		{"CFBundleName", "u:Blendér"},
		{"CFBundleShortVersionString", "4.5.1"},
	})))
	if v, err := plistString(b, "CFBundleShortVersionString"); err != nil || v != "4.5.1" {
		t.Errorf("binary: %q %v", v, err)
	}
	if v, err := plistString(b, "CFBundleName"); err != nil || v != "Blendér" {
		t.Errorf("binary UTF-16: %q %v", v, err)
	}
	if _, err := plistString(b, "Missing"); err == nil {
		t.Error("a missing key read as found")
	}
	for i := 0; i < 64; i++ { // truncated and corrupted plists are errors, never panics
		bad := bplist([][2]string{{"CFBundleShortVersionString", "4.5.1"}})
		if i < len(bad) {
			bad[len(bad)-1-i%len(bad)] ^= 0xFF
		}
		_, _ = bplistString(bad, "CFBundleShortVersionString")
		_, _ = bplistString(bad[:len(bad)-i%len(bad)], "CFBundleShortVersionString")
	}
}

type fake struct {
	root  string
	roots Roots
}

func newFake(t *testing.T, goos string) *fake {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mkdir(t, home)
	r := Roots{OS: goos, Home: home}
	switch goos {
	case "darwin":
		r.Apps = []string{filepath.Join(root, "Applications"), filepath.Join(home, "Applications")}
		r.Steam = []string{filepath.Join(home, "Library", "Application Support", "Steam", "steamapps", "common")}
		r.Epic = []string{filepath.Join(home, "Library", "Application Support", "Epic", "UnrealEngineLauncher", "LauncherInstalled.dat")}
	case "windows":
		r.Config = filepath.Join(home, "AppData", "Roaming")
		r.Apps = []string{filepath.Join(root, "Program Files"), filepath.Join(root, "Program Files (x86)")}
		r.Steam = []string{filepath.Join(root, "Program Files (x86)", "Steam", "steamapps", "common")}
		r.Epic = []string{filepath.Join(root, "ProgramData", "Epic", "UnrealEngineLauncher", "LauncherInstalled.dat")}
	default:
		r.Config = filepath.Join(home, ".config")
		r.Apps = []string{filepath.Join(root, "opt"), filepath.Join(root, "opt", "Foundry"), filepath.Join(root, "usr", "local"), filepath.Join(root, "usr", "autodesk")}
		r.Steam = []string{filepath.Join(home, ".steam", "steam", "steamapps", "common")}
	}
	return &fake{root: root, roots: r}
}

func (f *fake) epic(t *testing.T, engines map[string]string) {
	t.Helper()
	type entry struct{ InstallLocation, AppName, AppVersion string }
	var list []entry
	for name, loc := range engines {
		list = append(list, entry{loc, name, "x"})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].AppName < list[j].AppName })
	b, _ := json.MarshalIndent(map[string]any{"InstallationList": list}, "", "\t")
	write(t, f.roots.Epic[0], string(b))
}

type seen struct{ Name, Title, Version, Where, Status string }

func detected(r *Roots) []seen {
	var out []seen
	for _, f := range r.Detect() {
		out = append(out, seen{f.Name, f.Title, f.Version, f.Where, f.Status})
	}
	return out
}

func TestDetectDarwin(t *testing.T) {
	f := newFake(t, "darwin")
	apps, home := f.roots.Apps[0], f.roots.Home
	write(t, filepath.Join(apps, "Blender.app", "Contents", "Info.plist"), xmlPlist("4.2.3"))
	steam := filepath.Join(f.roots.Steam[0], "Blender", "Blender.app")
	write(t, filepath.Join(steam, "Contents", "Info.plist"), string(bplist([][2]string{{"CFBundleShortVersionString", "4.5.1"}})))
	// A plist this can't read: the version comes from the bundle's scripts folder.
	odd := filepath.Join(home, "Applications", "Blender 5.0.app")
	write(t, filepath.Join(odd, "Contents", "Info.plist"), "bplist00 not really")
	mkdir(t, filepath.Join(odd, "Contents", "Resources", "5.0", "scripts"))
	mkdir(t, filepath.Join(apps, "Houdini", "Houdini21.0.440"))
	mkdir(t, filepath.Join(apps, "Houdini", "Houdini21.0.512"))
	mkdir(t, filepath.Join(apps, "Houdini", "Houdini20.5.613"))
	mkdir(t, filepath.Join(apps, "Autodesk", "maya2025", "Maya.app"))
	mkdir(t, filepath.Join(apps, "Nuke15.1v3", "Nuke15.1v3.app"))
	mkdir(t, filepath.Join(apps, "Katana8.0v1")) // not a macOS application: ignored
	ue57 := filepath.Join(f.root, "Shared", "Epic Games", "UE_5.7")
	ue56 := filepath.Join(f.root, "Shared", "Epic Games", "UE_5.6")
	mkdir(t, ue57)
	mkdir(t, ue56)
	f.epic(t, map[string]string{
		"UE_5.7":           ue57,
		"FabPlugin_5.6":    ue56,                                    // a plug-in, not an engine
		"QuixelBridge_5.7": ue57,                                    // likewise
		"UE_5.3":           filepath.Join(f.root, "gone", "UE_5.3"), // listed, since removed
	})

	want := []seen{
		{"blender", "Blender 4.2", "4.2", filepath.Join(apps, "Blender.app"), StatusAvailable},
		{"blender", "Blender 4.5", "4.5", steam, StatusAvailable},
		{"blender", "Blender 5.0", "5.0", odd, StatusAvailable},
		{"houdini", "Houdini 20.5", "20.5", filepath.Join(apps, "Houdini", "Houdini20.5.613"), StatusAvailable},
		{"houdini", "Houdini 21.0", "21.0", filepath.Join(apps, "Houdini", "Houdini21.0.512"), StatusAvailable},
		{"maya", "Maya 2025", "2025", filepath.Join(apps, "Autodesk", "maya2025"), StatusAvailable},
		{"nuke", "Nuke 15.1", "15.1", filepath.Join(apps, "Nuke15.1v3"), StatusAvailable},
		{"unreal", "Unreal Engine 5.7", "5.7", ue57, StatusAvailable},
	}
	if got := detected(&f.roots); !reflect.DeepEqual(got, want) {
		t.Errorf("Detect:\n got %v\nwant %v", got, want)
	}
}

func TestDetectLinux(t *testing.T) {
	f := newFake(t, "linux")
	opt := f.roots.Apps[0]
	mkdir(t, filepath.Join(opt, "blender-4.2.3-linux-x64", "4.2", "scripts"))
	mkdir(t, filepath.Join(f.roots.Steam[0], "Blender", "4.5", "scripts"))
	mkdir(t, filepath.Join(opt, "hfs21.0.440"))
	mkdir(t, filepath.Join(f.roots.Apps[3], "maya2025"))
	mkdir(t, filepath.Join(f.roots.Apps[2], "Nuke15.1v3"))
	mkdir(t, filepath.Join(f.roots.Apps[1], "Katana8.0v1"))
	want := []seen{
		{"blender", "Blender 4.2", "4.2", filepath.Join(opt, "blender-4.2.3-linux-x64"), StatusAvailable},
		{"blender", "Blender 4.5", "4.5", filepath.Join(f.roots.Steam[0], "Blender"), StatusAvailable},
		{"houdini", "Houdini 21.0", "21.0", filepath.Join(opt, "hfs21.0.440"), StatusAvailable},
		{"katana", "Katana 8.0", "8.0", filepath.Join(f.roots.Apps[1], "Katana8.0v1"), StatusAvailable},
		{"maya", "Maya 2025", "2025", filepath.Join(f.roots.Apps[3], "maya2025"), StatusAvailable},
		{"nuke", "Nuke 15.1", "15.1", filepath.Join(f.roots.Apps[2], "Nuke15.1v3"), StatusAvailable},
	}
	if got := detected(&f.roots); !reflect.DeepEqual(got, want) {
		t.Errorf("Detect:\n got %v\nwant %v", got, want)
	}
}

func TestDetectWindows(t *testing.T) {
	f := newFake(t, "windows")
	pf := f.roots.Apps[0]
	mkdir(t, filepath.Join(pf, "Blender Foundation", "Blender 4.2", "4.2", "scripts"))
	mkdir(t, filepath.Join(f.roots.Steam[0], "Blender", "4.5", "scripts"))
	mkdir(t, filepath.Join(pf, "Side Effects Software", "Houdini 21.0.440"))
	mkdir(t, filepath.Join(pf, "Autodesk", "Maya2026"))
	mkdir(t, filepath.Join(pf, "Nuke16.0v2"))
	mkdir(t, filepath.Join(pf, "Katana7.5v1"))
	ue := filepath.Join(pf, "Epic Games", "UE_5.5")
	mkdir(t, ue)
	f.epic(t, map[string]string{"UE_5.5": ue, "FabPlugin_5.5": ue})
	want := []seen{
		{"blender", "Blender 4.2", "4.2", filepath.Join(pf, "Blender Foundation", "Blender 4.2"), StatusAvailable},
		{"blender", "Blender 4.5", "4.5", filepath.Join(f.roots.Steam[0], "Blender"), StatusAvailable},
		{"houdini", "Houdini 21.0", "21.0", filepath.Join(pf, "Side Effects Software", "Houdini 21.0.440"), StatusAvailable},
		{"katana", "Katana 7.5", "7.5", filepath.Join(pf, "Katana7.5v1"), StatusAvailable},
		{"maya", "Maya 2026", "2026", filepath.Join(pf, "Autodesk", "Maya2026"), StatusAvailable},
		{"nuke", "Nuke 16.0", "16.0", filepath.Join(pf, "Nuke16.0v2"), StatusAvailable},
		{"unreal", "Unreal Engine 5.5", "5.5", ue, StatusAvailable},
	}
	if got := detected(&f.roots); !reflect.DeepEqual(got, want) {
		t.Errorf("Detect:\n got %v\nwant %v", got, want)
	}
}

// snapshot is every file (contents and mode) and folder under dir.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[p] = "dir " + info.Mode().Perm().String()
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = info.Mode().Perm().String() + " " + string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// changed are the files (not folders) added or altered between a and b,
// and those removed.
func changed(a, b map[string]string) (written, removed []string) {
	for p, v := range b {
		if strings.HasPrefix(v, "dir ") {
			continue
		}
		if a[p] != v {
			written = append(written, p)
		}
	}
	for p, v := range a {
		if _, ok := b[p]; !ok && !strings.HasPrefix(v, "dir ") {
			removed = append(removed, p)
		}
	}
	sort.Strings(written)
	sort.Strings(removed)
	return
}

func diff(t *testing.T, what string, a, b map[string]string) {
	t.Helper()
	w, r := changed(a, b)
	var dirs []string
	for p := range a {
		if _, ok := b[p]; !ok {
			dirs = append(dirs, "-"+p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			dirs = append(dirs, "+"+p)
		}
	}
	if len(w) > 0 || len(r) > 0 || len(dirs) > 0 {
		t.Errorf("%s: the fake home differs:\n written %v\n removed %v\n paths %v", what, w, r, dirs)
	}
}

func find(t *testing.T, r *Roots, name, version string) Found {
	t.Helper()
	for _, f := range r.Detect() {
		if f.Name == name && f.Version == version {
			return f
		}
	}
	t.Fatalf("Detect found no %s %s", name, version)
	return Found{}
}

func plan(t *testing.T, f Found, on bool) Change {
	t.Helper()
	c, err := Plan(f, on)
	if err != nil {
		t.Fatalf("Plan(%s %s, %v): %v", f.Name, f.Version, on, err)
	}
	if c.Line == "" {
		t.Errorf("Plan(%s %s, %v) has no Line", f.Name, f.Version, on)
	}
	return c
}

func apply(t *testing.T, c Change) {
	t.Helper()
	if err := Apply(c); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// onOff turns C4 on in f, checks the plan named exactly the files Apply
// wrote, that a second on changes nothing, and that off leaves the home as
// it was.
func onOff(t *testing.T, fk *fake, f Found, check func(t *testing.T)) {
	t.Helper()
	home := fk.roots.Home
	before := snapshot(t, home)
	c := plan(t, f, true)
	if len(c.Removes) != 0 {
		t.Errorf("a first on removes %v", c.Removes)
	}
	if mid := snapshot(t, home); !reflect.DeepEqual(mid, before) {
		diff(t, "Plan wrote", before, mid)
	}
	apply(t, c)
	after := snapshot(t, home)
	written, removed := changed(before, after)
	if !reflect.DeepEqual(written, c.Writes) {
		t.Errorf("Plan listed writes\n %v\nApply wrote\n %v", c.Writes, written)
	}
	if len(removed) > 0 {
		t.Errorf("on removed %v", removed)
	}
	if st := find(t, &fk.roots, f.Name, f.Version).Status; st != StatusInstalled {
		t.Errorf("status after on: %q", st)
	}
	if check != nil {
		check(t)
	}

	again := plan(t, f, true)
	if len(again.Writes) != 0 || len(again.Removes) != 0 || !strings.Contains(again.Line, "already") {
		t.Errorf("a second on would change things: %q %v %v", again.Line, again.Writes, again.Removes)
	}
	apply(t, again)
	if s := snapshot(t, home); !reflect.DeepEqual(s, after) {
		diff(t, "a second on", after, s)
	}

	off := plan(t, f, false)
	apply(t, off)
	final := snapshot(t, home)
	diff(t, "on then off", before, final)
	_, offRemoved := changed(after, final)
	for _, p := range offRemoved {
		if !contains(off.Removes, p) {
			t.Errorf("off removed %s, which its plan didn't list", p)
		}
	}
	if st := find(t, &fk.roots, f.Name, f.Version).Status; st != StatusAvailable {
		t.Errorf("status after off: %q", st)
	}
	if again := plan(t, f, false); len(again.Removes) != 0 {
		t.Errorf("a second off would remove %v", again.Removes)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func darwinApps(t *testing.T) *fake {
	f := newFake(t, "darwin")
	apps := f.roots.Apps[0]
	write(t, filepath.Join(f.roots.Steam[0], "Blender", "Blender.app", "Contents", "Info.plist"), xmlPlist("5.2.2"))
	mkdir(t, filepath.Join(apps, "Houdini", "Houdini21.0.440"))
	mkdir(t, filepath.Join(apps, "Autodesk", "maya2025"))
	mkdir(t, filepath.Join(apps, "Nuke15.1v3"))
	mkdir(t, filepath.Join(apps, "Nuke16.0v1"))
	ue := filepath.Join(f.root, "Shared", "Epic Games", "UE_5.7")
	mkdir(t, ue)
	f.epic(t, map[string]string{"UE_5.7": ue})
	return f
}

func TestBlenderOnOff(t *testing.T) {
	fk := darwinApps(t)
	scripts := filepath.Join(fk.roots.Home, "Library", "Application Support", "Blender", "5.2", "scripts")
	// The person's own Blender settings are already there and are left alone.
	write(t, filepath.Join(scripts, "addons", "mine", "__init__.py"), "bl_info = {}\n")
	write(t, filepath.Join(scripts, "..", "config", "userpref.blend"), "prefs")
	onOff(t, fk, find(t, &fk.roots, "blender", "5.2"), func(t *testing.T) {
		addon := filepath.Join(scripts, "addons", "c4_blender")
		for _, p := range []string{"__init__.py", "panel.py", "host.py", "LICENSE", "vendor/c4submit/__init__.py", "vendor/c4submit/submitter.py"} {
			if _, err := os.Stat(filepath.Join(addon, filepath.FromSlash(p))); err != nil {
				t.Errorf("add-on: %v", err)
			}
		}
		if s := readFile(t, filepath.Join(scripts, "startup", "c4_farm_enable.py")); !strings.Contains(s, "def register():") ||
			!strings.Contains(s, `addon_utils.enable("c4_blender"`) {
			t.Errorf("the startup module doesn't register an enable:\n%s", s)
		}
	})
}

func TestBlenderOffTakesPythonsCopiesAndTheOnceMark(t *testing.T) {
	fk := darwinApps(t)
	f := find(t, &fk.roots, "blender", "5.2")
	before := snapshot(t, fk.roots.Home)
	apply(t, plan(t, f, true))
	addon := filepath.Join(fk.roots.Home, "Library", "Application Support", "Blender", "5.2", "scripts", "addons", "c4_blender")
	write(t, filepath.Join(addon, "__pycache__", "panel.cpython-311.pyc"), "compiled")
	write(t, filepath.Join(addon, "vendor", "c4submit", "__pycache__", "farm.cpython-311.pyc"), "compiled")
	write(t, filepath.Join(addon, ".c4-enabled"), "turned on once\n")
	off := plan(t, f, false)
	for _, p := range []string{filepath.Join(addon, "__pycache__", "panel.cpython-311.pyc"), filepath.Join(addon, ".c4-enabled")} {
		if !contains(off.Removes, p) {
			t.Errorf("off doesn't list %s", p)
		}
	}
	apply(t, off)
	diff(t, "off after Blender ran", before, snapshot(t, fk.roots.Home))
}

func TestUnrealOnOff(t *testing.T) {
	fk := darwinApps(t)
	f := find(t, &fk.roots, "unreal", "5.7")
	init := filepath.Join(fk.roots.Home, "Documents", "UnrealEngine", "Python", "init_unreal.py")
	c := plan(t, f, true)
	notes := strings.Join(c.Notes, " ")
	if !strings.Contains(notes, "Default Remote Executor") || !strings.Contains(notes, "C4FarmExecutor") || !strings.Contains(notes, "newer than") {
		t.Errorf("notes: %v", c.Notes)
	}
	onOff(t, fk, f, func(t *testing.T) {
		s := readFile(t, init)
		target := filepath.Join(fk.roots.Home, ".c4", "integrations", "unreal", "unreal", "python", "init_unreal.py")
		if !strings.HasPrefix(s, unrealMark) || !strings.Contains(s, target) {
			t.Errorf("init_unreal.py:\n%s", s)
		}
		if _, err := os.Stat(target); err != nil {
			t.Error(err)
		}
		if _, err := os.Stat(filepath.Join(fk.roots.Home, ".c4", "integrations", "unreal", "c4submit", "src", "c4submit", "__init__.py")); err != nil {
			t.Error(err)
		}
	})
}

func TestUnrealRefusesSomeoneElsesInit(t *testing.T) {
	fk := darwinApps(t)
	init := filepath.Join(fk.roots.Home, "Documents", "UnrealEngine", "Python", "init_unreal.py")
	write(t, init, "import studio_tools\n")
	before := snapshot(t, fk.roots.Home)
	_, err := Plan(find(t, &fk.roots, "unreal", "5.7"), true)
	if err == nil || !strings.Contains(err.Error(), "init_unreal.py") || !strings.Contains(err.Error(), "won't overwrite") {
		t.Errorf("Plan: %v", err)
	}
	diff(t, "a refused plan", before, snapshot(t, fk.roots.Home))
}

func TestHoudiniOnOff(t *testing.T) {
	fk := darwinApps(t)
	onOff(t, fk, find(t, &fk.roots, "houdini", "21.0"), func(t *testing.T) {
		p := filepath.Join(fk.roots.Home, "Library", "Preferences", "houdini", "21.0", "packages", "c4.json")
		var pkg struct {
			Env   []map[string]any
			Hpath string
		}
		if err := json.Unmarshal([]byte(readFile(t, p)), &pkg); err != nil {
			t.Fatal(err)
		}
		if pkg.Env[0]["C4_DCC"] != filepath.ToSlash(filepath.Join(fk.roots.Home, ".c4", "integrations", "houdini")) || pkg.Hpath != "$C4_DCC/houdini" {
			t.Errorf("package: %+v", pkg)
		}
		if _, err := os.Stat(filepath.Join(fk.roots.Home, ".c4", "integrations", "houdini", "houdini", "toolbar", "c4.shelf")); err != nil {
			t.Error(err)
		}
	})
}

func TestMayaOnOff(t *testing.T) {
	fk := darwinApps(t)
	onOff(t, fk, find(t, &fk.roots, "maya", "2025"), func(t *testing.T) {
		mod := readFile(t, filepath.Join(fk.roots.Home, "Library", "Preferences", "Autodesk", "maya", "2025", "modules", "c4.mod"))
		if !strings.HasPrefix(mod, "+ c4 1.0 "+filepath.ToSlash(filepath.Join(fk.roots.Home, ".c4", "integrations", "maya", "maya"))+"\n") {
			t.Errorf("c4.mod:\n%s", mod)
		}
		if _, err := os.Stat(filepath.Join(fk.roots.Home, ".c4", "integrations", "maya", "maya", "scripts", "userSetup.py")); err != nil {
			t.Error(err)
		}
	})
}

func TestNukeOnOffWithTheirInit(t *testing.T) {
	fk := darwinApps(t)
	init := filepath.Join(fk.roots.Home, ".nuke", "init.py")
	theirs := "import nuke\nnuke.pluginAddPath('./gizmos')\n# no newline at the end" // their last line has no newline
	write(t, init, theirs)
	if err := os.Chmod(init, 0o600); err != nil {
		t.Fatal(err)
	}
	onOff(t, fk, find(t, &fk.roots, "nuke", "15.1"), func(t *testing.T) {
		s := readFile(t, init)
		line := "import nuke; nuke.pluginAddPath('" + filepath.Join(fk.roots.Home, ".c4", "integrations", "nuke", "nuke") + "')  # added by c4 farm integrate nuke\n"
		if s != theirs+"\n"+line {
			t.Errorf("init.py:\n%s", s)
		}
		if b := readFile(t, init+".c4-backup"); b != theirs {
			t.Errorf("backup:\n%s", b)
		}
		if st, _ := os.Stat(init); st.Mode().Perm() != 0o600 {
			t.Errorf("init.py's mode became %v", st.Mode().Perm())
		}
	})
}

func TestNukeOnOffWithoutInit(t *testing.T) {
	fk := darwinApps(t)
	onOff(t, fk, find(t, &fk.roots, "nuke", "15.1"), func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(fk.roots.Home, ".nuke", "init.py.c4-backup")); err == nil {
			t.Error("a backup of a file that didn't exist")
		}
	})
}

func TestNukeTwoVersionsShareTheLine(t *testing.T) {
	fk := darwinApps(t)
	init := filepath.Join(fk.roots.Home, ".nuke", "init.py")
	write(t, init, "import nuke\n")
	before := snapshot(t, fk.roots.Home)
	n15, n16 := find(t, &fk.roots, "nuke", "15.1"), find(t, &fk.roots, "nuke", "16.0")
	apply(t, plan(t, n15, true))
	withLine := readFile(t, init)
	c := plan(t, n16, true)
	if contains(c.Writes, init) {
		t.Errorf("Nuke 16 rewrites init.py though the line is there: %v", c.Writes)
	}
	apply(t, c)
	apply(t, plan(t, n15, false))
	if s := readFile(t, init); s != withLine {
		t.Errorf("turning 15.1 off took the line Nuke 16 uses:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(fk.roots.Home, ".c4", "integrations", "nuke", "nuke", "menu.py")); err != nil {
		t.Errorf("turning 15.1 off took the files Nuke 16 uses: %v", err)
	}
	apply(t, plan(t, n16, false))
	diff(t, "both off", before, snapshot(t, fk.roots.Home))
}

func TestKatanaOnOff(t *testing.T) {
	fk := newFake(t, "linux")
	mkdir(t, filepath.Join(fk.roots.Apps[1], "Katana8.0v1"))
	write(t, filepath.Join(fk.roots.Home, ".katana", "init.py"), "print('studio')\n")
	onOff(t, fk, find(t, &fk.roots, "katana", "8.0"), func(t *testing.T) {
		s := readFile(t, filepath.Join(fk.roots.Home, ".katana", "init.py"))
		if !strings.HasPrefix(s, "print('studio')\nimport runpy; runpy.run_path(") || !strings.HasSuffix(s, "# added by c4 farm integrate katana\n") {
			t.Errorf("init.py:\n%s", s)
		}
	})
}

func TestLinuxAndWindowsPlaces(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			fk := newFake(t, goos)
			pf := fk.roots.Apps[0]
			var blender, houdini, maya string
			if goos == "windows" {
				mkdir(t, filepath.Join(pf, "Blender Foundation", "Blender 4.5", "4.5"))
				mkdir(t, filepath.Join(pf, "Side Effects Software", "Houdini 20.5.613"))
				mkdir(t, filepath.Join(pf, "Autodesk", "Maya2025"))
				blender = filepath.Join(fk.roots.Config, "Blender Foundation", "Blender", "4.5", "scripts", "startup", "c4_farm_enable.py")
				houdini = filepath.Join(fk.roots.Home, "Documents", "houdini20.5", "packages", "c4.json")
				maya = filepath.Join(fk.roots.Home, "Documents", "maya", "2025", "modules", "c4.mod")
			} else {
				mkdir(t, filepath.Join(pf, "blender-4.5.1", "4.5"))
				mkdir(t, filepath.Join(pf, "hfs20.5.613"))
				mkdir(t, filepath.Join(fk.roots.Apps[3], "maya2025"))
				blender = filepath.Join(fk.roots.Config, "blender", "4.5", "scripts", "startup", "c4_farm_enable.py")
				houdini = filepath.Join(fk.roots.Home, "houdini20.5", "packages", "c4.json")
				maya = filepath.Join(fk.roots.Home, "maya", "2025", "modules", "c4.mod")
			}
			for _, w := range []struct{ name, version, file string }{
				{"blender", "4.5", blender}, {"houdini", "20.5", houdini}, {"maya", "2025", maya},
			} {
				c := plan(t, find(t, &fk.roots, w.name, w.version), true)
				if !contains(c.Writes, w.file) {
					t.Errorf("%s: no %s in %v", w.name, w.file, c.Writes)
				}
			}
		})
	}
}

func TestTooOldRefused(t *testing.T) {
	fk := newFake(t, "darwin")
	mkdir(t, filepath.Join(fk.roots.Apps[0], "Nuke13.2v5"))
	if _, err := Plan(find(t, &fk.roots, "nuke", "13.2"), true); err == nil || !strings.Contains(err.Error(), "older") {
		t.Errorf("Plan for Nuke 13.2: %v", err)
	}
}

func TestLeavesFilesTheyChangedAndRefusesTheirs(t *testing.T) {
	fk := darwinApps(t)
	f := find(t, &fk.roots, "houdini", "21.0")
	pkg := filepath.Join(fk.roots.Home, "Library", "Preferences", "houdini", "21.0", "packages", "c4.json")

	write(t, pkg, `{"hpath": "/studio/c4"}`)
	if _, err := Plan(f, true); err == nil || !strings.Contains(err.Error(), "didn't put there") {
		t.Errorf("Plan over their own c4.json: %v", err)
	}
	os.Remove(pkg)

	apply(t, plan(t, f, true))
	write(t, pkg, `{"hpath": "/edited"}`)
	if _, err := Plan(f, true); err == nil || !strings.Contains(err.Error(), "changed since c4 put it there") {
		t.Errorf("Plan over a c4.json they edited: %v", err)
	}
	off := plan(t, f, false)
	if contains(off.Removes, pkg) || !strings.Contains(strings.Join(off.Notes, " "), pkg) {
		t.Errorf("off would remove the c4.json they edited, or doesn't say it leaves it: %v %v", off.Removes, off.Notes)
	}
	apply(t, off)
	if readFile(t, pkg) != `{"hpath": "/edited"}` {
		t.Error("off touched the edited c4.json")
	}
}

func TestApplyStopsWhenThingsChangedSincePlan(t *testing.T) {
	fk := darwinApps(t)
	init := filepath.Join(fk.roots.Home, ".nuke", "init.py")
	write(t, init, "import nuke\n")
	c := plan(t, find(t, &fk.roots, "nuke", "15.1"), true)
	write(t, init, "import nuke\nprint('edited after the plan')\n")
	if err := Apply(c); err == nil || !strings.Contains(err.Error(), "since the plan") {
		t.Errorf("Apply after init.py changed: %v", err)
	}
	if readFile(t, init) != "import nuke\nprint('edited after the plan')\n" {
		t.Error("Apply touched init.py")
	}
}

func TestNewerReleaseReplacesOnlyItsOwnFiles(t *testing.T) {
	fk := darwinApps(t)
	f := find(t, &fk.roots, "maya", "2025")
	before := snapshot(t, fk.roots.Home)
	apply(t, plan(t, f, true))

	// Make it look like an earlier release's install: one file had other
	// contents, one file this release no longer ships.
	mpath := fk.roots.manifestPath("maya", "2025")
	m, err := readManifest(mpath)
	if err != nil {
		t.Fatal(err)
	}
	m.Release = "earlier"
	menu := filepath.Join(fk.roots.payloadDir("maya"), "maya", "scripts", "c4_maya", "menu.py")
	gone := filepath.Join(fk.roots.payloadDir("maya"), "maya", "scripts", "c4_maya", "retired.py")
	write(t, menu, "# the earlier menu\n")
	write(t, gone, "# a module this release dropped\n")
	for i := range m.Files {
		if m.Files[i].Path == menu {
			m.Files[i].SHA256 = digest([]byte("# the earlier menu\n"))
		}
	}
	m.Files = append(m.Files, fileEntry{Path: gone, SHA256: digest([]byte("# a module this release dropped\n"))})
	if err := writeManifest(mpath, m); err != nil {
		t.Fatal(err)
	}
	// Something of the person's beside c4's files stays.
	theirs := filepath.Join(fk.roots.Home, "Library", "Preferences", "Autodesk", "maya", "2025", "modules", "studio.mod")
	write(t, theirs, "+ studio 1.0 /studio\n")

	if st := find(t, &fk.roots, "maya", "2025").Status; st != StatusOlder {
		t.Errorf("status: %q", st)
	}
	c := plan(t, f, true)
	if !contains(c.Writes, menu) || !contains(c.Removes, gone) || len(c.Removes) != 1 {
		t.Errorf("upgrade plan: writes %v removes %v", c.Writes, c.Removes)
	}
	apply(t, c)
	if readFile(t, menu) != string(embeddedFile("maya/scripts/c4_maya/menu.py")) {
		t.Error("menu.py wasn't replaced")
	}
	if _, err := os.Stat(gone); err == nil {
		t.Error("the dropped module is still there")
	}
	if find(t, &fk.roots, "maya", "2025").Status != StatusInstalled {
		t.Error("not installed after the upgrade")
	}
	os.Remove(theirs)
	apply(t, plan(t, f, false))
	diff(t, "upgrade then off", before, snapshot(t, fk.roots.Home))
}

// An Apply that fails part-way leaves no half-written file, can be
// finished by running on again, and can be undone by off.
func TestInterruptedApply(t *testing.T) {
	for _, app := range []struct{ name, version string }{{"nuke", "15.1"}, {"blender", "5.2"}, {"unreal", "5.7"}} {
		for n := 1; ; n++ {
			fk := darwinApps(t)
			if app.name == "nuke" {
				write(t, filepath.Join(fk.roots.Home, ".nuke", "init.py"), "import nuke\n")
			}
			f := find(t, &fk.roots, app.name, app.version)
			before := snapshot(t, fk.roots.Home)
			c := plan(t, f, true)
			want := map[string][]byte{}
			for _, w := range c.writes {
				want[w.path] = w.data
			}
			for _, e := range c.edits {
				want[e.path] = e.data
			}
			count := 0
			failWrite = func(p string) error {
				count++
				if count == n {
					return errors.New("disk full")
				}
				return nil
			}
			err := Apply(c)
			failWrite = nil
			if err == nil {
				if n == 1 {
					t.Fatalf("%s: no write was failed", app.name)
				}
				break // every write has been failed once
			}
			for p, v := range snapshot(t, fk.roots.Home) {
				if strings.Contains(p, ".c4-tmp-") {
					t.Errorf("%s, write %d failed: a temporary file is left: %s", app.name, n, p)
				}
				if strings.HasPrefix(v, "dir ") || before[p] == v {
					continue
				}
				data, ours := want[p]
				if p == c.mpath {
					continue // checked below
				}
				if !ours || v[strings.Index(v, " ")+1:] != string(data) {
					t.Errorf("%s, write %d failed: %s is half-written or not c4's", app.name, n, p)
				}
			}
			// The manifest names only files that are there, with c4's contents.
			st := find(t, &fk.roots, app.name, app.version).Status
			if m, err := readManifest(c.mpath); err == nil {
				for _, e := range m.Files {
					if cur, exists, _ := fileState(e.Path); !exists || digest(cur) != e.SHA256 {
						t.Errorf("%s, write %d failed: the manifest names %s, which isn't there as recorded", app.name, n, e.Path)
					}
				}
				if len(m.Files) == 0 && len(m.Lines) == 0 {
					t.Errorf("%s, write %d failed: an empty manifest is left", app.name, n)
				}
				if st != StatusIncomplete {
					t.Errorf("%s, write %d failed: status %q", app.name, n, st)
				}
			} else if st != StatusAvailable {
				t.Errorf("%s, write %d failed: no manifest, but status %q", app.name, n, st)
			}
			if n%2 == 0 { // finish it, then take it out
				apply(t, plan(t, f, true))
				if st := find(t, &fk.roots, app.name, app.version).Status; st != StatusInstalled {
					t.Errorf("%s: status after finishing: %q", app.name, st)
				}
			}
			apply(t, plan(t, f, false))
			diff(t, fmt.Sprintf("%s, write %d failed, then off", app.name, n), before, snapshot(t, fk.roots.Home))
		}
	}
}

// Every runtime file in the panel folders is embedded (a new module can't
// be left out of the install), and nothing else is.
func TestEmbedsEveryRuntimeFile(t *testing.T) {
	want := map[string]bool{"LICENSE": true, "nuke/menu.py": true}
	for _, dir := range runtimeFolders {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() && (name == "__pycache__" || (strings.HasPrefix(name, ".") && p != dir)) {
				return filepath.SkipDir
			}
			if d.IsDir() || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".pyc") {
				return nil
			}
			want[filepath.ToSlash(p)] = true
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	for _, p := range embedded(".") {
		got[p] = true
	}
	if !reflect.DeepEqual(got, want) {
		for p := range want {
			if !got[p] {
				t.Errorf("not embedded: %s", p)
			}
		}
		for p := range got {
			if !want[p] {
				t.Errorf("embedded but not a runtime file: %s", p)
			}
		}
	}
	if !bytes.Contains(embeddedFile("c4submit/src/c4submit/__init__.py"), []byte("")) || len(Release()) != 16 {
		t.Error("release")
	}
}

// Off works from the manifest alone, after the application is gone.
func TestOffAfterTheApplicationIsUninstalled(t *testing.T) {
	fk := darwinApps(t)
	before := snapshot(t, fk.roots.Home)
	apply(t, plan(t, find(t, &fk.roots, "unreal", "5.7"), true))
	apply(t, plan(t, find(t, &fk.roots, "houdini", "21.0"), true))
	if err := os.RemoveAll(filepath.Join(fk.roots.Apps[0], "Houdini")); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range fk.roots.Installed() {
		got[f.Title] = f.Where + "|" + f.Status
		apply(t, plan(t, f, false))
	}
	want := map[string]string{
		"Houdini 21.0":      "|" + StatusInstalled,
		"Unreal Engine 5.7": filepath.Join(fk.root, "Shared", "Epic Games", "UE_5.7") + "|" + StatusInstalled,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Installed: %v", got)
	}
	diff(t, "off after uninstalling", before, snapshot(t, fk.roots.Home))
	if len(fk.roots.Installed()) != 0 {
		t.Error("still listed after off")
	}
}
