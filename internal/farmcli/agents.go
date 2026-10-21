package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AgentsArgs are `c4 farm agents add`'s.
type AgentsArgs struct {
	Name string
	// Session is the Claude Code session to add; empty finds the newest
	// whose transcript names the agent Name.
	Session string
	// ConfigDir is the Claude Code config dir the session and its
	// settings.json are in (default $CLAUDE_CONFIG_DIR, else ~/.claude).
	ConfigDir string
	Yes       bool
	// AgentBin is c4-agent, whose CLI keeps the members (default
	// c4-agent on PATH).
	AgentBin string
}

// hookCommand is what marks c4-agent's Stop hook in a settings.json.
const hookCommand = "checkpoint --hook"

// agentBin is c4-agent, by its absolute path, for the hook to name.
func (a *AgentsArgs) agentBin() (string, error) {
	if a.AgentBin != "" {
		return a.AgentBin, nil
	}
	p, err := exec.LookPath("c4-agent")
	if err != nil {
		return "", errors.New("c4-agent is not on PATH; brew install avalanche-io/tap/c4 installs it beside c4 farm")
	}
	return filepath.Abs(p)
}

// agentCmd runs c4-agent and returns what it printed; its own message is
// the error when it fails.
func agentCmd(ctx context.Context, bin string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("c4-agent %s: %s", args[0], msg)
	}
	return string(out), nil
}

// AddAgent is `c4 farm agents add NAME`: the Claude Code session becomes
// a member the farm can run (c4-agent register), and its config dir's
// settings.json gets c4-agent's Stop hook, which saves each turn the
// member ends (shown, and asked once). Both are idempotent.
func AddAgent(ctx context.Context, o Out, a AgentsArgs) error {
	if a.Name == "" {
		return errors.New("give the agent's name: c4 farm agents add NAME")
	}
	bin, err := a.agentBin()
	if err != nil {
		return err
	}
	cdArgs := []string{}
	if a.ConfigDir != "" {
		cdArgs = []string{"--config-dir", a.ConfigDir}
	}
	session := a.Session
	if session == "" {
		out, err := agentCmd(ctx, bin, append([]string{"find", "--agent-name", a.Name}, cdArgs...)...)
		if err != nil {
			return err
		}
		first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
		session, _, _ = strings.Cut(first, "\t")
		if session == "" {
			return fmt.Errorf("no Claude Code session names the agent %q; give its session with --session ID (claude --resume lists them)", a.Name)
		}
	}
	if _, err := agentCmd(ctx, bin, append([]string{"register", "--name", a.Name, "--session", session}, cdArgs...)...); err != nil {
		return err
	}
	o.printf("member    %s · session %s\n", a.Name, session)

	cd := a.ConfigDir
	if cd == "" {
		if cd = os.Getenv("CLAUDE_CONFIG_DIR"); cd == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			cd = filepath.Join(home, ".claude")
		}
	}
	settings := filepath.Join(cd, "settings.json")
	b, err := os.ReadFile(settings)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if bytes.Contains(b, []byte(hookCommand)) && bytes.Contains(b, []byte("c4-agent")) {
		o.printf("hook      already in %s\n", settings)
		return nil
	}
	frag, err := agentCmd(ctx, bin, "hooks")
	if err != nil {
		return err
	}
	merged, added, err := withStopHook(b, []byte(frag))
	if err != nil {
		return fmt.Errorf("%s: %w", settings, err)
	}
	o.printf("hook      %s gains a Stop hook, so each turn the member ends is saved:\n%s\n", settings, added)
	if !a.Yes && !o.confirm("Install it?") {
		return errors.New("the member is added, but without the hook its turns are not saved; c4 farm agents add again installs it")
	}
	if len(b) > 0 {
		if err := os.WriteFile(settings+".c4-backup", b, 0o600); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(cd, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(settings, merged, 0o600); err != nil {
		return err
	}
	if len(b) > 0 {
		o.printf("installed the hook; the old file is %s.c4-backup\n", settings)
		return nil
	}
	o.printf("installed the hook\n")
	return nil
}

// withStopHook is a settings.json with c4-agent's Stop hook added to its
// hooks, every other key kept in the order written, and the group added.
func withStopHook(settings, frag []byte) ([]byte, string, error) {
	var err error
	var want struct {
		Hooks struct {
			Stop []json.RawMessage `json:"Stop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(frag, &want); err != nil || len(want.Hooks.Stop) != 1 {
		return nil, "", errors.New("c4-agent's hook fragment is not a single Stop group")
	}
	group := want.Hooks.Stop[0]
	top := &submissionDoc{vals: map[string]json.RawMessage{}}
	if len(bytes.TrimSpace(settings)) > 0 {
		if top, err = readDoc(settings); err != nil {
			return nil, "", errors.New("not a JSON object")
		}
	}
	hooks := &submissionDoc{vals: map[string]json.RawMessage{}}
	if raw, ok := top.vals["hooks"]; ok {
		if hooks, err = readDoc(raw); err != nil {
			return nil, "", errors.New("hooks is not an object")
		}
	}
	var stop []json.RawMessage
	if raw, ok := hooks.vals["Stop"]; ok {
		if err := json.Unmarshal(raw, &stop); err != nil {
			return nil, "", errors.New("hooks.Stop is not a list")
		}
	}
	stop = append(stop, group)
	sb, _ := json.Marshal(stop)
	hooks.set("Stop", sb)
	hb, _ := hooks.MarshalJSON()
	top.set("hooks", hb)
	tb, _ := top.MarshalJSON()
	var out bytes.Buffer
	if err := json.Indent(&out, tb, "", "  "); err != nil {
		return nil, "", err
	}
	out.WriteByte('\n')
	var shown bytes.Buffer
	json.Indent(&shown, group, "  ", "  ")
	return out.Bytes(), "  " + shown.String(), nil
}

// ListAgents is `c4 farm agents ls`: the members, whether each is busy,
// idle or stopped, and its last saved turn, as c4-agent ls gives them.
func ListAgents(ctx context.Context, o Out, a AgentsArgs) error {
	bin, err := a.agentBin()
	if err != nil {
		return err
	}
	out, err := agentCmd(ctx, bin, "ls")
	if err != nil {
		return err
	}
	if strings.Count(strings.TrimSpace(out), "\n") == 0 {
		o.printf("no agents yet; c4 farm agents add NAME adds a Claude Code session\n")
		return nil
	}
	o.printf("%s", out)
	return nil
}
