package farmcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// IntegrateQuestion is asked once, on the first c4 farm up or login at a
// terminal, before anything is changed: it names what yes changes on this
// machine, where, and how each comes out. runner says this machine would
// also run the farm's agents (a local farm's up).
func IntegrateQuestion(runner bool) string {
	q := "Connect Claude Code to this farm? It adds the farm's tools to Claude Code (" + tildePath(mcpConfig()) +
		"), and lets c4 farm agents add save an agent's turns with a hook in that agent's own folder, without asking again"
	if runner {
		q += ", and runs a background service (" + tildePath(serviceFile()) + ") so this machine can run your agents"
	}
	return q + ". c4 farm integrate claude off takes it all out."
}

// mcpConfig is the file claude mcp add --scope user writes.
func mcpConfig() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

// serviceFile is where the agent service is defined on this machine.
func serviceFile() string {
	s := &AgentService{}
	if s.fillPaths() != nil {
		return AgentLabel
	}
	return s.path()
}

// tildePath writes a path under the home folder from ~.
func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// mcpName is the farm's MCP server in Claude Code.
const mcpName = "c4-farm"

// Integration is Claude Code's integration with the farm on this machine:
// the farm's MCP server in Claude Code, c4-agent's hooks in each member's
// folder (agents add, which asks nothing more once it is on), and, for a
// local farm, this machine as one of its runners (the agent service).
type Integration struct {
	// FarmBin is c4-farm, which Claude Code runs as the MCP server
	// (default this executable); ClaudeBin is claude (default on PATH);
	// AgentBin is c4-agent (default on PATH).
	FarmBin, ClaudeBin, AgentBin string
	// ConfigDir is Claude Code's config dir, for the members' sessions.
	ConfigDir string
	// Runner makes this machine a runner, for a local farm: enroll, then
	// start the agent service. Nil leaves runners alone.
	Runner func(ctx context.Context, o Out) error
	// Service is the agent service off removes (default the machine's).
	Service *AgentService
}

func (in *Integration) fill() error {
	if in.FarmBin == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		in.FarmBin = exe
	}
	if in.ClaudeBin == "" {
		in.ClaudeBin, _ = exec.LookPath("claude")
	}
	if in.AgentBin == "" {
		if p, err := exec.LookPath("c4-agent"); err == nil {
			in.AgentBin, _ = filepath.Abs(p)
		}
	}
	if in.Service == nil {
		in.Service = &AgentService{AgentBin: in.AgentBin}
	}
	return nil
}

// On turns the integration on: the farm's MCP server in Claude Code
// (claude mcp add, for every folder), and this machine as a runner when
// Runner is given. A part that cannot be done is one line, and the rest
// still is.
func (in *Integration) On(ctx context.Context, o Out) error {
	if err := in.fill(); err != nil {
		return err
	}
	if in.ClaudeBin == "" {
		o.printf("claude    not on this machine, so it has no farm tools yet: install Claude Code (https://claude.com/claude-code), then c4 farm integrate on\n")
	} else {
		out, err := exec.CommandContext(ctx, in.ClaudeBin, "mcp", "add", "--scope", "user", mcpName, "--", in.FarmBin, "mcp").CombinedOutput()
		switch {
		case err == nil:
			o.printf("claude    has the farm's tools (%s in %s); c4 farm integrate claude off takes it all out\n", mcpName, tildePath(mcpConfig()))
		case strings.Contains(string(out), "already exists"):
			o.printf("claude    has the farm's tools (%s in %s, already); c4 farm integrate claude off takes it all out\n", mcpName, tildePath(mcpConfig()))
		default:
			o.printf("claude    has no farm tools yet: claude mcp add said %s\n", firstLine(string(out), err))
		}
	}
	if in.Runner != nil {
		if err := in.Runner(ctx, o); err != nil {
			o.printf("agents    %v\n", err)
		}
	}
	return nil
}

// Off turns the integration off and takes out what On and agents add put
// in: the MCP server, the agent service, and c4-agent's hooks in every
// member's folder. The members stay c4-agent's, for agents rm.
func (in *Integration) Off(ctx context.Context, o Out) error {
	if err := in.fill(); err != nil {
		return err
	}
	if in.ClaudeBin != "" {
		out, err := exec.CommandContext(ctx, in.ClaudeBin, "mcp", "remove", "--scope", "user", mcpName).CombinedOutput()
		switch {
		case err == nil:
			o.printf("claude    no longer has the farm's tools\n")
		case strings.Contains(string(out), "not found") || strings.Contains(string(out), "No "):
		default:
			o.printf("claude    still has the farm's tools: claude mcp remove said %s\n", firstLine(string(out), err))
		}
	}
	if err := in.Service.Stop(ctx, o); err != nil {
		o.printf("agent     %v\n", err)
	}
	if in.AgentBin == "" {
		return nil
	}
	members, err := listMembers(ctx, in.AgentBin)
	if err != nil {
		return err
	}
	a := AgentsArgs{ConfigDir: in.ConfigDir}
	cd, err := a.configDir()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, session := range members {
		folder := folderOf(transcriptOf(cd, session))
		if folder == "" || seen[folder] {
			continue
		}
		seen[folder] = true
		settings := localSettings(folder)
		b, err := os.ReadFile(settings)
		if err != nil {
			continue
		}
		stripped, removed, err := withoutHooks(b)
		if err != nil || len(removed) == 0 {
			continue
		}
		if err := writeAtomic(settings, stripped); err != nil {
			return err
		}
		o.printf("hooks     %s loses %s\n", settings, andList(removed))
	}
	return nil
}

func firstLine(out string, err error) string {
	l, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if l == "" && err != nil {
		return err.Error()
	}
	return l
}

// Claude is the Claude Code integration's name.
const Claude = "claude"

// Detected is one integration this machine could take, found by reading
// files only (never by starting the application): its name, what it is,
// and where it was found, or why it was not.
type Detected struct {
	Name, Title, Where string
	Found              bool
	// Question is what the person is asked before it is added, naming
	// what it changes and where.
	Question string
}

// Detect lists the integrations this machine could take. Today that is
// Claude Code; each DCC's (integrations/dcc) joins as it learns to install
// and remove itself.
// runner says this machine would also run the farm's agents.
func Detect(runner bool) []Detected {
	d := Detected{Name: Claude, Title: "Claude Code: the farm's tools, your agents' turns saved and shown, this machine as their runner",
		Question: IntegrateQuestion(runner)}
	if p, err := exec.LookPath("claude"); err == nil {
		d.Found, d.Where = true, p
	} else {
		d.Where = "not found (https://claude.com/claude-code installs it)"
	}
	return []Detected{d}
}

// Choose settles which integrations are on. --integrate=NAMES turns those
// on, --no-integrate turns every one off, and each is recorded; otherwise
// a recorded answer stands, and the person at a terminal is asked once
// about each one found and unanswered. With no terminal nothing is asked
// or recorded, and the install is minimal. It returns the names on.
func Choose(cfg *Config, found []Detected, integrate string, none, terminal bool, o Out) ([]string, error) {
	if cfg.Integrations == nil {
		cfg.Integrations = map[string]bool{}
	}
	known := map[string]bool{}
	for _, d := range found {
		known[d.Name] = true
	}
	switch {
	case integrate != "" && none:
		return nil, errors.New("--integrate and --no-integrate: give one")
	case none:
		for _, d := range found {
			cfg.Integrations[d.Name] = false
		}
	case integrate != "":
		for _, n := range strings.Split(integrate, ",") {
			n = strings.TrimSpace(n)
			if !known[n] {
				return nil, fmt.Errorf("no integration %q here; c4 farm integrations lists them", n)
			}
			cfg.Integrations[n] = true
		}
	case terminal && o.In != nil:
		for _, d := range found {
			if _, answered := cfg.Integrations[d.Name]; answered || !d.Found {
				continue
			}
			q := d.Question
			if q == "" {
				q = d.Title + "?"
			}
			cfg.Integrations[d.Name] = o.confirm(q)
		}
	}
	var on []string
	for _, d := range found {
		if cfg.Integrations[d.Name] {
			on = append(on, d.Name)
		}
	}
	return on, nil
}

// IntegrateLine is what up and login say of the Claude Code integration
// when it added nothing: off, or not set. On, the lines of what it did
// say it, and this is empty.
func IntegrateLine(cfg *Config) string {
	on, answered := cfg.Integrations[Claude]
	switch {
	case on:
		return "" // the claude and agents lines say what it did
	case answered:
		return "integration claude off: nothing added to Claude Code or this machine; Claude Code can drive the farm from its docs (c4 farm integrate claude on adds it)"
	}
	return "integration claude not set: nothing added to Claude Code or this machine (c4 farm integrations lists what it could add)"
}

// Integrations is c4 farm integrations: each one this machine could take,
// whether it was found, and whether it is on.
func Integrations(cfg *Config, found []Detected, o Out) {
	tw := tabwriter.NewWriter(o.W, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "INTEGRATION\tSTATE\tFOUND\tWHAT")
	for _, d := range found {
		state := "not set"
		if on, ok := cfg.Integrations[d.Name]; ok {
			state = map[bool]string{true: "on", false: "off"}[on]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, state, d.Where, d.Title)
	}
	tw.Flush()
	o.printf("c4 farm integrate NAME on|off adds or takes one out\n")
}
