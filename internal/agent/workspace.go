package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Workspace records a member's working tree at a checkpoint.
//
// Mode git (the default inside a git repository) records HEAD and the C4 ID
// of everything uncommitted: the binary diff against HEAD plus each
// untracked file. It runs only read commands, so a checkpoint never writes
// to the repository. Mode view hashes the whole tree as a C4 view (exact,
// and costly on a large tree). Mode none records nothing.
type Workspace struct {
	Mode string `json:"mode"`
	// Head is the commit checked out (mode git).
	Head string `json:"head,omitempty"`
	// Dirty is the C4 ID of the uncommitted changes (mode git); empty when
	// the tree is clean.
	Dirty string `json:"dirty,omitempty"`
	// Untracked counts untracked files included in Dirty.
	Untracked int `json:"untracked,omitempty"`
	// View is the tree's view ID (mode view).
	View string `json:"view,omitempty"`
	// Err notes a workspace that could not be recorded; the checkpoint is
	// still written, since the transcript is the member's state.
	Err string `json:"err,omitempty"`
}

// maxUntracked bounds how many untracked files a git checkpoint hashes.
const maxUntracked = 2000

// SealWorkspace records dir in the given mode ("", git, view or none). An
// empty mode means git when dir is in a git repository, else none.
func SealWorkspace(ctx context.Context, dir, mode string) Workspace {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if mode == "" {
		mode = "none"
		if _, err := git(ctx, dir, "rev-parse", "--is-inside-work-tree"); err == nil {
			mode = "git"
		}
	}
	w := Workspace{Mode: mode}
	switch mode {
	case "none":
	case "view":
		id, err := view.ScanID(dir)
		if err != nil {
			w.Err = err.Error()
			break
		}
		w.View = id.String()
	case "git":
		if err := sealGit(ctx, dir, &w); err != nil {
			w.Err = err.Error()
		}
	default:
		w.Err = fmt.Sprintf("unknown workspace mode %q", mode)
	}
	return w
}

func sealGit(ctx context.Context, dir string, w *Workspace) error {
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	w.Head = strings.TrimSpace(string(head))
	diff, err := git(ctx, dir, "diff", "--binary", "HEAD")
	if err != nil {
		return err
	}
	others, err := git(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	var paths []string
	for _, p := range bytes.Split(others, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	sort.Strings(paths)
	if len(paths) > maxUntracked {
		return fmt.Errorf("%d untracked files, more than %d", len(paths), maxUntracked)
	}
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root := strings.TrimSpace(string(top))
	var list strings.Builder
	for _, p := range paths {
		id, _, err := view.HashFile(filepath.Join(root, p))
		if err != nil {
			continue // removed since it was listed
		}
		fmt.Fprintf(&list, "%s %s\n", id, p)
	}
	w.Untracked = len(paths)
	if len(diff) == 0 && len(paths) == 0 {
		return nil
	}
	w.Dirty = chainHead(identify(diff), identify([]byte(list.String())))
	return nil
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
