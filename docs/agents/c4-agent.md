# c4-agent: agent sessions that survive restarts

`c4-agent` makes Claude Code sessions members of an agent bundle. It is the
first piece of agent bundles in c4-operator. It works on one machine today,
before any cluster is involved. It does four things:

- **Records each member**: its name, its Claude Code session ID, the
  directory it runs in, and the configuration directory its sign-in lives
  in.
- **Saves a checkpoint at the end of every turn.** Claude Code's Stop hook
  calls `c4-agent checkpoint --hook`. Each checkpoint names the session's
  transcript up to its last complete line, as a hash chain of C4 IDs, and
  records the working tree (the git commit and a C4 ID of everything
  uncommitted).
- **Resumes a member by its session ID**, with its full context, and never
  starts a fresh agent in its place. A different session for a member is a
  deliberate new phase (`--new-phase`), and is refused while the old one
  runs.
- **Keeps at most one live copy.** `claude --bg --resume <id>` continues a
  stopped session under the same ID, but if that session is already
  running it starts a copy instead. `c4-agent resume` checks `claude agents
  --json` first, and refuses to resume a running member. It also raises
  the member's lease number (its epoch) on every resume, and reports any
  copy the CLI starts anyway.

It drives Claude Code only through its published interfaces: transcript
files, hooks, and the `claude` CLI. It never changes Claude Code's settings.

## Commands

| command                                   | does                                                                                          |
| ----------------------------------------- | --------------------------------------------------------------------------------------------- |
| `c4-agent find --agent-name NAME`         | lists sessions whose transcript names that agent, newest first                                |
| `c4-agent register --name N --session ID` | adds a member (`--cwd`, `--config-dir`, `--flag` for resume flags, `--new-phase`)             |
| `c4-agent ls`                             | every member: busy, idle (at a turn boundary) or stopped, with its epoch and last checkpoint  |
| `c4-agent resume NAME [PROMPT]`           | continues a stopped member's session in the background; refused while it runs (exit 3)       |
| `c4-agent yield NAME [--timeout 10m]`     | stops a member at its next turn boundary, after a checkpoint; a busy member is left running if it does not finish in time (exit 5) |
| `c4-agent checkpoint NAME`                | saves a checkpoint now                                                                        |
| `c4-agent checkpoint --hook`              | the Stop hook: saves the turn that just ended; ignores sessions that are not members; always exits 0 |
| `c4-agent verify NAME`                    | checks the checkpoint chain against the transcript                                            |
| `c4-agent log NAME [-n 10]`               | recent checkpoints                                                                            |
| `c4-agent hooks`                          | prints the settings fragment that installs the Stop hook                                      |

Exit codes: 3 when the member is running (or already has a session), 4
when the CLI started a copy, 5 when a member is still busy.

## Environment

| variable             | meaning                                                                                     |
| -------------------- | ------------------------------------------------------------------------------------------- |
| `C4_AGENT_HOME`      | where members and checkpoints are kept (default `~/.c4/agents`)                             |
| `C4_AGENT_CLAUDE`    | the `claude` binary (default `claude` on `PATH`)                                            |
| `C4_AGENT_WORKSPACE` | `git`, `view` (a C4 view of the whole tree; exact but costly on large trees) or `none`; default `git` inside a repository |
| `CLAUDE_CONFIG_DIR`  | the default `--config-dir` for `register` and `find`                                        |

## Installing the hook

`c4-agent hooks` prints a settings fragment with one Stop hook. There are
two ways to use it. Neither changes how Claude Code itself is installed.

- **Per member**, with no change to any settings file: save the fragment to
  a file and register the member with `--flag --settings --flag
  <file>`. The hook then applies from the member's next resume.
- **For every session** under a configuration directory: add the fragment's
  `Stop` entry to that directory's `settings.json`. Sessions that are not
  members are ignored by the hook.

## The turn boundary

In this first version the boundary is the end of a turn, when Claude Code
fires its Stop hook. A session reported `idle` by `claude agents --json` is
at such a boundary. The design's finer boundary, after each model response
and before its tools run, needs more from the harness, and comes later.

Claude Code writes its transcript asynchronously, so a checkpoint waits
until the file stops growing (up to three seconds), and always ends at the
last complete line.

## Bundles on the farm: the host runner

An `AgentBundle` names a team of members, how many may run at once, its
deadline and its pools. The operator turns it into one WorkJob of class
`agent`, with one unit per member. The queue schedules those units like
any others that checkpoint:
- it releases members up to the bundle's cap;
- it asks a running member to yield when the bundle is suspended or the
  slots are needed;
- it releases the member again later, with the checkpoint it yielded at.

`c4-agent runner` is the other half, on the machine where the members'
sessions live. It is a host runner.

- It registers a Node for the machine, labeled `c4.avalanche.io/host=<node>`
  and tainted `c4.avalanche.io/host-runner` so only agent pods land there.
  It keeps the Node and its Lease fresh.
- It binds member pods whose nodeSelector matches the Node. When a pod
  starts, it resumes the member's session with `c4-agent resume` (never a
  fresh one; a running member is adopted, not resumed twice). Then it
  reports the pod Running, with the member's epoch.
- When the queue asks a member to yield, it waits for the end of the
  member's turn, checkpoints, stops the session, and reports the pod
  Succeeded with the checkpoint (`c4.avalanche.io/output-checkpoint`).
  The queue records it as the member's resume point.
- After a restart of the machine, a new runner finds the pods still
  Running and their sessions stopped, and resumes each one from its last
  turn.

The sessions run as the machine's own Claude Code sessions, under their
holder's own sign-in, so no credential leaves the machine.

`config/samples/agent-bundle/team.yaml` has a Pool for a runner and a
bundle of eight members with at most five running. Setting `suspend: true`
on the bundle yields every running member at its next turn boundary, and
clearing it brings them back.

The runner signs in as its machine's own service account,
`c4-hosts/<node>`, named for its Node (`--kubeconfig` with the account's
token, issued by the farm admin). `config/rbac/host-runner.yaml` and
`config/policy/host-runner.yaml` limit that account:
- its own Node: created only with the host-runner taint, then only its
  slot limit, capacity reason and status;
- its own Lease;
- pods bound to it: status and its two annotations;
- bindings to itself only.

Keep the host-runner taint on agent pools, so only agent pods select these
nodes. Under a hard power line, `--hard-stop-after` stops a member that
has not reached a turn boundary that long after the queue asked it to
yield, losing its turn in flight.

## Capacity under a power cap, and the bundle's clock

The runner can lower the slots its machine offers, for example from the
power headroom on a weak charger:
- `c4-agent runner --slots-file <path>` reads one line, `N [why]`, written
  by any power reader. `c4 farm agent`'s power duty will drive it
  directly.
- The runner writes the node's `c4.avalanche.io/slot-limit` label and puts
  the reason in `c4.avalanche.io/capacity-reason`. The queue counts the
  node at that many slots, never more than its Pool's `slotsPerNode`.
- A lower value applies at once; a higher one only after it has held for a
  minute, so a reading near a threshold never flaps the node.
- With fewer slots, the queue releases no new member. Shedding the
  members already running down to the new limit is the scheduler's change
  that follows (proposed to its owner).

Each AgentBundle keeps a virtual clock in `status.clock`:
- it starts the first time the bundle runs;
- it stops while the bundle is Suspended, and continues from where it
  stopped when the bundle resumes;
- `virtualNow` is the bundle's time and `driftSeconds` how far it trails
  the real clock.

Members don't read it yet; that comes with the bundle's mailbox.
