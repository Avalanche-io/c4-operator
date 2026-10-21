package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	agentenv "github.com/Avalanche-io/c4-operator/config/agent-env"
)

// Blocks on a host run in a clean config directory of their own, filled
// from an agent environment, never in the person's own ~/.claude: one
// directory for the machine's blocks, signed in once by its person
// through Anthropic's own flow, and filled to the version each block asks
// for ($C4_AGENT_ENV, the release's base when it names none). Filling
// replaces the environment's files and leaves the sign-in; a block asking
// for another version than the running blocks' waits for them.

// EnvAgentEnv names the agent environment version a block runs in.
const EnvAgentEnv = "C4_AGENT_ENV"

// BlocksEnvDir and EnvSourcesDir are, in an agents home, the blocks'
// config directory and the environment trees it is filled from, each in
// a folder named for its version.
func BlocksEnvDir(home string) string  { return filepath.Join(home, "blocks-env") }
func EnvSourcesDir(home string) string { return filepath.Join(home, "envs") }

// BaseEnv writes the release's base environment into the sources folder,
// in a folder named for its version, and returns the version.
func BaseEnv(sources string) (string, error) {
	if err := os.MkdirAll(sources, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(sources, ".base-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := os.CopyFS(tmp, agentenv.Base()); err != nil {
		return "", err
	}
	id, err := EnvID(tmp)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(sources, id)
	if isDir(dst) {
		return id, nil
	}
	return id, os.Rename(tmp, dst)
}

// PrepareBlocksEnv fills an agents home's blocks directory from the base
// environment, unless it has been filled before, and returns it.
func PrepareBlocksEnv(home string) (string, error) {
	dir := BlocksEnvDir(home)
	id, err := BaseEnv(EnvSourcesDir(home))
	if err != nil {
		return "", err
	}
	if filledWith(dir) != "" {
		return dir, nil
	}
	_, err = fillBlocksEnv(filepath.Join(EnvSourcesDir(home), id), dir, id)
	return dir, err
}

// filledWith is the version a config directory was filled with ("" for
// none).
func filledWith(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, envMark))
	return strings.TrimSpace(string(b))
}

// fillBlocksEnv fills the blocks' directory, without the environment's
// hooks: a block's own settings carry them, with this runner's command.
func fillBlocksEnv(src, dst, id string) (Env, error) {
	env, err := FillEnv(src, dst, id)
	if err != nil {
		return env, err
	}
	p := filepath.Join(dst, "settings.json")
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return env, nil
	}
	if err != nil {
		return env, err
	}
	var settings map[string]any
	if err := json.Unmarshal(b, &settings); err != nil {
		return env, fmt.Errorf("environment %s: settings.json: %w", id, err)
	}
	if _, ok := settings["hooks"]; !ok {
		return env, nil
	}
	delete(settings, "hooks")
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return env, err
	}
	return env, writeAtomic(p, out.Bytes())
}

// blockEnv is the config directory a block starts in and the flags its
// environment adds. wait is true while running blocks use another
// version of the directory.
func (r *Runner) blockEnv(p *corev1.Pod) (dir string, flags []string, wait bool, err error) {
	if r.BlocksEnv == "" {
		return "", nil, false, nil
	}
	id := strings.TrimSpace(podEnv(p, EnvAgentEnv))
	if id == "" {
		if r.baseEnv == "" {
			if r.baseEnv, err = BaseEnv(r.EnvSources); err != nil {
				return "", nil, false, err
			}
		}
		id = r.baseEnv
	}
	src := filepath.Join(r.EnvSources, id)
	if !isDir(src) {
		return "", nil, false, fmt.Errorf("the agent environment %s is not on this machine (%s)", id, r.EnvSources)
	}
	if r.blockEnvs == nil {
		r.blockEnvs = map[types.UID]string{}
	}
	if filledWith(r.BlocksEnv) != id {
		for uid, other := range r.blockEnvs {
			if uid != p.UID && other != id {
				return "", nil, true, nil
			}
		}
		env, err := fillBlocksEnv(src, r.BlocksEnv, id)
		if err != nil {
			return "", nil, false, err
		}
		if len(env.Missing) > 0 {
			r.logf("the agent environment %s needs %s, not on this machine's PATH", id, strings.Join(env.Missing, ", "))
		}
		r.logf("blocks run in the agent environment %s (%s)", id, r.BlocksEnv)
	}
	env, err := envFlags(r.BlocksEnv, src)
	if err != nil {
		return "", nil, false, err
	}
	r.blockEnvs[p.UID] = id
	return r.BlocksEnv, env, false, nil
}

// envFlags are the claude flags a filled directory's environment sets.
func envFlags(dir, src string) ([]string, error) {
	var meta struct {
		Flags []string `json:"flags"`
	}
	if b, err := os.ReadFile(filepath.Join(src, envMeta)); err == nil {
		if err := json.Unmarshal(b, &meta); err != nil {
			return nil, err
		}
	}
	flags := append([]string(nil), meta.Flags...)
	if _, err := os.Stat(filepath.Join(dir, "mcp.json")); err == nil {
		flags = append(flags, "--mcp-config", filepath.Join(dir, "mcp.json"))
	}
	return flags, nil
}

// SignedIn reports whether a config directory has a Claude Code sign-in,
// as the CLI's own status says it; it reads nothing else of it.
func (c *CLI) SignedIn(ctx context.Context, configDir string) (bool, error) {
	out, err := c.run(ctx, configDir, "", "auth", "status", "--json")
	if err != nil && len(out) == 0 {
		return false, err
	}
	var st struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return false, fmt.Errorf("claude auth status: %w", err)
	}
	return st.LoggedIn, nil
}
