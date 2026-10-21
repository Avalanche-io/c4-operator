package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Avalanche-io/c4/scan"
)

// An agent environment is the config directory C4 gives a member in place
// of a person's own ~/.claude: a tree (settings.json with C4's hooks,
// CLAUDE.md, mcp.json, c4-member.json, skills) whose version is the C4 ID of
// its listing. Every change for a build is a new version, so a member's
// checkpoints say exactly which environment it ran in.

// envEpoch is every file's time when a version is identified, so its ID
// depends on its content only (2026-01-01T00:00:00Z).
const envEpoch = 1767225600

// envMeta is the environment's own file: the flags members run with, and
// the commands they need.
const envMeta = "c4-member.json"

// envMark records, in a filled config directory, the version that filled it.
const envMark = ".c4-env"

// EnvID is the version of the environment tree at src: the C4 ID of its
// listing, every file 0644 and directory 0755, every time envEpoch.
func EnvID(src string) (string, error) {
	tmp, err := os.MkdirTemp("", "c4-env-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	tree := filepath.Join(tmp, "env")
	if err := envCopyTree(src, tree, nil); err != nil {
		return "", err
	}
	epoch := time.Unix(envEpoch, 0)
	var dirs []string
	err = filepath.WalkDir(tree, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return os.Chmod(p, 0o755)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			return err
		}
		return os.Chtimes(p, epoch, epoch)
	})
	if err != nil {
		return "", err
	}
	for i := len(dirs) - 1; i >= 0; i-- { // deepest first: a child's change moves its parent's time
		if err := os.Chtimes(dirs[i], epoch, epoch); err != nil {
			return "", err
		}
	}
	m, err := scan.NewGeneratorWithOptions(scan.WithMode(scan.ModeFull)).GenerateFromPath(tree)
	if err != nil {
		return "", err
	}
	return m.ComputeC4ID().String(), nil
}

// Env is an environment as filled into a member's config directory.
type Env struct {
	ID string
	// Flags are the member's claude flags the environment sets: its own
	// (c4-member.json), and its MCP configuration, alone.
	Flags []string
	// Missing are the commands it requires that are not on the PATH.
	Missing []string
}

// FillEnv fills the config directory dst from the environment at src, and
// returns its version. With want set, the version must be that one. dst may
// not be a person's own config directory (~/.claude, ~/.claude-*): a member
// runs in a clean environment, never in someone's hand-made one.
func FillEnv(src, dst, want string) (Env, error) {
	if err := notPersonal(dst); err != nil {
		return Env{}, err
	}
	id, err := EnvID(src)
	if err != nil {
		return Env{}, fmt.Errorf("environment %s: %w", src, err)
	}
	if want != "" && want != id {
		return Env{}, fmt.Errorf("environment %s is version %s, not %s", src, id, want)
	}
	var meta struct {
		Flags    []string `json:"flags"`
		Requires []string `json:"requires"`
	}
	if b, err := os.ReadFile(filepath.Join(src, envMeta)); err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&meta); err != nil {
			return Env{}, fmt.Errorf("environment %s: %s: %w", src, envMeta, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Env{}, err
	}
	if err := checkFlags(meta.Flags); err != nil {
		return Env{}, fmt.Errorf("environment %s: %w", src, err)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return Env{}, err
	}
	if err := envCopyTree(src, dst, func(rel string) bool { return rel == envMeta }); err != nil {
		return Env{}, err
	}
	if err := os.WriteFile(filepath.Join(dst, envMark), []byte(id+"\n"), 0o600); err != nil {
		return Env{}, err
	}
	env := Env{ID: id, Flags: meta.Flags}
	if _, err := os.Stat(filepath.Join(dst, "mcp.json")); err == nil {
		// the environment's servers, and no others the machine has
		env.Flags = append(append([]string(nil), env.Flags...), "--mcp-config", filepath.Join(dst, "mcp.json"), "--strict-mcp-config")
	}
	for _, r := range meta.Requires {
		if strings.ContainsAny(r, " /") {
			continue // a note, not a command
		}
		if _, err := exec.LookPath(r); err != nil {
			env.Missing = append(env.Missing, r)
		}
	}
	return env, nil
}

// notPersonal refuses a person's own Claude Code config directory.
func notPersonal(dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(home, abs); err == nil && !strings.Contains(rel, string(filepath.Separator)) &&
		(rel == ".claude" || strings.HasPrefix(rel, ".claude-")) {
		return fmt.Errorf("%s is a person's own Claude Code config directory; a member's environment goes in a directory of its own", dir)
	}
	return nil
}

// ImportTranscript copies a session's transcript, and its directory of
// subagents and tool results, from the config directory it ran in to dst,
// so the member resumes it there with its conversation. The source is not
// changed. A session running in the source is refused (it would fork), and
// so is one dst already has.
func (h *Harness) ImportTranscript(ctx context.Context, from, dst, cwd, session string) (string, error) {
	as, err := h.Claude.Active(ctx, from, false)
	if err != nil {
		return "", err
	}
	for _, a := range as {
		if strings.EqualFold(a.SessionID, session) {
			return "", fmt.Errorf("session %s is %s in %s: %w; stop it there before importing it, or the copy forks", session, a.Status, from, ErrLive)
		}
	}
	src, err := FindTranscript(from, cwd, session)
	if err != nil {
		return "", err
	}
	out := filepath.Join(dst, "projects", filepath.Base(filepath.Dir(src)), session+".jsonl")
	if _, err := os.Stat(out); err == nil {
		return "", fmt.Errorf("session %s is already in %s; it is not imported twice", session, dst)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return "", err
	}
	if err := envCopyFile(src, out, 0o600); err != nil {
		return "", err
	}
	if dir := strings.TrimSuffix(src, ".jsonl"); isDir(dir) {
		if err := envCopyTree(dir, strings.TrimSuffix(out, ".jsonl"), nil); err != nil {
			return "", err
		}
	}
	return out, nil
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// envCopyTree copies the files under src to dst, skipping those skip names
// (by path relative to src).
func envCopyTree(src, dst string, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if skip != nil && rel != "." && skip(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil // links and devices are not part of an environment
		}
		return envCopyFile(p, target, 0o600)
	})
}

func envCopyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}
