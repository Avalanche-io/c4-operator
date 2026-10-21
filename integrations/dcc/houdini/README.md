# C4 for Houdini

Two nodes that render on a C4 farm from inside Houdini, the way an artist
submits to any render queue:

- **C4 Render** (Solaris LOP): renders the stage wired into it, one job per
  ticked render settings prim.
- **C4 Submit** (ROP): renders the ROPs wired into it from the saved hip
  file, one job per ROP, each after the ROPs it reads.

Both are drawn from the parameters of the farm's tools and use
`c4submit`, the library under every C4 DCC panel.

## Install

1. Point Houdini at the package. Either set `HOUDINI_PACKAGE_DIR` to this
   folder (`integrations/dcc/houdini`), or copy `c4.json` into
   `$HOUDINI_USER_PREF_DIR/packages` and change its `C4_DCC` line to the
   absolute path of `integrations/dcc`. The package adds `python/` and
   `../c4submit/src` to `PYTHONPATH`, and this folder to `HOUDINI_PATH`
   (for `otls/` and the C4 shelf).
2. Set the studio's environment (in `houdini.env`, the package, or the
   launcher):

   | variable                            | what it is                                                                                         |
   | ----------------------------------- | -------------------------------------------------------------------------------------------------- |
   | `C4_HUD`                            | the farm's address, such as `https://c4.petrelfinch.example`                                       |
   | `C4_HUD_TOKEN_FILE`                 | a file holding the artist's sign-in token (read on every request); or `C4_HUD_TOKEN`               |
   | `C4_PROJECT`                        | the farm project, such as `saltmarsh`                                                              |
   | `C4_ROOT`                           | the folder on this machine that the farm sees as the project's top, such as `/show`                |
   | `C4_SHOT` or `C4_SHOT_PATTERN`      | the shot, or a regular expression with a `shot` group matched against the hip file's path          |
   | `C4_TOOL_PATH`                      | folders of tool folders, offered when the farm does not list tools yet (draw the form, save files) |

3. Optional: build the HDAs once, with a Houdini licence:
   `hython integrations/dcc/houdini/python/c4_houdini/build_hdas.py`
   writes `otls/c4.hda`. Without it, the **C4** shelf places a null LOP or
   ROP with the same parameters (`c4_houdini.nodes.install(node)`).

## What the artist sees

Select a LOP (or ROPs) and press **C4 Render** (or **C4 Submit**) on the C4
shelf, or find the node in the Tab menu once the HDA is built. The node's
parameters:

- **Tool**, **Project**, **Shot**, and **Needed by**: the shot's milestones,
  "When the farm suggests", or a time you type ("fri 18:00", "+3h").
- **Frames**, with **Linked** on to follow each job's own range.
- **Render settings** (C4 Render) or **ROPs** (C4 Submit): tick what to
  render. ROPs further up the chain are listed unticked; tick one to render
  it on the farm before the ROP that reads it.
- **Size**, when the tool offers bigger machines, and **Note**.
- The **Tool** folder: the tool's own fields. A field taken from the scene
  has **Linked** beside it; turn it off to type your own value.
- **Check**, **Submit**, **Show file**, **Save file**, and the summary.

The C4 Render node also has **Stage file** (where the stage is written,
`$HIP/usd/<shot>_<node>.usd`) and **Frame range** (`$FSTART` to `$FEND`).

## What Submit does

1. Writes the stage to the Stage file with a USD ROP (C4 Render), or saves
   the hip file (C4 Submit).
2. Lists every file the stage or hip file reads, sequences and UDIM tiles
   included, but not what a ticked ROP writes for another.
3. Asks the farm what it lacks and when it would be ready, and shows the
   summary: files to upload, licences, the estimate and the slack against
   **Needed by**. Press **Submit** to go ahead, **Show file** to see the
   submission, or **Cancel**.
4. Uploads only the files the farm lacks, then submits.

A check holds until the node, the scene's saved file or the hip file
changes: Submit, Show file and Save file reuse it, and check again
otherwise.

**Save file** writes the same submission as a file for
`c4 farm submit FILE`. Every file must be under `C4_ROOT`; the summary
names any that are not, and any that are missing.

C4 Submit reads every file reference in the hip file, so a missing file on
a node nobody renders also stops the submission. Delete or bypass that
node; a bypassed node's missing file is only a warning.

## For TDs: the tools

The nodes submit to tools the farm has published. `tools/` holds three,
ready to publish once they name the studio's Houdini:

| tool          | kind     | node       | what it runs                                                                                      |
| ------------- | -------- | ---------- | ------------------------------------------------------------------------------------------------- |
| `karma`       | `frames` | C4 Render  | `husk` over the stage the node writes, one ticked render settings prim per job, CPU or XPU         |
| `houdini-rop` | `frames` | C4 Submit  | `hython houdini_rop.py`: one ROP of the saved hip file over the unit's frames, without its inputs |
| `pdg-cook`    | `once`   | C4 Submit  | `hython pdg_cook.py`: a whole TOP network in one unit, with PDG's local scheduler                |

To publish one:

1. Copy its folder, and in `tool.yaml` set `runs.image` to the studio's
   own Houdini image (the folders say `registry.example.com/houdini:21.0`,
   a placeholder), with `hython` and `husk` on its `PATH`. On workstations
   that run units themselves (`runs.host`, tagged `houdini-21.0`), set
   `runs.host.exe` to where Houdini is installed there.
2. Add `needs` (CPU, memory, GPUs for XPU) once a real shot has been
   measured; until then the farm's defaults apply.
3. `c4 farm tool test <folder>`, then `c4 farm tool publish <folder>`. The
   farm runs the tool's test again and offers it only once that passes.

Each test makes its scene without a binary file in git. `karma`'s is a
small text stage, `test/cube.usda`. The two hython tools take a `.py` as
well as a hip file: the script runs it with the path of the hip file to
save (`<name>.hip` beside it), then opens that file. Their tests hold such
a `.py`: `test/fx/rop_setup.py` (a Geometry ROP writing two frames) and
`test/fx/pdg_setup.py` (a TOP network of three work items).

What the scripts do:

- **houdini-rop** renders the ROP with `ignore_inputs=True`. Each ROP
  upstream that the artist ticks is its own job, run first; one left
  unticked was rendered before and is read from disk. It prints
  `c4_frame: <frame> <file>` as each frame's file appears (the file is
  read from the ROP's output parameter, as C4 Submit reads it), and
  `c4_fail: permanent ...` for a missing hip file or ROP. Its units are 4
  frames each: a ROP whose frames depend on the ones before (a sim cache)
  needs a copy with `frames: {chunk: all, order: sequential}`.
- **pdg-cook** cooks the network's output node (the one with the display
  flag), or the TOP node named. It gives every TOP network above that node
  a local scheduler of its own, as many slots as the unit has CPUs, so a
  hip file saved with a farm scheduler still cooks inside the unit. It
  prints `openjd_progress` and `openjd_status` as work items finish, and
  fails the unit if one fails. The farm schedules none of the work items
  itself: the whole cook moves off the workstation, as "submit graph as
  job" does in other queues.

C4 Submit offers the project's tools whose scene parameter (`from:
scene.file`) takes `*.hip` files. For a tool whose `from:
scene.renderables` parameter is named `top`, it lists the hip file's TOP
networks (wherever they are: `/obj`, `/out`, `/tasks`) instead of ROPs:
one wired into it is ticked, or the only one there is, and the job's `top`
is the network's output node. Otherwise it lists ROPs, and a tool given a
ROP must render it without its inputs, as `houdini-rop` does: a tool that
rendered inputs too would render them again inside every job.

Two things the farm does not do yet, which these tools work within:

- Houdini needs its licence server. The tools declare no licences and no
  network, as the farm refuses both at submit for now, so a unit must
  reach the licence server without the farm opening the way (a host
  runner, or a cluster whose units reach it).
- Path mapping. The farm passes its path map as a file
  (`{{Run.PathMap}}`); Houdini reads `HOUDINI_PATHMAP` as JSON text. So
  the farm must see the project's files where the artist's `C4_ROOT`
  does, which is the default (a submission names its own root only when
  it differs).

## Tests

`python3 -m pytest -q integrations/dcc/houdini` runs against a fake `hou`
and `pxr` (`tests/fakehou.py`), the tools' scripts included (with a fake
`pdg` in `tests/test_houdini_tools.py`). What only a real Houdini can show is in
[CHECKLIST.md](CHECKLIST.md).
