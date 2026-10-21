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

## Tests

```
python -m pytest -q integrations/dcc/unreal
```

The tests use a fake `unreal` (`tests/fakeunreal.py`) and c4submit's fake
farm, with a project of real files. What only a real Unreal can show is in
[CHECKLIST.md](CHECKLIST.md).
