package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
