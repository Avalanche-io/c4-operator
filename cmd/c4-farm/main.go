// Command c4-farm is `c4 farm`, the user command for a C4 farm: submit
// work, watch it, move its deadline, and take its output home.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/internal/localfarm"
	"github.com/Avalanche-io/c4-operator/internal/mcpserver"
	"github.com/Avalanche-io/c4-operator/internal/power"
)

var version = "dev"

const usage = `c4 farm: submit work to a C4 farm, watch it, and take its output home.

The local farm
  up [--as ROLE]           make a farm on this machine in Docker and open its HUD (--port N if 30080 is taken)
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
  open JOB                 open the job view in the HUD (--widget for the side widget)
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
  agents add NAME          make a Claude Code session an agent the farm can run, with c4-agent's hooks in its folder (asked first)
  agents rm NAME           take out what agents add put in
  agents ls                your agents: busy, idle or stopped, and the last turn saved
  integrations             what this machine could add (Claude Code today), and which are on
  integrate NAME on|off    add an integration, or take out everything it added
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
	if err := run(os.Args[1:], os.Stdout, os.Stdin); err != nil {
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
		on, err := farmcli.Choose(cfg, farmcli.Detect(), *integrateOn, *integrateOff, isTerminal(stdin), farmcli.Out{W: stdout, In: stdin})
		if err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "signed in to %s as %s · projects %s\n", n, s.Viewer.User, strings.Join(s.Viewer.Projects, ", "))
		if slices.Contains(on, farmcli.Claude) {
			// a farm signed in to, not made here: its tools in Claude Code,
			// and no runner (that is the farm admin's to enroll)
			(&farmcli.Integration{}).On(ctx, farmcli.Out{W: stdout})
		}
		fmt.Fprintf(stdout, "%s\n", farmcli.IntegrateLine(cfg))
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
			_, err = farmcli.SubmitFile(ctx, c, o, farmcli.SubmitFileArgs{Path: pos[0], Project: project, DryRun: a.DryRun, Explain: *explain, Yes: a.Yes})
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
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "JOB [--widget]"); err != nil {
			return err
		}
		c, _, o, project, err := connect()
		if err != nil {
			return err
		}
		u, err := farmcli.Open(ctx, c, project, pos[0], *widget)
		if err != nil {
			return err
		}
		fmt.Fprintln(o.W, u)
		return openURL(u)

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
		return mcpserver.New(&mcpserver.Client{Base: c.Base, Token: c.Token, As: c.As, Agent: *agent}).Run(ctx, &mcp.StdioTransport{})

	case "samples":
		from := fs.String("from", "", "a local sample archive instead of the release's")
		localName := fs.String("name", localfarm.Name, "the local farm a scenario loads onto")
		dir := fs.String("dir", "", "where to unpack (default ~/c4-samples)")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		localfarm.Name = *localName
		if len(pos) > 0 && pos[0] == "load" {
			return loadScenario(ctx, stdout, cfg, cm.utc, pos[1:])
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
		port := fs.Int("port", localfarm.HUDPort, "the HUD's port on this machine")
		storePort := fs.Int("store-port", localfarm.StorePort, "the store's port on this machine")
		registryPort := fs.Int("registry-port", localfarm.RegistryPort, "the farm's image registry's port on this machine")
		integrateOn := fs.String("integrate", "", "the integrations to add, without asking: claude (c4 farm integrations lists them)")
		integrateOff := fs.Bool("no-integrate", false, "the minimal install: no integration added, without asking")
		owner := fs.String("owner", "", "who owns this farm and plays every role on it (default the name in your git user.email, else your login)")
		trackerOn := fs.Bool("tracker", false, "add a stand-in GitHub and c4-sync: issues you open become jobs, its reviews serve them (nothing leaves this machine)")
		syncInstall := fs.String("sync-install", "", "c4-sync's install manifest, with --tracker (default this release's)")
		syncImage := fs.String("sync-image", "", "the c4-sync image, with --tracker; one only in this machine's Docker (c4-sync:dev) is pushed to the farm's registry")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		localfarm.Name = *localName
		tz := localfarm.LocalTZ()
		lat, lon := localfarm.Place(tz)
		o := localfarm.UpOptions{Options: localfarm.Options{TZ: tz, Lat: lat, Lon: lon, OperatorImage: *image, HUDHostPort: *port, StoreHostPort: *storePort,
			RegistryHostPort: *registryPort, LocalImages: !strings.HasPrefix(version, "v") && *install != "", Owner: *owner, Tracker: *trackerOn},
			Install: *install, Version: version, ImageArchive: *archive, SyncInstall: *syncInstall, SyncImage: *syncImage, Log: stdout}
		// the owner is kept from one up to the next; --owner changes it
		was := cfg.Farms[localfarm.Name].Owner
		switch {
		case o.Owner != "":
			name, err := localfarm.OwnerName(o.Owner)
			if err != nil {
				return err
			}
			o.Owner = name
		case was != "":
			o.Owner = was
		default:
			if o.Owner = localfarm.DefaultOwner(); o.Owner == "" {
				return errors.New("no name for the farm's owner from git's user.email or the login; give one with --owner NAME")
			}
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
		// the tracker is offered once, on the farm's first up
		first := true
		if have, err := localfarm.Exists(localfarm.Name); err == nil {
			first = !have
		}
		if err := localfarm.Up(ctx, o); err != nil {
			return err
		}
		cfg.Farms[localfarm.Name] = farmcli.Farm{HUD: o.HUD(), As: v.Name, Project: localfarm.Project, Local: true, Owner: o.Owner}
		cfg.Current = localfarm.Name
		on, err := farmcli.Choose(cfg, farmcli.Detect(), *integrateOn, *integrateOff, isTerminal(stdin), farmcli.Out{W: stdout, In: stdin})
		if err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nfarm      %s   4 nodes · 6 slots, 4 more to rent · store ready\n", localfarm.Name)
		fmt.Fprintf(stdout, "project   %s   you: %s\n", localfarm.Project, v.Name)
		if was != "" && was != o.Owner {
			fmt.Fprintf(stdout, "owner     %s, no longer %s: %s now plays every role here\n", o.Owner, was, o.Owner)
		} else {
			fmt.Fprintf(stdout, "owner     %s, who plays every role here (c4 farm up --owner NAME to change it)\n", o.Owner)
		}
		if slices.Contains(on, farmcli.Claude) {
			in := farmcli.Integration{Runner: localRunner(o.Owner, stdin, stdout)}
			in.On(ctx, farmcli.Out{W: stdout})
		}
		fmt.Fprintf(stdout, "%s\n", farmcli.IntegrateLine(cfg))
		fmt.Fprintf(stdout, "HUD       %s\n", localfarm.NeedsLink(o.HUD(), v))
		fmt.Fprintf(stdout, "roles     artist, supervisor, wrangler, admin, facilities, td, to try their views (c4 farm up --as ROLE opens one)\n")
		if o.Tracker || localfarm.HasTracker(ctx) {
			fmt.Fprintf(stdout, "tracker   %s   a stand-in for GitHub; nothing leaves this machine\n", localfarm.SandboxPage(o.HUD(), v.Name))
		} else if first {
			fmt.Fprintf(stdout, "try a tracker: c4 farm up --tracker\n")
		}
		if *noOpen {
			return nil
		}
		return openURL(localfarm.NeedsLink(o.HUD(), v))

	case "down":
		localName := fs.String("name", localfarm.Name, "the local farm to delete")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		localfarm.Name = *localName
		if err := localfarm.Down(localfarm.Name); err != nil {
			return err
		}
		delete(cfg.Farms, localfarm.Name)
		if cfg.Current == localfarm.Name {
			cfg.Current = ""
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "the local farm is gone\n")
		return nil

	case "status":
		localName := fs.String("name", localfarm.Name, "the local farm")
		if _, err := parse(fs, rest); err != nil {
			return err
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
		// The HUD is where up put it.
		hud := localfarm.Options{}.HUD()
		if f, ok := cfg.Farms[localfarm.Name]; ok && f.HUD != "" {
			hud = f.HUD
		}
		c, err := farmcli.NewClient(farmcli.Farm{HUD: hud, As: "wrangler"})
		if err != nil {
			return err
		}
		s, err := farmcli.Snapshot(ctx, c)
		if err != nil {
			fmt.Fprintf(stdout, "cluster   %s is running; the HUD does not answer yet: %v\n", localfarm.Name, err)
			return nil
		}
		fmt.Fprintf(stdout, "farm      %s   %d nodes · %d of %d slots in use · %d units waiting\n", localfarm.Name, len(s.Nodes), s.Farm.SlotsUsed, s.Farm.SlotsTotal, s.Farm.QueuedUnits)
		fmt.Fprintf(stdout, "HUD       %s\n", hud)
		for _, v := range localfarm.Viewers {
			fmt.Fprintf(stdout, "  %-10s %s\n", v.Name, localfarm.Link(hud, v))
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
			return svc.Start(ctx, o)
		case len(pos) == 1 && pos[0] == "stop":
			return svc.Stop(ctx, o)
		}
		return errors.New("usage: c4 farm agent start | stop | run | power [--slots N] [--stop-below 25] [--resume-at 90] [--slots-file F] [--once]")

	case "integrations":
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		farmcli.Integrations(cfg, farmcli.Detect(), farmcli.Out{W: stdout})
		return nil

	case "integrate":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) != 2 || (pos[1] != "on" && pos[1] != "off") {
			return errors.New("usage: c4 farm integrate NAME on|off (c4 farm integrations lists them)")
		}
		if pos[0] != farmcli.Claude {
			return fmt.Errorf("no integration %q here; c4 farm integrations lists them", pos[0])
		}
		on := pos[1] == "on"
		o := farmcli.Out{W: stdout}
		in := farmcli.Integration{}
		if _, f, err := cfg.Resolve(cm.farm); err == nil && f.Local && on {
			in.Runner = localRunner(localfarm.DefaultOwner(), stdin, stdout)
		}
		if on {
			err = in.On(ctx, o)
		} else {
			err = in.Off(ctx, o)
		}
		if err != nil {
			return err
		}
		if cfg.Integrations == nil {
			cfg.Integrations = map[string]bool{}
		}
		cfg.Integrations[pos[0]] = on
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\n", farmcli.IntegrateLine(cfg))
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
			integrated := cfg.Integrations[farmcli.Claude] && pos[0] == "add"
			a := farmcli.AgentsArgs{Name: pos[1], Session: *session, ConfigDir: *configDir, Folder: *folder, Yes: *yes || integrated, Terminal: isTerminal(stdin)}
			if pos[0] == "rm" {
				return farmcli.RemoveAgent(ctx, o, a)
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
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// localRunner makes this machine one of the local farm's runners for its
// owner, as the Claude Code integration does on a local farm: c4-agent
// enroll, then the agent service (c4 farm agent start), which runs the
// power reader and the host runner. It never stops up: the farm is a farm
// without it, and its line says how to finish.
func localRunner(owner string, stdin io.Reader, stdout io.Writer) func(ctx context.Context, o farmcli.Out) error {
	return func(ctx context.Context, o farmcli.Out) error {
		bin, err := exec.LookPath("c4-agent")
		if err != nil {
			return errors.New("this machine runs no agents yet: c4-agent is not on PATH (brew install avalanche-io/tap/c4 brings it; then c4 farm integrate claude on)")
		}
		if bin, err = filepath.Abs(bin); err != nil {
			return err
		}
		e, err := localfarm.EnrollMachine(ctx, bin, owner, 4)
		if err != nil {
			return fmt.Errorf("this machine runs no agents yet: %v (c4 farm integrate claude on tries again)", err)
		}
		svc := &farmcli.AgentService{AgentBin: bin, Slots: 4}
		if err := svc.Start(ctx, farmcli.Out{W: io.Discard}); err != nil {
			return fmt.Errorf("%s is enrolled, but its runner did not start: %v (c4 farm agent start tries again)", e.Node, err)
		}
		fmt.Fprintf(o.W, "agents    this machine (%s) runs %s's agents, %s pool, while the battery allows; a background service keeps it going (c4 farm agent stop removes it)\n", e.Node, e.Owner, e.Pool)
		return signInBlocks(ctx, e, stdin, stdout)
	}
}

// signInBlocks signs the person in, once, where the machine's blocks run:
// their own clean config directory, not the person's ~/.claude. It runs
// Claude Code's own sign-in there, in the person's terminal, and copies no
// credential. Without a terminal it says the command instead.
func signInBlocks(ctx context.Context, e localfarm.Enrolled, stdin io.Reader, stdout io.Writer) error {
	if e.BlocksEnv == "" || e.BlocksSignedIn {
		return nil
	}
	step := "CLAUDE_CONFIG_DIR=" + e.BlocksEnv + " claude"
	claude, err := exec.LookPath("claude")
	if err != nil || !isTerminal(stdin) {
		fmt.Fprintf(stdout, "agents    one step left: sign in where the agents' blocks run, in your terminal: %s (sign in, then /exit)\n", step)
		return nil
	}
	fmt.Fprintf(stdout, "agents    sign in where the agents' blocks run (%s, apart from your own Claude Code): sign in, then /exit\n", e.BlocksEnv)
	cmd := exec.CommandContext(ctx, claude)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+e.BlocksEnv)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the blocks' sign-in did not finish (%v); run %s", err, step)
	}
	return nil
}

// openURL opens a link in the default browser.
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
