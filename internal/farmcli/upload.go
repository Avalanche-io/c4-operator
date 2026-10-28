package farmcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// What a folder sends (SUBMIT 3.2): every folder upload (submit, tool
// publish) goes by one rule, in the client, before the scan. .git is never
// sent; in a git working tree, the repository's own .gitignore files apply,
// by git's rules, nested files and negations included, and never the
// person's global excludes or .git/info/exclude, so one checkout is one
// tree and one C4 ID on every machine; a folder that is no working tree is
// sent whole. --all sends everything but .git.

// uploadScan is a folder as an upload sends it: its view, and how many
// files the rule left out.
type uploadScan struct {
	v    *view.View
	left int
}

// scanUpload scans dir as an upload sends it; all keeps every file but
// .git's.
func scanUpload(ctx context.Context, dir string, all bool) (uploadScan, error) {
	var listed map[string]bool
	if !all {
		var err error
		if listed, err = gitListed(ctx, dir); err != nil {
			return uploadScan{}, err
		}
	}
	// a directory is read only when something it holds is kept
	dirs := map[string]bool{}
	for f := range listed {
		for d := filepath.ToSlash(filepath.Dir(f)); d != "." && !dirs[d]; d = filepath.ToSlash(filepath.Dir(d)) {
			dirs[d] = true
		}
	}
	left := 0
	// a directory git lists itself (a submodule) goes whole
	var whole []string
	inWhole := func(rel string) bool {
		for _, w := range whole {
			if strings.HasPrefix(rel, w+"/") {
				return true
			}
		}
		return false
	}
	keep := func(rel string, isDir bool) bool {
		base := rel[strings.LastIndexByte(rel, '/')+1:]
		switch {
		case base == ".git":
			return false
		case listed == nil, inWhole(rel):
			return true
		case isDir && listed[rel]:
			whole = append(whole, rel)
			return true
		case isDir:
			if !dirs[rel] {
				left += countFiles(filepath.Join(dir, filepath.FromSlash(rel)))
				return false
			}
			return true
		case !listed[rel]:
			left++
			return false
		}
		return true
	}
	v, err := view.ScanDirKeep(dir, keep)
	return uploadScan{v: v, left: left}, err
}

// gitListed is what git would keep of dir, when dir is in a git working
// tree: tracked files, and untracked ones the repository's .gitignore
// files do not exclude; nil when dir is no working tree (or git is not
// here), which sends it whole.
func gitListed(ctx context.Context, dir string) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := gitIn(ctx, dir, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return nil, nil
	}
	// --exclude-per-directory reads .gitignore alone: not core.excludesFile
	// nor .git/info/exclude, which differ from one machine to the next
	out, err := gitIn(ctx, dir, "-c", "core.untrackedCache=false", "ls-files", "-z",
		"--cached", "--others", "--exclude-per-directory=.gitignore").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, errors.New("git ls-files: " + strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	listed := map[string]bool{}
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			listed[string(f)] = true
		}
	}
	// a folder the repository ignores whole is no part of its tree: it is
	// sent whole, as a folder outside any repository is
	if len(listed) == 0 {
		return nil, nil
	}
	return listed, nil
}

// gitIn is git run in dir, a folder that may be someone else's: a
// tarball's .git/config may set core.fsmonitor, a command git runs when it
// reads the index, and the person is only sending the folder. The system's
// config is not read, so the rule is the same on every machine.
func gitIn(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	return cmd
}

// treeStem is the stem of the jobs of work on no shot (SUBMIT's name):
// the repository's name, from its origin remote, so two checkouts of one
// repository share one series; else the folder's name. "" when neither
// makes a name.
func treeStem(ctx context.Context, dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	name := filepath.Base(abs)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := gitIn(ctx, abs, "remote", "get-url", "origin").Output(); err == nil {
		// https://host/team/textkit.git, git@host:team/textkit.git, a path
		u := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(string(out)), "/"), ".git")
		if i := strings.LastIndexAny(u, "/:\\"); i < len(u)-1 {
			name = u[i+1:]
		}
	}
	stem := jobStem(name)
	if len(stem) > 40 {
		stem = strings.TrimRight(stem[:40], "-")
	}
	return stem
}

// jobStem is s as a stem of job names: lower-case letters, digits and -.
func jobStem(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// allowAll refuses --all where no person chose it: it sends what the
// repository's .gitignore leaves out, and an agent may have been steered
// to a folder by something it read.
func allowAll(all, terminal bool) error {
	switch {
	case !all:
		return nil
	case InsideAgent():
		return errors.New("--all: an agent sends a folder as its .gitignore files say; to send everything, run it yourself at a terminal")
	case !terminal:
		return errors.New("--all sends what .gitignore leaves out, so it is for a person at a terminal")
	}
	return nil
}

// leftOut is the files line's note of what .gitignore left out.
func leftOut(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" · %d left out by .gitignore", n)
}

// countFiles is how many files a folder left out holds, for the line that
// says so.
func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
