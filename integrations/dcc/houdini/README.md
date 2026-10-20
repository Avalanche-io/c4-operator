# C4 for Houdini

Two nodes that render on a C4 farm from inside Houdini, the way an artist
submits to any render queue:

- **C4 Render** (Solaris LOP): renders the stage wired into it, one job per
  ticked render settings prim.
- **C4 Submit** (ROP): renders the ROPs wired into it from the saved hip
  file, one job per ROP, each after the ROPs it reads.

Both are drawn from the farm's tool definitions (TOOL.md section 6) and use
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

## Tests

`python3 -m pytest -q integrations/dcc/houdini` runs against a fake `hou`
and `pxr` (`tests/fakehou.py`). What only a real Houdini can show is in
[CHECKLIST.md](CHECKLIST.md).
