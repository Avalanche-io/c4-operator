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

## A clean environment for each member

A member runs in a config directory of its own, filled by C4 from an agent
environment, never in a person's `~/.claude`:
- **An environment is a tree:** `settings.json` (C4's Stop hook), `CLAUDE.md`
  (a member's working rules), `mcp.json` (its MCP servers), skills, and
  `c4-member.json` (the flags members run with, and the commands they need).
  `config/agent-env/base` is the clean one C4 ships.
- **Its version is its C4 ID:** the ID of its listing, every file 0644, every
  directory 0755, every time 2026-01-01T00:00:00Z, so the ID depends on
  content only. Every change for a build is a new version, recorded as a
  layer: what changed, why, who decided it.
- **`c4-agent register --name N --session ID --env DIR`** fills the member's
  own config directory (default `members/N/config` under the agents home)
  from the tree, with `--env-id ID` to insist on a version. It records the
  version on the member and on every checkpoint, so any result traces to the
  exact environment it was made in. The environment's MCP configuration is
  the member's only one (`--mcp-config`, `--strict-mcp-config`). A person's
  own `~/.claude` or `~/.claude-*` is refused as a member's directory.
- **`--import DIR`** brings an existing session in from the config directory
  it ran in (for example `~/.claude-b`): its transcript, subagents and tool
  results are copied into the member's directory, and the source is left as
  it was. A session still running there is refused, since the copy would
  fork; from then on the member is resumed only in its own directory.
- **Sign-in:** the new directory has none. The person signs in there once,
  through Anthropic's own flow (`CLAUDE_CONFIG_DIR=<dir> claude`), and
  `register` says so. No credential is copied between directories.

## Commands

| command                                   | does                                                                                                                               |
| ----------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `c4-agent find --agent-name NAME`         | lists sessions whose transcript names that agent, newest first                                                                     |
| `c4-agent register --name N --session ID` | adds a member (`--cwd`, `--config-dir`, `--flag` for resume flags, `--new-phase`; `--env DIR` runs it in a clean environment, `--import DIR` brings its session there) |
| `c4-agent ls`                             | every member: busy, idle (at a turn boundary) or stopped, with its epoch and last checkpoint                                       |
| `c4-agent resume NAME [PROMPT]`           | continues a stopped member's session in the background; refused while it runs (exit 3)                                             |
| `c4-agent yield NAME [--timeout 10m]`     | stops a member at its next turn boundary, after a checkpoint; a busy member is left running if it does not finish in time (exit 5); background work still running gets the same wait, then is recorded in the checkpoint and stopped |
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

**What the attempt reasoned with.** The same file says what the attempt
reasoned with, as its transcript records it: the observed side of a unit's
declared `reasoning` (`does`, `model`, `effort`, `tools`).

```json
"reasoning": {
  "does": "model",
  "model": "claude-opus-5-5",
  "effort": "medium",
  "thinking": "adaptive",
  "tools": "custom",
  "toolNames": ["Edit", "EndConversation", "Glob", "Grep", "Read", "Write"]
}
```

- `model` is the model that wrote most of the attempt's output, and
  `models` lists every model, when it used more than one.
- `effort` is the effort level at its last turn, and `efforts` lists
  every level it ran at, when there was more than one.
- `thinking` is the thinking mode it asked for.
- `tools` is the name of the tool a block declares (`C4_BLOCK_PROFILE`),
  `toolNames` the tools the session offered the model, and `mcpServers`
  the MCP servers among them (from `mcp__<server>__` names).

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

**A usage limit is a capacity drop.** On a host runner every member signs
in as the same person, so when one member's turn ends at a usage limit (the
session, weekly or spend window, or a model's own), the machine has no
capacity until it resets. The runner sees the CLI's rate-limit record in the
transcript, and offers no slots until a minute past the reset the CLI gave.
With no reset time (a spend limit, a model's limit), it tries again after
`C4_USAGE_LIMIT_PROBE` (default 30 minutes). The node's capacity reason says
so ("usage limit: You've hit your session limit · resets 1:10am ..."). The
queue yields and holds members as for any drop in capacity, and brings them
back when the slots return. In a pod, a turn that hits a limit ends the
attempt with exit 75 and the limit as its message, not as the block giving
up.

**An idle member is woken for its messages.** A member idle between turns
does not see a message until it takes a turn, and the CLI cannot put one
into a running session. So when a member's pod has messages it has not read
(steering from a person or the lead, a question to it, an answer to one of
its own), the runner saves a checkpoint, stops the idle session at that turn
boundary, and resumes it by its ID with the messages as its prompt. A
question comes with the command to answer it (`c4-unit answer --dir <its
unit directory> --ask <id> --answer ...`). The messages are then marked
read, so it is woken once for them. A member mid-turn, or with background
work open, is left alone; it is woken at its next idle turn boundary.

**Background agents and commands survive a yield, or are recorded.** A
session's turn can end with work still running beside it: a background
subagent, or a command run in the background. Stopping the session kills
that work. So a turn boundary with background work open is not yet a safe
stop:
- **The stop waits for it.** A yield, a block's end of turn, and
  `c4-agent yield` keep the session running until the work reports (the
  session takes a new turn with its result, and the stop comes at the end
  of that turn).
- **The wait is bounded.** It lasts `C4_YIELD_BACKGROUND_WAIT` (default
  30 minutes), and never past the farm's own deadline for the stop: a
  minute before a soft stop's `yield-by` (10 minutes after the request),
  30 seconds before a deleting pod's grace ends. So the farm never has to
  kill the session with its work unrecorded.
- **What it cannot wait for is recorded.** Past the bound, the session is
  stopped, and its checkpoint lists the work cut short (`background`: the
  kind, what it was doing, and a subagent's own transcript, so its work so
  far can be read). The pod's message says how many. When the member
  resumes, its first prompt says what was stopped and where its
  transcript is, so it can start again what it still needs. It is told
  once.
- **A hard stop** (`--hard-stop-after`) records the same before it stops
  the session mid-turn.
- **Open work** is read from the transcript: each launch the CLI reports
  (an agent `async_launched`, a command with a `backgroundTaskId`) since
  the session's process started, with no task notification for it and not
  stopped with TaskStop. Work an earlier process started died with that
  process.
- **In a pod** (`c4-agent block-pod`, print mode), each turn is its own CLI
  process, so background work a turn left running ended with it. The block
  records it in `.c4/background-lost.json`, which goes with the unit's
  output, and tells its next turn.

The sessions run as the machine's own Claude Code sessions, under their
holder's own sign-in, so no credential leaves the machine. So a runner
runs only its owners' work:
- `c4-agent runner --owner USER` (repeat for more; required) binds and runs
  a pod only if the WorkJob that controls it is owned by one of them. It
  reads the owner from the job, matched by the UID in the pod's controller
  reference, never from the pod. Anything else bound to its Node fails
  with reason `NotTheOwner`.
- A job's owner (`c4.avalanche.io/owner`) is whoever created it, and
  cannot change. A bundle is stamped the same way, and its member job
  carries the bundle's owner.
- Only the work queue creates pods a WorkJob controls (the `c4-host-pods`
  admission policy), so no one can point a pod of their own at someone
  else's job.
- A Pool's `owners` keeps everyone else's jobs out of it in the first
  place. A job of another owner that names only that pool is not
  schedulable, and says whose work the pool runs; a dry run of it is
  refused with the same words.

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
- pods bound to it: status and its output annotations (output-checkpoint,
  output-epoch, output-expand, output-verdict, output-measure,
  output-usage, output-tree, output-provenance), safepoint, asks, answers
  and steer-seen;
- bindings to itself only.

**Task records for host-run units.** With `--signer URL --signer-token-file
FILE` (the farm's certificate issuer, and a token for the machine's
`c4-hosts` account with audience `c4-signer`), the runner signs a task
record for each unit pod it ends, as the runner (`runner/<node>`): the
person's own machine is a deployment apart from any node daemon on the
same Node, owned by that person, not by the farm. The record says what the
unit left (its output view, its session's checkpoint), what it resumed
from, the job and unit, and when. Its chain says whom it accrues to: by the
unit and the member it is, for the job's owner (read from the job), under
that job's ownership, using its holder's own sign-in on this machine (a
`signIn` credential, no lease), on `<farm>/runner/<node>` (`--farm`). Until
deployments are bound, its `gap` says the runner has no binding. The record
goes to the project's store (`--store`) and its ID onto the pod
(`output-provenance`), as a node daemon records a capture.

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
for every later one, so each unit starts fresh and keeps its own context
across reworks and yields. `c4-agent runner --blocks-cwd DIR` turns it on.

| variable           | set by                                                            | what                                                                                                        |
| ------------------ | ----------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `C4_AGENT_BLOCK`   | the job's template                                                | `1`: the pod is a block                                                                                     |
| `C4_BLOCK_PROMPT`  | the template, or a unit's `env` in its document (the unit's wins) | the block's task                                                                                            |
| `C4_BLOCK_REPO`    | the template                                                      | the project's working tree on the host                                                                      |
| `C4_BLOCK_TOOLS`   | the template                                                      | the tools it may use, space separated (default `Read Edit Write Glob Grep Bash(git:*) Bash(go:*)`)          |
| `C4_BLOCK_MODEL`   | the template or a unit                                            | the model, when not the CLI's default                                                                       |
| `C4_BLOCK_EFFORT`  | the template or a unit                                            | the effort level (`--effort`: low, medium, high, xhigh, max), when not the CLI's default                    |
| `C4_BLOCK_PROFILE` | the template or a unit                                            | the name of its tool, reported in usage.json (default `default`, or `custom` with `C4_BLOCK_TOOLS`) |
| `C4_BLOCK_REWORK`  | a verdict's `with.env`                                            | a reworked attempt: what the review asks                                                                    |

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

### Acting on the brief, and questions

A block acts on its brief. It decides everything the brief leaves to it,
and never asks for confirmation of what the brief says, as a renderer
renders the light it was given.
- **Information only a person could give.** If the brief lacks it, the block writes `.c4/needs.json` (`{"needs": "what is missing", "why": "why the brief does not settle it", "about": ["unit:schema", "path:plates/"]}`, `about` optional) and ends its turn. That is a work-stopping error, not a question:
  - **The attempt fails with `NeedsInformation`,** and the text is its message. On a host that is the pod's reason; in a pod, `block-pod` exits 12 with the text as its termination message, as `c4-unit needs` does.
  - **It is not retried.** It shows as an incident, with its checkpoint and its session's view kept.
  - **When someone supplies the information** (HUD or MCP `c4_supply`), the unit runs again and resumes the same session. The prompt says what it reported missing (`$C4_NEEDED`) and what was supplied (`$C4_SUPPLIED`).
  - **It can heal.** Often another block is building the very thing that makes the question moot. `about` names that work: `unit:<name>` (a unit of the job), `path:<prefix>` (where it would land in the tree; a bare value is a path) or `job:<namespace>/<name>`. Its message carries it as an `about:` line.
    - **A re-check:** when that work lands, the farm runs a short extra attempt of the block (`$C4_RECHECK=1`, with `$C4_NEEDED` and what landed in `$C4_RECHECK_WHY`). It resumes the session for one turn, told only to decide whether the question still applies.
    - **Withdrawn:** the block writes `.c4/withdraw` (`{"why": "what settled it"}`, or `c4-unit withdraw --why` from a script), and the attempt ends with exit 0 and `withdrawn: <why>` as its message (a host runner: reason `Withdrawn`, the why). Only that withdraws it: a re-check that ends any other way, exit 0 included, keeps the question.
    - **Still needed:** `needs.json` again ends it with the new text. A turn that decides nothing, or that runs past `$C4_RECHECK_SECONDS` (the farm's `escalation.recheck.seconds`, 300 by default; the pod is stopped a minute later), ends still needed with the text it had.
    - **Nothing else changes:** the re-check spends no retry, and the runner ends it at that one turn's end, so it cannot drift into the task.
    - **After a withdrawal,** the unit runs again, and its prompt says what it asked and why the question was withdrawn (`$C4_WITHDRAWN`).
- **A person who decides on purpose,** such as a creative approval gate, is a step in the job's graph: a wait or a review that a person decides. The block resumed after it hears the decision (`$C4_DECISION`), with the reply's files at `$C4_REPLY`, or fetched into the host unit's `reply/`.
- **Questions between agents.** A block that lacks a fact another unit of its job has asks it. It writes `.c4/ask.json` (`{"to": "unit:<name>", "ask": "..."}`; without `to`, the job's `answerer`) and ends its turn.
  - **Asking:** the Stop hook puts the question to the farm (`c4-unit ask`) and records it open in `.c4/asking`.
  - **Holding:** the block keeps its slot. The runner, or `block-pod`, holds it idle and wakes its session with the answer, marking it read, for up to `C4_ASK_HOLD` (default 10 minutes).
  - **A slow answer:** past the hold, a question to a named unit becomes a wait served by that unit (`c4-unit wait --server unit:<name>`). The block gives up its slot and resumes with the answer as its decision. A question to the job's answerer, or one whose wait is refused, wakes the block instead to decide within its brief.
  - **Answering:** a question to a block arrives as a message, with how to answer. The block writes `.c4/answer.json` (`{"ask": "<id>", "answer": "..."}`, or `"cannot": "why"`), and the hook gives it to the farm (`c4-unit answer`).
  - **A question to a block that has finished** wakes it for an extra attempt (`$C4_ANSWER_ONLY`). The block is told to answer only, and the attempt completes when that turn ends.
  - The design is in DESIGN.md 10.6, "Agents answer agents" and "Missing information is an error, not a question".

### Steering a running block

A person can message a running block (HUD steer, MCP `c4_steer`). The
messages reach the unit's control directory. At the end of every turn
that goes on, the Stop hook reads the new ones (`c4-unit steer`, which
marks them seen) and keeps the session from stopping, with the messages
as what to do next. So the block takes them into account in the same
attempt. A turn the farm stops at reads none, and they wait for the
attempt that resumes it.

## Blocks in containers, and sessions as views

A block can run in a unit pod as well as on a host runner. Its session
history moves between them as a view.

**The session as a view.** When a block yields, its session leaves in its
unit's output view, in the folder `c4-session/`:

| path                                                   | what                                   |
| ------------------------------------------------------ | -------------------------------------- |
| `c4-session/session.json`                              | the member, its session ID, its folder |
| `c4-session/transcript.jsonl`                          | the session's transcript               |
| `c4-session/agents/members/<member>.json`              | the member record                      |
| `c4-session/agents/members/<member>.checkpoints.jsonl` | its checkpoint chain                   |

The next attempt is resumed from that view (`C4_RESUME_VIEW`), wherever it
runs.
- **On a host,** the runner fetches the view into the unit's output root.
- **In a pod,** the view is mounted there.

Either way, the session is imported for that machine's Claude Code store
and resumed by its ID. The chain is over the transcript's bytes, not its
path, so `c4-agent verify` passes on every machine the session has been
on. An import is refused while the session is running where it arrives:
one live copy only. On a host the runner stores the view and names it in
`c4.avalanche.io/output-tree`; in a pod the node daemon captures it.

**`c4-agent block-pod`** is a block as a unit pod's command.
- It runs each part as one Claude Code invocation in print mode, in `/private/tmp`, so the transcript is filed under the same folder on every machine.
- The writable input (`$C4_OUTPUT`) is mounted inside that folder, so the block's file tools reach it.
- It exits 0 when the task is done, 0 when it yields at the farm's stop (the session is already in its view), and 3 when the block gives up.
- It ignores SIGTERM: a deleted pod finishes its turn, and the farm's stop comes at its safe point.

**The sign-in in a pod** comes from a credential lease (credential-leases.md). `$C4_LEASE_DIR` names the mount, and `block-pod` links the pod's Claude Code store to it.

**The image** is `images/block/Dockerfile`, built with `make image-block` (`BLOCK_IMG`, default `c4-block:dev`): Claude Code with bash and git, plus c4-agent and c4-unit for the Stop hook. It holds no credential. It runs as uid 65532 and fits PodSecurity `restricted`:

```yaml
securityContext:              # the pod's
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile: {type: RuntimeDefault}
containers:
- name: block
  image: c4-block:dev
  securityContext:
    allowPrivilegeEscalation: false
    capabilities: {drop: [ALL]}
```

A run on the e2e cluster moved one session from a host to a pod and back, at safe points:
- a ten-part block;
- `c4-agent verify` passed on all 15 checkpoints;
- no copy was started.

## Capacity under a power cap, and the bundle's clock

The runner can lower the slots its machine offers, for example from the
power headroom on a weak charger:
- `c4-agent runner --slots-file <path>` reads one line, `N [why]`, written
  by any power reader.
- `c4 farm agent power` is that reader for a laptop. It reads the battery
  every minute and writes `~/.c4/agents/slots`, the file the runner reads
  given `--slots-file ~/.c4/agents/slots`. Work stops when the battery,
  not on power, falls below 25% (`--stop-below`). It resumes only once the
  battery is back at 90% (`--resume-at`): a charger arriving at 30% doesn't
  restart five agents that will run it flat again. On macOS a charger
  counts only when power is arriving from it, so a loose plug that still
  reads as attached doesn't hold the farm up. A machine with no battery
  always offers `--slots`. `--once` reads and writes once, and prints what
  it wrote:
  ```
  18:04 PDT  4 slots · battery 100%, on power → /Users/you/.c4/agents/slots
  ```
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
