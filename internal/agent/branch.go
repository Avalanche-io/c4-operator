package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A block on a host works on a branch of its own: a git worktree of the
// job's repository (C4_BLOCK_REPO, the folder the work was submitted from),
// on c4/<job>/<unit>, made at the unit's first attempt from the repository's
// HEAD and kept for its later ones. Its commits are the unit's output: the
// runner records the branch, its base and its head with the unit, and the
// branch is already in the developer's own repository to switch to, merge
// or drop. The worktree's folder goes once the unit succeeds; the branch
// stays.

// AnnotationOutputBranch is a unit's branch as the runner records it on its
// pod: JSON {repo, branch, base, head, commits}.
const AnnotationOutputBranch = "c4.avalanche.io/output-branch"

// branchFile is the same record in the unit's output.
const branchFile = "branch.json"

// UnitBranch is a block's branch and worktree.
type UnitBranch struct {
	Repo    string `json:"repo"`
	Dir     string `json:"dir,omitempty"`
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	Head    string `json:"head,omitempty"`
	Commits int    `json:"commits"`
}

var unsafeRef = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// blockBranch is where a block's branch and worktree are, from its pod: ok
// is false for a block with no repository.
func blockBranch(p *corev1.Pod) (UnitBranch, bool) {
	repo := strings.TrimSpace(podEnv(p, EnvBlockRepo))
	if repo == "" {
		return UnitBranch{}, false
	}
	if real, err := filepath.EvalSymlinks(repo); err == nil {
		repo = real
	}
	job := unsafeRef.ReplaceAllString(p.Labels[c4v1.LabelJob], "-")
	unit := podEnv(p, "C4_UNIT_NAME")
	if unit == "" {
		unit = p.Labels[c4v1.LabelUnit]
	}
	unit = unsafeRef.ReplaceAllString(unit, "-")
	if job == "" || unit == "" {
		return UnitBranch{}, false
	}
	return UnitBranch{Repo: repo, Branch: "c4/" + job + "/" + unit, Dir: filepath.Join(repo, ".worktrees", "c4-"+job+"-"+unit)}, true
}

// gitLine runs git in dir and returns its output, trimmed.
func gitLine(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := git(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

// ensureBranch makes a block's worktree, on its own branch, if it is not
// there yet, and returns it; ok is false when the block's repository is not
// a git repository (it then works in the folder as it is).
func ensureBranch(ctx context.Context, p *corev1.Pod) (UnitBranch, bool, error) {
	b, ok := blockBranch(p)
	if !ok {
		return b, false, nil
	}
	if repoTooBroad(b.Repo) {
		// a lead names the folder: never the whole disk or the home folder
		return b, false, fmt.Errorf("its repository %s is a whole disk, the home folder or a folder above it, which is no project's: a worker works in a project's folder", b.Repo)
	}
	top, err := gitLine(ctx, b.Repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return b, false, nil // not a repository: no branch of its own
	}
	if err := excludeWorktrees(ctx, top, b.Repo); err != nil {
		return b, false, err
	}
	key := "branch." + b.Branch + ".c4base"
	if isDir(b.Dir) {
		b.Base, _ = gitLine(ctx, b.Repo, "config", "--get", key)
		return b, true, nil
	}
	if _, err := gitLine(ctx, b.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+b.Branch); err == nil {
		// the branch from an earlier attempt, its folder gone: check it out again
		if _, err := gitLine(ctx, b.Repo, "worktree", "add", b.Dir, b.Branch); err != nil {
			return b, false, err
		}
		b.Base, _ = gitLine(ctx, b.Repo, "config", "--get", key)
		return b, true, nil
	}
	base, err := branchBase(ctx, b.Repo, podEnv(p, EnvBlockBase))
	if err != nil {
		return b, false, err
	}
	if _, err := gitLine(ctx, b.Repo, "worktree", "add", "-b", b.Branch, b.Dir, base); err != nil {
		return b, false, err
	}
	if _, err := gitLine(ctx, b.Repo, "config", key, base); err != nil {
		return b, false, err
	}
	b.Base = base
	return b, true, nil
}

// branchBase is the commit a block's new branch starts from: the one its
// pod names (C4_BLOCK_BASE), which must be a commit in repo, else the
// repository's HEAD.
func branchBase(ctx context.Context, repo, want string) (string, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		base, err := gitLine(ctx, repo, "rev-parse", "HEAD")
		if err != nil {
			return "", fmt.Errorf("%s has no commit to branch from: %w", repo, err)
		}
		return base, nil
	}
	// a name from the lead, never an option to git
	if strings.HasPrefix(want, "-") || strings.ContainsAny(want, " \t\n") {
		return "", fmt.Errorf("its base %q is not a commit in %s", want, repo)
	}
	base, err := gitLine(ctx, repo, "rev-parse", "--verify", "--quiet", "--end-of-options", want+"^{commit}")
	if err != nil || base == "" {
		return "", fmt.Errorf("its base %s is not a commit in %s", want, repo)
	}
	return base, nil
}

// repoTooBroad says a block's repository is no project's folder: a disk's
// root, the home folder, or a folder the home folder is inside, links
// resolved (as c4 farm's own folder guard says, internal/farmcli). A
// lead's call names the folder, so the runner holds it to one; it is a
// guard against a wrong or steered folder, not a boundary.
func repoTooBroad(repo string) bool {
	d, err := filepath.EvalSymlinks(repo)
	if err != nil {
		d = filepath.Clean(repo)
	}
	if !filepath.IsAbs(d) || d == filepath.Dir(d) {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	rel, err := filepath.Rel(d, home)
	return err == nil && filepath.IsLocal(rel) || rel == "."
}

// excludeWorktrees keeps the runner's worktrees (under repo, a folder in
// the repository whose top is top) out of the developer's own git status,
// through the repository's local exclude file (never committed).
func excludeWorktrees(ctx context.Context, top, repo string) error {
	gitDir, err := gitLine(ctx, top, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(top, gitDir)
	}
	rel, err := filepath.Rel(top, repo)
	if err != nil {
		return err
	}
	pattern := "/.worktrees/"
	if rel != "." {
		pattern = "/" + filepath.ToSlash(rel) + "/.worktrees/"
	}
	path := filepath.Join(gitDir, "info", "exclude")
	b, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == pattern {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + "# C4: the worktrees a host runner makes for blocks\n" + pattern + "\n")
	return err
}

// branchRecord reads a block's branch as it stands: its head, and how many
// commits it has over its base.
func branchRecord(ctx context.Context, b UnitBranch) UnitBranch {
	if head, err := gitLine(ctx, b.Repo, "rev-parse", "refs/heads/"+b.Branch); err == nil {
		b.Head = head
	}
	if b.Base != "" && b.Head != "" {
		if n, err := gitLine(ctx, b.Repo, "rev-list", "--count", b.Base+".."+b.Head); err == nil {
			fmt.Sscanf(n, "%d", &b.Commits)
		}
	}
	return b
}

// recordBranch writes a block's branch to its output and its pod, at the
// end of an attempt, and, when the unit is done, removes its worktree's
// folder if nothing is left uncommitted there. The branch stays.
func (r *Runner) recordBranch(ctx context.Context, p *corev1.Pod, u UnitDirs, done bool) {
	b, ok := blockBranch(p)
	if !ok || !isDir(b.Dir) {
		return
	}
	b.Base, _ = gitLine(ctx, b.Repo, "config", "--get", "branch."+b.Branch+".c4base")
	b = branchRecord(ctx, b)
	doc, _ := json.Marshal(b)
	if err := writeAtomic(filepath.Join(u.Out, ".c4", branchFile), append(doc, '\n')); err != nil {
		r.logf("%s/%s: its branch: %v", p.Namespace, p.Name, err)
	}
	if err := r.annotate(ctx, p, map[string]string{AnnotationOutputBranch: string(doc)}); err != nil {
		r.logf("%s/%s: its branch: %v", p.Namespace, p.Name, err)
	}
	if !done {
		return
	}
	if dirty, _ := gitLine(ctx, b.Dir, "status", "--porcelain"); dirty != "" {
		r.logf("%s/%s: its worktree %s has uncommitted work; left in place", p.Namespace, p.Name, b.Dir)
		return
	}
	if _, err := gitLine(ctx, b.Repo, "worktree", "remove", b.Dir); err != nil {
		r.logf("%s/%s: remove its worktree: %v", p.Namespace, p.Name, err)
	}
}
