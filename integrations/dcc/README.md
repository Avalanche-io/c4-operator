# DCC submitters

Submit to a C4 farm from inside the application you work in, the way you
submit to any render queue: pick what to render, check the frames, say when
it is needed, see when it will be ready, press Submit. No terminal, and no
Kubernetes anywhere.

| folder      | what                                                                                  |
| ----------- | ------------------------------------------------------------------------------------- |
| `c4submit/` | the shared Python library every panel uses (standard library only, Python 3.9 and up) |
| `nuke/`     | the Nuke panel: each Write node is a job                                              |
| `houdini/`  | the Solaris C4 Render LOP and the C4 Submit ROP                                       |
| `maya/`     | the Maya window: each renderable render layer is a job                                |
| `blender/`  | the Blender add-on: a C4 Farm panel in Render Properties; each view layer is a job    |
| `katana/`   | the Katana farm plug-in, on Katana's farm menus: each Render node is a job            |
| `unreal/`   | the Movie Render Queue's remote executor (Render (Remote)); each queue job is a job   |

Each panel has a `README.md` (install and use) and a `CHECKLIST.md` (what to
check by hand in the real application). Their tests run against fakes of
each application's Python API, so CI needs no licences:

```sh
pip install pytest pyyaml
python -m pytest integrations/dcc
```

## What a panel does

In short:

1. **The form comes from the tool.** A published tool's parameters (Open
   Job Description's parameter definitions, plus C4's `show` and `from`)
   draw the panel's fields. A field with `from: scene.file`,
   `scene.renderables`, `scene.frames` or `scene.selection` is filled in
   from the open scene and shows a link toggle; typing into it unlinks it.
   A panel offers the tools that take the open scene's kind of file.
2. **"Needed by", never priority.** The choices are the project's
   milestones (tonight's dailies, Friday's client review), the farm's
   suggestion, or a typed time (`fri 18:00`, `+3h`).
3. **Collect.** The panel lists every file the scene reads (Reads, USD
   layers and references, textures with `<UDIM>`, `$F4` and `####`
   sequences) under the project folder (`C4_ROOT`), names each by its C4
   ID, and computes the ID of the tree they make, exactly as the farm will
   (`c4-operator tree-id`). IDs are cached by path, size and time, so a
   second submission of the same shot hashes only what changed.
4. **Check.** It asks the farm which files it lacks and asks it to check the
   submission (`POST /submissions` with `dryRun`). Nothing is created or
   uploaded. The panel shows what will run, the estimate and the slack
   against the due time, licence seats, and every error with its fix:

   ```
     files      98 found from sm0140_comp_v004.nk · 6.2 GB · 12 to upload (240 MB)
   sm0140-comp-bg-v004   Nuke comp · 48 frames · 12 units
     licences   foundry/nuke_r, 1 per unit · 6 of 12 seats free now
     estimate   ready 16:20 PDT (23:20 UTC), p90 16:31 PDT
     due        18:00 PDT (01:00 UTC) · dailies · slack 1h 29m
   sm0140-comp-main-v004   Nuke comp · 48 frames · 12 units · after comp-bg
     ...
   ```

5. **Submit.** It uploads only what the farm lacks, records the tree
   (checking the farm names it as the panel did) and submits. The farm
   makes one job per job in the file.
6. **Show file / Save file.** The submission is a small YAML file
   (`c4: submit/1`), the same one `c4 farm submit FILE` takes, so a panel's
   submission can be kept, diffed and sent again by a script or an agent.

## Configuration

| variable                              | meaning                                                                                                                                                                          |
| ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `C4_HUD`                              | the farm's HUD URL                                                                                                                                                               |
| `C4_HUD_TOKEN_FILE` or `C4_HUD_TOKEN` | your sign-in token (the file is reread on every request)                                                                                                                         |
| `C4_HUD_AS`                           | a development viewer, for a local farm made with `c4 farm up` (a studio's farm refuses it)                                                                                       |
| `C4_PROJECT`                          | the project                                                                                                                                                                      |
| `C4_ROOT`                             | the folder on this machine that is the top of the project's tree (the farm mounts it as `/show` or the project's root path); default the project's root path when it exists here |
| `C4_SHOT`, or `SHOT`                  | the shot                                                                                                                                                                         |
| `C4_SHOT_PATTERN`                     | a regular expression with a `shot` group, matched against the scene's path, when no variable names the shot                                                                      |
| `C4_TOOL_PATH`                        | folders of tool folders (`<name>/tool.yaml`) a panel offers when the farm does not list tools yet, so it can still draw its form and save the file                               |
| `C4SUBMIT_CACHE`                      | where file IDs are cached (default `~/.cache/c4submit/ids.json`)                                                                                                                 |

A studio sets these in its launcher, as it does for other pipeline tools.
Without `C4_HUD`, a panel talks to the farm `c4 farm` does: the current
farm in `~/.c4/farm.yaml` (or `C4_FARM_CONFIG`; `C4_FARM` picks another),
with its sign-in and its default project. So an artist who has run
`c4 farm login` or `c4 farm up` sets nothing more.

## Writing a panel for another DCC

A panel is a `c4submit.host.Host` (what the scene is: its file, frame
range, renderables and the files it reads) and a way to draw
`c4submit.form.Form` in the application's own widgets. A host may also:

- give a renderable its own `key` (the job's key) and `values` for the
  tool's derived fields, by parameter name (`{"view_layer": "bg"}`) or by
  where a field comes from (`{"scene.camera": "shotCam"}`);
- read its range and renderables in one pass (`read()`), when reading is
  costly;
- say what only it knows a check depends on (`signature()`: the scene's
  path and unsaved state by default), so a changed scene makes the last
  check stale;
- set `writes_scene` when it writes the scene for every submission (a USD
  stage from a LOP network);
- tag a file with the renderables that read it (`Dependency.renderables`:
  `None` when unknown, so every job carries it);
- follow a USD file into what it reads in turn (`c4submit.usd.dependencies`:
  USD's own resolver where the application has `pxr`, else the text
  layers read directly, a binary layer named so the host can warn);
- tell the artist how it read the scene (`warnings()`: that every file was
  sent because the application could not say which ones the jobs read).

Everything else (collection, IDs, the check, the summary, uploads, the
file) is `c4submit`'s. `c4submit.testing` has a fake farm (in memory, and
over HTTP) and a fake host for tests. Where an application has its own farm
seam, the submitter plugs into it rather than drawing a panel: Katana's
FarmAPI, Unreal's Movie Render Queue executor.

## Status

A panel uses the farm's routes as API.md gives them: `GET /tools` and
`/tools/{project}/{tool}` for the form, `GET /projects/{project}` for
"Needed by" and the project's root path, `POST /submissions` (with
`dryRun`) for the check and the submit. A farm without them still lets a
panel collect, check the file locally and save it (with `C4_TOOL_PATH` for
the form); Submit needs them. A panel's local check refuses what the schema
refuses (`schema/`), in the same words, and its tests run the schema's
fixtures. A panel writes `root` (where the tree's top is on the artist's
machine, for path mapping) only when it differs from the project's root
path. Until the farm counts licence seats, it refuses tools that declare
licences, and the summary shows seats only once the farm reports them.
