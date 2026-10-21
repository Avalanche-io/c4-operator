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

| variable                       | what it is                                                                                                                                                                      |
| ------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `C4_HUD`                       | the farm's address, for example `https://c4.petrelfinch.example`                                                                                                                |
| `C4_HUD_TOKEN_FILE`            | a file holding the artist's sign-in token, reread on each request so a renewed token takes effect (or `C4_HUD_TOKEN`)                                                           |
| `C4_PROJECT`                   | the project, for example `saltmarsh`. The artist can change it in the dialog                                                                                                    |
| `C4_ROOT`                      | the folder on this machine that is the top of the project's tree, for example `/show`. Every file a Katana project reads must be under it                                       |
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

A Render node's own outputs are never collected as another's inputs. Known
limits: the files a file names in turn are not read. A USD stage's sublayers
and references, the textures inside a look file, and a live group's own
contents must already be under the project folder (`C4_ROOT`). An expression
that names a different file on each frame is read at the first frame only.

## On Submit

Submit is available only after a check passed and nothing has changed since,
whether in the dialog or in the project. It sends only the files the farm
lacks and records them as the job's tree. It then submits one job per ticked
Render node, each `after` the ones it depends on. The summary lists the jobs
the farm made.

## Tests

```
python -m pytest -q integrations/dcc/katana
```

The tests use a fake `Katana` (`tests/fakekatana.py`, with a fake Qt in
`tests/fakekatanaqt.py`) and c4submit's fake farm, with real files. What only a
real Katana can show is in [CHECKLIST.md](CHECKLIST.md).
