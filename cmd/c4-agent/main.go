// c4-agent makes Claude Code sessions members of an agent bundle: it records
// each member's session, saves a checkpoint at the end of every turn,
// resumes a member by its session ID (never a fresh agent in its place),
// and keeps at most one live copy of each member.
//
//	c4-agent register --name lead --session <id> [--cwd DIR] [--config-dir DIR] [--flag F]... [--new-phase]
//	                  [--env DIR [--env-id ID] [--import CONFIGDIR]]
//	c4-agent observe [--all] [--json]
//	c4-agent find --agent-name NAME [--config-dir DIR]
//	c4-agent ls
//	c4-agent unregister --name NAME
//	c4-agent checkpoint NAME | --hook
//	c4-agent resume NAME [PROMPT]
//	c4-agent yield NAME [--timeout 10m]
//	c4-agent verify NAME
//	c4-agent log NAME [-n 10]
//	c4-agent hooks
//	c4-agent enroll --owner USER [--node NAME] [--pool agent] [--slots N] [--kubeconfig K] [--context C]
//	c4-agent runner --owner USER [--node NAME] [--slots N] [--label K=V]...
//	c4-agent block-pod [--cwd DIR]
//	c4-agent lease --name N --namespace NS --holder H --owner O --pool P [--ttl 8h] [--renew-before 15m] [--config-dir DIR]
//
// State lives in $C4_AGENT_HOME (default ~/.c4/agents). The claude binary is
// $C4_AGENT_CLAUDE, else claude on PATH.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Avalanche-io/c4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/agent"
	"github.com/Avalanche-io/c4-operator/internal/lease"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/tier"
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
		input, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
		if err != nil {
			fmt.Fprintln(stderr, "c4-agent hook:", err)
			return 0
		}
		if agent.HookEvent(input) == "PostToolUse" {
			// after each tool call: a stop the farm asked for ends the turn
			// at this step, not at the turn's own end
			if why, ok, err := h.StopAsked(input); err != nil {
				fmt.Fprintln(stderr, "c4-agent hook:", err)
			} else if ok {
				stdout.Write(agent.HookDecision(why))
			}
			return 0
		}
		cp, err := h.Hook(ctx, bytes.NewReader(input))
		if err != nil {
			fmt.Fprintln(stderr, "c4-agent hook:", err)
		}
		hookSafepoint(ctx, h, input, cp, stdout, stderr)
		return 0
	}
	if cmd == "block-pod" {
		return runBlockPod(ctx, rest, stderr)
	}
	if cmd == "lease" {
		return runLease(ctx, rest, stderr)
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
		fs.StringVar(&o.Env, "env", "", "an agent environment tree to run the member in: its own config directory (--config-dir, default members/NAME/config under the agents home) is filled from it, never a person's ~/.claude")
		fs.StringVar(&o.EnvID, "env-id", "", "with --env: the version (C4 ID) the environment must be")
		fs.StringVar(&o.Import, "import", "", "with --env: the config directory the session ran in until now (e.g. ~/.claude-b); its transcript is copied in, the source unchanged")
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
		if o.Env != "" && !explicit(fs, "config-dir") {
			o.ConfigDir = "" // the member's own, not the caller's CLAUDE_CONFIG_DIR
		}
		m, err := h.Register(ctx, o)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: session %s, phase %d, epoch %d, transcript %s\n", m.Name, m.Session, m.Phase, m.Epoch, m.Transcript)
		if m.Env != "" {
			fmt.Fprintf(out, "%s: environment %s in %s; sign in there once (CLAUDE_CONFIG_DIR=%s claude) before it runs\n", m.Name, m.Env, m.ConfigDir, m.ConfigDir)
		}
	case "observe":
		all := fs.Bool("all", false, "also the background sessions that are not running")
		asJSON := fs.Bool("json", false, "print the sessions as JSON")
		cd := fs.String("config-dir", os.Getenv("CLAUDE_CONFIG_DIR"), "the CLAUDE_CONFIG_DIR to read")
		if err := fs.Parse(args); err != nil {
			return err
		}
		obs, err := h.Observe(ctx, *cd, *all, time.Now())
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(agent.ObservedReport{Machine: agent.Machine(), ObservedAt: time.Now().UTC(), Sessions: obs})
		}
		printObserved(out, obs)
	case "unregister":
		name := fs.String("name", "", "the member to take out")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *name == "" {
			return errors.New("unregister: --name is required")
		}
		m, err := h.Unregister(*name)
		if errors.Is(err, agent.ErrNoMember) {
			return fmt.Errorf("unregister: no member named %s", *name)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "unregistered %s; its session %s is left as it is\n", m.Name, agent.ShortID(m.Session))
		return nil
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
			switch {
			case res.Copy != "" && res.CopyStopped:
				fmt.Fprintf(out, "the CLI started a copy, %s, and c4-agent stopped it; the member's own session was not resumed\n", res.Copy)
			case res.Copy != "":
				fmt.Fprintf(out, "the CLI started a copy, %s; stop it with: claude stop %s\n", res.Copy, agent.ShortID(res.Copy))
			}
			return err
		}
		fmt.Fprintf(out, "%s: resumed session %s, epoch %d\n", res.Member.Name, res.Member.Session, res.Member.Epoch)
	case "yield":
		timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for a busy session to finish its turn")
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		// background work gets the same wait, then is recorded and stopped
		res, err := h.Yield(ctx, fs.Arg(0), *timeout, time.Now().Add(*timeout))
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
	case "enroll":
		return runEnroll(ctx, h, fs, args, out)
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
           [--env DIR [--env-id ID] [--import CONFIGDIR]]
                       --env: run the member in its own config directory, filled
                       from an agent environment; --import: bring its session there
                       add a member for an existing session (a different session
                       for an existing member needs --new-phase)
  observe [--all] [--json]
                       this machine's Claude Code sessions as they are, read-only:
                       each one's state in a word, what needs you, what it has used
                       (--all: finished and stopped ones too)
  find --agent-name N  list sessions whose transcript names that agent, newest first
  ls                 members, whether each is busy, idle or stopped, and its last checkpoint
  unregister --name N  take a member out (its session and transcript are left as they are)
  checkpoint NAME      save a turn boundary now
  checkpoint --hook    the Claude Code Stop hook: save the turn that just ended
  resume NAME [PROMPT] continue a stopped member's session (refused while it runs)
  yield NAME [--timeout D]
                       stop a member at its next turn boundary, after a checkpoint
  verify NAME          check the checkpoint chain against the transcript
  log NAME [-n N]      recent checkpoints
  hooks                print the settings fragment that installs the Stop hook
  enroll --owner USER [--node N] [--pool agent] [--slots N] [--kubeconfig K] [--context C]
                       with the farm admin's access: make this machine's own
                       account and a pool its owner owns, and write the
                       runner's kubeconfig, so runner needs no flags
  runner --owner USER [--node N] [--slots N] [--label K=V]... [--every 5s] [--yield-timeout 1m]
                       run this machine's members for the farm's queue (a host
                       runner: a Node for the machine, member pods bound to it)
  block-pod [--cwd DIR]
                       run a block as a unit pod's command
  lease --name N --namespace NS --holder H --owner O --pool P [--ttl 8h] [--context C]
                       the holder's agent for a holder-transfer credential
                       lease: keeps its Secret holding the holder's current
                       short-lived token until the lease ends or it stops

State: $C4_AGENT_HOME (default ~/.c4/agents). claude: $C4_AGENT_CLAUDE or claude on PATH.
Exit codes: 3 running or already has a session, 4 a copy was started, 5 still busy.
`

// runRunner runs the host runner against the cluster in the current
// kubeconfig.
func runRunner(ctx context.Context, h *agent.Harness, fs *flag.FlagSet, args []string, out io.Writer) error {
	dir := agent.RunnerDir(h.Store.Dir)
	enrolled, isEnrolled, err := agent.ReadEnrolled(dir)
	if err != nil {
		return err
	}
	defNode := defaultNode()
	if isEnrolled {
		defNode = enrolled.Node
	}
	node := fs.String("node", defNode, "the machine's Node name (default: the one it was enrolled as)")
	slots := fs.Int("slots", 4, "members that may run here at once")
	every := fs.Duration("every", 5*time.Second, "how often to check pods and sessions")
	yieldTimeout := fs.Duration("yield-timeout", time.Minute, "how long one yield waits for a busy member before trying again")
	hardStop := fs.Duration("hard-stop-after", 0, "stop a member mid-turn if it has not reached a turn boundary this long after a yield request (0: wait for the boundary)")
	kubeconfig := fs.String("kubeconfig", "", "the runner's kubeconfig: the service account c4-hosts/<node> the farm admin issued for this machine (default: the one c4-agent enroll wrote, else $KUBECONFIG or ~/.kube/config)")
	slotsFile := fs.String("slots-file", "", "a file whose first line is the slots to offer now (and, after a space, why), written by a power reader; missing or unreadable means --slots")
	unitsDir := fs.String("units-dir", "", "where each running unit's directory lives (its output root, control directory and podinfo; default the agents home's units/)")
	store := fs.String("store", "", "a regional store replica (http://host:port) to store generator members' expansion documents in, so the queue can read them; without it a generator member adds nothing")
	storeToken := fs.String("store-token-file", "", "a file holding the bearer token for --store")
	storeProject := fs.String("store-project", "", "the project to store documents in (default: the Project listing the pod's namespace)")
	blocksEnv := fs.String("blocks-env", "", "the config directory blocks run in, filled from the farm's agent environment (C4_AGENT_ENV, the base by default) and signed in once by its person (default: the agents home's blocks-env; never a person's own ~/.claude)")
	blocksCwd := fs.String("blocks-cwd", "", "run plan jobs' blocks (pods with C4_AGENT_BLOCK=1), each a fresh session started in this folder, which the claude CLI must trust; without it the runner runs registered members only")
	signerURL := fs.String("signer", "", "the farm's certificate issuer (https://host:port): sign a task record for each unit this machine runs, as its node; without it, host-run units leave none")
	holder := fs.String("holder", "", "the person whose Claude Code sign-in this machine's members use, named on each record's chain (default: --owner, when there is one)")
	observe := fs.Bool("observe", false, "report this machine's Claude Code sessions (read-only, the person's own config dir) to the farm's HUD every minute, for the person's own view; needs --hud")
	hud := fs.String("hud", "", "the farm's HUD (https://host:port), where --observe reports")
	hudToken := fs.String("hud-token-file", "", "with --hud: a file holding this machine's token (its c4-hosts account)")
	farm := fs.String("farm", "farm", "the farm's name, in this runner's deployment name (<farm>/runner/<node>) on its records")
	signerToken := fs.String("signer-token-file", "", "with --signer: a file holding this machine's token for the issuer (its c4-hosts account, audience c4-signer)")
	var owners multi
	fs.Var(&owners, "owner", "a user whose jobs this machine runs, by each job's owner (c4.avalanche.io/owner); repeat for more. Required: a pod of anyone else's job is never bound or run")
	var lbls multi
	fs.Var(&lbls, "label", "a Node label K=V for the agent pool's nodeSelector (default c4.avalanche.io/host=<node>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(owners) == 0 && isEnrolled {
		owners = multi{enrolled.Owner}
	}
	if len(owners) == 0 {
		return errors.New("runner: --owner is required: the users whose jobs this machine runs (or enroll the machine: c4-agent enroll --owner USER)")
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
	// an enrolled machine signs in as itself, and keeps its own tokens
	tokens := ""
	if *kubeconfig == "" && isEnrolled && *node == enrolled.Node {
		*kubeconfig, tokens = enrolled.Kubeconfig, enrolled.Tokens
		if *signerToken == "" {
			*signerToken = agent.TokenFile(tokens, "signer")
		}
	}
	if *kubeconfig != "" {
		os.Setenv("KUBECONFIG", *kubeconfig)
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
	r := &agent.Runner{Client: cl, Harness: h, Node: *node, Labels: set, Owners: owners, Holder: *holder, Slots: *slots, YieldTimeout: *yieldTimeout, HardStopAfter: *hardStop, Tokens: tokens,
		Logf: func(f string, a ...any) { fmt.Fprintf(out, time.Now().Format("15:04:05 ")+f+"\n", a...) }}
	if *blocksCwd != "" {
		hook, err := blockHookCommand()
		if err != nil {
			return err
		}
		r.BlocksCwd, r.HookCommand, r.UnitBin = *blocksCwd, hook, os.Getenv("C4_UNIT_BIN")
		r.BlocksEnv, r.EnvSources = *blocksEnv, agent.EnvSourcesDir(h.Store.Dir)
		if r.BlocksEnv == "" {
			if r.BlocksEnv, err = agent.PrepareBlocksEnv(h.Store.Dir); err != nil {
				return err
			}
		}
		if in, err := agent.NewCLI().SignedIn(ctx, r.BlocksEnv); err == nil && !in {
			fmt.Fprintf(out, "blocks run in %s, which is not signed in yet: sign in once there, through Anthropic's own flow (CLAUDE_CONFIG_DIR=%s claude)\n", r.BlocksEnv, r.BlocksEnv)
		}
	}
	if *observe {
		if *hud == "" {
			return errors.New("runner: --observe needs --hud, where the sessions are reported")
		}
		path := *hudToken
		r.Observe, r.HUD, r.ObserveConfigDir = true, *hud, os.Getenv("CLAUDE_CONFIG_DIR")
		r.HUDToken = func() string {
			b, _ := os.ReadFile(path)
			return strings.TrimSpace(string(b))
		}
	}
	if *signerURL != "" {
		if *store == "" {
			return errors.New("runner: --signer needs --store, where the records go")
		}
		path := *signerToken
		token := func() (string, error) {
			b, err := os.ReadFile(path)
			if err != nil || strings.TrimSpace(string(b)) == "" {
				return "", fmt.Errorf("no issuer token at %s", path)
			}
			return strings.TrimSpace(string(b)), nil
		}
		r.Signer, r.Farm = provenance.LazySigner(provenance.RemoteIssuer(*signerURL, token, nil)), *farm
		go func() {
			if err := r.Signer.Ready(ctx, 5*time.Second); err == nil {
				fmt.Fprintf(out, "signing task records as %s\n", r.Signer.Identity())
			}
		}()
	}
	if *slotsFile != "" {
		r.Capacity = func() (int, string) { return readSlotsFile(*slotsFile, *slots) }
	}
	r.UnitsDir, r.DocsProject = *unitsDir, *storeProject
	if *store != "" {
		src := &tier.HTTPSource{SourceName: "store", SourceKind: "regional", Base: strings.TrimRight(*store, "/"), Client: http.DefaultClient}
		if *storeToken != "" {
			path := *storeToken
			src.Token = func() string {
				b, _ := os.ReadFile(path)
				return strings.TrimSpace(string(b))
			}
		}
		up := &tier.Uploader{Candidates: func(string, c4.ID) []*tier.HTTPSource { return []*tier.HTTPSource{src} },
			Breakers: tier.NewBreakers(), Backoff: tier.Backoff{Base: 200 * time.Millisecond, Max: 5 * time.Second}, Client: http.DefaultClient, Rounds: 3}
		r.Docs, r.Views, r.ViewSrc = up, up, src
	}
	fmt.Fprintf(out, "runner %s: %d slots, labels %v, the jobs of %s\n", *node, *slots, set, strings.Join(owners, ", "))
	return r.Run(ctx, *every)
}

// defaultNode is the Node name a machine runs as unless told otherwise.
func defaultNode() string {
	host, _ := os.Hostname()
	return "c4-host-" + strings.ToLower(strings.Split(host, ".")[0])
}

// runEnroll enrolls this machine on a farm, with the farm admin's access
// (the local farm's own, for c4 farm up): its account, its pool and its
// kubeconfig, in the agents home's runner folder, so `c4-agent runner`
// then needs no flags.
func runEnroll(ctx context.Context, h *agent.Harness, fs *flag.FlagSet, args []string, out io.Writer) error {
	owner := fs.String("owner", "", "the person whose work this machine runs (required)")
	node := fs.String("node", defaultNode(), "the machine's Node name")
	pool := fs.String("pool", "agent", "the pool to make for this machine, owned by --owner")
	slots := fs.Int("slots", 4, "the pool's slots on this machine")
	kubeconfig := fs.String("kubeconfig", "", "the farm admin's kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	kubecontext := fs.String("context", "", "its context for the farm (default: its current context)")
	asJSON := fs.Bool("json", false, "print what was enrolled as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *owner == "" {
		return errors.New("enroll: --owner is required: the person whose work this machine runs")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if *kubeconfig != "" {
		rules.ExplicitPath = *kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: *kubecontext})
	raw, err := cc.RawConfig()
	if err != nil {
		return err
	}
	if err := clientcmdapi.FlattenConfig(&raw); err != nil {
		return err
	}
	name := *kubecontext
	if name == "" {
		name = raw.CurrentContext
	}
	kctx, ok := raw.Contexts[name]
	if !ok || raw.Clusters[kctx.Cluster] == nil {
		return fmt.Errorf("enroll: no farm in the kubeconfig's context %q", name)
	}
	cfg, err := cc.ClientConfig()
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
	e, err := agent.Enroll(ctx, cl, agent.EnrollOptions{Node: *node, Owner: *owner, Pool: *pool, Slots: *slots,
		Dir: agent.RunnerDir(h.Store.Dir), Farm: raw.Clusters[kctx.Cluster]})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(e)
	}
	fmt.Fprintf(out, "enrolled %s for %s's work, in the pool %s\n", e.Node, e.Owner, e.Pool)
	for _, m := range e.Made {
		fmt.Fprintf(out, "  made %s\n", m)
	}
	fmt.Fprintf(out, "its kubeconfig: %s\nstart its runner: c4-agent runner\n", e.Kubeconfig)
	return nil
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

// runLease is the holder's agent for a holder-transfer CredentialLease:
// it keeps the lease's Secret holding the holder's current short-lived
// Claude Code token (from --config-dir's sign-in, the refresh token never
// copied), renews it before it expires, and when the lease's TTL ends or
// the agent is stopped (SIGINT, SIGTERM) deletes the Secret. It prints
// when the lease is good until, never the credential.
func runLease(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("lease", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "the lease's name (its Secret is c4-lease-<name>)")
	ns := fs.String("namespace", "", "the namespace of the holder's jobs")
	holder := fs.String("holder", "", "the person the sign-in belongs to")
	account := fs.String("account", "anthropic", "the model account it serves")
	var owners, pools multi
	fs.Var(&owners, "owner", "an identity whose jobs are the holder's own (a WorkJob's c4.avalanche.io/owner); repeatable")
	fs.Var(&pools, "pool", "a pool whose unit pods may mount it; repeatable")
	ttl := fs.Duration("ttl", 8*time.Hour, "the lease's life")
	renewBefore := fs.Duration("renew-before", 15*time.Minute, "renew this long before the token expires")
	configDir := fs.String("config-dir", os.Getenv("CLAUDE_CONFIG_DIR"), "the Claude Code config dir whose sign-in it leases (default $CLAUDE_CONFIG_DIR, else ~/.claude)")
	kubeconfig := fs.String("kubeconfig", "", "the holder's kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	kubecontext := fs.String("context", "", "the kubeconfig context of the farm (default: its current context)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" || *ns == "" || *holder == "" || len(owners) == 0 || len(pools) == 0 {
		fmt.Fprintln(stderr, "c4-agent lease: needs --name, --namespace, --holder, --owner and --pool")
		return 2
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stop()
	src, err := lease.ClaudeSource(*configDir)
	if err != nil {
		fmt.Fprintln(stderr, "c4-agent lease:", err)
		return 1
	}
	if *kubeconfig != "" {
		os.Setenv("KUBECONFIG", *kubeconfig)
	}
	cfg, err := ctrlconfig.GetConfigWithContext(*kubecontext)
	if err != nil {
		fmt.Fprintln(stderr, "c4-agent lease:", err)
		return 1
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(stderr, "c4-agent lease:", err)
		return 1
	}
	a := &lease.Agent{Client: cl, Namespace: *ns, Name: *name, Source: src,
		Spec: c4v1.CredentialLeaseSpec{Account: *account, Holder: *holder, BoundTo: c4v1.LeaseBinding{Owners: owners},
			Mode: c4v1.LeaseHolderTransfer, Target: c4v1.LeaseTarget{Pools: pools},
			TTL: metav1.Duration{Duration: *ttl}, RenewBefore: metav1.Duration{Duration: *renewBefore}},
		Logf: func(format string, args ...any) {
			fmt.Fprintf(stderr, "%s c4-agent lease: %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
		}}
	if err := a.Run(ctx); err != nil {
		fmt.Fprintln(stderr, "c4-agent lease:", err)
		return 1
	}
	return 0
}

// runBlockPod runs a block as a unit pod's command: one attempt, its
// session in the unit's output view ($C4_OUTPUT), the holder's Claude Code
// store as CLAUDE_CONFIG_DIR. It ignores SIGTERM, so a pod the farm
// deletes mid-turn finishes the turn within its grace period; a stop
// comes at a safe point, through the Stop hook.
func runBlockPod(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("block-pod", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cwd := fs.String("cwd", "/private/tmp", "the folder the session runs in; the same on every machine the session moves to")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// a sealed unit reaches the model and nothing else (model-api): none
	// of the CLI's error reports or telemetry
	os.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	out, cfg := os.Getenv("C4_OUTPUT"), os.Getenv("CLAUDE_CONFIG_DIR")
	if out == "" || cfg == "" {
		fmt.Fprintln(stderr, "c4-agent block-pod: needs $C4_OUTPUT (a writable input) and $CLAUDE_CONFIG_DIR (the holder's store)")
		return 2
	}
	signal.Ignore(syscall.SIGTERM)
	os.Setenv("C4_AGENT_HOME", filepath.Join(out, agent.SessionDir, "agents"))
	hook, err := blockHookCommand()
	if err != nil {
		fmt.Fprintln(stderr, "c4-agent block-pod:", err)
		return 1
	}
	claude := os.Getenv("C4_AGENT_CLAUDE")
	if claude == "" {
		claude = "claude"
	}
	b := &agent.PodBlock{Out: out, ConfigDir: cfg, LeaseDir: os.Getenv("C4_LEASE_DIR"), Cwd: *cwd, Claude: claude, HookCommand: hook, Env: os.Getenv,
		Logf: func(format string, args ...any) { fmt.Fprintf(stderr, "c4-agent block-pod: "+format+"\n", args...) }}
	code, err := b.Run(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "c4-agent block-pod:", err)
	}
	return code
}

// blockHookCommand is the Stop hook block sessions run: this binary's
// checkpoint --hook, with the runner's agents home and c4-unit, since a
// background session's hooks need not see the runner's environment.
func blockHookCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	var env []string
	for _, k := range []string{"C4_AGENT_HOME", "C4_UNIT_BIN", "C4_AGENT_CLAUDE"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+shellQuote(v))
		}
	}
	if len(env) == 0 {
		return shellQuote(exe) + " checkpoint --hook", nil
	}
	return "env " + strings.Join(env, " ") + " " + shellQuote(exe) + " checkpoint --hook", nil
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// explicit reports whether the flag was given on the command line.
func explicit(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == name })
	return given
}

// printObserved writes one line per session: its word, its name, and the
// one thing that needs its person, else how long since it last wrote.
func printObserved(out io.Writer, obs []agent.Observed) {
	needs := 0
	for _, o := range obs {
		name := o.Name
		if name == "" {
			name = agent.ShortID(o.Session)
		}
		what := o.NeedsYou
		switch {
		case what != "":
			needs++
		case o.Limit != nil:
			what = o.Limit.Text
		case o.Transcript == "":
			what = "no transcript yet"
		case o.TurnAge == "just now":
			what = "last wrote just now"
		default:
			what = "last wrote " + o.TurnAge + " ago"
		}
		if o.BackgroundOpen == 1 {
			what += ", 1 background task running"
		} else if o.BackgroundOpen > 1 {
			what += fmt.Sprintf(", %d background tasks running", o.BackgroundOpen)
		}
		if w, rd := o.Usage.Wrote(), o.Usage.Read(); w+rd > 0 {
			what += " · wrote " + tokenWords(w) + ", read " + tokenWords(rd) + " tokens"
		}
		fmt.Fprintf(out, "%-13s %-24s %s\n", o.Word, name, what)
	}
	switch {
	case len(obs) == 0:
		fmt.Fprintln(out, "No Claude Code sessions are running here.")
	case needs == 0:
		fmt.Fprintln(out, "Nothing needs you.")
	}
}

// tokenWords says a token count briefly: 950, 12k, 3.4M, 1.4B.
func tokenWords(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 999_500:
		return strconv.FormatInt((n+500)/1000, 10) + "k"
	}
	if n < 999_950_000 {
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	}
	return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + "B"
}
