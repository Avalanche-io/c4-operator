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
  agent power              offer this machine's agent slots by its battery: stop below 25%, resume at 90% (--once to read it now)
  agents                   the Claude Code sessions on this machine, read-only: each one's state, and what needs you (--all, --open for the HUD)
  agents add NAME          make a Claude Code session an agent the farm can run, and install its Stop hook
  agents ls                your agents: busy, idle or stopped, and the last turn saved
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
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "signed in to %s as %s · projects %s\n", n, s.Viewer.User, strings.Join(s.Viewer.Projects, ", "))
		return nil

	case "whoami":
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		c, _, o, _, err := connect()
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
		as := fs.String("as", "artist", "the role to open the HUD as: artist, supervisor, wrangler, admin, facilities or td")
		plan := fs.Bool("plan", false, "print the cluster config and the objects, and change nothing")
		install := fs.String("install", "", "the operator's install manifest (default this release's)")
		archive := fs.String("image-archive", "", "an operator image archive to load into the cluster (a development build)")
		image := fs.String("image", "", "the operator image the cluster runs, with --image-archive")
		noOpen := fs.Bool("no-open", false, "don't open the HUD")
		port := fs.Int("port", localfarm.HUDPort, "the HUD's port on this machine")
		storePort := fs.Int("store-port", localfarm.StorePort, "the store's port on this machine")
		registryPort := fs.Int("registry-port", localfarm.RegistryPort, "the farm's image registry's port on this machine")
		owner := fs.String("owner", "", "who owns this farm and plays every role on it (default the name in your git user.email, else your login)")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		localfarm.Name = *localName
		v, err := localfarm.LookupViewer(*as)
		if err != nil {
			return err
		}
		tz := localfarm.LocalTZ()
		lat, lon := localfarm.Place(tz)
		o := localfarm.UpOptions{Options: localfarm.Options{TZ: tz, Lat: lat, Lon: lon, OperatorImage: *image, HUDHostPort: *port, StoreHostPort: *storePort,
			RegistryHostPort: *registryPort, LocalImages: !strings.HasPrefix(version, "v") && *install != "", Owner: *owner},
			Install: *install, Version: version, ImageArchive: *archive, Log: stdout}
		if o.Owner == "" {
			o.Owner = localfarm.DefaultOwner()
		}
		if *plan {
			fmt.Fprintf(stdout, "%s---\n%s", localfarm.KindConfig(o.Options), localfarm.YAML(localfarm.Farm(o.Options)))
			fmt.Fprintf(stdout, "---\n# manager arguments added: %s\n", strings.Join(localfarm.ManagerArgs(o.Options), " "))
			return nil
		}
		if err := localfarm.CheckDocker(ctx); err != nil {
			return err
		}
		if err := localfarm.Up(ctx, o); err != nil {
			return err
		}
		cfg.Farms[localfarm.Name] = farmcli.Farm{HUD: o.HUD(), As: v.Name, Project: localfarm.Project, Local: true}
		cfg.Current = localfarm.Name
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nfarm      %s   4 nodes · 6 slots, 4 more to rent · store ready\n", localfarm.Name)
		fmt.Fprintf(stdout, "project   %s   you: %s\n", localfarm.Project, v.Name)
		fmt.Fprintf(stdout, "owner     %s, who plays every role here (c4 farm up --owner NAME to change it)\n", o.Owner)
		fmt.Fprintf(stdout, "HUD       %s\n", localfarm.NeedsLink(o.HUD(), v))
		fmt.Fprintf(stdout, "roles     artist, supervisor, wrangler, admin, facilities, td (c4 farm up --as ROLE opens another)\n")
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
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 || pos[0] != "power" {
			return errors.New("usage: c4 farm agent power [--slots N] [--stop-below 25] [--resume-at 90] [--slots-file F] [--once]")
		}
		o := farmcli.Out{W: stdout, Clock: farmcli.Clock{UTC: cm.utc}}
		return farmcli.AgentPower(ctx, o, farmcli.PowerArgs{SlotsFile: *slotsFile, Every: *every, Once: *once,
			Policy: power.Policy{Slots: *slots, StopBelow: *stopBelow, ResumeAt: *resumeAt}})

	case "agents":
		session := fs.String("session", "", "with add: the Claude Code session (default the newest whose transcript names the agent)")
		configDir := fs.String("config-dir", "", "with add: the Claude Code config dir (default $CLAUDE_CONFIG_DIR, else ~/.claude)")
		yes := fs.Bool("yes", false, "with add: install the Stop hook without asking")
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
		case len(pos) == 2 && pos[0] == "add":
			return farmcli.AddAgent(ctx, o, farmcli.AgentsArgs{Name: pos[1], Session: *session, ConfigDir: *configDir, Yes: *yes})
		case len(pos) == 1 && pos[0] == "ls":
			return farmcli.ListAgents(ctx, o, farmcli.AgentsArgs{})
		}
		return errors.New("usage: c4 farm agents add NAME [--session ID] [--config-dir DIR] [--yes] | agents ls")

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
