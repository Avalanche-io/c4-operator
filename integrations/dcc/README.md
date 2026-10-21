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

## Installing into the applications

`c4 farm integrations` lists the applications on the machine, and
`c4 farm integrate <name> on|off` puts a panel in or takes it out. Both use
this folder's Go package (`dcc`), which carries every panel's files and
`c4submit` inside `c4 farm` itself, so no checkout and no Python install is
needed.

- **Finding them** reads files and folder names only, and runs nothing:
  - **Blender:** on macOS, the `CFBundleShortVersionString` in each
    `Blender*.app`'s `Info.plist` (XML or binary) in `/Applications`,
    `~/Applications` and Steam's library. If the plist can't be read, the
    bundle's `Contents/Resources/<major.minor>` folder gives the version. On
    Linux and Windows, the `<major.minor>` folder inside each install folder
    and inside Steam's `Blender` folder.
  - **Unreal:** the engines (`UE_<major.minor>` entries whose folder exists)
    in the Epic Games Launcher's `LauncherInstalled.dat`. Plug-in entries
    are skipped.
  - **Houdini, Maya, Nuke and Katana:** the install folders' names
    (`Houdini21.0.440`, `hfs21.0.440`, `maya2025`, `Nuke15.1v3`,
    `Katana8.0v1`) in the usual places for each system. Katana is not looked
    for on macOS, because it isn't made for it.

  A later version may deliberately run an application's own command-line
  entry point (`hython`, `mayapy`) for a version a folder name can't give.
  That would be done only after a test on a real machine.
- **On** shows each file it would write and asks first. The panel goes in
  `~/.c4/integrations/<name>/`, and the application gets a small pointer in
  a folder it already reads:

  | application | its pointer                                                                                             |
  | ----------- | ------------------------------------------------------------------------------------------------------- |
  | Blender     | the add-on in the version's `scripts/addons`, and a `scripts/startup` module that turns it on once      |
  | Unreal      | `~/Documents/UnrealEngine/Python/init_unreal.py`; refused if one is there that isn't C4's               |
  | Houdini     | `packages/c4.json` in the version's preferences folder                                                  |
  | Maya        | `c4.mod` in the version's user `modules` folder                                                         |
  | Nuke        | one marked line in `~/.nuke/init.py`, after a backup of the file                                        |
  | Katana      | one marked line in `~/.katana/init.py`, after a backup of the file                                      |

- **Every write is whole**: a file is written beside its place and renamed
  in, and a new folder is built beside its place and renamed in. A file that
  is already there and isn't C4's is never overwritten. A start-up file that
  is a symbolic link stays one: C4 edits the file it points to, and refuses
  a link that points to nothing. The temporary names start with a dot, which
  Blender's add-on and start-up scans skip. A run that was killed leaves its
  temporary files behind, and the next `on` or `off` clears them.
- **One run at a time**: Apply holds a lock file, `~/.c4/integrations/.lock`.
  If another run of `c4` changed C4's integrations since the plan, Apply
  stops and asks for a new plan.
- **Off removes exactly what on wrote**, as recorded in
  `~/.c4/integrations/<name>-<version>.json`, plus Python's compiled copies
  of those modules. This works even after the application is uninstalled. A
  file changed since C4 wrote it is left, and Plan says so. Nuke's and
  Katana's start-up files lose only C4's line, and get back their last line
  ending as it was.
- **Versions share some files.** Every version of an application uses the
  same panel folder, Nuke's and Katana's line and backup, and Unreal's
  start-up script. While another version still has C4 on, off leaves these
  files and records them in that version's manifest, so that version's off
  removes them.
- **Running on again** changes nothing. C4's line stays where it is, and the
  backup keeps the file as it was before C4 first changed it. After a newer
  `c4` release, on replaces only C4's own files.

## The tools on the farm

A panel submits to a tool the farm already has. Each application's folder
ships the tool its panel expects, under `tools/`, ready for a TD to point at
the studio's own image and publish (`c4 farm tool publish <folder>`):

| folder           | tool                               | one unit                                                                               |
| ---------------- | ---------------------------------- | -------------------------------------------------------------------------------------- |
| `nuke/tools/`    | `nuke-render`                      | one Write node over a chunk of frames                                                  |
| `houdini/tools/` | `houdini-rop`, `karma`, `pdg-cook` | one ROP's frames; one stage's frames with husk; a whole TOP network cooked in one unit |
| `maya/tools/`    | `maya-render`                      | one render layer's frames                                                              |
| `katana/tools/`  | `katana-render`                    | one Render node's frames                                                               |
| `blender/tools/` | `blender-render`                   | one view layer's frames (on the farm's own Blender image)                              |
| `unreal/tools/`  | `unreal-render`                    | a whole Movie Render Queue job                                                         |

Each prints a line per finished frame, says why it failed and whether a
retry could help, and carries a small test the farm runs before it offers
the tool. A Go test beside them (`tools_test.go`) checks each reads as
`c4 farm` reads a tool, passes the farm's publish-time check and asks for
nothing the farm refuses at submit. The licensed applications declare no
licences yet: the farm cannot count seats today, and refuses a tool that
asks for them. Their tests need a route to the studio's licence server, so
on a farm without one they fail at publish until licences arrive.

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
