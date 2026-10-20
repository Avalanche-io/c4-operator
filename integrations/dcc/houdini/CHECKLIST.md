# Checking the C4 nodes in a real Houdini

The tests run against a fake `hou` and `pxr`. They prove the logic, not
Houdini's or USD's behaviour. Run these checks in **Houdini 20.5 and 21**
(Python 3.11) before artists use the nodes. Each step says what to expect.
Steps marked **[API]** check a call whose behaviour was assumed from
SideFX's or Pixar's documentation and never run; if one fails, the fix
belongs in `python/c4_houdini/` and its fake in `tests/fakehou.py`.

Paths below are from the c4-operator checkout (`$C4OP`).

## 0. A farm to submit to

Use a real farm, or the fake one from c4submit, which takes submissions and
stores uploads in memory:

```
python3 $C4OP/integrations/dcc/houdini/tests/serve_fake_farm.py
```

It prints `fake farm at http://127.0.0.1:PORT (project saltmarsh)`. Use that
address as `C4_HUD` below. Its project has the milestones "dailies" and
"client-review" and the tools `karma` and `houdini-rop`.

Make a project folder to stand for the show, such as `/tmp/show`, and start
Houdini with:

```
C4_HUD=http://127.0.0.1:PORT  C4_PROJECT=saltmarsh  C4_ROOT=/tmp/show
C4_SHOT_PATTERN='/(?P<shot>sm\d{4})/'  HOUDINI_PACKAGE_DIR=$C4OP/integrations/dcc/houdini
```

## 1. The package loads

1. **[API]** In the Python Shell: `import c4_houdini, c4submit; print(c4_houdini.__file__, c4submit.__file__)`.
   Expect both paths under `$C4OP/integrations/dcc`. This checks
   `$HOUDINI_PACKAGE_PATH/..` in `c4.json` and the `PYTHONPATH` entries with
   `"method": "prepend"`.
2. `hou.getenv("C4_DCC")` is `$C4OP/integrations/dcc` (not a path relative
   to the packages folder).
3. **[API]** The shelf set has a **C4** shelf with **C4 Render** and
   **C4 Submit** (`toolbar/c4.shelf` found through the `hpath` entry). If
   it is missing, add the shelf by hand from `toolbar/c4.shelf` and record
   the fix.
4. Copy `c4.json` into `$HOUDINI_USER_PREF_DIR/packages`, set its `C4_DCC` to
   the absolute path, unset `HOUDINI_PACKAGE_DIR`, restart: steps 1 to 3 hold.

## 2. Building the HDAs

1. In a terminal with Houdini's environment:
   `hython $C4OP/integrations/dcc/houdini/python/c4_houdini/build_hdas.py`.
   Expect `built c4::c4render::1.0 in .../otls/c4.hda` and the same for
   `c4::c4submit::1.0`, and no traceback.
2. **[API]** Calls assumed here: `Node.createDigitalAsset(name=,
   hda_file_name=, description=, min_num_inputs=, max_num_inputs=,
   ignore_external_references=)`, `HDADefinition.setParmTemplateGroup`,
   `addSection`, `setExtraFileOption("PythonModule/IsPython", True)`,
   `setIcon`, `save(path)`; a LOP `subnet` holding an `output` node wired
   from `indirectInputs()[0]`; a ROP subnet in `/out` taking 64 inputs.
3. Restart Houdini. In a LOP network the Tab menu offers **C4 Render**; in
   `/out` it offers **C4 Submit**. Type Properties of each: the parameters
   are Tool through Note, then Check, Submit, Show file, Save file and the
   summary; the Scripts tab has PythonModule, OnCreated and OnLoaded.
4. Placing C4 Render after a LOP: the stage passes through (the node's
   viewport shows the same scene as its input).
5. Do not commit `otls/c4.hda`; `otls/.gitignore` keeps it out.

## 3. C4 Render on a Solaris shot

Build a small shot under `/tmp/show/sm0220/light/`: save the hip as
`sm0220_light_v001.hip`, then in `/stage`: a Reference LOP of a USD asset
under `/tmp/show/assets/` whose material uses a `<UDIM>` texture (tiles
1001 and 1002 on disk), a Dome Light with an HDRI under `/tmp/show`, a
Camera, and two Karma Render Settings LOPs, one making
`/Render/rendersettings` (set as the stage's default render settings) and
one making `/Render/beauty_only`.

1. Select the last LOP; press **C4 Render** on the shelf (with and without
   the HDA built). Expect a node wired after the selection, named
   `c4render`, with:
   - Tool **karma** (its menu lists "Karma render" only: houdini-rop does
     not render `.usd`), Project `saltmarsh`, Shot `sm0220` (from the
     pattern), Needed by "suggested";
   - **[API]** Stage file `$HIP/usd/`chs("c4_shot")`_$OS.usd` evaluating to
     `/tmp/show/sm0220/light/usd/sm0220_c4render.usd`;
   - **[API]** Frame range showing `$FSTART`, `$FEND`, 1 as expressions,
     and its three fields called `c4_f1`, `c4_f2`, `c4_f3`;
   - Frames greyed out showing the range, Linked on;
   - **[API]** Render settings: a menu of checkboxes listing both prims,
     only `/Render/rendersettings` ticked (`stage.GetMetadata
     ("renderSettingsPrimPath")`, `Usd.PrimRange`, `prim.IsA
     (UsdRender.Settings)`);
   - Size visible with "Normal" and "heavy" (karma has sizes);
   - the Tool folder: USD stage and Render settings greyed out with Linked
     on beside them, Engine a menu of the two Karma engines.
2. **[API]** Needed by: the menu lists "Dailies, Thu 18:00", "Client review,
   Fri 10:00", "When the farm suggests"; typing `fri 18:00` keeps the
   typed text (a `StringReplace` menu).
3. **[API]** The menus are Python menu scripts written as function bodies
   (`return nodes.menu(...)`). If a menu is empty and the console shows a
   SyntaxError, the script form is wrong.
4. **[API]** Turn off Linked beside Render settings: the field becomes
   editable (`disable_when`) and keeps its value; type `/Render/beauty_only`;
   turn Linked on: it shows `/Render/rendersettings` again.
5. Pick Engine `BRAY_HdKarma`, then type `Cycles` into the field if it
   allows typing: the summary names the problem ("Engine: must be one of
   ..."). **[API]** The summary cannot be edited (it is locked between
   writes).
6. Press **Check**. Expect:
   - **[API]** a progress bar with Cancel (`hou.InterruptableOperation(...,
     open_interrupt_dialog=True)`, `updateLongProgress`), "Reading files"
     with counts on a larger shot; Houdini stays responsive enough to
     press Cancel;
   - **[API]** `/tmp/show/sm0220/light/usd/sm0220_c4render.usd` written by
     a temporary `usd` ROP in `/out` (`loppath`, `lopoutput`, `trange`,
     `render(frame_range=)`), which is gone afterwards. Open the file in
     usdview or a Sublayer LOP: it holds every frame of the range as time
     samples, and the in-memory layers (SOP Import, the lights) are in it,
     not left as anonymous layers or written beside it under other names;
   - the summary, in this shape:
     ```
       files      N found from sm0220_c4render.usd · ... · N to upload
     sm0220-light-v001   Karma render · 48 frames · 48 units
       licences   sesi/karma, 1 per unit · 6 of 12 seats free now
       estimate   ready ...
     ```
     and, with Needed by "client-review", a `due` line with "client
     review · slack".
   - **[API]** N counts the stage file, the referenced asset's layers,
     both UDIM tiles and the HDRI, and nothing under `$HFS`
     (`UsdUtils.ComputeAllDependencies(Sdf.AssetPath(path))` returning
     layers, assets and unresolved paths; check whether UDIM textures come
     back as the `<UDIM>` path or as tiles: both are handled).
7. Rename a texture tile away and Check: the summary has an error naming
   the texture. Put it back.
8. Move the HDRI outside `/tmp/show` and Check: the error says it is
   outside the project folder. Put it back.
9. Press **Submit**. **[API]** A dialog shows the same summary with Submit,
   Show file and Cancel (`hou.ui.displayMessage(buttons=...,
   close_choice=2)`). Show file opens the submission in the details pane
   (`details=`, `details_expanded=True`) and returns to the dialog. Submit:
   the summary starts "Submitted sm0220-light-v001, N files uploaded"; the
   fake farm's terminal, on Ctrl-C, reports 1 submission.
10. Submit again unchanged: at most the rewritten stage file is uploaded
    (nothing at all if the USD ROP writes the same bytes twice; note which)
    and the job is `-v002`.
11. Tick both render settings, turn Frames' Linked off and type `1001-1010`,
    Submit: two jobs, `light-render-beauty-only` and
    `light-render-rendersettings`, each 10 frames; the stage file holds
    frames 1001 to 1010.
12. Press **Check**, then Cancel in the progress dialog while files are
    read (use a shot with a few thousand files, or a slow disk): the
    summary says "Check cancelled." and nothing reached the farm.
13. **Save file**: **[API]** a file chooser in write mode opens at `$HIP`
    proposing `sm0220-light.yaml` (`hou.ui.selectFile(chooser_mode=
    hou.fileChooserMode.Write, pattern="*.yaml")`); the file saved starts
    `# Saved from houdini by c4submit` and `c4 farm submit` accepts it.
14. Save the hip, quit, reopen: the C4 Render node keeps its Tool folder
    (spare parameters saved with the node) and every value typed; the first
    change re-reads the farm without error.

## 4. C4 Submit on an FX hip file

Under `/tmp/show/sm0140/fx/`, save `sm0140_fx_v001.hip` with: a geometry
ROP `/out/sim` writing `$HIP/cache/sim.$F4.bgeo.sc`; a File SOP reading
that cache; a Mantra or Karma ROP `/out/mantra1` wired after `/out/sim`,
writing `$HIP/render/sm0140.$F4.exr`; an OpenGL ROP `/out/flip`; a Merge ROP
taking mantra1 and flip; a File SOP reading a `$F4` sequence that exists
on disk; and a texture with `<UDIM>` tiles.

1. Select the Merge ROP; press **C4 Submit** on the shelf. Expect Tool
   **houdini-rop** (karma does not render `.hip`), and ROPs listing `sim`
   "(upstream of mantra1)" unticked, `mantra1` and `flip` ticked.
2. **[API]** The ROP graph: `node.inputs()` through the Merge (type name
   `merge`), `trange` 0 meaning the current frame, the `f` tuple
   (`f1`, `f2`, `f3`). Bypass flip: it is listed "(bypassed)" and unticked.
3. Press **Check** with the sim cache not on disk: an error names the File
   SOP and `sim.$F4.bgeo.sc` ("no such files"). Tick `sim` and Check again:
   no error; the jobs are `render-out-sim`, `render-out-mantra1` (after
   `render-out-sim`) and `render-out-flip`.
4. **[API]** `hou.fileReferences()`: the files counted include the `$F4`
   sequence frame by frame, the UDIM tiles, the hip file and any studio HDA
   under `/tmp/show`; they do not include the ROPs' outputs, files under
   `$HFS`, or `op:` paths. Check that `parm.parmTemplate().tags()
   ["filechooser_mode"]` is `"write"` on Houdini's output parameters
   (`sopoutput`, `vm_picture`, `picture`, `lopoutput`), that
   `parm.unexpandedString()` keeps `$F4`, and what happens with a file
   parameter holding an expression (it is read at the current frame).
5. **[API]** `hou.expandString` leaves the `C4KEEP0X` placeholders (used to
   keep `$F4` while expanding `$HIP`) and `<UDIM>` untouched.
6. Make a change without saving and press Submit: **[API]** the hip file
   is saved first (`hou.hipFile.hasUnsavedChanges()`, `save()`); in an
   untitled session the summary says to save the hip file first
   (`hou.hipFile.isNewFile()`).
7. Submit: three jobs, as in step 3, with `hip:
   sm0140/fx/sm0140_fx_v001.hip` and `rop: /out/...`.
8. A Fetch ROP pointing at mantra1, wired into C4 Submit: it renders
   mantra1's frames and its outputs are not collected.
9. Do not render the C4 Submit ROP itself (Render to Disk): Houdini would
   render its inputs here. It is a submit node; Submit is the action.

## 5. Without a farm

1. Start Houdini without `C4_HUD`; place C4 Render: the summary says
   "C4: the farm's address is not set: set C4_HUD to the HUD's URL" and
   there is no Tool folder. Nothing raises.
2. Stop the fake farm and press Check on a node made while it ran: the
   summary names the address it cannot reach and says to check C4_HUD and
   the sign-in.

## 6. Words

Read every label, help text, menu and summary the nodes show. Nothing says
image, pod, namespace, priority, GPU request or any Kubernetes term.
"Needed by" is the only control for urgency.
