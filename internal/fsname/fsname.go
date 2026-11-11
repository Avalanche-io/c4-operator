// Package fsname says whether a filesystem can hold a c4m name as it is.
//
// A c4m name is any bytes but "/" and NUL (Joshua, 2026-11-10: "C4m files
// only considers forward slash a path seperator. Everything else is a
// literal or encodable character"). Where a platform cannot hold a name,
// the writer refuses that entry, says which and why, and writes the rest:
// it never renames it, maps it, or joins it into a path, as git does
// (Joshua: "I think we should probably follow the git rules"). But git's
// set is the most a writer may refuse, not the rule (Joshua: "if git is
// over cautious in places where c4 can round trip successfuly then we
// should use ours"), so each refusal here is cited: to git's code where
// nothing has been measured to beat it, or to a measurement.
package fsname

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Refusal says why a filesystem on goos cannot hold name (one path
// segment) as it is, or "" when it can. Linux holds every byte but "/"
// and NUL, which c4m names never have. macOS (APFS) holds no name that is
// not UTF-8 (measured: "Illegal byte sequence"); it refuses some UTF-8 too
// (noncharacters such as U+FFFE, unassigned code points), which is left
// to the write to fail on, loudly. Windows is git's rule.
func Refusal(goos, name string) string {
	switch goos {
	case "windows":
		return windows(name)
	case "darwin", "ios":
		if !utf8.ValidString(name) {
			return "APFS holds no name that is not UTF-8 (measured)"
		}
	}
	return ""
}

// TargetRefusal says why a link's target, a slash-separated path the
// filesystem resolves, cannot be written as it is on goos: on Windows any
// segment Refusal refuses; elsewhere never, since a target is bytes kept
// as they are.
func TargetRefusal(goos, target string) (segment, reason string) {
	if goos != "windows" {
		return "", ""
	}
	return PathRefusal(goos, target)
}

// PathRefusal applies Refusal to each segment of a slash-separated path,
// and returns the first segment refused and why.
func PathRefusal(goos, path string) (segment, reason string) {
	for _, seg := range strings.Split(strings.TrimSuffix(path, "/"), "/") {
		if r := Refusal(goos, seg); r != "" {
			return seg, r
		}
	}
	return "", ""
}

// windows is git's rule for a path segment Git for Windows will not write,
// as of git v2.47.0: compat/mingw.c is_valid_win32_path (characters, a
// trailing space or dot, the reserved device names) and read-cache.c
// verify_path_internal (a backslash, refused on native Windows). Git's
// repository guards (is_ntfs_dotgit: ".git" look-alikes, "GIT~1") are not
// taken: they protect a git repository, not the filesystem.
//
// Not measured yet: a trailing space or dot and the device names can be
// written through \\?\ paths and may read back exactly; until a Windows
// writer measures that, git's rule stands for them (fsname_test.go names
// which rules are cited and which are measured).
func windows(name string) string {
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '\\':
			return "a backslash is a separator on Windows (git v2.47.0, read-cache.c verify_path_internal)"
		case strings.IndexByte(`:<>"|?*`, c) >= 0:
			return fmt.Sprintf("%q is not allowed in a Windows name (git v2.47.0, compat/mingw.c is_valid_win32_path)", c)
		case c > 0 && c < 0x20:
			return fmt.Sprintf("a control character (0x%02x) is not allowed in a Windows name (git v2.47.0, compat/mingw.c is_valid_win32_path)", c)
		}
	}
	if name != "." && name != ".." && (strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".")) {
		return "a Windows name cannot end in a space or a dot (git v2.47.0, compat/mingw.c is_valid_win32_path; not measured)"
	}
	if d := device(name); d != "" {
		return fmt.Sprintf("%s is a reserved device name on Windows, with any extension (git v2.47.0, compat/mingw.c is_valid_win32_path; not measured)", d)
	}
	return ""
}

// device returns the reserved device name name begins with, as git
// matches it: case-insensitively, followed by nothing, spaces, an
// extension or a stream (".", ":"); COM1 to COM9, LPT0 to LPT9.
func device(name string) string {
	upper := strings.ToUpper(name)
	stem := upper
	if i := strings.IndexAny(stem, ".:"); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.TrimRight(stem, " ")
	switch stem {
	case "AUX", "CON", "CONIN$", "CONOUT$", "NUL", "PRN":
		return stem
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") && stem[3] >= '1' && stem[3] <= '9' ||
		strings.HasPrefix(stem, "LPT") && stem[3] >= '0' && stem[3] <= '9') {
		return stem
	}
	return ""
}
