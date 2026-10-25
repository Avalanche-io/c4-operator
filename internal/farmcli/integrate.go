package farmcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/Avalanche-io/c4-operator/integrations/dcc"
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
	return q + ". c4 farm integrate off takes it all out."
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
	// Record is where On notes what it added (the person's Config's
	// Integrations), so Off takes out only that.
	Record map[string]bool
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
// Runner is given. Record keeps what it added, so Off takes out only that
// (Record["claude.mcp"]: the MCP entry is c4's). A part that cannot be
// done is one line, and the rest still is.
func (in *Integration) On(ctx context.Context, o Out) error {
	if err := in.fill(); err != nil {
		return err
	}
	if in.Record == nil {
		in.Record = map[string]bool{}
	}
	if in.ClaudeBin == "" {
		o.printf("claude    not on this machine, so it has no farm tools yet: install Claude Code (https://claude.com/claude-code), then c4 farm integrate on\n")
	} else {
		in.mcpOn(ctx, o)
	}
	if in.Runner != nil {
		if err := in.Runner(ctx, o); err != nil {
			o.printf("agents    %v\n", err)
		}
	}
	return nil
}

// mcpOn adds the farm's MCP server to Claude Code, or says how the one
// there already differs: one c4 added before and that names another
// c4-farm is pointed here; one the person added is left as it is.
func (in *Integration) mcpOn(ctx context.Context, o Out) {
	add := func() ([]byte, error) {
		return exec.CommandContext(ctx, in.ClaudeBin, "mcp", "add", "--scope", "user", mcpName, "--", in.FarmBin, "mcp").CombinedOutput()
	}
	out, err := add()
	switch {
	case err == nil:
		in.Record["claude.mcp"] = true
		o.printf("claude    has the farm's tools (%s in %s); c4 farm integrate off takes it all out\n", mcpName, tildePath(mcpConfig()))
		return
	case !strings.Contains(string(out), "already exists"):
		o.printf("claude    has no farm tools yet: claude mcp add said %s\n", firstLine(string(out), err))
		return
	}
	cmd := mcpCommand()
	switch {
	case cmd == "" || cmd == in.FarmBin:
		o.printf("claude    has the farm's tools (%s in %s, already)\n", mcpName, tildePath(mcpConfig()))
	case in.Record["claude.mcp"]:
		// c4's own entry, naming a c4-farm moved or replaced: point it here
		exec.CommandContext(ctx, in.ClaudeBin, "mcp", "remove", "--scope", "user", mcpName).Run()
		if out, err := add(); err != nil {
			o.printf("claude    still runs %s as %s: claude mcp add said %s\n", tildePath(cmd), mcpName, firstLine(string(out), err))
			return
		}
		o.printf("claude    has the farm's tools (%s in %s, now this c4-farm, not %s)\n", mcpName, tildePath(mcpConfig()), tildePath(cmd))
	default:
		o.printf("claude    has an MCP server named %s already, running %s, which c4 did not add: left as it is\n", mcpName, tildePath(cmd))
	}
}

// mcpCommand is the command Claude Code runs for the farm's MCP server, as
// its user config holds it, or "" when it cannot be read.
func mcpCommand() string {
	b, err := os.ReadFile(mcpConfig())
	if err != nil {
		return ""
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return cfg.MCPServers[mcpName].Command
}

// Off turns the integration off and takes out what On and agents add put
// in: the MCP server, if c4 added it, the agent service, and c4-agent's
// hooks in every member's folder. It says what it leaves: the members
// (c4-agent's, for agents rm) and this machine's enrollment, which the
// farm keeps until the farm itself goes.
func (in *Integration) Off(ctx context.Context, o Out) error {
	if err := in.fill(); err != nil {
		return err
	}
	if in.Record == nil {
		in.Record = map[string]bool{}
	}
	switch {
	case in.ClaudeBin == "":
	case !in.Record["claude.mcp"]:
		if mcpCommand() != "" {
			o.printf("claude    keeps its MCP server %s: c4 did not add it, so it is left as it was\n", mcpName)
		}
	default:
		out, err := exec.CommandContext(ctx, in.ClaudeBin, "mcp", "remove", "--scope", "user", mcpName).CombinedOutput()
		switch {
		case err == nil:
			delete(in.Record, "claude.mcp")
			o.printf("claude    no longer has the farm's tools\n")
		case strings.Contains(string(out), "not found") || strings.Contains(string(out), "No "):
			delete(in.Record, "claude.mcp")
		default:
			o.printf("claude    still has the farm's tools: claude mcp remove said %s\n", firstLine(string(out), err))
		}
	}
	if err := in.Service.Stop(ctx, o); err != nil {
		o.printf("agent     %v\n", err)
	}
	if enrolled, err := os.Stat(enrollment()); err == nil && !enrolled.IsDir() {
		o.printf("machine   stays enrolled on the farm (its account and pool) until the farm goes; c4 farm down takes the local farm away\n")
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

// enrollment is the file c4-agent enroll writes for this machine's runner.
func enrollment() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".c4", "agents", "runner", "enrolled.json")
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
	// what it changes and where; DefaultYes makes Enter yes (Claude Code,
	// as PLAN 1a.1 records), else Enter is no (an application's own
	// setup). Either way only a person at a terminal answers it.
	Question   string
	DefaultYes bool
	// Runner says yes also makes this machine one of the farm's runners
	// (a local farm's up), recorded under RunnerKey.
	Runner bool
	// Versions are an application's installed versions (integrations/dcc).
	Versions []dcc.Found
}

// Detect lists the integrations this machine could take: Claude Code, and
// each application integrations/dcc finds installed, by reading files only.
// runner says this machine would also run the farm's agents.
func Detect(runner bool) []Detected {
	d := Detected{Name: Claude, Title: "Claude Code: the farm's tools, your agents' turns saved and shown, this machine as their runner",
		Question: IntegrateQuestion(runner), DefaultYes: true, Runner: runner}
	if p, err := exec.LookPath("claude"); err == nil {
		d.Found, d.Where = true, p
	} else {
		d.Where = "not found (https://claude.com/claude-code installs it)"
	}
	return append([]Detected{d}, apps(dcc.Detect())...)
}

// apps groups an application's installed versions into one integration,
// asked about in one line however many versions there are. The offer
// reads nothing but what Detect read: an application's plan may look in a
// folder the system guards (macOS ~/Documents, for Unreal), so Plan runs
// only after the person says yes.
func apps(found []dcc.Found) []Detected {
	var out []Detected
	byName := map[string]int{}
	for _, f := range found {
		i, ok := byName[f.Name]
		if !ok {
			byName[f.Name] = len(out)
			out = append(out, Detected{Name: f.Name, Found: true, Where: f.Where})
			i = len(out) - 1
		}
		out[i].Versions = append(out[i].Versions, f)
	}
	for i := range out {
		var titles, todo []string
		for _, f := range out[i].Versions {
			titles = append(titles, f.Title)
			if f.Status != dcc.StatusInstalled {
				todo = append(todo, f.Title)
			}
		}
		out[i].Title = andList(titles) + ": the C4 Farm panel, to submit from inside it"
		if len(todo) > 0 {
			out[i].Question = "Add the C4 Farm panel to " + andList(todo) + "? It changes that application's own setup; c4 farm integrate " +
				out[i].Name + " --show says each file first, and c4 farm integrate off " + out[i].Name + " takes it out."
		}
	}
	return out
}

// RunnerKey records the person's yes to this machine running the farm's
// agents (the agent service), apart from Claude Code's tools: a yes at
// login, which runs no service, does not cover it.
const RunnerKey = "claude.runner"

// RunnerQuestion is asked at a local farm's up when Claude Code's tools
// are on but the runner was never asked about.
func RunnerQuestion() string {
	return "Run your agents on this machine too? It runs a background service (" + tildePath(serviceFile()) +
		") so this machine is one of the farm's runners. c4 farm integrate off takes it out."
}

// enterYes is a question whose Enter is yes, answered by a person: only
// a line they ended counts, so the end of input (a script, /dev/null, a
// closed pipe) is no, never a yes nobody gave.
func enterYes(o Out, q string) (yes, answered bool) {
	o.printf("%s [Y/n] ", q)
	line, err := bufio.NewReader(o.In).ReadString('\n')
	if err != nil {
		o.printf("\n")
		return false, false
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "" || a == "y" || a == "yes", true
}

// InsideAgent says this runs inside Claude Code (its CLAUDECODE
// environment): an agent, not the person, whatever its flags say.
func InsideAgent() bool { return os.Getenv("CLAUDECODE") != "" }

// Consent is the person's leave to change this machine: never from inside
// an agent, even with --yes (an agent must not give itself the
// integration); otherwise --yes, or a person at a terminal to ask.
// command is what the person runs themselves.
func Consent(yes, terminal bool, command string) error {
	switch {
	case InsideAgent():
		return fmt.Errorf("this changes your machine, so you run it, not an agent: in your own terminal, %s", command)
	case !yes && !terminal:
		return fmt.Errorf("this asks before it changes your machine, and no one is at a terminal to answer: run %s at one, or pass --yes", command)
	}
	return nil
}

// Choose settles which integrations are on. --integrate=NAMES turns those
// on, --no-integrate turns every one off, and each is recorded; otherwise
// a recorded answer stands, and the person at a terminal is asked once
// about each one found and unanswered, each yes covering only what its
// question named. An application already said yes to is asked again only
// about its versions C4 is not yet in (a newer one, or an earlier c4
// release's files, or an interrupted install), once. Inside an agent
// nothing is asked or turned on, and with no terminal nothing is asked or
// recorded: the install is minimal, and a line says how to bring an
// application up to date. It returns what to turn on now: Claude Code if
// it is on, and each application (NAME, or NAME@VERSION) a yes covers.
func Choose(cfg *Config, found []Detected, integrate string, none, terminal bool, o Out) ([]string, error) {
	if cfg.Integrations == nil {
		cfg.Integrations = map[string]bool{}
	}
	byName := map[string]Detected{}
	for _, d := range found {
		byName[d.Name] = d
	}
	var apps []string // applications to change now
	asked := false
	switch {
	case integrate != "" && none:
		return nil, errors.New("--integrate and --no-integrate: give one")
	case none:
		for _, d := range found {
			cfg.Integrations[d.Name] = false
			if d.Runner {
				cfg.Integrations[RunnerKey] = false
			}
		}
	case integrate != "":
		if err := Consent(true, terminal, "c4 farm up --integrate="+integrate); err != nil {
			return nil, err
		}
		for _, n := range strings.Split(integrate, ",") {
			n = strings.TrimSpace(n)
			d, ok := byName[n]
			if !ok {
				return nil, fmt.Errorf("no integration %q here; c4 farm integrations lists them", n)
			}
			cfg.Integrations[n] = true
			if d.Runner {
				cfg.Integrations[RunnerKey] = true
			}
			if len(d.Versions) > 0 {
				apps = append(apps, n)
			}
		}
	case terminal && o.In != nil && !InsideAgent():
		asked = true
		for _, d := range found {
			on, answered := cfg.Integrations[d.Name]
			switch {
			case !d.Found || d.Question == "":
			case !answered && d.DefaultYes:
				// no answer (the input ended) records nothing: the person is
				// asked when they are there
				yes, gave := enterYes(o, d.Question)
				if !gave {
					continue
				}
				cfg.Integrations[d.Name] = yes
				if d.Runner {
					cfg.Integrations[RunnerKey] = yes
				}
			case !answered:
				o.printf("%s [y/N] ", d.Question)
				line, err := bufio.NewReader(o.In).ReadString('\n')
				if err != nil {
					o.printf("\n")
					continue // the input ended: no answer, none recorded
				}
				a := strings.ToLower(strings.TrimSpace(line))
				cfg.Integrations[d.Name] = a == "y" || a == "yes"
				if cfg.Integrations[d.Name] {
					apps = append(apps, d.Name)
				}
			case on && len(d.Versions) > 0:
				apps = append(apps, offerUpdates(cfg, d, o)...)
			case on && d.Runner:
				if _, asked := cfg.Integrations[RunnerKey]; !asked {
					if yes, gave := enterYes(o, RunnerQuestion()); gave {
						cfg.Integrations[RunnerKey] = yes
					}
				}
			}
		}
	}
	var on []string
	for _, d := range found {
		if len(d.Versions) == 0 && cfg.Integrations[d.Name] {
			on = append(on, d.Name)
		}
		if !asked && !none && integrate == "" && cfg.Integrations[d.Name] {
			if todo := updates(cfg, d); len(todo) > 0 {
				o.printf("%s: c4 farm integrate %s brings C4's panel up to date there\n", andList(titles(todo)), d.Name)
			}
		}
	}
	return append(on, apps...), nil
}

// declined is the record of a no to an offer for f: by version and c4
// release, so a newer release offers it once more.
func declined(f dcc.Found) string { return f.Name + "@" + f.Version + "#" + dcc.Release() }

// updates are an application's versions C4 is not fully in, whose offer
// this release of c4 never had turned down.
func updates(cfg *Config, d Detected) []dcc.Found {
	var todo []dcc.Found
	for _, f := range d.Versions {
		if _, no := cfg.Integrations[declined(f)]; f.Status == dcc.StatusInstalled || no {
			continue
		}
		todo = append(todo, f)
	}
	return todo
}

func titles(fs []dcc.Found) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Title)
	}
	return out
}

// offerUpdates asks, once, about an application already said yes to whose
// versions C4 is not fully in: each one an earlier c4 release's files are
// in, an install was interrupted in, or C4 was never added to. A yes
// covers those versions (NAME@VERSION); a no is recorded for each, so up
// doesn't ask again until a newer c4 release; no answer records nothing.
func offerUpdates(cfg *Config, d Detected, o Out) []string {
	todo := updates(cfg, d)
	if len(todo) == 0 {
		return nil
	}
	var update, finish, add []string
	for _, f := range todo {
		switch f.Status {
		case dcc.StatusOlder:
			update = append(update, f.Title)
		case dcc.StatusIncomplete:
			finish = append(finish, f.Title)
		default:
			add = append(add, f.Title)
		}
	}
	var parts []string
	if len(update) > 0 {
		parts = append(parts, "update it in "+andList(update)+" (an earlier c4 release's)")
	}
	if len(finish) > 0 {
		parts = append(parts, "finish it in "+andList(finish)+" (an install was interrupted)")
	}
	if len(add) > 0 {
		parts = append(parts, "add it to "+andList(add))
	}
	o.printf("C4 Farm panel: %s? c4 farm integrate %s --show says each file first. [y/N] ", andList(parts), d.Name)
	line, err := bufio.NewReader(o.In).ReadString('\n')
	if err != nil {
		o.printf("\n")
		return nil
	}
	a := strings.ToLower(strings.TrimSpace(line))
	var out []string
	for _, f := range todo {
		if a == "y" || a == "yes" {
			out = append(out, f.Name+"@"+f.Version)
		} else {
			for k := range cfg.Integrations { // an earlier release's no, or one kept by version alone
				if k == f.Name+"@"+f.Version || strings.HasPrefix(k, f.Name+"@"+f.Version+"#") {
					delete(cfg.Integrations, k)
				}
			}
			cfg.Integrations[declined(f)] = false
		}
	}
	return out
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
		return "integration claude off: nothing added to Claude Code or this machine; Claude Code can drive the farm from its docs (c4 farm integrate on adds it)"
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
		if len(d.Versions) == 0 {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, state, d.Where, d.Title)
			continue
		}
		for _, f := range d.Versions {
			fmt.Fprintf(tw, "%s@%s\t%s\t%s\t%s: the C4 Farm panel\n", f.Name, f.Version, f.Status, f.Where, f.Title)
		}
	}
	tw.Flush()
	o.printf("c4 farm integrate on|off for Claude Code; c4 farm integrate APP[@VERSION] adds an application's, integrate off APP takes it out (--show says each file, and changes nothing)\n")
}

// App turns an application's integration on or off: on adds C4 to each
// installed version not done yet (NAME@VERSION, one), off takes out each
// one c4 recorded, even if the application is gone. Each change says what
// it does, and what is left for the person; show says what each would
// write or remove and changes nothing. confirm, when given, is asked of
// each change first (nil: already agreed, as at up). A version that cannot
// be done says why, and the others still are.
func App(o Out, name string, on, show bool, confirm func(string) bool) error {
	name, version, _ := strings.Cut(name, "@")
	var found []dcc.Found
	if on {
		found = dcc.Detect()
	} else {
		found = dcc.Installed()
	}
	var mine []dcc.Found
	for _, f := range found {
		if f.Name == name && (version == "" || f.Version == version) {
			mine = append(mine, f)
		}
	}
	if len(mine) == 0 {
		if on {
			return fmt.Errorf("no %s found on this machine; c4 farm integrations lists what is", name)
		}
		o.printf("%s     c4 is in no version of it\n", name)
		return nil
	}
	for _, f := range mine {
		if on && f.Status == dcc.StatusInstalled {
			o.printf("%s  has C4 already\n", f.Title)
			continue
		}
		// asked before anything is planned: a plan may read a folder the
		// system guards (macOS ~/Documents, for Unreal), and only a yes
		// may lead there
		if confirm != nil && !confirm(f.Title+": "+map[bool]string{true: "add the C4 Farm panel", false: "take C4 out"}[on]+"? (--show lists each file first)") {
			o.printf("%s  not changed\n", f.Title)
			continue
		}
		c, err := dcc.Plan(f, on)
		if err != nil {
			o.printf("%s  not changed: %v\n", f.Title, err)
			continue
		}
		if show {
			o.printf("%s  would: %s\n", f.Title, c.Line)
			for _, w := range c.Writes {
				o.printf("          writes  %s\n", tildePath(w))
			}
			for _, r := range c.Removes {
				o.printf("          removes %s\n", tildePath(r))
			}
			continue
		}
		if err := dcc.Apply(c); err != nil {
			o.printf("%s  not changed: %v\n", f.Title, err)
			continue
		}
		o.printf("%s  %s\n", f.Title, c.Line)
		for _, n := range c.Notes {
			o.printf("          then: %s\n", n)
		}
	}
	return nil
}
