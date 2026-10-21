# C4 for Blender

The **C4 Farm** panel, in Render Properties, submits the open Blender file to a
C4 farm from inside Blender, with no terminal. Each ticked view layer is a
job. A scene whose compositor reads another scene's render waits for it.

## Install

Blender 4.2 LTS and later. The add-on is `addons/c4_blender`. It needs
`c4submit`, the library under every C4 panel, and finds it in either of two
places:

1. **From the repository.** In Preferences > File Paths > Script
   Directories, add `integrations/dcc/blender` (the folder holding
   `addons/`), restart Blender, and enable **C4 Farm** under Preferences >
   Add-ons. The add-on finds `c4submit` beside it, in
   `integrations/dcc/c4submit/src`, so keep the repository's layout.
2. **From one zip.** Run `python integrations/dcc/blender/build_addon.py`
   (standard library only). It writes `dist/c4_blender-0.1.0.zip`, with a
   copy of `c4submit` inside the add-on (`c4_blender/vendor`). In
   Preferences > Add-ons, choose **Install from Disk...**, pick the zip, and
   enable **C4 Farm**. The add-on uses its own copy first.

Set these in the studio's environment, as for other pipeline tools:

| variable                       | what it is                                                                                                                                                            |
| ------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `C4_HUD`                       | the farm's address, for example `https://c4.petrelfinch.example`                                                                                                      |
| `C4_HUD_TOKEN_FILE`            | a file holding the artist's sign-in token, read on each request so a renewed token takes effect (or `C4_HUD_TOKEN`)                                                   |
| `C4_PROJECT`                   | the project, for example `saltmarsh`. The artist can change it in the panel                                                                                           |
| `C4_ROOT`                      | the folder on this machine that is the top of the project's tree, for example `/show`. Every file a scene reads must be under it                                      |
| `C4_SHOT` or `C4_SHOT_PATTERN` | the shot, or a regular expression whose `shot` group finds it in the file's path, for example `/(?P<shot>sh\d{3})/`                                                   |
| `C4_TOOL_PATH`                 | optional: folders of tools (each holding a `tool.yaml`), used when the farm does not list its tools yet, so the panel can still draw the form and save the submission |

## What the artist sees

Properties > Render > **C4 Farm**, top to bottom:

- **Tool**: the farm's tools that render `.blend` files.
- **Project** and **Shot**, from the studio's environment.
- **Needed by**: the project's milestones (Dailies, Client review),
  **When the farm suggests**, or **At a time I type** ("fri 18:00", "+3h").
  This is the only urgency the artist sets.
- **Frames**, linked to each view layer's scene range until the artist types
  into it.
- **View layers**: one checkbox per view layer with "Use for Rendering" on.
  The panel's scene's view layers are ticked. Other scenes' view layers are
  listed unticked, with the scene they belong to and what they come after.
- **Size**, when the tool has sizes, and **Note**.
- **The tool's own parameters**, drawn from its definition. A value taken
  from the file has a link toggle beside it. Typing into the field unlinks
  it. Advanced parameters are in a folded **Advanced** section.
- **Check**, **Submit**, **Show file**, **Save file...**, and the summary.

The panel's choices are saved with the file. Each job gives the tool's
`scene.renderables` parameters its own values. A parameter named `scene` gets
the view layer's scene, and one named `view_layer` gets the view layer. Any
other gets both together, as `Scene/ViewLayer`. A `scene.camera` parameter gets
that scene's camera.

**Check** saves the file. It then reads every file the ticked view layers need
(textures, with `<UDIM>` tile sets and sequences over the frames they are
read; linked libraries; Alembic and USD caches; OpenVDB sequences; fonts;
sound; the sequencer's strips; cache folders) and asks the farm what it lacks
and whether the submission is good. This runs on a worker thread, with
progress in the panel and the cursor. Esc or **Cancel** stops it. The summary
shows each job, the files found and how many must be sent, the estimate, and
the slack against Needed by. Errors come with their fix. Packed files travel
inside the `.blend`. A missing file that no render node uses (a reference
picture, a sound) is a warning, not an error.

**Show file** shows the submission as the file it is, in place of the summary,
with a button that copies it. **Save file...** saves it, to submit later with
`c4 farm submit FILE`.

If the farm lists no tools for the project, the panel says so and draws no
form. Setting `C4_TOOL_PATH` to the studio's tool folders lets it draw the form
and save the file. Check then reads the files and checks the file locally, and
the submission waits until the farm takes it.

## On Submit

Submit is available only after a check passed and nothing has changed since,
whether in the panel or in the file (Blender's unsaved-changes mark counts).
It sends only the files the farm lacks and records them as the job's tree. It
then submits one job per ticked view layer, each `after` the view layers whose
renders it reads. The summary lists the jobs the farm made. Submitting the
same file again sends nothing new.

## Known limits

- A texture sequence or OpenVDB sequence is collected over the frames its
  users read across the panel's scene's frame range, not per job. When the
  ticked view layers' scenes, or a typed Frames, cover fewer frames, the
  panel sends more files than the jobs read. That is the safe side; nothing
  a job needs is left out.

## For TDs: the tool

The panel submits to a tool named `blender-render`, and
[`tools/blender-render/`](tools/blender-render) is that tool, ready to
publish. It runs in `c4/job-sim`, the farm's own Blender image (Blender 5.2
LTS, Cycles on the CPU, no GPU), so a farm needs no image of its own to
render Blender files:

```sh
c4 farm tool test integrations/dcc/blender/tools/blender-render
c4 farm tool publish integrations/dcc/blender/tools/blender-render
```

The test builds a two-view-layer scene from `test/shot/scene.py` and renders
two frames of one view layer. For GPUs or another Blender, copy the folder,
set `runs.image` to the studio's image (with `blender` on its `PATH`, or
`BLENDER` set to it), add `needs` from a measured run (`gpu: 1` for GPU
Cycles or EEVEE), and publish the copy. A file saved in a newer Blender than
the image's may not open; keep the image's Blender at or above the artists'.

Each unit runs `blender -b FILE --python-exit-code 1 --python render.py`,
which:

- renders the job's `scene` and only its `view_layer` (the others are turned
  off for the run; the file is not saved), with `camera`, `engine` (`file`
  keeps the scene's) and `samples` (0 keeps the file's) when given;
- renders each of the unit's frames, and prints `c4_frame` with the file's
  path once it is on disk;
- refuses, as a permanent failure, a scene, view layer or camera the file
  does not have, naming the ones it has.

Frames go where the scene's output path says, from the file's folder, with
two changes. When the scene has more than one view layer to render, each
goes in a folder of its name (`render/bg/sh010_1001.exr`), so the jobs of
one scene do not write the same files. An output outside the job's tree goes
to `render/` at its top.

A tool's `.py` file in place of the `.blend` is run by Blender first to build
the `.blend` beside it, which is how the test needs no binary file in git.

Not handled yet: a compositor's File Output nodes write where the file says,
without a folder per view layer; a scene whose compositor combines several
view layers renders with only the job's on. Python in the file (drivers,
scripts) runs only as far as Blender allows without auto-run, as on an
artist's machine with Blender's defaults.

## Tests

```
python -m pytest -q integrations/dcc/blender
```

The tests use a fake `bpy` (`tests/fakebpy.py`) and c4submit's fake farm, with
real files. What only a real Blender can show is in
[CHECKLIST.md](CHECKLIST.md).
