package fsname

import (
	"strings"
	"testing"
)

// Each rule, with where it comes from. "git" is git v2.47.0's own rule
// (compat/mingw.c is_valid_win32_path, read-cache.c verify_path_internal),
// taken as the most a writer may refuse; a rule beaten by a measurement on
// Windows moves to "measured" and is no longer refused.
func TestWindowsRefusesWhatGitDoes(t *testing.T) {
	for _, c := range []struct {
		name, from string
		refused    bool
	}{
		{`back\slash.txt`, "git", true},
		{`a:b`, "git", true},
		{`C:foo`, "git", true},
		{`a<b`, "git", true},
		{`a>b`, "git", true},
		{`a"b`, "git", true},
		{`a|b`, "git", true},
		{`a?b`, "git", true},
		{`a*b`, "git", true},
		{"tab\there", "git", true},
		{"a\x1fb", "git", true},
		{"a.", "git", true},
		{"a ", "git", true},
		{"...", "git", true},
		{"CON", "git", true},
		{"con.txt", "git", true},
		{"Nul", "git", true},
		{"AUX ", "git", true},
		{"CON .txt", "git", true},
		{"CONIN$ .x", "git", true},
		{"nul:stream", "git", true},
		{"PRN.tar.gz", "git", true},
		{"CONIN$", "git", true},
		{"conout$.log", "git", true},
		{"COM1", "git", true},
		{"lpt0", "git", true},
		{"LPT9.x", "git", true},
		// Not refused, as git does not.
		{".", "git", false},
		{"..", "git", false},
		{"COM0", "git", false},
		{"NULL", "git", false},
		{"CONIN", "git", false},
		{"CON$", "git", false},
		{"PRN1", "git", false},
		{"a b", "git", false},
		{".hidden", "git", false},
		{"a\x7fb", "git", false},
		{"ünïcödé", "git", false},
		// Git's repository guards are not taken: they protect a git
		// repository, not the filesystem.
		{".git", "ours", false},
		{"GIT~1", "ours", false},
		{".gitmodules", "ours", false},
	} {
		got := Refusal("windows", c.name)
		if (got != "") != c.refused {
			t.Errorf("%q (%s): refused %q, want refused %v", c.name, c.from, got, c.refused)
		}
		if got != "" && !strings.Contains(got, "git v2.47.0") {
			t.Errorf("%q: the refusal does not cite its rule: %q", c.name, got)
		}
		// Off Windows nothing that is UTF-8 is refused.
		for _, goos := range []string{"linux", "darwin"} {
			if r := Refusal(goos, c.name); r != "" {
				t.Errorf("%q on %s: refused %q", c.name, goos, r)
			}
		}
	}
}

func TestPathRefusalNamesTheSegment(t *testing.T) {
	if seg, r := PathRefusal("windows", "shots/sh010/a:b/render.exr"); seg != "a:b" || r == "" {
		t.Fatalf("got %q, %q", seg, r)
	}
	if seg, r := PathRefusal("windows", "shots/sh010/"); seg != "" || r != "" {
		t.Fatalf("a directory path that Windows holds: %q, %q", seg, r)
	}
	if seg, r := PathRefusal("linux", `a\b/c:d`); seg != "" || r != "" {
		t.Fatalf("on Linux: %q, %q", seg, r)
	}
}

// Linux holds any bytes; APFS holds only UTF-8 names.
func TestNamesThatAreNotUTF8(t *testing.T) {
	bad := "caf\xe9.txt"
	if r := Refusal("linux", bad); r != "" {
		t.Fatalf("Linux: %q", r)
	}
	if r := Refusal("darwin", bad); !strings.Contains(r, "UTF-8") {
		t.Fatalf("macOS: %q", r)
	}
	if r := Refusal("darwin", "café.txt"); r != "" {
		t.Fatalf("macOS, UTF-8: %q", r)
	}
}
