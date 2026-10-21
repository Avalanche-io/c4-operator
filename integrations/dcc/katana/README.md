# C4 for Katana

C4 as a Katana farm plug-in. Artists submit a Katana project to a C4 farm from
Katana's own farm menus, with no terminal. Each Render node is a job. A Render
node that depends on another (it reads its render) waits for it.

## Install

Add this folder to `KATANA_RESOURCES`, and keep `../c4submit` beside it, as it
is in the repository. At start-up Katana runs `Startup/init.py`. That script
puts `python/` and c4submit on the path, and registers the farm menu entries
once Katana has started. A studio with its own start-up script can instead put
both folders on `PYTHONPATH` and call:

```python
import c4_katana.farm
c4_katana.farm.install()
```

Set these in the studio's environment:

| variable                       | what it is                                                                                                                                                                       |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `C4_HUD`                       | the farm's address, for example `https://c4.petrelfinch.example`                                                                                                                 |
| `C4_HUD_TOKEN_FILE`            | a file holding the artist's sign-in token, reread on each request so a renewed token takes effect (or `C4_HUD_TOKEN`)                                                            |
| `C4_PROJECT`                   | the project, for example `saltmarsh`. The artist can change it in the dialog                                                                                                     |
| `C4_ROOT`                      | the folder on this machine that is the top of the project's tree, for example `/show`. Every file a Katana project reads must be under it                                        |
| `C4_SHOT` or `C4_SHOT_PATTERN` | the shot, or a regular expression whose `shot` group finds it in the project file's path, for example `/(?P<shot>sm\d{4})/`                                                      |
| `C4_TOOL_PATH`                 | optional: folders of tools (each holding a `tool.yaml`), used when the farm does not list its tools yet, so the dialog can still draw the form and save the submission as a file |

Katana 7 and 8 are supported.

## What the artist sees

The farm menu has **Submit to C4...**, which opens the dialog with every Render
node ticked. A Render node's farm menu has **Submit this Render node to
C4...**, which ticks only that node. Top to bottom:

- **Tool**: the farm's tools that render Katana projects (`.katana`).
- **Project** and **Shot**.
- **Needed by**: the project's milestones (Dailies, Client review),
  **When the farm suggests**, or **At a time I type** ("fri 18:00", "+3h").
  This is the only urgency the artist sets.
- **Frames**: linked to each Render node's own range (or the project's) until
  the artist types into it.
- **Render nodes**: one checkbox per Render node that is not bypassed, with its
  range and what it comes after.
- **Size**, when the tool has sizes, and **Note**.
- **The tool's own parameters**, drawn from its definition (the project file,
  the Render node). A value taken from the project has a **linked** box beside
  it. Advanced ones are folded away.
- **Check**, **Submit**, **Show file**, **Save file...**, and the summary.

**Check** saves the project. It then reads every file the ticked Render nodes
need, asks the farm what it lacks, and asks the farm to check the submission.
All of this runs behind a progress dialog whose Cancel stops it. The summary
shows each job and what it waits for, the files found and how many must be
sent, the estimate, and the slack against Needed by. A job that waits for
another gets its estimate once that one exists. Errors come with their fix.
**Show file** shows the submission as a file. **Save file...** saves it, to
submit later with `c4 farm submit FILE`.

## What is collected

Every file a node's parameters name, on nodes upstream of a ticked Render
node:

- Alembic caches, USD stages, look files and live groups;
- textures on shading nodes (`<UDIM>`, `_MAPID_` and frame tokens are
  expanded);
- any parameter with a file widget, or whose value is a file's path.

A Render node's own outputs are never collected as another's inputs. A USD
stage (a UsdIn's file, or any parameter naming a `.usd`, `.usda` or
`.usdc`) is followed into what it reads in turn: its sublayers, references,
payloads and textures, collected for the Render nodes downstream of the
node that reads it. Katana carries USD's Python, which does this exactly.
Without it, text layers are read directly, and a binary layer is sent with
a warning that what it reads was not collected. Known limits: the textures
inside a look file and a live group's own contents are not read, so they
must already be under the project folder (`C4_ROOT`).

A file parameter given by an expression that names a sequence is sent for
the frames of the jobs that read it: an expression whose value holds frame
tokens (`fog.%04d.vdb`, `####`), or one that names this frame's file and
the next frame's in turn (`smoke.1001.abc`, `smoke.1002.abc`). Any other
expression (one that adds an offset to the frame, say) is read at the
first frame only.

## On Submit

Submit is available only after a check passed and nothing has changed since,
whether in the dialog or in the project. It sends only the files the farm
lacks and records them as the job's tree. It then submits one job per ticked
Render node, each `after` the ones it depends on. The summary lists the jobs
the farm made.

## For TDs: the tool

The dialog offers the farm's tools that take a `.katana`.
`tools/katana-render/` is one to start from: it renders one Render node of
the saved project over each unit's frames with Katana's batch render.
Python 3 runs `katana_render.py`, which runs:

```
katana --batch --katana-file=<project> --render-node=<node> -t <start>-<end>
```

Katana is `$KATANA_ROOT/katana`, else `katana` on the PATH. The script
passes Katana's log through and reports each frame with `c4_frame: <frame>
<image>` once its image is written: from the log where it names the file,
and from the new images under the job's tree when the batch ends. A frame
with no image fails the unit, to be retried; a missing project or Render
node fails it for good. When the artist's project folder is somewhere else
on their machine, the script renders a copy of the project with the farm's
paths in place of theirs, beside it, and removes it after.

| parameter | from                | what it is                                                           |
| --------- | ------------------- | -------------------------------------------------------------------- |
| `scene`   | `scene.file`        | the saved `.katana`                                                  |
| `node`    | `scene.renderables` | the Render node's name, each one ticked a job                        |
| `threads` | (advanced)          | the renderer's threads (`--threads3d`); 0 leaves them to the project |

Before publishing:

1. Copy the folder and set `runs.image` to the studio's own Katana image,
   with its renderer's plug-in and Python 3 (`registry.example.com/katana:8.0`
   is a placeholder). If the studio renders on its own machines, set
   `runs.host.has` to what they advertise.
2. Measure a typical shot and set `needs` (`cpu: 16, memory: 64G` is a
   ceiling, not a measurement).
3. Try it on your machine, then publish it to the project:

   ```sh
   c4 farm tool test katana-render
   c4 farm tool publish katana-render
   ```

The test's tree holds `katana/build_project.py`, not a project: Katana's
script mode runs it to build a small project (a sphere and a camera,
rendered with 3Delight to `render/test.####.exr`), which is then rendered
for frames 1001-1002. A unit runs with no network, so Katana and its
renderer must find their licences without one until the farm counts
licence seats; today the farm refuses a tool that declares licences. What
only a real Katana can confirm is in [CHECKLIST.md](CHECKLIST.md).

## Tests

```
python -m pytest -q integrations/dcc/katana
```

The tests use a fake `Katana` (`tests/fakekatana.py`, with a fake Qt in
`tests/fakekatanaqt.py`) and c4submit's fake farm, with real files. What only a
real Katana can show is in [CHECKLIST.md](CHECKLIST.md).
