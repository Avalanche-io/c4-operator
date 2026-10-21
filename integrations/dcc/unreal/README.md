# C4 for Unreal Engine

C4 is a **remote executor for the Movie Render Queue**. The artist builds the
queue as for any render and presses **Render (Remote)**. Each enabled job in
the queue becomes a job on the C4 farm. There is no other window to learn.

## Install

Unreal Engine 5.4 to 5.6.

1. Enable three plug-ins in Edit > Plugins: **Python Editor Script Plugin**,
   **Editor Scripting Utilities**, and **Movie Render Queue**. Restart.
2. Add `integrations/dcc/unreal/python` to Unreal's Python path, either in
   Project Settings > Plugins > Python > **Additional Paths**, or in
   `UE_PYTHONPATH` in the studio's launcher. Keep `integrations/dcc/c4submit`
   beside it, as it is in the repository. At startup Unreal runs that
   folder's `init_unreal.py`, which loads C4. The Output Log says "C4:
   C4FarmExecutor is ready".
3. In Project Settings > Plugins > Movie Render Pipeline, set **Default
   Remote Executor** to `C4FarmExecutor`
   (`/Engine/PythonTypes.C4FarmExecutor`).

Set the studio's environment as for the other C4 panels: `C4_HUD`,
`C4_HUD_TOKEN_FILE` (or `C4_HUD_TOKEN`), `C4_PROJECT`, and `C4_ROOT` (the
folder that is the top of the show's tree, with the project inside it). Set
`C4_SHOT`, or `C4_SHOT_PATTERN`, a regular expression whose `shot` group is
matched against the path of the project, then of each job's sequence and map.
`C4_TOOL_PATH` is optional. Set `C4_UNREAL_CONTENT=referenced` to send only
the packages the jobs reference (see below).

## What the artist sees

**Render (Remote)** opens **C4 Farm: Needed by**, a details panel with:

- **Project** and **Shot**.
- **Tool**: the farm's tools that render `.uproject` projects.
- **Needed by**: the project's milestones, or "When the farm suggests".
- **Or at a time**: "fri 18:00", "+3h".
- **Size** and **Note**.

The artist's Needed by and Note are kept for the next render in the same
session. OK first shows Unreal's own Save dialog for anything unsaved. Then
C4 checks, behind a progress dialog whose Cancel stops it. It reads the
project's files and asks the farm what it lacks and whether the submission is
good.

Next comes the summary: each job, with the farm's estimate and the slack
against Needed by, the files found and how many must be sent, and every error
with its fix.

- **Yes** submits.
- **No** shows the submission as the file it is. From there, Yes saves it as
  `Saved/C4/<shot>-<job>.yaml`, to submit later with `c4 farm submit FILE`,
  and No goes back to the summary. The whole file is also in the Output Log.
- **Cancel** sends nothing.

When the check finds errors, there is no Submit: the artist fixes them and
renders again. Unreal's message boxes have fixed buttons, so each dialog's
last lines say what Yes, No and Cancel do.

## What a job is

Each enabled queue job is one farm job. Its frames are the job's custom range
when its Output settings set one, else its level sequence's playback range.
Its tool fields come from the job itself:

- `map`: the map's package (`/Game/Maps/Sh010`).
- `sequence`: the level sequence (`/Game/Cinematics/sh010_seq.sh010_seq`).
- `config`: the job's render settings.

The render settings are written for every submission to
`C4/queue/<nn>-<job>.utxt`, as a Movie Render Queue manifest holding that one
job, which the farm passes to `-MoviePipelineConfig`. The path is from the
project's folder. Add `C4/` to the project's ignore list in source control.

A typical tool is `unreal-render`. Its `project` is a PATH taken from the
scene file (`*.uproject`), and its `map`, `sequence` and `config` are STRING
fields taken from the jobs.

## What is sent

The project is the scene. C4 sends the `.uproject`, `Config/`, `Content/` and
the project's `Plugins/`, never `Saved/`, `Intermediate/`,
`DerivedDataCache/` or `Binaries/`. C4 names every file by its content, so a
second submission uploads only the packages that changed since the first.
The first upload of a large project is the expensive one.

With `C4_UNREAL_CONTENT=referenced`, C4 sends only the packages the asset
registry lists as reached from each job's map and sequence. That includes:

- a World Partition map's external actors and objects, and its built lighting;
- the packages the project's config names (the game mode, default maps);
- every file in `Content/` that is not a package (movies, data).

Each package goes with the jobs that reach it. A package the registry names
but that is not on disk is a warning. If the registry fails, all of Content is
sent. Whatever is loaded by a name in code and nowhere else is missed, which
is why sending all of Content is the default.

## Known limits

- A C++ project's `Binaries/` are never sent, so the farm's Unreal must build
  the project's code or already have it. The summary says so under "Worth
  knowing" when the project or a plug-in has C++ modules, and also when the
  asset registry failed and all of Content was sent.
- The tool's own fields, beyond those it takes from the jobs, keep their
  defaults. A job's render settings belong in its queue config.
- The summary dialogs are Unreal's message boxes. Long summaries scroll in the
  Output Log, not in the box.

## For TDs: the tool

Render (Remote) submits to a tool named `unreal-render`, and
[`tools/unreal-render/`](tools/unreal-render) is that tool. Unreal is not
free to ship in an image, so its `runs.image` is a placeholder,
`registry.example.com/unreal:5.5`: set it to the studio's own Unreal image
(Linux, with the engine built and a GPU driver; images made the way
ue4-docker makes them keep the engine at `/home/ue4/UnrealEngine`), set
`runs.exe` to that image's
`Engine/Binaries/ThirdParty/Python3/Linux/bin/python3`, then:

```sh
c4 farm tool test integrations/dcc/unreal/tools/unreal-render
c4 farm tool publish integrations/dcc/unreal/tools/unreal-render
```

`runs.host` (`has: [unreal-5.5]`, with the Windows and Linux paths of
Unreal's own Python) is there for studios that render on their Windows
workstations; set the paths to where Unreal is installed. Until the farm can
reach hosts, units run in the image.

**A unit is a whole job** (`frames: {chunk: all}`). Movie Render Queue renders
a job's range in one process, and each start costs minutes (the engine,
shader compiles, loading the map, the job's warm-up frames), so splitting a
job's frames over several Unreals would pay that start for each. A
submission's frames still decide what is rendered.

Each unit runs `unreal_render.py` with Unreal's own Python (standard library
only), which finds `UnrealEditor-Cmd` beside it (or at `UNREAL_EDITOR`) and:

1. reads the job's queue manifest (`config`, from the project's folder) and
   refuses one that does not render the job's `map`;
2. writes a copy under the project's `Saved/MovieRenderPipeline/C4/`, with
   the Output settings' custom range set to the unit's frames and the output
   folder set to `render/<job>/` at the top of the job's tree (the job's own
   file name format is kept);
3. starts Unreal as its own Render (Local, new process) does: the project,
   `MoviePipelineEntryMap` with the Movie Pipeline game mode, `-game
   -MoviePipelineConfig=MovieRenderPipeline/C4/<unit>.utxt`, offscreen
   (`-RenderOffscreen`), unattended, logging to its output;
4. passes Unreal's log through, and prints `c4_frame` for each frame once a
   later frame's file is written (or Unreal has exited), with
   `openjd_progress`; a frame Unreal did not write fails the unit, to be
   retried.

A job whose config is a Movie Render Graph has no Output settings to set,
and is refused for now. Frames with gaps (1001-1010, 1020-1030) are refused
too: Movie Render Queue renders one range, every frame or every Nth.

The test project (`test/Test/`) holds only a `.uproject`. Its job's config is
a `.py`, which the wrapper runs in the editor (`-run=pythonscript`) to build a
map, a level sequence over 1001-1003 and the job's manifest, then renders
two frames at 64x36. So the test needs only the studio's image, not a
project of its own.

## Tests

```
python -m pytest -q integrations/dcc/unreal
```

The tests use a fake `unreal` (`tests/fakeunreal.py`) and c4submit's fake
farm, with a project of real files. What only a real Unreal can show is in
[CHECKLIST.md](CHECKLIST.md).
