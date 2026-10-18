// c4-agent makes Claude Code sessions members of an agent bundle: it records
// each member's session, saves a checkpoint at the end of every turn,
// resumes a member by its session ID (never a fresh agent in its place),
// and keeps at most one live copy of each member.
//
//	c4-agent register --name lead --session <id> [--cwd DIR] [--config-dir DIR] [--flag F]... [--new-phase]
//	c4-agent find --agent-name NAME [--config-dir DIR]
//	c4-agent ls
//	c4-agent checkpoint NAME | --hook
//	c4-agent resume NAME [PROMPT]
//	c4-agent yield NAME [--timeout 10m]
//	c4-agent verify NAME
//	c4-agent log NAME [-n 10]
//	c4-agent hooks
//	c4-agent runner [--node NAME] [--slots N] [--label K=V]...
//
// State lives in $C4_AGENT_HOME (default ~/.c4/agents). The claude binary is
// $C4_AGENT_CLAUDE, else claude on PATH.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/agent"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, " ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	home, err := agent.DefaultHome()
	if err != nil {
		fmt.Fprintln(stderr, "c4-agent:", err)
		return 1
	}
	h := agent.New(&agent.Store{Dir: home}, agent.NewCLI())
	h.Workspace = os.Getenv("C4_AGENT_WORKSPACE")
	cmd, rest := args[0], args[1:]
	if cmd == "checkpoint" && len(rest) > 0 && rest[0] == "--hook" {
		// A hook must never fail or block the agent: report and exit 0.
		if _, err := h.Hook(ctx, stdin); err != nil {
			fmt.Fprintln(stderr, "c4-agent hook:", err)
		}
		return 0
	}
	if err := dispatch(ctx, h, cmd, rest, stdout); err != nil {
		fmt.Fprintln(stderr, "c4-agent:", err)
		switch {
		case errors.Is(err, agent.ErrLive), errors.Is(err, agent.ErrHasSession):
			return 3
		case errors.Is(err, agent.ErrForked):
			return 4
		case errors.Is(err, agent.ErrBusy):
			return 5
		}
		return 1
	}
	return 0
}

func dispatch(ctx context.Context, h *agent.Harness, cmd string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch cmd {
	case "register":
		var o agent.RegisterOptions
		var flags multi
		fs.StringVar(&o.Name, "name", "", "member name")
		fs.StringVar(&o.Session, "session", "", "Claude Code session ID")
		fs.StringVar(&o.Cwd, "cwd", "", "directory the session runs in (default: the current directory)")
		fs.StringVar(&o.ConfigDir, "config-dir", os.Getenv("CLAUDE_CONFIG_DIR"), "the session's CLAUDE_CONFIG_DIR")
		fs.Var(&flags, "flag", "a claude flag for resuming from stopped (repeatable)")
		fs.BoolVar(&o.NewPhase, "new-phase", false, "give the member a different session on purpose")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if o.Cwd == "" {
			o.Cwd, _ = os.Getwd()
		}
		if abs, err := filepath.Abs(o.Cwd); err == nil {
			o.Cwd = abs
		}
		if len(flags) > 0 {
			o.Flags = flags
		}
		m, err := h.Register(ctx, o)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: session %s, phase %d, epoch %d, transcript %s\n", m.Name, m.Session, m.Phase, m.Epoch, m.Transcript)
	case "find":
		var name, cd string
		fs.StringVar(&name, "agent-name", "", "the agentName to look for")
		fs.StringVar(&cd, "config-dir", os.Getenv("CLAUDE_CONFIG_DIR"), "CLAUDE_CONFIG_DIR to search")
		if err := fs.Parse(args); err != nil {
			return err
		}
		paths, err := agent.FindByAgentName(cd, name)
		if err != nil {
			return err
		}
		for _, p := range paths {
			fmt.Fprintf(out, "%s\t%s\n", strings.TrimSuffix(filepath.Base(p), ".jsonl"), p)
		}
	case "ls":
		sts, err := h.Status(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "MEMBER\tSTATE\tSESSION\tEPOCH\tPHASE\tLAST CHECKPOINT")
		for _, s := range sts {
			last := "-"
			if s.Last != nil {
				last = fmt.Sprintf("#%d %s %s", s.Last.Seq, s.Last.Event, s.Last.At.Local().Format("Jan 2 15:04"))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n", s.Member.Name, s.State, s.Member.Session, s.Member.Epoch, s.Member.Phase, last)
		}
		return tw.Flush()
	case "checkpoint":
		if err := fs.Parse(args); err != nil {
			return err
		}
		cp, changed, err := h.Checkpoint(ctx, fs.Arg(0), "manual")
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintf(out, "%s: unchanged since #%d\n", fs.Arg(0), cp.Seq)
			return nil
		}
		fmt.Fprintf(out, "%s: #%d at %d bytes, head %s\n", fs.Arg(0), cp.Seq, cp.Offset, short(cp.Head))
	case "resume":
		if err := fs.Parse(args); err != nil {
			return err
		}
		res, err := h.Resume(ctx, fs.Arg(0), strings.Join(fs.Args()[min(1, fs.NArg()):], " "))
		if err != nil {
			if res.Copy != "" {
				fmt.Fprintf(out, "a copy was started as %s; stop it with: claude stop %s\n", res.Copy, res.Copy)
			}
			return err
		}
		fmt.Fprintf(out, "%s: resumed session %s, epoch %d\n", res.Member.Name, res.Member.Session, res.Member.Epoch)
	case "yield":
		timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for a busy session to finish its turn")
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		res, err := h.Yield(ctx, fs.Arg(0), *timeout)
		if err != nil {
			return err
		}
		if res.Checkpoint != nil {
			fmt.Fprintf(out, "%s: %s at checkpoint #%d\n", fs.Arg(0), res.State, res.Checkpoint.Seq)
		} else {
			fmt.Fprintf(out, "%s: %s\n", fs.Arg(0), res.State)
		}
	case "verify":
		if err := fs.Parse(args); err != nil {
			return err
		}
		r, err := h.Store.Verify(fs.Arg(0))
		if err != nil {
			return err
		}
		if r.Broken != 0 {
			return fmt.Errorf("%s: checkpoint #%d: %s (%d verified before it)", fs.Arg(0), r.Broken, r.Reason, r.Checked)
		}
		fmt.Fprintf(out, "%s: %d checkpoints verified against the transcript\n", fs.Arg(0), r.Checked)
	case "log":
		n := fs.Int("n", 10, "how many checkpoints")
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		cps, err := h.Store.Checkpoints(fs.Arg(0))
		if err != nil {
			return err
		}
		if len(cps) > *n {
			cps = cps[len(cps)-*n:]
		}
		for _, cp := range cps {
			ws := cp.Workspace.Mode
			switch {
			case cp.Workspace.Err != "":
				ws += " (" + cp.Workspace.Err + ")"
			case cp.Workspace.Head != "":
				ws += " " + cp.Workspace.Head[:min(12, len(cp.Workspace.Head))]
				if cp.Workspace.Dirty != "" {
					ws += " +dirty"
				}
			}
			fmt.Fprintf(out, "#%d\t%s\t%s\tepoch %d\t%d bytes\t%s\t%s\t%s\n", cp.Seq, cp.At.Local().Format("Jan 2 15:04:05"), cp.Event, cp.Epoch, cp.Offset, short(cp.Head), ws, cp.Note)
		}
	case "runner":
		return runRunner(ctx, h, fs, args, out)
	case "hooks":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		data, err := agent.HookSettings(exe)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
	default:
		return fmt.Errorf("unknown command %q (try c4-agent help)", cmd)
	}
	return nil
}

// reorder moves flags ahead of the positional name, so "yield lead
// --timeout 5m" parses.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func short(id string) string {
	if len(id) > 14 {
		return id[:6] + "..." + id[len(id)-4:]
	}
	return id
}

const usage = `c4-agent: Claude Code sessions as members of an agent bundle.

  register --name N --session ID [--cwd DIR] [--config-dir DIR] [--flag F]... [--new-phase]
                       add a member for an existing session (a different session
                       for an existing member needs --new-phase)
  find --agent-name N  list sessions whose transcript names that agent, newest first
  ls                   members, whether each is busy, idle or stopped, and its last checkpoint
  checkpoint NAME      save a turn boundary now
  checkpoint --hook    the Claude Code Stop hook: save the turn that just ended
  resume NAME [PROMPT] continue a stopped member's session (refused while it runs)
  yield NAME [--timeout D]
                       stop a member at its next turn boundary, after a checkpoint
  verify NAME          check the checkpoint chain against the transcript
  log NAME [-n N]      recent checkpoints
  hooks                print the settings fragment that installs the Stop hook
  runner [--node N] [--slots N] [--label K=V]... [--every 5s] [--yield-timeout 1m]
                       run this machine's members for the farm's queue (a host
                       runner: a Node for the machine, member pods bound to it)

State: $C4_AGENT_HOME (default ~/.c4/agents). claude: $C4_AGENT_CLAUDE or claude on PATH.
Exit codes: 3 running or already has a session, 4 a copy was started, 5 still busy.
`

// runRunner runs the host runner against the cluster in the current
// kubeconfig.
func runRunner(ctx context.Context, h *agent.Harness, fs *flag.FlagSet, args []string, out io.Writer) error {
	host, _ := os.Hostname()
	host = strings.ToLower(strings.Split(host, ".")[0])
	node := fs.String("node", "c4-host-"+host, "the machine's Node name")
	slots := fs.Int("slots", 4, "members that may run here at once")
	every := fs.Duration("every", 5*time.Second, "how often to check pods and sessions")
	yieldTimeout := fs.Duration("yield-timeout", time.Minute, "how long one yield waits for a busy member before trying again")
	hardStop := fs.Duration("hard-stop-after", 0, "stop a member mid-turn if it has not reached a turn boundary this long after a yield request (0: wait for the boundary)")
	slotsFile := fs.String("slots-file", "", "a file whose first line is the slots to offer now (and, after a space, why), written by a power reader; missing or unreadable means --slots")
	var lbls multi
	fs.Var(&lbls, "label", "a Node label K=V for the agent pool's nodeSelector (default c4.avalanche.io/host=<node>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set := map[string]string{}
	for _, l := range lbls {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return fmt.Errorf("--label %q: want K=V", l)
		}
		set[k] = v
	}
	if len(set) == 0 {
		set["c4.avalanche.io/host"] = *node
	}
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := c4v1.AddToScheme(scheme); err != nil {
		return err
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	r := &agent.Runner{Client: cl, Harness: h, Node: *node, Labels: set, Slots: *slots, YieldTimeout: *yieldTimeout, HardStopAfter: *hardStop,
		Logf: func(f string, a ...any) { fmt.Fprintf(out, time.Now().Format("15:04:05 ")+f+"\n", a...) }}
	if *slotsFile != "" {
		r.Capacity = func() (int, string) { return readSlotsFile(*slotsFile, *slots) }
	}
	fmt.Fprintf(out, "runner %s: %d slots, labels %v\n", *node, *slots, set)
	return r.Run(ctx, *every)
}

// readSlotsFile reads "N [reason...]" from path; anything unreadable gives
// def and no reason.
func readSlotsFile(path string, def int) (int, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return def, ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	num, reason, _ := strings.Cut(line, " ")
	n, err := strconv.Atoi(num)
	if err != nil {
		return def, ""
	}
	return n, strings.TrimSpace(reason)
}
