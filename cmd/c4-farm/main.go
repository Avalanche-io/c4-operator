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

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/internal/mcpserver"
)

var version = "dev"

const usage = `c4 farm: submit work to a C4 farm, watch it, and take its output home.

Farms
  use FARM                 switch the current farm
  login HUD-URL            add a farm and sign in (--token-file, or --as on a local farm)
  whoami                   who the farm sees you as, and your projects

Work
  samples get [SET]        download the sample shots and repositories to ~/c4-samples
  types                    the prebuilt job types
  submit TYPE ASSET        plan a job, show the estimate, and submit it
  ls                       the queue, in run order (--mine, --at-risk, --all)
  watch JOB                the job's progress, deadline and slack as they change
  open JOB                 open the job view in the HUD (--widget for the side widget)
  deadline JOB WHEN        move a job's deadline (18:00, fri 18:00, +3h, suggested)
  hold JOB [--stop]        hold a job; --stop also stops its running units
  release JOB              release a held job
  boost JOB N              ask for N times the slots its deadline needs (1 to 2)
  requeue JOB UNITS        run units again ("14", "0-9,14")
  get JOB|VIEW [PATH...] DIR   write a job's output under DIR, checking every C4 ID

Agents
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
		role := "artist"
		if s.Viewer.All {
			role = "wrangler"
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
		pos, err := parse(fs, rest)
		if err != nil {
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
		dir := fs.String("dir", "", "where to unpack (default ~/c4-samples)")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) == 0 || pos[0] != "get" {
			return errors.New("usage: c4 farm samples get [SET] [--dir DIR] [--from ARCHIVE]")
		}
		set := ""
		if len(pos) > 1 {
			set = pos[1]
		}
		o := farmcli.Out{W: stdout, Clock: farmcli.Clock{UTC: cm.utc}}
		return farmcli.GetSamples(ctx, o, farmcli.SamplesArgs{Set: set, Version: version, Dir: *dir, From: *from})

	case "up", "down", "status", "machine", "image", "power":
		return fmt.Errorf("%s is part of the local farm installer, which is not in this build yet; to use a farm that is already running, `c4 farm login http://127.0.0.1:30080 --as artist`", cmd)
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
