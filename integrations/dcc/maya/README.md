# C4 for Maya

The window artists use to submit a Maya scene to a C4 farm, from inside Maya.
It needs no terminal. Each renderable render layer is a job.

## Install

Either install it as a Maya module, or have the studio's own `userSetup.py`
load it.

**As a module.** Put a module file such as `c4.mod` in a folder on
`MAYA_MODULE_PATH`. Point it at this folder, and keep `../c4submit` beside
it, as it is in the repository:

```
+ c4 0.1.0 /studio/pipeline/c4/integrations/dcc/maya
PYTHONPATH +:= ../c4submit/src
```

Maya then puts `scripts/` on its Python path and runs `scripts/userSetup.py`
at startup. That file adds the **C4** menu once the main window exists. It
also finds c4submit beside this folder if the module file did not add it.

**From the studio's `userSetup.py`.** Put `integrations/dcc/maya/scripts` and
`integrations/dcc/c4submit/src` on `PYTHONPATH`, then:

```python
import c4_maya.menu
c4_maya.menu.install()
```

Set these in the studio's environment:

| variable                       | what it is                                                                                                                                                                     |
| ------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `C4_HUD`                       | the farm's address, for example `https://c4.petrelfinch.example`                                                                                                               |
| `C4_HUD_TOKEN_FILE`            | a file holding the artist's sign-in token, reread on each request so a renewed token takes effect (or `C4_HUD_TOKEN`)                                                          |
| `C4_PROJECT`                   | the project, for example `saltmarsh`. The artist can change it in the window                                                                                                   |
| `C4_ROOT`                      | the folder on this machine that is the top of the project's tree, for example `/show`. Every file a scene reads must be under it                                               |
| `C4_SHOT` or `C4_SHOT_PATTERN` | the shot, or a regular expression whose `shot` group finds it in the scene's path, for example `/(?P<shot>sm\d{4})/`                                                            |
| `C4_TOOL_PATH`                 | optional: folders of tools (each holding a `tool.yaml`), used when the farm does not list its tools yet, so the window can still draw the form and save the submission as a file |

Maya 2024, 2025 and 2026 are supported (Python 3.10 and 3.11). Batch Maya gets
no menu.

## What the artist sees

**C4 > Submit to farm...** opens the window. Top to bottom:

- **Tool**: the farm's tools that render Maya scenes (`.ma`, `.mb`).
- **Project** and **Shot**.
- **Needed by**: the project's milestones (Dailies, Client review),
  **When the farm suggests**, or **At a time I type** ("fri 18:00", "+3h").
  This is the only urgency the artist sets.
- **Frames**: linked to each render layer's own range (or the scene's) until
  the artist types into it.
- **Render layers**: one checkbox per renderable layer, with its range. The
  master layer is listed when it is renderable. Layers that are not
  renderable are not listed.
- **Size**, when the tool has sizes, and **Note**.
- **The tool's own parameters**, drawn from its definition (the scene, the
  render layer, the camera, the renderer). A value taken from the scene has
  a **linked** box beside it. Advanced ones are folded away.
- **Check**, **Submit**, **Show file**, **Save file...**, and the summary.

**Check** saves the scene. It then reads every file the ticked layers need:
textures with their `<UDIM>` tiles and frame sequences, references, Arnold
standins, volumes and images, and Alembic and GPU caches. It asks the farm
what it lacks and asks the farm to check the submission. All of this runs
behind Maya's progress window, which Esc cancels. The summary shows each job,
the files found and how many must be sent, the estimate, and the slack against
Needed by. Errors come with their fix. **Show file** shows the submission as a
file. **Save file...** saves it, to submit later with `c4 farm submit FILE`.

A file that only objects in some layers use is collected only when one of
those layers is ticked. References and Alembic caches are read whenever the
scene opens, so every layer collects them.

If the farm lists no tools for the project, the window says so and draws no
form. Setting `C4_TOOL_PATH` to the studio's tool folders lets it draw the form
and save the file. Check then reads the files and checks the file locally,
and the submission waits until the farm takes it.

## On Submit

Submit is available only after a check passed and nothing has changed since,
whether in the window or in the scene (an edit, or a save). It sends only the
files the farm lacks and records them as the job's tree. It then submits one
job per ticked render layer, each with its own layer and frames. The summary
lists the jobs the farm made. Submitting the same scene again sends nothing
new.

## Tests

```
python -m pytest -q integrations/dcc/maya
```

The tests use a fake `maya` (`tests/fakemaya.py`) and c4submit's fake farm,
with real files. What only a real Maya can show is in
[CHECKLIST.md](CHECKLIST.md).
