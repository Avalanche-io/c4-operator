# Quickstart: a farm on your machine, a render, and your first tool

This builds a complete C4 farm inside Docker, renders a sample shot on it,
watches the render in the HUD, moves its deadline, takes the frames home with
every file checked, and then teaches the farm a small job of your own. No
Kubernetes command or field appears anywhere: the farm's administrators
have [docs/install.md](install.md) for that.

You need Docker Desktop running, with at least 6 GB of memory and 4 CPUs
(Settings → Resources).

## 1. Install `c4 farm`

```sh
brew install avalanche-io/tap/c4-farm
```

This installs `c4-farm`, or take a binary for your machine from the
[release page](https://github.com/Avalanche-io/c4-operator/releases). If you
have the `c4` command, `c4 farm` runs it too; this guide writes it that way.

## 2. Start the farm

```sh
c4 farm up
```

```
farm      c4-local   4 nodes · 6 slots, 4 more to rent · store ready
project   saltmarsh   you: artist
HUD       http://127.0.0.1:30080/?as=artist
roles     artist, supervisor, wrangler, admin, facilities, td (c4 farm up --as ROLE opens another)
```

That is a farm, not a demo of one: render nodes in two pools (`cpu` and
`gpu`), a burst pool the farm rents from only when a deadline needs it, a
store that names every file by its C4 ID, a project (`saltmarsh`, Petrel &
Finch's feature) and a sign-in for every role. The HUD opens in your
browser. The prebuilt tools (render, comp, transcode, usd, sim, ci, lora and
infer) are published to every project.

```sh
c4 farm tools
```

## 3. Get the sample shot

```sh
c4 farm samples get saltmarsh
```

This unpacks Petrel & Finch's sample shots into `~/c4-samples/saltmarsh`:
sm0140, a heron lifting off the marsh at dusk, as a USD stage with its
geometry, lights and textures.

## 4. Render it

```sh
c4 farm submit render ~/c4-samples/saltmarsh/sm0140/sm0140.usda --frames 1001-1048 --deadline +2h
```

```
sm0140-light-v001   render · 48 frames · Cycles · pool cpu

  files     12 files found · 1.2 MB · 12 to upload (1.2 MB)
  estimate  ready ~15:42 PDT  (p90 ~15:51 PDT)
  deadline  16:30 PDT today (23:30 UTC) · slack ~39m

Submit? [Y/n]
```

`c4 farm` named every file of the shot's folder by its C4 ID, found which
the store already holds, and asked the farm for a plan before anything ran.
The estimate is the farm's: when the job will be ready, given everything
else in the queue. **Slack** is the time to spare between ready and due.
Press Enter, and only the files the store lacked go up.

## 5. Watch it

```sh
c4 farm watch sm0140-light-v001
```

Or open the job in the HUD (`c4 farm open sm0140-light-v001`). The frames
render coarse to fine, in binary order: first and last, then the middle,
then the quarters, so the whole shot can be played early. Each frame shows
its preview as it lands.

## 6. Move the deadline

Production says the shot is for Friday's review, not this afternoon:

```sh
c4 farm deadline sm0140-light-v001 "fri 18:00"
```

There is no priority to set, on this farm or any other C4 farm. A deadline
is what changes the order: the farm re-plans the whole queue around it at
once, and every job's slack shows the result.

## 7. Take the frames home

```sh
c4 farm get sm0140-light-v001 ~/renders/sm0140
```

```
48 files · 46 MB · every ID checked
view      c4…
in        /Users/you/renders/sm0140
```

Every file arrives named by its C4 ID and is checked against it, so a frame
that arrived damaged is caught, not copied. The `view` is the C4 ID of the
whole output tree: the same ID on every machine that has these frames.

## 8. Teach the farm a job of your own

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

and set `expect` in `~/tools/notes/tool.yaml` to the files it now writes:

```yaml
  expect: [notes/listing.txt, notes/notes.txt]
```

Test it on your machine, then publish it to the project:

```sh
c4 farm tool test ~/tools/notes
c4 farm tool publish ~/tools/notes
```

`test` checks the file, then runs the tool in Docker exactly as a unit runs
on the farm: as an unprivileged user, with no network, the tool's folder
read-only and a scratch copy of its test tree writable. `publish` stores the
folder as a C4 tree (its ID is the tool's version), runs the test again on
the farm, and only then offers the tool to the project. No image was built,
and no cluster command was run.

Now run it over the shot's geometry:

```sh
c4 farm submit notes ~/c4-samples/saltmarsh/sm0140/geo
c4 farm get sm0140-notes-v001 ~/notes
```

`~/notes` holds the shot's files plus `notes/listing.txt` and
`notes/notes.txt`. The farm mounted the shot's tree, captured what the unit
wrote as a change to it, and named the result by its ID: the job's output
is the tree it left. The node that ran the unit signed a record of it.

## 9. Read a file's history

In the HUD, open `sm0140-light-v001`, click its output view, pick a frame
and choose **Provenance**. Each step from the frame back to your upload is a
record signed by whoever did it: the node that rendered it, the farm's queue
and controller, you. Anyone holding the files can check the chain, with no
access to the farm.

## 10. Ask an agent

```sh
claude mcp add c4-farm -- c4 farm mcp
```

Then ask, for example, "which frames of sm0140-light-v001 are done, and
will it make its deadline?" The agent has your access and nothing more. The
tools it gets are listed in [docs/agents/tools.md](agents/tools.md), and
every tool you publish appears among them, as `c4_run_<tool>`.

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
