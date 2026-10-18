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
