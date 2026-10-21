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
	"text/tabwriter"

	"github.com/Avalanche-io/c4-operator/internal/agent"
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
	// AgentBin is the c4-agent the Stop hook runs (default c4-agent on
	// PATH); Harness is the member store (default c4-agent's own).
	AgentBin string
	Harness  *agent.Harness
}

// hookCommand is what marks c4-agent's Stop hook in a settings.json.
const hookCommand = "checkpoint --hook"

func (a *AgentsArgs) harness() (*agent.Harness, error) {
	if a.Harness != nil {
		return a.Harness, nil
	}
	home, err := agent.DefaultHome()
	if err != nil {
		return nil, err
	}
	return agent.New(&agent.Store{Dir: home}, agent.NewCLI()), nil
}

// AddAgent is `c4 farm agents add NAME`: the Claude Code session becomes
// a member the farm can run, and its config dir's settings.json gets the
// Stop hook that saves each turn the member ends (shown, and asked once).
// Both are idempotent.
func AddAgent(ctx context.Context, o Out, a AgentsArgs) error {
	if a.Name == "" {
		return errors.New("give the agent's name: c4 farm agents add NAME")
	}
	bin := a.AgentBin
	if bin == "" {
		p, err := exec.LookPath("c4-agent")
		if err != nil {
			return errors.New("c4-agent is not on PATH; brew install avalanche-io/tap/c4 installs it beside c4 farm")
		}
		if bin, err = filepath.Abs(p); err != nil {
			return err
		}
	}
	h, err := a.harness()
	if err != nil {
		return err
	}
	session := a.Session
	if session == "" {
		paths, err := agent.FindByAgentName(a.ConfigDir, a.Name)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			return fmt.Errorf("no Claude Code session names the agent %q; give its session with --session ID (claude --resume lists them)", a.Name)
		}
		session = strings.TrimSuffix(filepath.Base(paths[0]), ".jsonl")
	}
	m, err := h.Register(ctx, agent.RegisterOptions{Name: a.Name, Session: session, ConfigDir: a.ConfigDir})
	if err != nil {
		return err
	}
	o.printf("member    %s · session %s\n", m.Name, m.Session)

	cd, err := agent.ConfigDirOrDefault(a.ConfigDir)
	if err != nil {
		return err
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
	merged, added, err := withStopHook(b, bin)
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
func withStopHook(settings []byte, bin string) ([]byte, string, error) {
	frag, err := agent.HookSettings(bin)
	if err != nil {
		return nil, "", err
	}
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
// idle or stopped, and its last saved turn.
func ListAgents(ctx context.Context, o Out, a AgentsArgs) error {
	h, err := a.harness()
	if err != nil {
		return err
	}
	sts, err := h.Status(ctx)
	if err != nil {
		return err
	}
	if len(sts) == 0 {
		o.printf("no agents yet; c4 farm agents add NAME adds a Claude Code session\n")
		return nil
	}
	tw := tabwriter.NewWriter(o.W, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATE\tSESSION\tLAST TURN SAVED")
	for _, s := range sts {
		last := "-"
		if s.Last != nil {
			last = o.Clock.At(s.Last.At)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Member.Name, s.State, s.Member.Session, last)
	}
	return tw.Flush()
}
