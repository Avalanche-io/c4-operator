package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// c4PathNote is a line for a person whose `c4` cannot run `c4 farm`: the
// first c4 on the PATH is an older one, from before c4 dispatched `farm`
// to c4-farm (a hand-installed or development build ahead of Homebrew's,
// say). It names a later one on the PATH that can, if there is one. It is
// empty when the first c4 lists `farm` in its help, or when there is no c4
// at all (then c4-farm is the command).
func c4PathNote(ctx context.Context) string {
	all := c4sOnPath()
	if len(all) == 0 || hasFarm(ctx, all[0]) {
		return ""
	}
	for _, p := range all[1:] {
		if hasFarm(ctx, p) {
			return "note      the c4 first on your PATH (" + tilde(all[0]) + ") has no `c4 farm`; " + tilde(p) +
				" does: put " + tilde(filepath.Dir(p)) + " ahead of " + tilde(filepath.Dir(all[0])) + " (which -a c4 shows the order), or run c4-farm"
		}
	}
	return "note      the c4 on your PATH (" + tilde(all[0]) + ") has no `c4 farm` yet: brew upgrade mrjoshuak/tap/c4 brings one that has, and until then c4-farm is the command"
}

// c4Exe is c4's file name here.
var c4Exe = map[bool]string{true: "c4.exe", false: "c4"}[runtime.GOOS == "windows"]

// c4sOnPath are the c4 executables on the PATH, first first, as which -a
// lists them.
func c4sOnPath() []string {
	var out []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, c4Exe)
		fi, err := os.Stat(p)
		// Windows keeps no execute bits; a c4.exe is one
		if err != nil || fi.IsDir() || (runtime.GOOS != "windows" && fi.Mode()&0o111 == 0) || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// hasFarm says c4's help lists `farm`, as a c4 that dispatches it does.
func hasFarm(ctx context.Context, c4 string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, c4, "help").CombinedOutput()
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) > 1 && (f[0] == "farm" || (f[0] == "c4" && f[1] == "farm")) {
			return true
		}
	}
	return false
}

func tilde(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}
