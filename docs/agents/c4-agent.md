# c4-agent: agent sessions that survive restarts

`c4-agent` makes Claude Code sessions members of an agent bundle. It is the
first piece of agent bundles in c4-operator. It works on one machine today,
before any cluster is involved. It does four things:

- **Records each member**: its name, its Claude Code session ID, the
  directory it runs in, and the configuration directory its sign-in lives
  in.
- **Saves a checkpoint at the end of every turn.** Claude Code's Stop hook,
  installed in `settings.json` (see Installing the hook), calls
  `c4-agent checkpoint --hook`. It also saves one at every yield and
  resume, hook or not. Each checkpoint names the session's
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

What it relies on from the CLI, checked on a real session on 2026-10-18:
- `claude --bg --resume <id>` with no other flags continues a stopped
  background session under the same ID. With extra flags, or while the
  session runs, it starts a copy under a new ID instead. So `c4-agent
  resume` passes a member's saved flags only the first time, when the CLI
  does not yet know the session as a background session (`claude agents
  --json --all`), and never again.
- It prints `backgrounded · <short id> · <name>`. The short ID is the
  session ID's first eight characters.
- `claude stop` takes the short ID.

If the CLI starts a copy anyway (a race with another resume), `c4-agent
resume` stops the copy at once and fails with exit 4. A copy is never a
member.

## Commands

| command                                   | does                                                                                                                               |
| ----------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `c4-agent find --agent-name NAME`         | lists sessions whose transcript names that agent, newest first                                                                     |
| `c4-agent register --name N --session ID` | adds a member (`--cwd`, `--config-dir`, `--flag` for resume flags, `--new-phase`)                                                  |
| `c4-agent ls`                             | every member: busy, idle (at a turn boundary) or stopped, with its epoch and last checkpoint                                       |
| `c4-agent resume NAME [PROMPT]`           | continues a stopped member's session in the background; refused while it runs (exit 3)                                             |
| `c4-agent yield NAME [--timeout 10m]`     | stops a member at its next turn boundary, after a checkpoint; a busy member is left running if it does not finish in time (exit 5) |
| `c4-agent checkpoint NAME`                | saves a checkpoint now                                                                                                             |
| `c4-agent checkpoint --hook`              | the Stop hook: saves the turn that just ended; ignores sessions that are not members; always exits 0                               |
| `c4-agent verify NAME`                    | checks the checkpoint chain against the transcript                                                                                 |
| `c4-agent log NAME [-n 10]`               | recent checkpoints                                                                                                                 |
| `c4-agent hooks`                          | prints the settings fragment that installs the Stop hook                                                                           |

Exit codes: 3 when the member is running (or already has a session), 4
when the CLI started a copy, 5 when a member is still busy.

## Environment

| variable             | meaning                                                                                                                   |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| `C4_AGENT_HOME`      | where members and checkpoints are kept (default `~/.c4/agents`)                                                           |
| `C4_AGENT_CLAUDE`    | the `claude` binary (default `claude` on `PATH`)                                                                          |
| `C4_AGENT_WORKSPACE` | `git`, `view` (a C4 view of the whole tree; exact but costly on large trees) or `none`; default `git` inside a repository |
| `CLAUDE_CONFIG_DIR`  | the default `--config-dir` for `register` and `find`                                                                      |

## Installing the hook

`c4-agent hooks` prints a settings fragment with one Stop hook. Per-turn
checkpoints need it in the configuration directory's `settings.json`: add
the fragment's `Stop` entry there. Sessions that are not members are
ignored by the hook. This does not change how Claude Code itself is
installed.

Giving the fragment with `--settings` instead does not work for members.
Measured with Claude Code 2.1.302: hooks given that way do not carry into a
background resume with no flags, which is the only resume that continues
a session rather than starting a copy, so the hook never runs on a resumed
member.

Without the hook, `c4-agent` still saves a checkpoint when it yields a
member and before it resumes one, and one live copy, resume by session ID
and the hash chain all hold. What is lost is the checkpoint at the end of
each turn in between, so a hard stop can lose every turn since the last
yield or resume.

## The turn boundary

In this first version the boundary is the end of a turn, when Claude Code
fires its Stop hook. A session reported `idle` by `claude agents --json` is
at such a boundary. The design's finer boundary, after each model response
and before its tools run, needs more from the harness, and comes later.

Claude Code writes its transcript asynchronously, so a checkpoint waits
until the file stops growing (up to three seconds), and always ends at the
last complete line.

**The turn's end is the unit's safe point.** When the session runs inside a
farm unit (the queue sets `$C4_PODINFO` on every unit pod), the Stop hook
also calls `c4-unit safepoint --checkpoint <session>#<seq>:<head>` after
saving the turn (the session ID alone for a session that is not a member).
That tells the farm how often the agent reaches a safe point, and asks it
whether to stop there: a stop asked earlier is reconsidered then, and
dropped if its reason has passed. `c4-unit` waits up to eight seconds for
the answer (`--wait 8s`), and without one falls back on the last request
it sees. When the answer is stop, `c4-unit` exits 10 and the hook prints
`{"continue": false, ...}`, so Claude Code ends the session there, with the
turn saved. Outside a unit, or without `c4-unit` on the PATH
(`$C4_UNIT_BIN` overrides it), the hook does what it always did, and it
still always exits 0.

**The turn's tokens.** Inside a unit the hook also writes the attempt's
token use so far to the unit's `.c4/usage.json`, before the safe point, so
a stop there loses nothing. It totals the session's transcript per model:
input, output, cache reads and cache writes, each API message counted once
(the transcript repeats one message over several lines). The attempt
starts where the checkpoint it resumed from ends, recorded in
`.c4/usage.from` at its first turn, so a resumed member's earlier attempts
are not counted again. The file is rewritten whole each turn; for a member
session the hook owns it. It sets no cost: a session knows tokens, and the
price comes from the metering proxy or a price table. The node daemon (or
the host runner) stores it as `c4.avalanche.io/output-usage`, and the queue
counts it in the job's model spend.

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
- pods bound to it: status and seven annotations (output-checkpoint,
  output-epoch, output-expand, output-verdict, output-measure,
  output-usage, safepoint);
- bindings to itself only.

Keep the host-runner taint on agent pools, so only agent pods select these
nodes. Under a hard power line, `--hard-stop-after` stops a member that
has not reached a turn boundary that long after the queue asked it to
yield, losing its turn in flight.

**Safe points on the host.** A member run here has no pod sandbox, so the
runner keeps a unit directory for each pod it runs, under `--units-dir`
(default the agents home's `units/<pod UID>/`), and names it on the
member:

| path       | what                                                                         |
| ---------- | ---------------------------------------------------------------------------- |
| `out/`     | the unit's output root (`$C4_OUTPUT`): `c4-unit` writes `.c4/safepoint` here |
| `control/` | `$C4_CONTROL`: the runner writes the queue's `safepoint.decision` here       |
| `podinfo`  | `$C4_PODINFO`: the pod's annotations, as the downward API writes them        |

The Stop hook reads the directory from the member record, so it does not
depend on the session's environment. The runner relays as the node daemon
does: each new safe point becomes the pod's `c4.avalanche.io/safepoint`
annotation, and the queue's `c4.avalanche.io/safepoint-decision` comes back
to `control/safepoint.decision`. When the decision is stop, the hook ends
the session at that turn, and the runner reports the pod **Yielded** with
its checkpoint, not finished.

**A generator member** leaves its expansion document at
`out/.c4/expand.json`, or at `.c4/expand.json` in its working directory.
When its session ends, the runner stores the document and names it on the
pod (`c4.avalanche.io/output-expand`) for the queue to expand. A review
member's verdict (`.c4/verdict.json`) goes the same way, named in
`c4.avalanche.io/output-verdict`. That needs
`--store` (a regional store replica), `--store-token-file` (its bearer
token), and `--store-project` (else the runner reads the Project listing
the pod's namespace). Without a store, the runner says so, and the queue
adds nothing under the generator.

## Blocks: a plan job's units as fresh sessions

A plan job (class `graph`, its units from an expansion document) runs on a
host as **blocks**: each unit is its own Claude Code session, which the
runner starts for the unit's first attempt and resumes by its session ID
for every later one. This is the builder driver of proposals agent-jobs
10.14. `c4-agent runner --blocks-cwd DIR` turns it on.

| variable          | set by                   | what                                                         |
| ----------------- | ------------------------ | ------------------------------------------------------------ |
| `C4_AGENT_BLOCK`  | the job's template       | `1`: the pod is a block                                      |
| `C4_BLOCK_PROMPT` | the template, or a unit's `env` in its document (the unit's wins) | the block's task                |
| `C4_BLOCK_REPO`   | the template             | the project's working tree on the host                       |
| `C4_BLOCK_TOOLS`  | the template             | the tools it may use, space separated (default `Read Edit Write Glob Grep Bash(git:*) Bash(go:*)`) |
| `C4_BLOCK_MODEL`  | the template or a unit   | the model, when not the CLI's default                        |
| `C4_BLOCK_REWORK` | a verdict's `with.env`   | a reworked attempt: what the review asks                     |

- **The member** is `<namespace>.<job>.<unit>`, registered when its session
  starts, and marked a block.
- **The first attempt** starts `claude --bg` in `--blocks-cwd` with the
  block's task, its repository, its unit's output directory and the rules
  for its commands as the first prompt. The flags are kept by the CLI with
  the session:
  - `--restricted --strict-mcp-config`: none of the machine's user,
    project or local settings, CLAUDE.md, connectors or MCP servers reach a
    block, and its file tools stay in its working folders;
  - `--tools` and `--allowedTools` from `C4_BLOCK_TOOLS`, under
    `--permission-mode dontAsk`, so a tool the job did not allow is refused,
    never prompted for;
  - `--settings` with the Stop hook (this binary's `checkpoint --hook`,
    with its agents home and `c4-unit`).
- **A later attempt** (a rework, a yield) wakes the same session by its ID,
  with what the review asks (`C4_BLOCK_REWORK`) or that the farm resumed
  it, and the attempt's new output directory.
- **A turn's end is a safe point.** When the CLI reports the session idle
  after a turn this attempt saved, the runner stops it, and then:
  - if the farm answered **stop** at that safe point, the pod **Yields**
    with its checkpoint, and the next attempt resumes the session by ID;
  - if the block wrote a note of its **next part** (`.c4/more`; an empty
    note is withdrawn), the runner wakes the same session for that part, in
    the same attempt, so every part's end is a safe point and a turn
    boundary;
  - if the block wrote **`.c4/done`** (its one-line summary), or the CLI
    reports it `done`, the pod is **Completed**. Its control documents come
    from its own output directory only; block sessions share their folder,
    so the folder's `.c4/` never counts. The block's own word decides: the
    CLI's state is a reading of its last message, which can call a finished
    task blocked;
  - otherwise it gave up, and the attempt fails (**BlockGaveUp**), for the
    job's retries or a person.
- **A yield waits for the farm's decision.** A block asked to yield (a
  hold-stop, a preemption, or its pod's deletion, which the farm treats as
  a drain) is left to reach its next safe point without blocking the
  runner. There, a stop yields it, and a continue (the stop no longer
  holds) lets it go on. A pod the farm is deleting is finished by the
  runner, as a kubelet would when its grace ends: the host's role may
  delete pods, and its policy only its own pods the farm is already
  deleting. A block withdrawn before its session started is let go.
- **A session that cannot become the member** (its transcript never
  appears) is stopped, so a retry never leaves one running.

What the host needs:
- `--blocks-cwd` must be a folder the claude CLI trusts **exactly** (a
  background session refuses an untrusted folder, and trust is not
  inherited by subfolders). Keep the repository, `--units-dir` and the
  tools' binaries under it, so the file tools can reach them.
- The commands a block runs must match `C4_BLOCK_TOOLS` whole: the prompt
  asks for one command at a time, absolute paths, `git -C` and `go -C`.
- The account the sessions run as still shows in their context (its
  email); restricted mode removes everything else of the machine's.

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
