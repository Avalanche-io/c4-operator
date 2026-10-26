// Command c4-farm is `c4 farm`, the user command for a C4 farm: submit
// work, watch it, move its deadline, and take its output home.
package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"golang.org/x/term"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/internal/localfarm"
	"github.com/Avalanche-io/c4-operator/internal/mcpserver"
	"github.com/Avalanche-io/c4-operator/internal/power"
)

var version = "dev"

const usage = `c4 farm: submit work to a C4 farm, watch it, and take its output home.

The local farm
  up [--as ROLE]           make a farm on this machine in Docker and open its HUD (another farm's ports taken, it takes free ones)
  down                     delete the local farm
  status                   the local farm's nodes, slots and sign-ins
  image load IMAGE...      push images from this machine's Docker into the local farm's registry

Farms
  use FARM                 switch the current farm
  login HUD-URL            add a farm and sign in (--token-file, or --as on a local farm)
  whoami                   who the farm sees you as, and your projects

Work
  samples get [SET]        download the sample shots and repositories to ~/c4-samples
  samples load [NAME]      load a guide's scenario onto the local farm (no NAME lists them)
  types                    the prebuilt job types
  submit TYPE ASSET        plan a job, show the estimate, and submit it
  submit FILE              send a submission (c4: submit/1): its plan and estimate first (--dry-run, --explain)
  ls                       the queue, in run order (--mine, --at-risk, --all)
  watch JOB                the job's progress, deadline and slack as they change
  wait UNIT...             wait until units end or stop on a question, then print what to act on (job/unit; --any for the first, --json, --timeout D); exit 0 done, 1 failed, 2 timed out, 10 a question
  open [JOB]               open the job view in the HUD (--widget for the side widget); no JOB, what is waiting on you (--as ROLE: a local farm's role)
  deadline JOB WHEN        move a job's deadline (18:00, fri 18:00, +3h, suggested)
  hold JOB [--stop]        hold a job; --stop also stops its running units
  release JOB              release a held job
  boost JOB N              ask for N times the slots its deadline needs (1 to 2)
  requeue JOB UNITS        run units again ("14", "0-9,14")
  get JOB|VIEW [PATH...] DIR   write a job's output under DIR, checking every C4 ID
  trace JOB FILE           a file of its output: the signed record of the unit that made it, and what it read

Tools
  tools                    the tools you can submit
  tool new KIND DIR        start a tool: a folder with a tool.yaml, a script and a test (frames or once)
  tool test DIR            check a tool and run its test here, in Docker (--frames, --with K=V, --keep, --log FILE)
  tool publish DIR         store a tool, test it on the farm, offer it to the project (--farm-wide: every project)
  tool show NAME           a published tool and its parameters

Trackers
  grant TRACKER            let a tracker submit work for you (--pools, --classes, --until 30d)
  grants [--tracker NS/T]  your grants, or who has granted a tracker
  revoke NS/GRANT          take a grant back

Agents
  agent start              keep this machine's power reader and host runner running, as a service (agent stop removes it)
  agent run                what the service runs: the power reader and c4-agent runner, restarted if it stops
  agent power              offer this machine's agent slots by its battery: stop below 25%, resume at 90% (--once to read it now)
  agents                   the Claude Code sessions on this machine, read-only: each one's state, and what needs you (--all, --open for the HUD)
  enroll NODE CODE         make a machine yours with the code c4-agent enroll-offer prints on it: your host work runs there (--for PERSON adds them, on your sign-in)
  agents add [NAME]        make a Claude Code session an agent the farm can run, with c4-agent's hooks in its folder (asked first; no NAME lists the sessions to pick from)
  agents rm NAME           take out what agents add put in
  agents ls                your agents: busy, idle or stopped, and the last turn saved
  diagnose                 a redacted bundle of the farm's decisions for a bug report: no names, paths or contents (--show first)
  telemetry                the farm's telemetry: its tier, what it keeps and its key; telemetry on|off|rotate, telemetry resolve PSEUDONYM
  integrations             what this machine could add (Claude Code, and each application found), and which are on
  integrate on|off         Claude Code's integration: the farm's tools, the agents' hooks, this machine as runner
  integrate APP            add the C4 Farm panel to an application (APP@VERSION for one; --show says each file); integrate off APP takes it out
  mcp                      serve the farm's tools to an agent over stdio

Flags for every command: --farm NAME, --project NAME, --utc (times in UTC).
Times are shown on this machine's clock.
`

// common are the flags every command takes.
type common struct {
	farm, project string
	utc           bool
}

func (c *common) add(fs *flag.FlagSet) {
	fs.StringVar(&c.farm, "farm", "", "the farm to use (default the current one)")
	fs.StringVar(&c.project, "project", "", "the project (default the farm's, or your only one)")
	fs.BoolVar(&c.utc, "utc", false, "show times in UTC instead of this machine's time")
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	err := run(os.Args[1:], os.Stdout, os.Stdin)
	var exit *farmcli.Exit
	switch {
	case errors.Is(err, flag.ErrHelp):
		// asked for help, given it: not an error
	case errors.As(err, &exit):
		os.Exit(exit.Code) // the command said what it had to
	case err != nil:
		fmt.Fprintln(os.Stderr, "c4 farm:", err)
		os.Exit(1)
	}
}

// parse parses flags that may come after positional arguments
// (`c4 farm deadline sm0140 15:00 --project saltmarsh`).
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func run(args []string, stdout io.Writer, stdin io.Reader) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		if n := c4PathNote(context.Background()); n != "" {
			fmt.Fprintf(stdout, "\n%s\n", n)
		}
		return nil
	}
	if strings.HasPrefix(version, "v") {
		// the prebuilt job images are released with the binary
		farmcli.ImageTag = version
	} else {
		farmcli.DevBuild = true
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cm common
	cm.add(fs)

	cfg, err := farmcli.LoadConfig(farmcli.ConfigPath())
	if err != nil {
		return err
	}
	// connect resolves the farm, its client, the project and the output.
	connect := func() (*farmcli.Client, farmcli.Farm, farmcli.Out, string, error) {
		_, f, err := cfg.Resolve(cm.farm)
		if err != nil {
			return nil, f, farmcli.Out{}, "", err
		}
		c, err := farmcli.NewClient(f)
		if err != nil {
			return nil, f, farmcli.Out{}, "", err
		}
		o := farmcli.Out{W: stdout, In: stdin, Clock: farmcli.Clock{UTC: cm.utc}}
		return c, f, o, cm.project, nil
	}
	need := func(pos []string, n int, what string) error {
		if len(pos) != n {
			return fmt.Errorf("usage: c4 farm %s %s", cmd, what)
		}
		return nil
	}

	switch cmd {
	case "version":
		fmt.Fprintln(stdout, "c4-farm", version)
		return nil

	case "use":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "FARM"); err != nil {
			return err
		}
		if _, _, err := cfg.Resolve(pos[0]); err != nil {
			return err
		}
		cfg.Current = pos[0]
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "using %s\n", pos[0])
		return nil

	case "login":
		name := fs.String("name", "", "a name for the farm (default the HUD's host)")
		as := fs.String("as", "", "a development viewer, on a local farm")
		tokenFile := fs.String("token-file", "", "a file holding your bearer token")
		integrateOn := fs.String("integrate", "", "the integrations to add, without asking: claude")
		integrateOff := fs.Bool("no-integrate", false, "add no integration, without asking")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "HUD-URL [--as VIEWER | --token-file FILE]"); err != nil {
			return err
		}
		if *as == "" && *tokenFile == "" {
			return errors.New("sign in with --token-file FILE (your studio's farm) or --as VIEWER (a local farm)")
		}
		hud := strings.TrimSuffix(pos[0], "/")
		n := *name
		if n == "" {
			n = strings.TrimPrefix(strings.TrimPrefix(hud, "https://"), "http://")
			n = strings.SplitN(n, "/", 2)[0]
			n = strings.ReplaceAll(n, ":", "-")
		}
		f := farmcli.Farm{HUD: hud, As: *as, TokenFile: *tokenFile, Project: cm.project,
			Local: strings.Contains(hud, "127.0.0.1") || strings.Contains(hud, "localhost")}
		c, err := farmcli.NewClient(f)
		if err != nil {
			return err
		}
		s, err := farmcli.Snapshot(ctx, c)
		if err != nil {
			return err
		}
		cfg.Farms[n], cfg.Current = f, n
		wasOn := cfg.Integrations[farmcli.Claude]
		on, err := farmcli.Choose(cfg, farmcli.Detect(false), *integrateOn, *integrateOff, isTerminal(stdin), farmcli.Out{W: stdout, In: stdin})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "signed in to %s as %s · projects %s\n", n, s.Viewer.User, strings.Join(s.Viewer.Projects, ", "))
		switch in := (&farmcli.Integration{Record: cfg.Integrations}); {
		case slices.Contains(on, farmcli.Claude):
			// a farm signed in to, not made here: its tools in Claude Code,
			// and no runner (that is the farm admin's to enroll)
			in.On(ctx, farmcli.Out{W: stdout})
		case *integrateOff && wasOn:
			// --no-integrate takes out what is there, not only the answer
			in.Off(ctx, farmcli.Out{W: stdout})
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		applyApps(stdout, on)
		if l := farmcli.IntegrateLine(cfg); l != "" {
			fmt.Fprintf(stdout, "%s\n", l)
		}
		return nil

	case "whoami":
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		c, f, o, _, err := connect()
		if err != nil {
			return err
		}
		s, err := farmcli.Snapshot(ctx, c)
		if err != nil {
			return err
		}
		role := s.Viewer.Role
		switch {
		case role != "":
		case s.Viewer.All:
			role = "wrangler"
		default:
			role = "artist"
		}
		if f.Owner != "" && s.Viewer.User == f.Owner {
			// the farm's one person: every role is theirs
			role = "owner, every role"
		} else if f.Owner != "" {
			role += ", a role's view of " + f.Owner + "'s farm"
		}
		fmt.Fprintf(o.W, "%s · %s · projects %s\n", s.Viewer.User, role, strings.Join(s.Viewer.Projects, ", "))
		return nil

	case "types":
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		for _, t := range farmcli.Types() {
			fmt.Fprintf(stdout, "%-10s %-7s %s\n", t.Name, t.Class, t.Summary)
		}
		return nil

	case "submit":
		var a farmcli.SubmitArgs
		var sets, after multi
		fs.StringVar(&a.Options.Frames, "frames", "", "the frame range, for example 1001-1048")
		fs.StringVar(&a.Deadline, "deadline", "", "when it is due: 18:00, fri 18:00, +3h, RFC 3339, or suggested (the default)")
		fs.StringVar(&a.Name, "name", "", "the job's name (default <shot>-<stage>-vNNN, the next version)")
		fs.StringVar(&a.Shot, "shot", "", "the shot (default from the asset's name)")
		fs.StringVar(&a.Stage, "stage", "", "the pipeline stage, the middle of the default name (default the type's: light, layout, fx, comp, review, ...)")
		fs.StringVar(&a.Options.Renderer, "renderer", "", "cycles (default), karma-xpu, karma-cpu, prman, arnold, vray")
		fs.StringVar(&a.Options.Image, "image", "", "run this image instead of the type's")
		fs.StringVar(&a.Template, "template", "", "a WorkJob spec file to submit instead of a prebuilt type")
		fs.StringVar(&a.Root, "root", "", "the folder the job mounts (default the one holding .c4project, else the asset's)")
		pools := fs.String("pools", "", "pools the units may use, comma-separated")
		fs.Var(&sets, "set", "a type parameter KEY=VALUE (repeatable)")
		fs.Var(&after, "after", "wait for this job and mount its output (repeatable)")
		fs.BoolVar(&a.DryRun, "dry-run", false, "show the plan and change nothing")
		fs.BoolVar(&a.Yes, "yes", false, "don't ask before submitting")
		explain := fs.Bool("explain", false, "with a submission file: show what each job becomes on the farm, and change nothing")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) == 1 && a.Template == "" && submissionFile(pos[0]) {
			c, f, o, project, err := connect()
			if err != nil {
				return err
			}
			if project == "" {
				project = f.Project
			}
			if a.Yes {
				o.In = nil
			}
			_, err = farmcli.SubmitFile(ctx, c, o, farmcli.SubmitFileArgs{Path: pos[0], Project: project, DryRun: a.DryRun, Explain: *explain, Yes: a.Yes,
				Terminal: isTerminal(stdin)})
			return err
		}
		switch {
		case a.Template != "" && len(pos) == 1:
			base := filepath.Base(a.Template)
			a.Type, a.Asset = strings.TrimSuffix(base, filepath.Ext(base)), pos[0]
		case len(pos) == 2:
			a.Type, a.Asset = pos[0], pos[1]
		default:
			return errors.New("usage: c4 farm submit TYPE ASSET [flags], or --template FILE ASSET (c4 farm types lists the types)")
		}
		a.Options.Set = map[string]string{}
		for _, s := range sets {
			k, v, ok := strings.Cut(s, "=")
			if !ok {
				return fmt.Errorf("--set %q: want KEY=VALUE", s)
			}
			a.Options.Set[k] = v
		}
		if *pools != "" {
			a.Options.Pools = strings.Split(*pools, ",")
		}
		a.After = after
		c, f, o, project, err := connect()
		if err != nil {
			return err
		}
		if a.Project, err = farmcli.ResolveProject(ctx, c, project, f.Project); err != nil {
			return err
		}
		if a.Yes {
			o.In = nil
		}
		_, err = farmcli.Submit(ctx, c, o, a)
		return err

	case "tools":
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		c, f, o, project, err := connect()
		if err != nil {
			return err
		}
		if project, err = farmcli.ResolveProject(ctx, c, project, f.Project); err != nil {
			return err
		}
		return farmcli.ListTools(ctx, c, o, project)

	case "tool":
		return toolCommand(ctx, fs, rest, stdout, connect)

	case "ls":
		var a farmcli.LsArgs
		fs.BoolVar(&a.Mine, "mine", false, "only your jobs")
		fs.BoolVar(&a.AtRisk, "at-risk", false, "only jobs that will miss their deadlines")
		fs.BoolVar(&a.All, "all", false, "include finished jobs")
		fs.BoolVar(&a.ToolTests, "tool-tests", false, "also the farm's tests of its tools, which publishing a tool runs")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		c, _, o, project, err := connect()
		if err != nil {
			return err
		}
		a.Project = project
		return farmcli.Ls(ctx, c, o, a)

	case "trace":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 2, "JOB FILE"); err != nil {
			return err
		}
		c, _, o, project, err := connect()
		if err != nil {
			return err
		}
		return farmcli.Trace(ctx, c, o, project, pos[0], pos[1])

	case "wait":
		anyOne := fs.Bool("any", false, "return when the first of the units settles")
		asJSON := fs.Bool("json", false, "print the units as JSON")
		timeout := fs.Duration("timeout", 0, "give up after this long (exit 2); 0 waits as long as it takes")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "UNIT..."); err != nil {
			return err
		}
		c, _, o, project, err := connect()
		if err != nil {
			return err
		}
		return farmcli.Wait(ctx, c, o, project, farmcli.WaitArgs{Units: pos, Any: *anyOne, JSON: *asJSON, Timeout: *timeout, Errs: os.Stderr})

	case "watch":
		every := fs.Duration("every", 2*time.Second, "how often to look")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "JOB"); err != nil {
			return err
		}
		c, _, o, project, err := connect()
		if err != nil {
			return err
		}
		return farmcli.Watch(ctx, c, o, project, pos[0], *every)

	case "open":
		widget := fs.Bool("widget", false, "the side widget, for a second monitor")
		as := fs.String("as", "", "on a local farm, open it as one of its roles to try that view: artist, supervisor, wrangler, admin, facilities or td")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) > 1 {
			return errors.New("usage: c4 farm open [JOB] [--widget] [--as ROLE]")
		}
		c, f, o, project, err := connect()
		if err != nil {
			return err
		}
		if *as != "" {
			if !f.Local {
				return errors.New("--as opens a local farm's role; a studio's farm opens as you")
			}
			v, err := localfarm.LookupViewer(*as)
			if err != nil {
				return err
			}
			c.As = v.Name
		}
		// no JOB: what is waiting on you
		u := c.Web("needs", nil)
		if len(pos) == 1 {
			if u, err = farmcli.Open(ctx, c, project, pos[0], *widget); err != nil {
				return err
			}
		}
		if err := openSignedIn(ctx, f, u); err != nil {
			return err
		}
		// the link is the page, and signs no other browser in
		fmt.Fprintf(o.W, "opened    in your browser, signed in: %s\n", u)
		return nil

	case "deadline":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return errors.New("usage: c4 farm deadline JOB WHEN (18:00, fri 18:00, +3h, RFC 3339, or suggested)")
		}
		c, _, o, project, err := connect()
		if err != nil {
			return err
		}
		return farmcli.Deadline(ctx, c, o, project, pos[0], strings.Join(pos[1:], " "))

	case "grant":
		pools := fs.String("pools", "", "only jobs in these pools (comma-separated)")
		classes := fs.String("classes", "", "only jobs of these classes (comma-separated)")
		until := fs.String("until", "", "when it ends: an RFC 3339 time or a span like 30d (default 30d, at most 90d)")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "TRACKER [--pools a,b] [--classes graph] [--until 30d]"); err != nil {
			return err
		}
		c, _, o, project, err := connect()
		if err != nil {
			return err
		}
		return farmcli.Grant(ctx, c, o, project, api.GrantRequest{Tracker: pos[0], Pools: list(*pools), Classes: list(*classes), Until: *until})

	case "grants":
		tracker := fs.String("tracker", "", "who has granted this tracker (namespace/name)")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		c, _, o, _, err := connect()
		if err != nil {
			return err
		}
		return farmcli.Grants(ctx, c, o, *tracker)

	case "revoke":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "NAMESPACE/GRANT"); err != nil {
			return err
		}
		c, _, o, _, err := connect()
		if err != nil {
			return err
		}
		return farmcli.Revoke(ctx, c, o, pos[0])

	case "hold", "release":
		stopRunning := fs.Bool("stop", false, "also stop the units running now")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "JOB"); err != nil {
			return err
		}
		c, _, o, project, err := connect()
		if err != nil {
			return err
		}
		body := map[string]any{"hold": cmd == "hold"}
		if cmd == "hold" && *stopRunning {
			body = map[string]any{"holdStop": true}
		}
		return farmcli.Control(ctx, c, o, project, pos[0], "nudge", body)

	case "boost":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 2, "JOB N (1 to 2)"); err != nil {
			return err
		}
		var n float64
		if _, err := fmt.Sscanf(pos[1], "%g", &n); err != nil || n < 1 || n > 2 {
			return fmt.Errorf("boost %q: a number from 1 to 2", pos[1])
		}
		c, _, o, project, err := connect()
		if err != nil {
			return err
		}
		return farmcli.Control(ctx, c, o, project, pos[0], "nudge", map[string]any{"boost": n})

	case "requeue":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 2, `JOB UNITS ("14", "0-9,14")`); err != nil {
			return err
		}
		c, _, o, project, err := connect()
		if err != nil {
			return err
		}
		return farmcli.Control(ctx, c, o, project, pos[0], "requeue", map[string]any{"units": pos[1]})

	case "get":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return errors.New("usage: c4 farm get JOB|VIEW-ID [PATH ...] DIR")
		}
		c, f, o, project, err := connect()
		if err != nil {
			return err
		}
		a := farmcli.GetArgs{Ref: pos[0], Paths: pos[1 : len(pos)-1], Dir: pos[len(pos)-1]}
		if a.Project, err = farmcli.ResolveProject(ctx, c, project, f.Project); err != nil {
			return err
		}
		return farmcli.Get(ctx, c, o, a)

	case "mcp":
		agent := fs.String("agent", "", "the agent's name; the book records its actions as agent/NAME for you")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		c, _, _, _, err := connect()
		if err != nil {
			return err
		}
		mcpserver.Version = version
		return mcpserver.Serve(ctx, &mcpserver.Client{Base: c.Base, Token: c.Token, As: c.As, Installer: c.Installer, Agent: *agent}, &mcp.StdioTransport{})

	case "samples":
		from := fs.String("from", "", "a local sample archive instead of the release's")
		localName := fs.String("name", "", "the local farm a scenario loads onto (default the current farm, when it is local)")
		dir := fs.String("dir", "", "where to unpack (default ~/c4-samples)")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) > 0 && pos[0] == "load" {
			return loadScenario(ctx, stdout, cfg, cm.utc, *localName, pos[1:])
		}
		if len(pos) == 0 || pos[0] != "get" {
			return errors.New("usage: c4 farm samples get [SET] [--dir DIR] [--from ARCHIVE], or c4 farm samples load [SCENARIO]")
		}
		set := ""
		if len(pos) > 1 {
			set = pos[1]
		}
		o := farmcli.Out{W: stdout, Clock: farmcli.Clock{UTC: cm.utc}}
		return farmcli.GetSamples(ctx, o, farmcli.SamplesArgs{Set: set, Version: version, Dir: *dir, From: *from})

	case "up":
		localName := fs.String("name", localfarm.Name, "the local farm's name, for a second farm beside the first (with --port and --store-port)")
		as := fs.String("as", "", "open the HUD as a role instead of as the owner: artist, supervisor, wrangler, admin, facilities or td")
		plan := fs.Bool("plan", false, "print the cluster config and the objects, and change nothing")
		install := fs.String("install", "", "the operator's install manifest (default this release's)")
		archive := fs.String("image-archive", "", "an operator image archive to load into the cluster (a development build)")
		image := fs.String("image", "", "the operator image the cluster runs, with --image-archive")
		noOpen := fs.Bool("no-open", false, "don't open the HUD")
		port := fs.Int("port", 0, "for a new farm, the HUD's port on this machine (default 30080, or the next free one); an existing farm keeps its own")
		storePort := fs.Int("store-port", 0, "for a new farm, the store's port on this machine (default 30900, or the next free one); an existing farm keeps its own")
		registryPort := fs.Int("registry-port", localfarm.RegistryPort, "the farm's image registry's port on this machine")
		integrateOn := fs.String("integrate", "", "the integrations to add, without asking: claude (c4 farm integrations lists them)")
		integrateOff := fs.Bool("no-integrate", false, "the minimal install: no integration added, without asking")
		owner := fs.String("owner", "", "for a new farm: who owns it and plays every role on it (default the name in your git user.email, else your login); an existing farm keeps its owner")
		trackerOn := fs.Bool("tracker", false, "add a stand-in GitHub and c4-sync: issues you open become jobs, its reviews serve them (nothing leaves this machine)")
		syncInstall := fs.String("sync-install", "", "c4-sync's install manifest, with --tracker (default this release's)")
		syncImage := fs.String("sync-image", "", "the c4-sync image, with --tracker; one only in this machine's Docker (c4-sync:dev) is pushed to the farm's registry")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		if n := c4PathNote(ctx); n != "" {
			fmt.Fprintf(stdout, "%s\n", n)
		}
		localfarm.Name = *localName
		tz := localfarm.LocalTZ()
		lat, lon := localfarm.Place(tz)
		o := localfarm.UpOptions{Options: localfarm.Options{TZ: tz, Lat: lat, Lon: lon, OperatorImage: *image, HUDHostPort: *port, StoreHostPort: *storePort,
			RegistryHostPort: *registryPort, LocalImages: !strings.HasPrefix(version, "v") && *install != "", Owner: *owner, Tracker: *trackerOn},
			Install: *install, Version: version, ImageArchive: *archive, SyncInstall: *syncInstall, SyncImage: *syncImage, Log: stdout,
			Timings: &localfarm.Timings{}}
		// the owner is chosen when the farm is made, and kept after
		if o.Owner, err = localfarm.ChooseOwner(cfg.Farms[localfarm.Name].Owner, o.Owner); err != nil {
			return err
		}
		// the owner signs in as themselves; --as opens a role's view
		v := localfarm.OwnerViewer(o.Owner)
		if *as != "" {
			if v, err = localfarm.LookupViewer(*as); err != nil {
				return err
			}
		}
		if *plan {
			fmt.Fprintf(stdout, "%s---\n%s", localfarm.KindConfig(o.Options), localfarm.YAML(localfarm.Farm(o.Options)))
			fmt.Fprintf(stdout, "---\n# networks tools may declare; up adds only those the farm does not define yet\n%s", localfarm.YAML([]map[string]any{localfarm.NetworksObject()}))
			if *trackerOn {
				objs, err := localfarm.TrackerObjects(o.Options)
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "---\n# with --tracker, after c4-sync's install manifest\n%s", localfarm.YAML(objs))
			}
			fmt.Fprintf(stdout, "---\n# manager arguments added: %s\n", strings.Join(localfarm.ManagerArgs(o.Options), " "))
			return nil
		}
		if err := localfarm.CheckDocker(ctx); err != nil {
			return err
		}
		if o.InstallerToken, err = localfarm.InstallerToken(localfarm.Name); err != nil {
			return err
		}
		// the tracker is offered once, on the farm's first up
		first := true
		if have, err := localfarm.Exists(localfarm.Name); err == nil {
			first = !have
		}
		// the ports the farm has (an existing one's, from its node) or, for
		// a new one, free ones: never an assumed 30080 another farm holds
		if err := localfarm.ResolvePorts(ctx, &o.Options, !first, func(f string, a ...any) { fmt.Fprintf(stdout, f, a...) }); err != nil {
			return err
		}
		if err := localfarm.Up(ctx, o); err != nil {
			return err
		}
		if err := bindFarm(ctx, o.Options); err != nil {
			return err
		}
		cfg.Farms[localfarm.Name] = farmcli.Farm{HUD: o.HUD(), As: v.Name, Project: localfarm.Project, Local: true, Owner: o.Owner,
			InstallerTokenFile: localfarm.InstallerTokenFile(localfarm.Name)}
		cfg.Current = localfarm.Name
		ui := upIntegration{integrate: *integrateOn, none: *integrateOff, stdin: stdin, stdout: stdout,
			detect: farmcli.Detect(true), runner: localRunner(o.Owner, o.HUD(), stdin, stdout)}
		on, wasOn, err := ui.choose(cfg)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nfarm      %s   %s · store ready\n", localfarm.Name, localfarm.Capacity())
		fmt.Fprintf(stdout, "project   %s   you: %s\n", localfarm.Project, v.Name)
		fmt.Fprintf(stdout, "owner     %s, who plays every role here\n", o.Owner)
		if err := ui.apply(ctx, cfg, on, wasOn); err != nil {
			return err
		}
		// a command, not a link: the HUD takes a browser only once c4 farm
		// open has signed it in, and a printed sign-in lapses in minutes
		fmt.Fprintf(stdout, "HUD       %s   c4 farm open, any time, opens it signed in\n", o.HUD())
		fmt.Fprintf(stdout, "roles     artist, supervisor, wrangler, admin, facilities, td, to try their views (c4 farm open --as ROLE)\n")
		if o.Tracker || localfarm.HasTracker(ctx) {
			// a command, not a link: the page is the HUD's, which takes a
			// browser c4 farm open has signed in
			fmt.Fprintf(stdout, "tracker   the stand-in tracker: c4 farm open, then /sandbox/ in that window; a stand-in for GitHub, and nothing leaves this machine\n")
		} else if first {
			fmt.Fprintf(stdout, "try a tracker: c4 farm up --tracker\n")
		}
		// where up's time went, so a slow first run says load or code
		fmt.Fprintf(stdout, "time      %s\n", o.Timings)
		if *noOpen {
			return nil
		}
		return openSignedIn(ctx, cfg.Farms[localfarm.Name], localfarm.NeedsLink(o.HUD(), v))

	case "down":
		localName := fs.String("name", "", "the local farm to delete (default the current farm, when it is local)")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		// with no --name, the current local farm, never a guess: another
		// person's farm may run on this machine too
		if *localName == "" {
			clusters, err := localfarm.Clusters()
			if err != nil {
				return err
			}
			if *localName, _, err = localFarm(cfg, clusters, "", "down deletes"); err != nil {
				return err
			}
		}
		localfarm.Name = *localName
		// this machine's runner goes with the farm only if it serves this
		// farm: read before the farm (and its API server) is gone
		home, _ := os.UserHomeDir()
		serves := home != "" && localfarm.RunnerServes(home, localfarm.Name)
		if err := localfarm.Down(localfarm.Name); err != nil {
			return err
		}
		if err := localfarm.RemoveInstallerToken(localfarm.Name); err != nil {
			return err
		}
		if serves {
			(&farmcli.AgentService{}).Stop(ctx, farmcli.Out{W: io.Discard})
			os.RemoveAll(filepath.Join(home, ".c4", "agents", "runner"))
			fmt.Fprintf(stdout, "agents    this machine's runner for it is stopped and its enrollment removed\n")
		}
		delete(cfg.Farms, localfarm.Name)
		if cfg.Current == localfarm.Name {
			cfg.Current = ""
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "the local farm %s is gone\n", localfarm.Name)
		return nil

	case "status":
		localName := fs.String("name", "", "the local farm (default the current farm, when it is local)")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		if *localName == "" {
			clusters, err := localfarm.Clusters()
			if err != nil {
				return err
			}
			*localName, _, err = localFarm(cfg, clusters, "", "status reports on")
			if errors.Is(err, errNoLocalFarm) {
				fmt.Fprintf(stdout, "no local farm; `c4 farm up` makes one\n")
				return nil
			}
			if err != nil {
				return err
			}
		}
		localfarm.Name = *localName
		have, err := localfarm.Exists(localfarm.Name)
		if err != nil {
			return err
		}
		if !have {
			fmt.Fprintf(stdout, "no local farm; `c4 farm up` makes one\n")
			return nil
		}
		// The HUD is where up put it, and where the farm's node maps it.
		hud, inst := localfarm.Options{}.HUD(), ""
		if f, ok := cfg.Farms[localfarm.Name]; ok && f.HUD != "" {
			hud, inst = f.HUD, f.InstallerTokenFile
		}
		if p, _, err := localfarm.MappedPorts(ctx, localfarm.Name); err == nil && p != 0 {
			if real := (localfarm.Options{HUDHostPort: p}).HUD(); real != hud {
				fmt.Fprintf(stdout, "note      farm.yaml has %s's HUD at %s, and its node maps it to %s; c4 farm up --name %s records it\n", localfarm.Name, hud, real, localfarm.Name)
				hud = real
			}
		}
		c, err := farmcli.NewClient(farmcli.Farm{HUD: hud, As: "wrangler", InstallerTokenFile: inst})
		if err != nil {
			return err
		}
		s, err := farmcli.Snapshot(ctx, c)
		if err != nil {
			// a farm whose HUD does not answer is not well: a script sees it
			why := err.Error()
			if u, perr := url.Parse(hud); perr == nil {
				if p, _ := strconv.Atoi(u.Port()); p != 0 {
					if owner := localfarm.PortOwner(ctx, p); owner != "" && owner != localfarm.Name {
						why += fmt.Sprintf("; 127.0.0.1:%d belongs to %s", p, owner)
					}
				}
			}
			return fmt.Errorf("the local farm %s is running, and its HUD does not answer yet: %s", localfarm.Name, why)
		}
		fmt.Fprintf(stdout, "farm      %s   %d nodes · %d of %d slots in use · %d units waiting\n", localfarm.Name, len(s.Nodes), s.Farm.SlotsUsed, s.Farm.SlotsTotal, s.Farm.QueuedUnits)
		fmt.Fprintf(stdout, "HUD       %s   c4 farm open opens it signed in\n", hud)
		for _, v := range localfarm.Viewers {
			fmt.Fprintf(stdout, "  %-10s c4 farm open --as %s\n", v.Name, v.Name)
		}
		return nil

	case "image":
		rp := fs.Int("registry-port", localfarm.RegistryPort, "the farm's image registry's port on this machine")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) < 2 || pos[0] != "load" {
			return errors.New("usage: c4 farm image load IMAGE... (push images from this machine's Docker into the local farm's registry)")
		}
		names, err := localfarm.LoadImages(ctx, *rp, pos[1:])
		for i, n := range names {
			fmt.Fprintf(stdout, "loaded    %s as %s\n", pos[1+i], n)
		}
		return err

	case "agent":
		slotsFile := fs.String("slots-file", "", "the host runner's --slots-file (default ~/.c4/agents/slots)")
		slots := fs.Int("slots", 4, "the slots to offer while work runs")
		stopBelow := fs.Int("stop-below", 25, "stop work when the battery, not charging, falls below this percent")
		resumeAt := fs.Int("resume-at", 90, "resume stopped work once the battery is back at this percent")
		every := fs.Duration("every", time.Minute, "how often to read the battery")
		once := fs.Bool("once", false, "read the battery, write the slots and stop")
		agentBin := fs.String("agent-bin", "", "with run or start: c4-agent (default c4-agent on PATH)")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		o := farmcli.Out{W: stdout, Clock: farmcli.Clock{UTC: cm.utc}}
		svc := &farmcli.AgentService{AgentBin: *agentBin, Slots: *slots}
		switch {
		case len(pos) == 1 && pos[0] == "power":
			return farmcli.AgentPower(ctx, o, farmcli.PowerArgs{SlotsFile: *slotsFile, Every: *every, Once: *once,
				Policy: power.Policy{Slots: *slots, StopBelow: *stopBelow, ResumeAt: *resumeAt}})
		case len(pos) == 1 && pos[0] == "run":
			return farmcli.AgentRun(ctx, o, farmcli.AgentRunArgs{AgentBin: *agentBin, Slots: *slots, SlotsFile: *slotsFile})
		case len(pos) == 1 && pos[0] == "start":
			// a background service on this machine: its person starts it
			if err := farmcli.Consent(false, isTerminal(stdin), "c4 farm agent start"); err != nil {
				return err
			}
			return svc.Start(ctx, o)
		case len(pos) == 1 && pos[0] == "stop":
			return svc.Stop(ctx, o)
		}
		return errors.New("usage: c4 farm agent start | stop | run | power [--slots N] [--stop-below 25] [--resume-at 90] [--slots-file F] [--once]")

	case "diagnose":
		window := fs.String("window", "", "only the decisions within this long of the latest (30m, 2h; default all the farm keeps)")
		out := fs.String("out", "", "where to write the bundle (default its own name, in this folder)")
		show := fs.Bool("show", false, "print what the bundle holds, its README and every field, and write nothing")
		seal := fs.Bool("seal", false, "also write it sealed for C4's maintainers (not yet in this build)")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		if *seal {
			return errors.New("--seal is not in this build yet: it seals to the C4 project's key, which comes with a later release; attach the plain bundle only where you would show its README")
		}
		c, _, o, _, err := connect()
		if err != nil {
			return err
		}
		return farmcli.Diagnose(ctx, c, o, farmcli.DiagnoseArgs{Window: *window, Out: *out, Show: *show})

	case "telemetry":
		yes := fs.Bool("yes", false, "rotate without asking, where no one is at a terminal")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		usage := errors.New("usage: c4 farm telemetry [on | off | rotate [--yes] | resolve PSEUDONYM]")
		sub := ""
		if len(pos) > 0 {
			sub = pos[0]
		}
		switch {
		case len(pos) > 2, (len(pos) == 2) != (sub == "resolve"), !slices.Contains([]string{"", "on", "off", "rotate", "resolve"}, sub):
			return usage
		case sub == "rotate" && !*yes:
			// the decisions kept go with the old key, so the person says so
			if !isTerminal(stdin) {
				return errors.New("rotating the farm's telemetry key lets go of the decisions it keeps: pass --yes where no one is at a terminal")
			}
			if !farmcli.Ask(farmcli.Out{W: stdout, In: stdin}, "Rotate the farm's telemetry key? Pseudonyms from now on are unrelated to earlier ones, and the decisions the farm keeps are let go.") {
				fmt.Fprintf(stdout, "nothing was changed\n")
				return nil
			}
		}
		c, _, o, _, err := connect()
		if err != nil {
			return err
		}
		switch sub {
		case "resolve":
			return farmcli.TelemetryResolve(ctx, c, o, pos[1])
		case "on":
			return farmcli.TelemetryTier(ctx, c, o, "b")
		case "off":
			return farmcli.TelemetryTier(ctx, c, o, "a")
		case "rotate":
			return farmcli.TelemetryRotate(ctx, c, o)
		}
		return farmcli.TelemetryStatus(ctx, c, o)

	case "integrations":
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		farmcli.Integrations(cfg, farmcli.Detect(false), farmcli.Out{W: stdout})
		return nil

	case "integrate":
		show := fs.Bool("show", false, "say every file it would write or remove, and change nothing (an application's)")
		yes := fs.Bool("yes", false, "change it without asking, where no one is at a terminal (never from inside an agent)")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		// integrate on|off (Claude Code), integrate APP, integrate off APP;
		// APP on|off too
		var target string
		on := true
		switch {
		case len(pos) == 1 && (pos[0] == "on" || pos[0] == "off"):
			target, on = farmcli.Claude, pos[0] == "on"
		case len(pos) == 1:
			target = pos[0]
		case len(pos) == 2 && pos[0] == "off":
			target, on = pos[1], false
		case len(pos) == 2 && (pos[1] == "on" || pos[1] == "off"):
			target, on = pos[0], pos[1] == "on"
		default:
			return errors.New("usage: c4 farm integrate on|off (Claude Code) | integrate APP[@VERSION] [--show] [--yes] | integrate off APP (c4 farm integrations lists them)")
		}
		pos = []string{target}
		o := farmcli.Out{W: stdout}
		name, _, _ := strings.Cut(pos[0], "@")
		// nothing here changes this machine without its person: never from
		// inside an agent, and otherwise --yes or a yes at a terminal
		command := "c4 farm integrate " + map[bool]string{true: "", false: "off "}[on] + pos[0]
		if name == farmcli.Claude {
			command = "c4 farm integrate " + map[bool]string{true: "on", false: "off"}[on]
		}
		terminal := isTerminal(stdin)
		if err := farmcli.Consent(*yes, terminal, command); err != nil {
			return err
		}
		asker := farmcli.Out{W: stdout, In: bufio.NewReader(stdin)}
		if cfg.Integrations == nil {
			cfg.Integrations = map[string]bool{}
		}
		if name != farmcli.Claude {
			var confirm func(string) bool
			if !*yes && !*show {
				confirm = func(q string) bool { return farmcli.Ask(asker, q) }
			}
			if err := farmcli.App(o, pos[0], on, *show, confirm); err != nil || *show {
				return err
			}
		} else {
			in := farmcli.Integration{Record: cfg.Integrations}
			_, f, ferr := cfg.Resolve(cm.farm)
			runner := ferr == nil && f.Local && on
			if !*yes {
				q := "Take Claude Code's integration out? It removes what c4 added: the farm's tools from Claude Code, the agents' hooks, and this machine's agent service."
				if on {
					q = farmcli.IntegrateQuestion(runner)
				}
				if !farmcli.Ask(asker, q) {
					fmt.Fprintf(stdout, "nothing was changed\n")
					return nil
				}
			}
			if runner {
				// the farm's own owner, whose work this machine runs
				in.Runner = localRunner(cmp.Or(f.Owner, localfarm.DefaultOwner()), f.HUD, stdin, stdout)
				cfg.Integrations[farmcli.RunnerKey] = true
			}
			if on {
				err = in.On(ctx, o)
			} else {
				err = in.Off(ctx, o)
				cfg.Integrations[farmcli.RunnerKey] = false
			}
			if err != nil {
				return err
			}
		}
		cfg.Integrations[name] = on
		if err := cfg.Save(); err != nil {
			return err
		}
		if l := farmcli.IntegrateLine(cfg); l != "" && name == farmcli.Claude {
			fmt.Fprintf(stdout, "%s\n", l)
		}
		return nil

	case "enroll":
		forWhom := fs.String("for", "", "enroll it for someone else too, whose jobs then spend your sign-in (only the machine's holder may)")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 2, "NODE CODE"); err != nil {
			return err
		}
		if err := farmcli.EnrollConsent(pos[0]); err != nil {
			return err
		}
		c, _, o, _, err := connect()
		if err != nil {
			return err
		}
		e, err := farmcli.Enroll(ctx, c, pos[0], pos[1], *forWhom)
		if err != nil {
			return err
		}
		fmt.Fprintf(o.W, "%s\nowners    %s\n", e.Message, strings.Join(e.Owners, ", "))
		return nil

	case "agents":
		session := fs.String("session", "", "with add: the Claude Code session (default the newest whose transcript names the agent)")
		configDir := fs.String("config-dir", "", "with add: the Claude Code config dir (default $CLAUDE_CONFIG_DIR, else ~/.claude)")
		folder := fs.String("folder", "", "with add or rm: the member's folder, whose .claude/settings.local.json gets the hooks (default the session's working folder)")
		yes := fs.Bool("yes", false, "with add or rm: change the settings without asking (needed where no one is at a terminal)")
		all := fs.Bool("all", false, "with no subcommand: finished and stopped sessions too")
		openHUD := fs.Bool("open", false, "with no subcommand: show them in the HUD, served on this machine, read-only")
		port := fs.Int("port", 0, "with --open: the port on 127.0.0.1 (default any free one)")
		noBrowser := fs.Bool("no-browser", false, "with --open: serve the HUD but don't open a browser")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		o := farmcli.Out{W: stdout, In: stdin, Clock: farmcli.Clock{UTC: cm.utc}}
		switch {
		case len(pos) == 0 && *openHUD:
			srv := farmcli.SessionsServer{All: *all}
			return srv.Serve(ctx, *port, func(url string) {
				fmt.Fprintf(stdout, "HUD       %s (read-only, this machine's sessions; Ctrl-C stops it)\n", url)
				if !*noBrowser {
					openURL(url)
				}
			})
		case len(pos) == 0:
			return farmcli.Sessions(ctx, o, farmcli.SessionsArgs{All: *all})
		case len(pos) == 2 && (pos[0] == "add" || pos[0] == "rm"):
			// the integration, once on, is the person's yes to an agent's hooks
			// the recorded yes stands in for add's question only for a
			// person at a terminal; an agent or a script needs its own
			integrated := cfg.Integrations[farmcli.Claude] && pos[0] == "add" && isTerminal(stdin)
			a := farmcli.AgentsArgs{Name: pos[1], Session: *session, ConfigDir: *configDir, Folder: *folder, Yes: *yes || integrated, Terminal: isTerminal(stdin)}
			if pos[0] == "rm" {
				return farmcli.RemoveAgent(ctx, o, a)
			}
			return farmcli.AddAgent(ctx, o, a)
		case len(pos) == 1 && pos[0] == "add":
			a := farmcli.AgentsArgs{ConfigDir: *configDir, Folder: *folder, Yes: *yes || (cfg.Integrations[farmcli.Claude] && isTerminal(stdin)), Terminal: isTerminal(stdin)}
			if err := farmcli.PickAgent(ctx, &o, &a); err != nil {
				return err
			}
			return farmcli.AddAgent(ctx, o, a)
		case len(pos) == 1 && pos[0] == "ls":
			return farmcli.ListAgents(ctx, o, farmcli.AgentsArgs{})
		}
		return errors.New("usage: c4 farm agents add NAME [--session ID] [--folder DIR] [--config-dir DIR] [--yes] | agents rm NAME | agents ls")

	case "machine":
		return fmt.Errorf("%s is not in this build yet; `c4 farm up` makes a local farm, and `c4 farm login HUD-URL` signs in to a running one", cmd)
	}
	var names []string
	for _, l := range strings.Split(usage, "\n") {
		if f := strings.Fields(l); len(f) > 0 && strings.HasPrefix(l, "  ") {
			names = append(names, f[0])
		}
	}
	sort.Strings(names)
	return fmt.Errorf("no command %q; try: %s", cmd, strings.Join(names, ", "))
}

// isTerminal says r is a terminal a person types at, not a pipe, a file or
// an agent's tool.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	// a terminal, not merely a character device: a background job's stdin
	// is /dev/null, which is one too, and nobody is there to answer
	return ok && term.IsTerminal(int(f.Fd()))
}

// upIntegration is up's integration path, apart from making the farm: the
// question, asked only of a person at a terminal (never from inside an
// agent, never answered by an ended input), then what the answers turn
// on. With no terminal nothing is asked, recorded or installed.
type upIntegration struct {
	integrate string
	none      bool
	stdin     io.Reader
	stdout    io.Writer
	detect    []farmcli.Detected
	runner    func(ctx context.Context, o farmcli.Out) error
}

// choose settles the answers, and says whether Claude Code's was on before.
func (u upIntegration) choose(cfg *farmcli.Config) (on []string, wasOn bool, err error) {
	wasOn = cfg.Integrations[farmcli.Claude]
	on, err = farmcli.Choose(cfg, u.detect, u.integrate, u.none, isTerminal(u.stdin), farmcli.Out{W: u.stdout, In: u.stdin})
	return on, wasOn, err
}

// apply does what the answers say, keeps them, and says what it did.
func (u upIntegration) apply(ctx context.Context, cfg *farmcli.Config, on []string, wasOn bool) error {
	in := farmcli.Integration{Record: cfg.Integrations}
	switch {
	case slices.Contains(on, farmcli.Claude):
		// the runner only on its own yes: one given at login named no service
		if cfg.Integrations[farmcli.RunnerKey] {
			in.Runner = u.runner
		}
		in.On(ctx, farmcli.Out{W: u.stdout})
	case u.none && wasOn:
		in.Off(ctx, farmcli.Out{W: u.stdout})
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	applyApps(u.stdout, on)
	if l := farmcli.IntegrateLine(cfg); l != "" {
		fmt.Fprintf(u.stdout, "%s\n", l)
	}
	return nil
}

// applyApps adds C4 to each application chosen at up or login (every
// integration but Claude Code's, which has its own steps).
func applyApps(w io.Writer, on []string) {
	for _, name := range on {
		if name != farmcli.Claude {
			farmcli.App(farmcli.Out{W: w}, name, true, false, nil)
		}
	}
}

// bindFarm binds the local farm, its HUD answering, to its owner as one
// person's farm: every deployment of it, its records accruing to them. The
// same owner again changes nothing; a farm bound to someone else is
// refused, with how an owner changes.
func bindFarm(ctx context.Context, o localfarm.Options) error {
	c, err := farmcli.NewClient(farmcli.Farm{HUD: o.HUD(), As: o.Owner, Local: true,
		InstallerTokenFile: localfarm.InstallerTokenFile(cmp.Or(o.Name, localfarm.Name))})
	if err != nil {
		return err
	}
	if _, err := farmcli.Bind(ctx, c, o.BindRequest()); err != nil {
		return fmt.Errorf("the farm is up, but not bound to its owner %s: %v", o.Owner, err)
	}
	return nil
}

// localRunner makes this machine one of the local farm's runners for its
// owner, as the Claude Code integration does on a local farm: c4-agent
// enroll, then the agent service (c4 farm agent start), which runs the
// power reader and the host runner. The owner is the machine's holder
// (its runner uses their sign-in), so the runner's offer of the machine is
// accepted as them, with no step for the person. It never stops up: the
// farm is a farm without it, and its line says how to finish.
func localRunner(owner, hud string, stdin io.Reader, stdout io.Writer) func(ctx context.Context, o farmcli.Out) error {
	return func(ctx context.Context, o farmcli.Out) error {
		bin, err := releaseAgent(ctx)
		if err != nil {
			return err
		}
		// a c4-agent that offers its machine is told the HUD to offer it at
		offers := strings.Contains(agentHelp(ctx, bin), "enroll-offer")
		if !offers {
			hud = ""
		}
		e, err := localfarm.EnrollMachine(ctx, bin, owner, hud, 4)
		if err != nil {
			return fmt.Errorf("this machine runs no agents yet: %v (c4 farm integrate on tries again)", err)
		}
		svc := &farmcli.AgentService{AgentBin: bin, Slots: 4}
		if err := svc.Start(ctx, farmcli.Out{W: io.Discard}); err != nil {
			return fmt.Errorf("%s is enrolled, but its runner did not start: %v (c4 farm agent start tries again)", e.Node, err)
		}
		if offers {
			if err := acceptOffer(ctx, bin, owner, hud); err != nil {
				return fmt.Errorf("%s's runner is started, but the machine is not yet %s's, so the farm places no host work on it: %v (c4 farm integrate on tries again; or c4-agent enroll-offer prints a code, and c4 farm enroll %s CODE takes it)", e.Node, owner, err, e.Node)
			}
		}
		// said only as it is: the runner runs the owner's agents once its
		// blocks have their folder and a sign-in there
		switch {
		case e.BlocksFolder == "":
			fmt.Fprintf(o.W, "agents    this machine (%s) is enrolled for %s's agents and its runner is started; agents run here once c4-agent runs blocks (a newer c4-agent)\n", e.Node, e.Owner)
			return nil
		case e.BlocksSignedIn && e.BlocksTrusted:
			fmt.Fprintf(o.W, "agents    this machine (%s) runs %s's agents, %s pool, while the battery allows; a background service keeps it going (c4 farm agent stop removes it)\n", e.Node, e.Owner, e.Pool)
			return nil
		}
		fmt.Fprintf(o.W, "agents    this machine (%s) is enrolled for %s's agents and its runner is started; they run here once you sign in where they run\n", e.Node, e.Owner)
		return signInBlocks(e, stdin, stdout)
	}
}

// releaseAgent is the c4-agent of this c4-farm's own release: the one
// beside it (Homebrew installs them together), else the first on the PATH;
// one too old to enroll a machine is named, not used.
func releaseAgent(ctx context.Context) (string, error) {
	bin := ""
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), "c4-agent"); fileExists(p) {
			bin = p
		}
	}
	if bin == "" {
		p, err := exec.LookPath("c4-agent")
		if err != nil {
			return "", errors.New("this machine runs no agents yet: c4-agent is not beside c4-farm or on the PATH (brew install mrjoshuak/tap/c4 brings it; then c4 farm integrate on)")
		}
		bin = p
	}
	bin, _ = filepath.Abs(bin)
	if !strings.Contains(agentHelp(ctx, bin), "enroll") {
		return "", fmt.Errorf("this machine runs no agents yet: %s is a c4-agent too old to enroll a machine (brew upgrade mrjoshuak/tap/c4, or put the c4-agent beside c4-farm first on the PATH; then c4 farm integrate on)", tilde(bin))
	}
	return bin, nil
}

// agentHelp is c4-agent's help, which names the commands it has.
func agentHelp(ctx context.Context, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "help").CombinedOutput()
	return string(out)
}

// acceptOffer has the machine's runner offer it, and accepts the offer as
// owner, the machine's holder: the code goes from the runner to the farm
// on this machine, never through anyone's hands.
func acceptOffer(ctx context.Context, bin, owner, hud string) error {
	offer, err := localfarm.OfferMachine(ctx, bin)
	if err != nil {
		return err
	}
	c, err := farmcli.NewClient(farmcli.Farm{HUD: hud, As: owner, Local: true, InstallerTokenFile: localfarm.InstallerTokenFile(localfarm.Name)})
	if err != nil {
		return err
	}
	_, err = farmcli.Enroll(ctx, c, offer.Node, offer.Code, "")
	return err
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// signInBlocks signs the person in, once, where the machine's blocks run:
// their own clean config directory, not the person's ~/.claude, in their
// one folder. Claude Code's own sign-in and its trust prompt for that
// folder come in the same session, in the person's terminal, and no
// credential is copied. Without a terminal it says the command instead.
func signInBlocks(e localfarm.Enrolled, stdin io.Reader, stdout io.Writer) error {
	// only in the blocks' own folder: never wherever up was started
	if e.BlocksEnv == "" || e.BlocksFolder == "" || (e.BlocksSignedIn && e.BlocksTrusted) {
		return nil
	}
	step := "cd " + e.BlocksFolder + " && CLAUDE_CONFIG_DIR=" + e.BlocksEnv + " claude"
	claude, err := exec.LookPath("claude")
	if err != nil || !isTerminal(stdin) {
		fmt.Fprintf(stdout, "agents    one step left, in your terminal: %s (sign in, trust the folder, then /exit); until then the agents' blocks wait\n", step)
		return nil
	}
	fmt.Fprintf(stdout, "agents    sign in where the agents' blocks run, apart from your own Claude Code: sign in, trust the folder, then /exit\n")
	if err := os.MkdirAll(e.BlocksFolder, 0o755); err != nil {
		return err
	}
	// not tied to up's context: a Ctrl-C reaches Claude Code itself, which
	// ends as it does, rather than being killed with the terminal left raw
	cmd := exec.Command(claude)
	cmd.Dir = e.BlocksFolder
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+e.BlocksEnv)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the blocks' sign-in did not finish (%v); run %s", err, step)
	}
	return nil
}

// openURL opens a link in the default browser.
// openSignedIn opens u, a page of farm f's HUD, in the browser. On a local
// farm it goes by a one-time code the installer token gets, which the HUD
// trades for its cookie, so the token is never in an address or the
// browser's history.
func openSignedIn(ctx context.Context, f farmcli.Farm, u string) error {
	c, err := farmcli.NewClient(f)
	if err != nil {
		return err
	}
	if u, err = farmcli.SignedIn(ctx, c, u); err != nil {
		return err
	}
	return openURL(u)
}

func openURL(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

// list splits a comma-separated flag; empty is none.
func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
