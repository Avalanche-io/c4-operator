package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A block on a host works on its own branch: a worktree of its job's
// repository on c4/<job>/<unit>, from HEAD, kept out of the developer's git
// status. Its commits are its output: recorded with the unit and on its pod
// (base, head, how many), and when the unit is done its folder goes and the
// branch stays. A folder that is not a repository gets no branch.
func TestBlockWorksOnItsOwnBranch(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "README"), []byte("hello\n"), 0o644)
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-q", "-m", "start")
	base := gitT(t, repo, "rev-parse", "HEAD")

	e := newEnv(t)
	p := hostPod()
	p.Labels[c4v1.LabelJob] = "port"
	p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: EnvBlock, Value: "1"},
		corev1.EnvVar{Name: EnvBlockRepo, Value: repo}, corev1.EnvVar{Name: "C4_UNIT_NAME", Value: "notes"})
	b, ok, err := ensureBranch(ctx, p)
	if err != nil || !ok || b.Branch != "c4/port/notes" || b.Base != base || !isDir(b.Dir) {
		t.Fatalf("branch %+v %v %v", b, ok, err)
	}
	if again, _, err := ensureBranch(ctx, p); err != nil || again.Dir != b.Dir || again.Base != base {
		t.Fatalf("a later attempt: %+v %v", again, err)
	}
	if st := gitT(t, repo, "status", "--porcelain"); st != "" {
		t.Errorf("the developer's status shows the worktree: %q", st)
	}
	if got := whereThingsAre(p, UnitDirsAt(t.TempDir())); !strings.Contains(got, "c4/port/notes, checked out at "+b.Dir) {
		t.Errorf("the prompt does not name the branch:\n%s", got)
	}

	os.WriteFile(filepath.Join(b.Dir, "NOTES"), []byte("notes\n"), 0o644)
	gitT(t, b.Dir, "add", ".")
	gitT(t, b.Dir, "commit", "-q", "-m", "notes")
	r, c := hostRunner(t, e, p)
	u := UnitDirsAt(t.TempDir())
	os.MkdirAll(filepath.Join(u.Out, ".c4"), 0o755)
	r.recordBranch(ctx, getPod(t, c), u, true)
	var rec UnitBranch
	if err := json.Unmarshal([]byte(getPod(t, c).Annotations[AnnotationOutputBranch]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Branch != "c4/port/notes" || rec.Base != base || rec.Commits != 1 || rec.Head == "" || rec.Head == base {
		t.Fatalf("record %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(u.Out, ".c4", branchFile)); err != nil {
		t.Errorf("not in the unit's output: %v", err)
	}
	if isDir(b.Dir) {
		t.Error("the done unit's worktree folder was left")
	}
	if head := gitT(t, repo, "rev-parse", "c4/port/notes"); head != rec.Head {
		t.Errorf("the branch is gone or moved: %s", head)
	}

	plain := hostPod()
	plain.Spec.Containers[0].Env = append(plain.Spec.Containers[0].Env, corev1.EnvVar{Name: EnvBlockRepo, Value: t.TempDir()}, corev1.EnvVar{Name: "C4_UNIT_NAME", Value: "x"})
	if _, ok, err := ensureBranch(ctx, plain); ok || err != nil {
		t.Errorf("a folder that is not a repository: %v %v", ok, err)
	}
}

// A block's branch starts from the commit its pod names (a lead's HEAD
// when it submitted the worker), not wherever the repository's HEAD has
// moved since. A base that is not a commit in the repository, or that
// looks like an option to git, is refused and makes no branch; a later
// attempt keeps the base its branch started from.
func TestBlockBranchStartsAtItsBase(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "README"), []byte("hello\n"), 0o644)
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-q", "-m", "start")
	lead := gitT(t, repo, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(repo, "README"), []byte("hello again\n"), 0o644)
	gitT(t, repo, "commit", "-q", "-am", "moved on")

	pod := func(unit, base string) *corev1.Pod {
		p := hostPod()
		p.Labels[c4v1.LabelJob] = "port"
		p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: EnvBlock, Value: "1"},
			corev1.EnvVar{Name: EnvBlockRepo, Value: repo}, corev1.EnvVar{Name: "C4_UNIT_NAME", Value: unit},
			corev1.EnvVar{Name: EnvBlockBase, Value: base})
		return p
	}
	b, ok, err := ensureBranch(ctx, pod("lexer", lead[:12]))
	if err != nil || !ok || b.Base != lead || gitT(t, b.Dir, "rev-parse", "HEAD") != lead {
		t.Fatalf("from its base: %+v %v %v", b, ok, err)
	}
	if again, _, err := ensureBranch(ctx, pod("lexer", "main")); err != nil || again.Base != lead {
		t.Fatalf("a later attempt moved its base: %+v %v", again, err)
	}
	for _, bad := range []string{"0123456789abcdef0123456789abcdef01234567", "no-such-branch", "--output=/tmp/x", "main extra"} {
		_, _, err := ensureBranch(ctx, pod("parser", bad))
		if err == nil || !strings.Contains(err.Error(), "is not a commit in") {
			t.Errorf("base %q: %v", bad, err)
		}
		if _, err := gitLine(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/c4/port/parser"); err == nil {
			t.Fatalf("base %q made a branch", bad)
		}
	}
}

// Blocks run in a clean config directory of their own, filled from the
// agent environment each asks for (the base when it names none), without
// the environment's hooks (a block's own settings carry them), never the
// person's ~/.claude. Refilling it for another version leaves its sign-in;
// a block asking for another version than a running block's waits; one
// naming an environment the machine lacks is refused.
func TestBlocksRunInTheirOwnEnvironment(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	p := hostPod()
	p.UID = "block-1"
	r, _ := hostRunner(t, e, p)
	dir, err := PrepareBlocksEnv(home)
	if err != nil {
		t.Fatal(err)
	}
	r.BlocksEnv, r.EnvSources = dir, EnvSourcesDir(home)
	const base = "c44qpoXu39vLhhB2jGYg8HAagoXJ9qGA6KtzGTd74eju3YDDrN9ftTcNtot8z3PyXVGzVQa57emJGYxntqDVR5Uxgv"
	got, flags, wait, err := r.blockEnv(p)
	if err != nil || wait || got != dir || filledWith(dir) != base {
		t.Fatalf("blockEnv: %q %v %v, filled with %q", got, wait, err, filledWith(dir))
	}
	if !slices.Contains(flags, "--mcp-config") {
		t.Errorf("flags %q", flags)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "settings.json")); strings.Contains(string(b), "hooks") {
		t.Errorf("the environment's hooks are in the blocks' settings:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Error(err)
	}
	signIn := filepath.Join(dir, ".claude.json")
	os.WriteFile(signIn, []byte(`{"signedIn": true}`), 0o600)

	// a second version: the base and one more file
	next := filepath.Join(t.TempDir(), "next")
	if err := os.CopyFS(next, os.DirFS(filepath.Join(EnvSourcesDir(home), base))); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(next, "NOTES.md"), []byte("more\n"), 0o644)
	id2, err := EnvID(next)
	if err != nil {
		t.Fatal(err)
	}
	os.Rename(next, filepath.Join(EnvSourcesDir(home), id2))
	q := hostPod()
	q.UID = "block-2"
	q.Spec.Containers[0].Env = append(q.Spec.Containers[0].Env, corev1.EnvVar{Name: EnvAgentEnv, Value: id2})
	if _, _, wait, err := r.blockEnv(q); err != nil || !wait {
		t.Fatalf("another version while a block runs: wait %v, %v", wait, err)
	}
	delete(r.blockEnvs, p.UID) // the first block finished
	if _, _, wait, err := r.blockEnv(q); err != nil || wait || filledWith(dir) != id2 {
		t.Fatalf("after it: wait %v, %v, filled with %q", wait, err, filledWith(dir))
	}
	if _, err := os.Stat(signIn); err != nil {
		t.Error("refilling lost the sign-in")
	}
	if _, err := os.Stat(filepath.Join(dir, "NOTES.md")); err != nil {
		t.Error("not filled with the second version")
	}

	x := hostPod()
	x.UID = "block-3"
	x.Spec.Containers[0].Env = append(x.Spec.Containers[0].Env, corev1.EnvVar{Name: EnvAgentEnv, Value: "c4nothere"})
	if _, _, _, err := r.blockEnv(x); err == nil || !strings.Contains(err.Error(), "not on this machine") {
		t.Errorf("an unknown environment: %v", err)
	}
}

// A block waits, pending, until its folder is trusted in the blocks'
// config directory, as the CLI records the trust dialog's answer there;
// then it starts. Only that answer is read.
func TestBlocksWaitForTrust(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	folder := filepath.Join(home, "blocks")
	os.MkdirAll(folder, 0o700)
	dir, err := PrepareBlocksEnv(home)
	if err != nil {
		t.Fatal(err)
	}
	if Trusted(dir, folder) {
		t.Fatal("trusted before anyone said so")
	}
	p := hostPod()
	p.UID = "block-trust"
	p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: EnvBlock, Value: "1"}, corev1.EnvVar{Name: EnvBlockPrompt, Value: "port the notes"})
	r, c := hostRunner(t, e, p)
	r.BlocksCwd, r.BlocksEnv, r.EnvSources, r.HookCommand = folder, dir, EnvSourcesDir(home), "c4-agent checkpoint --hook"
	var logs []string
	r.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	if err := r.startBlock(context.Background(), p, "ns.port.0"); err != nil {
		t.Fatal(err)
	}
	if got := getPod(t, c); got.Status.Phase == corev1.PodFailed || len(e.fake.active) != 0 {
		t.Fatalf("a block started or failed in an untrusted folder: %s, %d sessions", got.Status.Phase, len(e.fake.active))
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "trust it once") {
		t.Errorf("logs %q", logs)
	}
	abs, _ := filepath.Abs(folder)
	os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"projects":{"`+abs+`":{"hasTrustDialogAccepted":true}}}`), 0o600)
	if !Trusted(dir, folder) {
		t.Fatal("not trusted after the dialog's answer")
	}
}
