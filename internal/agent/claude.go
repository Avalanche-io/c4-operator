package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Active is one running Claude Code session, as `claude agents --json`
// reports it.
type Active struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"` // idle or busy
	Kind      string `json:"kind"`   // interactive or background
	Pid       int    `json:"pid"`
	Cwd       string `json:"cwd"`
}

// Idle reports whether the session is waiting for input: at a turn
// boundary.
func (a Active) Idle() bool { return a.Status == "idle" }

// Claude is the part of the `claude` CLI the harness uses.
//
// Facts about the CLI the harness relies on (Claude Code 2.1.302, checked
// against a throwaway session on 2026-10-18):
//   - `claude agents --json` lists running sessions with full IDs; with
//     `--all` it also lists stopped background sessions.
//   - `claude --bg --resume <full id>` with no other flags continues a
//     stopped background session under the same ID. With extra flags, or
//     while the session runs, it starts a copy under a new ID instead.
//   - It prints "backgrounded · <short id> · <name>": the short ID is the
//     first eight characters of the session ID.
//   - `claude stop` takes the short ID; the full ID is "No job matching".
type Claude interface {
	// Active lists running sessions. With all, it also lists stopped
	// background sessions.
	Active(ctx context.Context, configDir string, all bool) ([]Active, error)
	// Resume continues a stopped session in the background, in dir, and
	// returns the ID the CLI printed: a short ID, or a full one.
	Resume(ctx context.Context, configDir, dir, session string, flags []string, prompt string) (string, error)
	// Stop stops a background session, given its full ID.
	Stop(ctx context.Context, configDir, session string) error
}

// ShortID is the CLI's short form of a session ID: its first eight
// characters, lowercase.
func ShortID(session string) string {
	s := strings.ToLower(session)
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// SameSession reports whether id, full or short, names session.
func SameSession(id, session string) bool {
	id, session = strings.ToLower(id), strings.ToLower(session)
	return id != "" && strings.HasPrefix(session, id) && (len(id) == 8 || id == session)
}

// CLI runs the real `claude` binary: $C4_AGENT_CLAUDE, else claude on PATH.
type CLI struct {
	Path string
}

// NewCLI returns the CLI the environment names.
func NewCLI() *CLI {
	p := os.Getenv("C4_AGENT_CLAUDE")
	if p == "" {
		p = "claude"
	}
	return &CLI{Path: p}
}

func (c *CLI) run(ctx context.Context, configDir, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Path, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	if configDir != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+configDir)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %v: %s", c.Path, strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()+" "+stdout.String()))
	}
	return append(stdout.Bytes(), stderr.Bytes()...), nil
}

// Active runs `claude agents --json` (and `--all`).
func (c *CLI) Active(ctx context.Context, configDir string, all bool) ([]Active, error) {
	args := []string{"agents", "--json"}
	if all {
		args = append(args, "--all")
	}
	out, err := c.run(ctx, configDir, "", args...)
	if err != nil {
		return nil, err
	}
	i := bytes.IndexByte(out, '[')
	if i < 0 {
		return nil, fmt.Errorf("claude agents --json: no JSON array in %q", truncate(out, 200))
	}
	var as []Active
	if err := json.NewDecoder(bytes.NewReader(out[i:])).Decode(&as); err != nil {
		return nil, fmt.Errorf("claude agents --json: %w", err)
	}
	return as, nil
}

var (
	fullInText  = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	shortInText = regexp.MustCompile(`(?:^|[^0-9A-Za-z])([0-9a-f]{8})(?:[^0-9A-Za-z-]|$)`)
)

// Resume runs `claude --bg --resume <session> [flags] [prompt]` and returns
// the ID it printed, full or short. The harness gives flags only when the
// CLI does not already know the session as a background session.
func (c *CLI) Resume(ctx context.Context, configDir, dir, session string, flags []string, prompt string) (string, error) {
	args := append([]string{"--bg", "--resume", strings.ToLower(session)}, flags...)
	if prompt != "" {
		args = append(args, prompt)
	}
	out, err := c.run(ctx, configDir, dir, args...)
	if err != nil {
		return "", err
	}
	return printedID(string(out))
}

// printedID finds the session ID the CLI printed: a full ID if there is
// one, else the short ID of "backgrounded · <short id> · <name>".
func printedID(out string) (string, error) {
	if id := fullInText.FindString(out); id != "" {
		return strings.ToLower(id), nil
	}
	if m := shortInText.FindStringSubmatch(out); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("claude --bg --resume printed no session ID: %q", truncate([]byte(out), 200))
}

// Stop runs `claude stop <short id>`; the CLI does not match full IDs.
func (c *CLI) Stop(ctx context.Context, configDir, session string) error {
	_, err := c.run(ctx, configDir, "", "stop", ShortID(session))
	return err
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// ConfigDirOrDefault returns dir, or ~/.claude when it is empty.
func ConfigDirOrDefault(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

var slugRE = regexp.MustCompile(`[^A-Za-z0-9-]`)

// ProjectSlug is the directory name Claude Code keeps a working
// directory's transcripts under: every character other than a letter, a
// digit or '-' becomes '-' (/Users/a/.w becomes -Users-a--w).
func ProjectSlug(cwd string) string { return slugRE.ReplaceAllString(cwd, "-") }

// FindTranscript locates a session's transcript: first under the project
// directory derived from cwd, then under any project directory.
func FindTranscript(configDir, cwd, session string) (string, error) {
	cd, err := ConfigDirOrDefault(configDir)
	if err != nil {
		return "", err
	}
	if cwd != "" {
		p := filepath.Join(cd, "projects", ProjectSlug(cwd), session+".jsonl")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	matches, _ := filepath.Glob(filepath.Join(cd, "projects", "*", session+".jsonl"))
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("session %s: %d transcripts under %s; give --cwd", session, len(matches), cd)
	}
	return "", fmt.Errorf("session %s: no transcript under %s/projects", session, cd)
}

// FindByAgentName scans transcripts under configDir for sessions whose
// first lines name agentName, newest first. Only the first 64 KB of each
// file is read.
func FindByAgentName(configDir, agentName string) ([]string, error) {
	cd, err := ConfigDirOrDefault(configDir)
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(cd, "projects", "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	needle := []byte(`"agentName":"` + agentName + `"`)
	type hit struct {
		path string
		mod  int64
	}
	var hits []hit
	buf := make([]byte, 64*1024)
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		n, _ := f.Read(buf)
		st, _ := f.Stat()
		f.Close()
		if bytes.Contains(buf[:n], needle) && st != nil {
			hits = append(hits, hit{p, st.ModTime().UnixNano()})
		}
	}
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].mod > hits[j-1].mod; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	var out []string
	for _, h := range hits {
		out = append(out, h.path)
	}
	return out, nil
}
