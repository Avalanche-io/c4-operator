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

| variable                       | what it is                                                                                                                                                                       |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `C4_HUD`                       | the farm's address, for example `https://c4.petrelfinch.example`                                                                                                                 |
| `C4_HUD_TOKEN_FILE`            | a file holding the artist's sign-in token, reread on each request so a renewed token takes effect (or `C4_HUD_TOKEN`)                                                            |
| `C4_PROJECT`                   | the project, for example `saltmarsh`. The artist can change it in the window                                                                                                     |
| `C4_ROOT`                      | the folder on this machine that is the top of the project's tree, for example `/show`. Every file a scene reads must be under it                                                 |
| `C4_SHOT` or `C4_SHOT_PATTERN` | the shot, or a regular expression whose `shot` group finds it in the scene's path, for example `/(?P<shot>sm\d{4})/`                                                             |
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

A USD stage the scene reads (a Maya USD proxy shape's file, a reference to
a `.usd`, an Arnold standin on one) is followed into what it reads in turn:
its sublayers, references, payloads and textures, collected for the layers
that read the stage. Maya's USD plug-in carries USD's Python, which does
this exactly. Without it, text layers are read directly, and a binary layer
is sent with a warning that what it reads was not collected.

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

## For TDs: the tool

The window offers the farm's tools that take a `.ma` or `.mb`.
`tools/maya-render/` is one to start from: it renders one render layer of
the saved scene over each unit's frames with Maya's command-line renderer.
`mayapy` runs `maya_render.py`, which runs `Render` from beside it:

```
Render -r <renderer> -proj <project> -s <start> -e <end> -b <step> -rl <layer> -cam <camera> <scene>
```

The project is the nearest folder above the scene with a `workspace.mel`.
The script passes `Render`'s log through and reports each frame with
`c4_frame: <frame> <image>` once its image is written: from the renderer's
log where it names the file (Arnold, asked for `-ai:lve 2`), and from the
new images under the job's tree when `Render` ends. A frame with no image
fails the unit, to be retried; a missing scene fails it for good. When the
artist's project folder is somewhere else on their machine, the farm's path
map is applied with Maya's `dirmap`.

| parameter  | from                | what it is                                                                     |
| ---------- | ------------------- | ------------------------------------------------------------------------------ |
| `scene`    | `scene.file`        | the saved scene                                                                |
| `layer`    | `scene.renderables` | the layer's node (`rs_beauty`, `defaultRenderLayer`), each one ticked a job    |
| `camera`   | `scene.camera`      | each layer's renderable camera, as the window reads it; empty, the layer's own |
| `renderer` | (advanced)          | `file` (the scene's own), `arnold`, `redshift`, `vray`, `renderman`, `sw`      |

Before publishing:

1. Copy the folder and set `runs.image` to the studio's own Maya image,
   with its renderer's plug-in (`registry.example.com/maya:2025` is a
   placeholder). If the studio renders on its own machines, set
   `runs.host.has` to what they advertise and `runs.host.exe` to their
   `mayapy`.
2. Measure a typical shot and set `needs` (`cpu: 16, memory: 64G` is a
   ceiling, not a measurement).
3. Try it on your machine, then publish it to the project:

   ```sh
   c4 farm tool test maya-render
   c4 farm tool publish maya-render
   ```

The test renders `test/maya/scenes/test.ma` (a cube, Maya Software) for
frames 1-2. A unit runs with no network, so Maya and its renderer must find
their licences without one until the farm counts licence seats; today the
farm refuses a tool that declares licences. What only a real Maya can
confirm is in [CHECKLIST.md](CHECKLIST.md).

## Tests

```
python -m pytest -q integrations/dcc/maya
```

The tests use a fake `maya` (`tests/fakemaya.py`) and c4submit's fake farm,
with real files. What only a real Maya can show is in
[CHECKLIST.md](CHECKLIST.md).
