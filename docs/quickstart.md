# Quickstart: your own agents first, then a farm on your machine

If you use Claude Code, start with what you already run: one command shows
your sessions as work, what each is doing and what needs you, without
moving or changing anything. Then this builds a complete C4 farm inside
Docker, renders a sample shot on it, watches the render in the HUD, moves
its deadline, takes the frames home with every file checked, and teaches
the farm a small job of your own. No Kubernetes command or field appears
anywhere: the farm's administrators have [docs/install.md](install.md) for
that.

## 1. Install `c4 farm`

```sh
brew install mrjoshuak/tap/c4
```

This installs the `c4` command and its farm tools; `c4 farm` runs
`c4-farm`. If you installed `c4` another way before, `which -a c4` should
list Homebrew's first; an older `c4` ahead of it has no `c4 farm` (`c4-farm`
says so). Or take a `c4-farm` binary for your machine from the
[release page](https://github.com/Avalanche-io/c4-operator/releases) and
write `c4-farm` wherever this guide writes `c4 farm`.

## 2. See your own agents

```sh
c4 farm agents
```

```
AGENT   STATE      FOLDER         USED  FOR
port    needs you  ~/src/textkit  41k   2h 10m
docs    working    ~/src/textkit  12k   25m
review  idle       ~/src/site     950   3h 5m

port needs you: Should I rename the package too?
```

One line per Claude Code session on this machine: its state in a word,
its folder, the tokens it has used and how long it has run, and then what
needs you, or "Nothing needs you." It reads what is already on your
machine, needs no farm and no Docker, and moves, stops and signs in to
nothing. `c4 farm agents --open` shows the same in the HUD, served on this
machine only.

## 3. Start the farm

You need Docker Desktop running, with at least 6 GB of memory and 4 CPUs
(Settings → Resources).

```sh
c4 farm up
```

The first time, at a terminal, it asks one question before it changes
anything, and remembers the answer:

```
Connect Claude Code to this farm? It adds the farm's tools to Claude Code (~/.claude.json), and lets c4 farm agents add save an agent's turns with a hook in that agent's own folder, without asking again, and runs a background service (~/Library/LaunchAgents/io.avalanche.c4.agent.plist) so this machine can run your agents. c4 farm integrate off takes it all out. [Y/n]
```

```
farm      c4-local   4 nodes · 6 slots, 4 more to rent · store ready
project   saltmarsh   you: sam
owner     sam, who plays every role here
claude    has the farm's tools (c4-farm in ~/.claude.json); c4 farm integrate off takes it all out
agents    this machine (c4-host-laptop) is enrolled for sam's agents and its runner is started; they run here once you sign in where they run
HUD       http://127.0.0.1:30080/?as=sam#/needs   (c4 farm open opens it signed in)
roles     artist, supervisor, wrangler, admin, facilities, td, to try their views (c4 farm up --as ROLE opens one)
time      up in 1m4s: cluster 41s, operator 12s, store 6s, tools 4s
```

The first time, a yes also opens Claude Code once, in the folder the farm's
agents run in (`~/.c4/blocks`), apart from your own Claude Code: sign in,
trust the folder, then `/exit`. Until then the agents wait, and `up` says so.

The machine becomes yours as a device sign-in does: it offers itself to
the farm for a code, and the person whose Claude Code sign-in its runner
uses gives the code back. That is you here, so `up` does both. On another
machine, its holder runs `c4-agent enroll-offer` there and then `c4 farm
enroll NODE CODE` (`--for PERSON` lets someone else's work run there too,
on the holder's sign-in).
Until a machine is someone's, the farm places no one's work on it.

The HUD takes you, or any role, only from you: `up` keeps a token in
`~/.c4/c4-local/installer-token` that only you can read, and every `c4
farm` command sends it. The browser `up` opens is signed in by a one-time
code, and so is any `c4 farm open`. The farm's own jobs can reach the HUD,
but never that file, so a job cannot act as you or as a role.

Saying no, or `c4 farm up --no-integrate`, adds nothing to Claude Code or
this machine. Run without a terminal (a script, CI), `up` asks nothing,
adds nothing, and says so. Either way `c4 farm integrations` lists what it
could add, and `c4 farm integrate on` adds it later. An agent cannot turn
it on for you: from inside Claude Code, `c4 farm integrate` and `c4 farm
agents add` refuse and print the command for you to run in your own
terminal.

That is a farm, not a demo of one: render nodes in two pools (`cpu` and
`gpu`), a burst pool the farm rents from only when a deadline needs it, a
store that names every file by its C4 ID, a project (`saltmarsh`, Petrel &
Finch's feature) and a sign-in for every role. The HUD opens in your
browser. The prebuilt tools are published to every project:

```sh
c4 farm tools
```

```
comp       frames  v1    Composite · farm-wide
infer      frames  v1    Model pass · farm-wide
render     frames  v1    USD render · farm-wide
sim        frames  v1    Fluid sim bake · farm-wide
transcode  once    v1    Review movie · farm-wide
usd        once    v1    Flatten and check a USD stage · farm-wide
```

Repository tests and LoRA training run too, as the farm's built-in jobs
(`c4 farm submit ci DIR`, `c4 farm submit lora DIR`).

## 4. Get the sample shot

```sh
c4 farm samples get saltmarsh
```

This unpacks Petrel & Finch's sample shots into `~/c4-samples/saltmarsh`:
sm0140, a heron lifting off the marsh at dusk, as a USD stage with its
geometry, lights and textures.

## 5. Render it

```sh
c4 farm submit render ~/c4-samples/saltmarsh/sm0140/sm0140.usda --frames 1001-1048 --deadline +2h
```

```
sm0140-render-v001   48 frames in 48 units
  files     12 files in ~/c4-samples/saltmarsh · 1.2 MB · 12 to upload (1.2 MB)
  estimate  ready ~15:42 PDT  (p90 ~15:51 PDT)
  deadline  16:30 PDT today (23:30 UTC) · slack ~39m

Submit? [Y/n]
```

`c4 farm` named every file of the shot's folder by its C4 ID, found which
the store already holds, and asked the farm for a plan before anything ran.
The job is named for the shot, the tool and its version. The estimate is the
farm's: when the job will be ready, given everything else in the queue.
**Slack** is the time to spare between ready and due. Press Enter, and only
the files the store lacked go up.

A new farm has not timed a frame of anything yet, so its first estimate is
cautious, and may say the job will miss. It tightens as the first frames
land.

## 6. Watch it

```sh
c4 farm watch sm0140-render-v001
```

Or open the job in the HUD (`c4 farm open sm0140-render-v001`). The frames
render coarse to fine, in binary order: first and last, then the middle,
then the quarters, so the whole shot can be played early. Each frame shows
its preview as it lands.

## 7. Move the deadline

Production says the shot is for Friday's review, not this afternoon:

```sh
c4 farm deadline sm0140-render-v001 "fri 18:00"
```

There is no priority to set, on this farm or any other C4 farm. A deadline
is what changes the order: the farm re-plans the whole queue around it at
once, and every job's slack shows the result.

## 8. Take the frames home

```sh
c4 farm get sm0140-render-v001 render ~/renders/sm0140
```

```
48 files · 530 MB · every ID checked
view      c4…
in        /Users/you/renders/sm0140
```

A job's output is the whole tree it left: the shot's files, plus what it
wrote. `render` picks the frames, which land in `~/renders/sm0140/render/`;
leave it out to take everything. Every file arrives named by its C4 ID and
is checked against it, so a frame that arrived damaged is caught, not
copied. The `view` is the C4 ID of the whole output tree: the same ID on
every machine that has these frames.

## 9. Teach the farm a job of your own

A tool is a folder: a `tool.yaml` that tells the farm how to run it and what
it takes, and the script it runs. This one lists a folder of the shot and
writes a note beside it, so you can see what the farm captures.

```sh
c4 farm tool new once ~/tools/notes
```

Replace `~/tools/notes/notes.sh` with:

```sh
#!/bin/bash
# notes: list a folder of the job's tree, and leave a note beside it.
set -euo pipefail
folder="$1"
mkdir -p "$C4_TREE/notes"
ls -l "$folder" >"$C4_TREE/notes/listing.txt"
echo "looks good" >"$C4_TREE/notes/notes.txt"
echo "openjd_progress: 100"
```

and in `~/tools/notes/tool.yaml`, say where it now writes, and which files
its test should leave:

```yaml
writes: [notes/]
```

```yaml
  expect: [notes/listing.txt, notes/notes.txt]
```

Test it on your machine, then publish it to the project:

```sh
c4 farm tool test ~/tools/notes
c4 farm tool publish ~/tools/notes
```

```
as        td: publishing takes the TD role, and the local farm signs you in as it
notes v1   c41w…qu9T · 3 files, 3 to upload · image ghcr.io/avalanche-io/c4-runtime-base@sha256:6f5a…
test on the farm   saltmarsh/notes-test-1w4xvo7vmy
                   ok in 9s
published to saltmarsh: c4 farm submit notes runs it, and c4 farm tools lists it as "notes"
```

`test` checks the file, then runs the tool in Docker exactly as a unit runs
on the farm: as an unprivileged user, with no network, the tool's folder
read-only and a scratch copy of its test tree writable. `publish` stores the
folder as a C4 tree, pins its image by digest, runs the test again on the
farm, and only then offers the tool to the project as `notes v1`. No image
was built, and no cluster command was run.

On the farm, too, a unit has no network unless its tool declares one
(`needs: {network: [studio-api]}`, a network the farm admin defines). A job
sent as a bare image and command, with no tool (the HUD's `POST /jobs`, or
MCP's raw form), reaches nothing, and once its units run it says so: this
job names no tool, so it has no network: name a tool that declares one.

Now run it over the shot's geometry:

```sh
c4 farm submit notes ~/c4-samples/saltmarsh/sm0140/geo
c4 farm get sm0140-notes-v001 ~/notes
```

`~/notes` holds the shot's files plus `notes/listing.txt` and
`notes/notes.txt`. The farm mounted the shot's tree, captured what the unit
wrote as a change to it, and named the result by its ID: the job's output
is the tree it left. The node that ran the unit signed a record of it.

## 10. Read a file's history

In the HUD, open `sm0140-render-v001`, click its output view, pick a frame
and choose **Provenance**. Each step from the frame back to your upload is a
record signed by whoever did it: the node that rendered it, the farm's queue
and controller, you. Anyone holding the files can check the chain, with no
access to the farm.

## 11. Ask an agent

If you said yes at `c4 farm up`, Claude Code has the farm's tools already;
otherwise:

```sh
c4 farm integrate on
```

Then ask, for example, "which frames of sm0140-render-v001 are done, and
will it make its deadline?" The agent has your access and nothing more. The
tools it gets are listed in [docs/agents/tools.md](agents/tools.md), and
every tool you publish appears among them, as `c4_run_<tool>`, even one
published while the session runs.

## Clean up

```sh
c4 farm down
```

## Next

- The guides for each role, starting at the HUD's
  [Start here](https://github.com/Avalanche-io/c4-hud/tree/main/docs/start).
- [docs/install.md](install.md): the operator on a cluster of your own, for
  administrators.
- [DESIGN.md](../DESIGN.md): how views, tiers, isolation, the queue and
  provenance work.
