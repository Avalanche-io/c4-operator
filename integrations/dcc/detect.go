package dcc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Found is one installed version of an application C4 can integrate with.
type Found struct {
	Name    string // "blender", "unreal", "houdini", "maya", "nuke" or "katana"
	Title   string // what to call it, such as "Blender 4.5"
	Version string // the version its settings are kept by, such as "4.5", "21.0" or "2025"
	Where   string // where it is installed (read from files; it is never launched)
	Status  string // StatusAvailable, StatusInstalled, StatusOlder or StatusIncomplete

	roots *Roots
}

// What Found.Status says.
const (
	StatusAvailable  = "available"             // C4 is not in it
	StatusInstalled  = "installed"             // this c4 release's files are in it
	StatusOlder      = "installed, older"      // another c4 release's files are in it; on replaces them
	StatusIncomplete = "installed, incomplete" // an install was interrupted; on finishes it, off takes it out
)

// Roots are the folders Detect looks in and Plan writes under. DefaultRoots
// gives this machine's; tests give fake ones.
type Roots struct {
	// OS is "darwin", "linux" or "windows": which layout to expect.
	OS string
	// Home is the person's home folder. Every write is under it.
	Home string
	// Config is where per-user application settings live: on Windows
	// %APPDATA%, on Linux $XDG_CONFIG_HOME or ~/.config. Unused on macOS.
	Config string
	// Apps are folders applications are installed in: on macOS
	// /Applications and ~/Applications; on Linux /opt, /opt/Foundry,
	// /usr/local, /usr/autodesk and /usr/share; on Windows the Program
	// Files folders.
	Apps []string
	// Steam are Steam libraries' steamapps/common folders.
	Steam []string
	// Epic are the Epic Games Launcher's LauncherInstalled.dat files,
	// which list the Unreal Engines it installed.
	Epic []string
	// Guarded are folders the system asks the person about before any
	// program reads them: on macOS ~/Documents, ~/Desktop and ~/Downloads.
	// A plan to add C4 where its files go under one lists them without
	// looking there; Apply looks, after the person has said yes.
	Guarded []string
}

// DefaultRoots are this machine's roots.
func DefaultRoots() Roots {
	home, _ := os.UserHomeDir()
	r := Roots{OS: runtime.GOOS, Home: home}
	switch r.OS {
	case "darwin":
		r.Apps = []string{"/Applications", filepath.Join(home, "Applications")}
		r.Steam = []string{filepath.Join(home, "Library", "Application Support", "Steam", "steamapps", "common")}
		r.Epic = []string{filepath.Join(home, "Library", "Application Support", "Epic", "UnrealEngineLauncher", "LauncherInstalled.dat")}
		for _, d := range []string{"Documents", "Desktop", "Downloads"} {
			r.Guarded = append(r.Guarded, filepath.Join(home, d))
		}
	case "windows":
		r.Config = os.Getenv("APPDATA")
		if r.Config == "" {
			r.Config = filepath.Join(home, "AppData", "Roaming")
		}
		for _, v := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
			if d := os.Getenv(v); d != "" {
				r.Apps = append(r.Apps, d)
			}
		}
		if len(r.Apps) == 0 {
			r.Apps = []string{`C:\Program Files`, `C:\Program Files (x86)`}
		}
		for _, a := range r.Apps {
			r.Steam = append(r.Steam, filepath.Join(a, "Steam", "steamapps", "common"))
		}
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		r.Epic = []string{filepath.Join(pd, "Epic", "UnrealEngineLauncher", "LauncherInstalled.dat")}
	default:
		r.Config = os.Getenv("XDG_CONFIG_HOME")
		if r.Config == "" {
			r.Config = filepath.Join(home, ".config")
		}
		r.Apps = []string{"/opt", "/opt/Foundry", "/usr/local", "/usr/autodesk", "/usr/share"}
		r.Steam = []string{
			filepath.Join(home, ".steam", "steam", "steamapps", "common"),
			filepath.Join(home, ".local", "share", "Steam", "steamapps", "common"),
		}
	}
	return r
}

// Detect lists every supported application installed on this machine.
func Detect() []Found {
	r := DefaultRoots()
	return r.Detect()
}

// Detect lists every supported application under the roots, reading files
// only: folder listings, Info.plists and the Epic launcher's list. It
// never launches an application or runs anything.
func (r *Roots) Detect() []Found {
	var all []Found
	all = append(all, r.blender()...)
	all = append(all, r.unreal()...)
	all = append(all, r.houdini()...)
	all = append(all, r.maya()...)
	all = append(all, r.nuke()...)
	all = append(all, r.katana()...)

	seen := map[string]bool{}
	var out []Found
	for _, f := range all {
		k := f.Name + "-" + f.Version
		if f.Version == "" || seen[k] {
			continue
		}
		seen[k] = true
		f.roots = r
		f.Status = r.status(f.Name, f.Version)
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return versionLess(out[i].Version, out[j].Version)
	})
	return out
}

// titles are what each application is called.
var titles = map[string]string{
	"blender": "Blender",
	"unreal":  "Unreal Engine",
	"houdini": "Houdini",
	"maya":    "Maya",
	"nuke":    "Nuke",
	"katana":  "Katana",
}

// Installed lists every integration c4 has recorded on this machine, from
// the manifests in ~/.c4/integrations, whether or not the application is
// still installed (Where is then ""), so off works after an application
// has been uninstalled.
func Installed() []Found {
	r := DefaultRoots()
	return r.Installed()
}

// Installed lists the integrations recorded under these roots.
func (r *Roots) Installed() []Found {
	where := map[string]string{}
	for _, f := range r.Detect() {
		where[f.Name+"-"+f.Version] = f.Where
	}
	ps, _ := filepath.Glob(filepath.Join(r.manifestDir(), "*.json"))
	var out []Found
	for _, p := range ps {
		m, err := readManifest(p)
		if err != nil || titles[m.Name] == "" || p != r.manifestPath(m.Name, m.Version) {
			continue
		}
		out = append(out, Found{
			Name: m.Name, Title: titles[m.Name] + " " + m.Version, Version: m.Version,
			Where: where[m.Name+"-"+m.Version], Status: r.status(m.Name, m.Version), roots: r,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return versionLess(out[i].Version, out[j].Version)
	})
	return out
}

func (r *Roots) status(name, version string) string {
	m, err := readManifest(r.manifestPath(name, version))
	switch {
	case err != nil:
		return StatusAvailable
	case !m.Complete:
		return StatusIncomplete
	case m.Release != release:
		return StatusOlder
	}
	return StatusInstalled
}

// glob lists the folders (or files, if files is set) under each root
// matching pattern, sorted, without following anything it can't read.
func glob(roots []string, pattern string, files bool) []string {
	var out []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		ms, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, m := range ms {
			st, err := os.Stat(m)
			if err != nil || st.IsDir() == files {
				continue
			}
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

var (
	majorMinor = regexp.MustCompile(`^(\d+)\.(\d+)`)
	bareMM     = regexp.MustCompile(`^\d+\.\d+$`)
)

// mm is a version's major.minor ("4.5" of "4.5.3"), or "".
func mm(v string) string {
	m := majorMinor.FindStringSubmatch(v)
	if m == nil {
		return ""
	}
	return m[1] + "." + m[2]
}

// versionLess compares dotted versions number by number.
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, ex := strconv.Atoi(as[i])
		y, ey := strconv.Atoi(bs[i])
		if ex != nil || ey != nil {
			if as[i] != bs[i] {
				return as[i] < bs[i]
			}
			continue
		}
		if x != y {
			return x < y
		}
	}
	return len(as) < len(bs)
}

// versionFolders are a folder's children named like "4.5" (Blender keeps
// its own scripts in one, inside the install).
func versionFolders(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && bareMM.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// appVersion is a macOS bundle's version: CFBundleShortVersionString from
// its Info.plist, or "" if the plist can't be read.
func appVersion(app string) string {
	v, err := plistString(filepath.Join(app, "Contents", "Info.plist"), "CFBundleShortVersionString")
	if err != nil {
		return ""
	}
	return v
}

func (r *Roots) blender() []Found {
	var out []Found
	add := func(version, where string) {
		if v := mm(version); v != "" {
			out = append(out, Found{Name: "blender", Title: "Blender " + v, Version: v, Where: where})
		}
	}
	if r.OS == "darwin" {
		apps := glob(r.Apps, "Blender*.app", false)
		apps = append(apps, glob(r.Steam, filepath.Join("Blender", "Blender.app"), false)...)
		for _, app := range apps {
			v := appVersion(app)
			if v == "" { // a plist this can't read: the bundle keeps its scripts in Resources/<major.minor>
				if vs := versionFolders(filepath.Join(app, "Contents", "Resources")); len(vs) == 1 {
					v = vs[0]
				}
			}
			add(v, app)
		}
		return out
	}
	// Linux and Windows: an install folder holds a <major.minor> folder of
	// Blender's own scripts.
	var dirs []string
	if r.OS == "windows" {
		dirs = glob(r.Apps, filepath.Join("Blender Foundation", "Blender*"), false)
	} else {
		dirs = glob(r.Apps, "blender*", false)
	}
	dirs = append(dirs, glob(r.Steam, "Blender", false)...)
	for _, d := range dirs {
		for _, v := range versionFolders(d) {
			add(v, d)
		}
	}
	return out
}

func (r *Roots) unreal() []Found {
	var out []Found
	for _, dat := range r.Epic {
		b, err := os.ReadFile(dat)
		if err != nil {
			continue
		}
		var list struct {
			InstallationList []struct {
				InstallLocation string
				AppName         string
				AppVersion      string
			}
		}
		b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) // a byte-order mark, as Windows editors write
		if json.Unmarshal(b, &list) != nil {
			continue
		}
		for _, e := range list.InstallationList {
			v, ok := strings.CutPrefix(e.AppName, "UE_")
			if !ok || !bareMM.MatchString(v) || !isDir(e.InstallLocation) {
				continue // a plug-in (FabPlugin_5.6) or an engine since removed
			}
			out = append(out, Found{Name: "unreal", Title: "Unreal Engine " + v, Version: v, Where: e.InstallLocation})
		}
	}
	return out
}

var houdiniVer = regexp.MustCompile(`^(?:Houdini ?|hfs)(\d+\.\d+)(?:\.\d+)?$`)

func (r *Roots) houdini() []Found {
	var dirs []string
	switch r.OS {
	case "darwin":
		dirs = glob(r.Apps, filepath.Join("Houdini", "Houdini*"), false)
	case "windows":
		dirs = glob(r.Apps, filepath.Join("Side Effects Software", "Houdini *"), false)
	default:
		dirs = glob(r.Apps, "hfs*", false)
	}
	var out []Found
	// Newest build first, so two builds of 21.0 report the newer one.
	sort.SliceStable(dirs, func(i, j int) bool { return versionLess(houdiniBuild(dirs[j]), houdiniBuild(dirs[i])) })
	for _, d := range dirs {
		m := houdiniVer.FindStringSubmatch(filepath.Base(d))
		if m == nil {
			continue
		}
		out = append(out, Found{Name: "houdini", Title: "Houdini " + m[1], Version: m[1], Where: d})
	}
	return out
}

func houdiniBuild(dir string) string {
	b := filepath.Base(dir)
	b = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(b, "Houdini"), " "), "hfs")
	return b
}

var mayaVer = regexp.MustCompile(`^(?i:maya)(\d{4})$`)

func (r *Roots) maya() []Found {
	var dirs []string
	switch r.OS {
	case "darwin", "windows":
		dirs = glob(r.Apps, filepath.Join("Autodesk", "[Mm]aya*"), false)
	default:
		dirs = glob(r.Apps, "maya*", false)
	}
	var out []Found
	for _, d := range dirs {
		m := mayaVer.FindStringSubmatch(filepath.Base(d))
		if m == nil {
			continue
		}
		out = append(out, Found{Name: "maya", Title: "Maya " + m[1], Version: m[1], Where: d})
	}
	return out
}

// foundryVer reads Nuke's and Katana's folder names: Nuke15.1v3, Katana8.0v1.
var foundryVer = regexp.MustCompile(`^(?:Nuke|Katana) ?(\d+\.\d+)v\d+$`)

func (r *Roots) foundry(name, prefix string) []Found {
	dirs := glob(r.Apps, prefix+"*", false)
	sort.SliceStable(dirs, func(i, j int) bool { return dirs[i] > dirs[j] })
	var out []Found
	for _, d := range dirs {
		m := foundryVer.FindStringSubmatch(filepath.Base(d))
		if m == nil {
			continue
		}
		out = append(out, Found{Name: name, Title: titles[name] + " " + m[1], Version: m[1], Where: d})
	}
	return out
}

func (r *Roots) nuke() []Found { return r.foundry("nuke", "Nuke") }

func (r *Roots) katana() []Found {
	if r.OS == "darwin" { // Katana is made for Linux and Windows only
		return nil
	}
	return r.foundry("katana", "Katana")
}
