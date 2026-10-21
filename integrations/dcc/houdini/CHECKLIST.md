# Checking the C4 nodes in a real Houdini

The tests run against a fake `hou` and `pxr`. They prove the logic, not
Houdini's or USD's behaviour. Run these checks in **Houdini 20.5 and 21**
(Python 3.11) before artists use the nodes. Each step says what to expect.
Steps marked **[API]** check a call whose behaviour was assumed from
SideFX's or Pixar's documentation and never run; if one fails, the fix
belongs in `python/c4_houdini/` and its fake in `tests/fakehou.py`.
Steps marked **[GUESS]** check a parameter name, flag or message written
from memory, less certain still; a tool's fix belongs in its folder under
`tools/`.

Paths below are from the c4-operator checkout (`$C4OP`).

## 0. A farm to submit to

Use a real farm, or the fake one from c4submit, which takes submissions and
stores uploads in memory:

```
python3 $C4OP/integrations/dcc/houdini/tests/serve_fake_farm.py
```

It prints `fake farm at http://127.0.0.1:PORT (project saltmarsh)`. Use that
address as `C4_HUD` below. Its project has the milestones "dailies" and
"client-review" and the tools `karma`, `houdini-rop` and `pdg-cook`.

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
    Submit: two jobs, `light-beauty-only` and
    `light-rendersettings`, each 10 frames; the stage file holds
    frames 1001 to 1010.
12. Press **Check**, then Cancel in the progress dialog while files are
    read (use a shot with a few thousand files, or a slow disk): the
    summary says "Check cancelled." and nothing reached the farm.
13. **Save file**: **[API]** a file chooser in write mode opens at `$HIP`
    proposing `sm0220-light.yaml` (`hou.ui.selectFile(chooser_mode=
    hou.fileChooserMode.Write, pattern="*.yaml")`); the file saved starts
    `# Saved from houdini by c4submit` and `c4 farm submit` accepts it.
14. Press Check, then Save file and Show file without changing anything:
    no progress bar and no new stage file (the check holds). Change a
    field, or a LOP upstream (the hip then has unsaved changes): Save file
    checks again first.
15. Save the hip, quit, reopen: the C4 Render node keeps its Tool folder
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
   no error; the jobs are `render-sim`, `render-mantra1` (after
   `render-sim`) and `render-flip`.
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
10. TOP networks. Add a TOP network `/obj/topnet1` (Generic Generator into
    a Python Script TOP, the display flag on the Python Script) and one in
    `/tasks`. Pick Tool **pdg-cook**: ROPs lists the TOP networks instead
    of ROPs, each "(cooks pythonscript1)"; with one network it is ticked.
    **[API]** `hou.nodeTypeCategories()`, `hou.nodeType(category,
    "topnet").instances()` finds both (check what `/tasks/topnet1`'s
    category is), and `displayNode()` or `isDisplayFlagSet()` names the
    output node. The Tool folder's TOP network field shows
    `/obj/topnet1/pythonscript1`. Submit: one job, no frames, `with: {hip:
    ..., top: /obj/topnet1/pythonscript1}`. Pick houdini-rop again: the
    ROPs are back.
11. A sequence over the jobs' frames. Put 48 frames of the `$F4` sequence
    on disk and set mantra1 to render 1001-1010 (flip renders the current
    frame). Check: only the sequence's frames 1001-1010 are counted, not
    all 48. Wire a Time Shift SOP after the File SOP reading it (frame
    1001, for a rest pose) and Check again: all 48 are counted, as are
    those of a File SOP whose geometry an Object Merge in another network
    feeds into a Time Shift. **[API]** that `Node.outputs()` and
    `Node.dependents(include_children=False)` find the Time Shift (the
    second through the Object Merge's `objpath1`), and that `/obj` is a
    `Manager` node, where the walk stops. **[GUESS]** that the type
    names in `RETIMING` (`timeshift`, `timewarp`, `retime`, `timeblend`,
    `trail`, `cache`, `timecache`, `dopnet`, the `dopimport` family),
    any type ending in `solver` and any DOP are the nodes that cook
    other frames; note any other (a CHOP-driven retime, a studio HDA)
    that a real hip file uses. **[GUESS]** that motion blur reads `$F`
    files only at the frame rendered: with deformation blur on in
    mantra1, render frame 1010 on the farm and check its log for a read
    of frame 1011.

## 5a. The tools, run by hand

Each tool runs here as a unit runs it, with `C4_TREE` set to a copy of
its test tree. From a shell with Houdini's environment (`source
houdini_setup` in `$HFS`), for each tool `T` in `karma`, `houdini-rop`,
`pdg-cook`:

```
cp -R $C4OP/integrations/dcc/houdini/tools/T/test/. /tmp/t-T
```

1. **houdini-rop**:
   `cd /tmp/t-houdini-rop && C4_TREE=$PWD hython $C4OP/integrations/dcc/houdini/tools/houdini-rop/houdini_rop.py $PWD/fx/rop_setup.py /out/cache 1001-1002`.
   Expect `openjd_status: building the scene with rop_setup.py`, then
   `c4_frame: 1001 /tmp/t-houdini-rop/fx/geo/box.1001.bgeo.sc` and the
   same for 1002, exit 0, and `fx/rop_setup.hip` beside the `.py`.
   - **[GUESS]** the builder: `hou.hipFile.clear(suppress_save_prompt=)`,
     the `xform` SOP's `ty`, the Geometry ROP's `soppath`, `sopoutput`,
     `trange`, `f`.
   - **[API]** `hou.hipFile.load(..., suppress_save_prompt=True,
     ignore_load_warnings=False)` raising `hou.LoadWarning` with the file
     open (rename an HDA the hip uses: the warning is printed and the
     render goes on).
   - **[API]** `RopNode.addRenderEventCallback(cb)` calling `cb(rop,
     hou.ropRenderEventType.PostFrame, time)` per frame, and
     `hou.timeToFrame(time)` giving the frame: the `c4_frame` lines come
     as each frame finishes, not all at the end (watch a slow Mantra
     render of 10 frames). `removeRenderEventCallback` exists.
   - **[API]** `render(frame_range=(a, b, step), ignore_inputs=True,
     verbose=True, output_progress=True)`: wire a second Geometry ROP into
     `/out/cache`'s input and check it is not rendered. A ROP error raises
     `hou.OperationFailed`, or leaves `rop.errors()`: point `soppath` at
     nothing and expect `c4_fail: retry /out/cache ...`.
   - `Parm.evalAtFrame(f)` on `sopoutput` gives each frame's file with
     `$F4` filled in; on Mantra's `vm_picture` and Karma's `picture` too.
   - `.../houdini_rop.py $PWD/fx/rop_setup.hip /out/nope 1001`: `c4_fail:
     permanent the hip file has no node /out/nope`, exit 2. With
     `/obj/box/box1`: `... is a box node, not a ROP`. Try a ROP inside a
     SOP network (`rop_geometry`): it renders (**[API]** it is a
     `hou.RopNode`).
2. **pdg-cook**:
   `cd /tmp/t-pdg-cook && C4_TREE=$PWD hython $C4OP/integrations/dcc/houdini/tools/pdg-cook/pdg_cook.py $PWD/fx/pdg_setup.py /obj/tops`.
   Expect `fx/pdg/item.0.txt` to `item.2.txt`, `openjd_progress` lines
   rising to 100, `openjd_status: 3 work items cooked`, exit 0.
   - **[GUESS]** the builder: the `genericgenerator` TOP's `itemcount`,
     the `pythonscript` TOP's `script` and `work_item.index` in it, and
     that it runs in a process the local scheduler starts.
   - **[GUESS]** the local scheduler: node type `localscheduler`, the TOP
     network's default scheduler parameter `topscheduler`, and Total Slots
     as `maxprocsmenu` = `-1` (custom) with `maxprocs`. Check in the
     scheduler's parameters that slots is the unit's CPU count; if a name
     is wrong the log says `houdini: could not set ...` and it cooks with
     the default slots.
   - Save a hip whose TOP network's scheduler is an HQueue or Deadline
     scheduler: it still cooks here (the network's default is the
     `c4_local` scheduler made for the cook). A node with its own
     scheduler override would still use it: note which parameter that is.
   - **[API]** `TopNode.getPDGGraphContext()` (not None before the first
     cook?), `GraphContext.addEventHandler(fn,
     pdg.EventType.WorkItemStateChange)` calling `fn(event)` with
     `event.workItemId`, `event.node.name` and `event.currentState` (a
     `pdg.workItemState` whose `.name` is `CookedSuccess`, `CookedCache`,
     `CookedFail` or `CookedCancel`), and `removeEventHandler`. If the
     log says `cooking; progress shows once the cook is done`, the
     context was None and the work items were counted after the cook
     (`getPDGNode().workItems`, each `.id` and `.state`).
   - **[API]** `TopNode.cookWorkItems(block=True)` returns once the cook
     is done. Make the Python Script raise for one index: expect `c4_fail:
     retry 1 work items failed, in write` and exit 1 (does cookWorkItems
     raise `hou.OperationFailed` instead? either is handled).
   - In a container whose CPU limit is 4 on a larger machine, the slots
     are 4 (`/sys/fs/cgroup/cpu.max`, cgroup v2; on cgroup v1 it falls back
     to the CPUs the process may run on: note it).
3. **karma**: `cd /tmp/t-karma && husk --renderer BRAY_HdKarma --settings
   /Render/rendersettings -f 1 -n 1 --make-output-path -V 2a cube.usda`.
   Expect `render/cube.0001.exr`, 64 by 64, a lit cube.
   - **[GUESS]** `--make-output-path` makes `render/`; `-V 2a` prints
     `ALF_PROGRESS NN%` lines (the tool's progress pattern); a relative
     product name is relative to the working directory (the tool runs in
     the job's tree).
   - **[GUESS]** the test stage is enough for husk: the RenderVar
     `sourceName = "C"`, `dataType = "color4f"`, and the camera found
     through the settings' `camera` relationship.
   - A stage written by C4 Render with a frame range: `-f 1001 -n 1`
     renders frame 1001's product name (from its time samples, or with
     `$F4` expanded: note which Houdini writes).
4. Messages for the tools' `errors` patterns. Only the licence line is in
   the folders; confirm it, and record what Houdini prints for the
   others before adding them:
   - with no licence server reachable, `hython` and `husk` both print
     `No licenses could be found to run this application` (confirm for
     husk and for Houdini Engine licences);
   - husk on a missing stage, and on a settings prim the stage lacks (the
     schema's example tool guessed `Unable to open stage` and `No
     RenderSettings prim`);
   - Karma XPU with no usable GPU (guessed `CUDA error`, `OptiX error`).
5. The tools' runs.host.exe paths (`/opt/hfs21.0/bin/hython`, the
   Windows and macOS install paths with build 21.0.440) match a real
   install; Houdini's Linux image has `hython` and `husk` on `PATH`.
6. `c4 farm tool test` on each folder, with `runs.image` set to a studio
   image: it passes only if the container reaches a licence server (the
   test runs with no network; record what happens).

## 5. Without a farm

1. Start Houdini without `C4_HUD`; place C4 Render: the summary says
   "C4: the farm's address is not set: set C4_HUD to the HUD's URL" and
   there is no Tool folder. Nothing raises.
2. Stop the fake farm and press Check on a node made while it ran: the
   summary names the address it cannot reach and says to check C4_HUD and
   the sign-in.

3. Against a farm that does not list tools yet: the summary says to set
   `C4_TOOL_PATH`. Set it to a folder holding `karma/tool.yaml`, change
   Project (or reopen): the Tool folder is drawn, Check says the file is
   checked here only, and Save file writes the submission.

## 6. Words

Read every label, help text, menu and summary the nodes show. Nothing says
image, pod, namespace, priority, GPU request or any Kubernetes term.
"Needed by" is the only control for urgency.

## Path mapping on the farm

- [ ] Save a hip under a root other than the farm's (a Windows `S:/shows/...` or a home folder), with a file SOP and a texture under that root, and submit it with C4 Submit. Expect the submission to carry `root:`, and the unit's log to show `HOUDINI_PATHMAP` as JSON from that root to the farm's (`{"S:/shows/saltmarsh": "/show"}`); the cache and texture load, and the render matches a local one.
- [ ] Submit a hip saved under the farm's own root. Expect no `root:` in the submission and no `HOUDINI_PATHMAP` in the unit's environment.

## 7. Installed by `c4 farm integrate houdini on`

Run `c4 farm integrate houdini on`, start Houdini. **Expect:** the C4 shelf
and the C4 nodes, with nothing set in `houdini.env`. **[guess]** that the
version's preferences folder (`~/Library/Preferences/houdini/<version>` on
macOS, `~/houdini<version>` on Linux, `~/Documents/houdini<version>` on
Windows) has its `packages` folder read with no `HOUDINI_PACKAGE_DIR` set.
