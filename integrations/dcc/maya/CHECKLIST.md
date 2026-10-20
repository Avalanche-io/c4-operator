# Checking the Maya window in a real Maya

The tests drive the window through a fake `maya` module (`tests/fakemaya.py`).
They show that the logic is right. They cannot show that real Maya behaves the
way the fake does. This list covers what only a real Maya can show. Each item
marked **[guess]** is a place where the code assumes something about Maya's
API that no test here can confirm. If an item fails, note the Maya version and
fix the code (and the fake, so the tests keep matching Maya).

Run the whole list in **Maya 2024, 2025 and 2026** (Python 3.10, 3.11 and
3.11), with Arnold (MtoA) loaded. Maya 2022 and earlier (Python 3.7) are not
supported. There, expect no C4 menu and the line "C4: submitting to the farm
needs Maya 2023 or later (Python 3.9); no C4 menu added" in the Script Editor.

## Setup

1. Write `c4.mod` as the README shows, pointing at
   `integrations/dcc/maya`, and put its folder on `MAYA_MODULE_PATH`.
   **[guess]** that `PYTHONPATH +:= ../c4submit/src` resolves against the
   module's folder. If it does not, `userSetup.py` still finds c4submit
   beside it.
2. Build a show folder (for example `/tmp/show`) with the scene from
   `tests/maya_scenes.py`. You need: a workspace at `seq/sm0140/maya`; heron
   textures `assets/heron/tex/heron_diffuse.1001.exr`, `.1002`, `.1011`; ten
   caustics frames `seq/sm0140/maya/sourceimages/caustics/caustic.0001.exr`…;
   `assets/heron/heron_rig.ma`; 48 standin frames
   `fx/splash/splash.1001.ass`…; ten VDBs `fx/fog/fog.1001.vdb`…;
   `anim/sm0140_heron_anim.abc`; and `assets/hdri/sky.exr`. Any small files
   will do, as long as Maya can open them.
3. Run a farm that answers. Without a real HUD, use c4submit's fake farm over
   HTTP. In the Script Editor (Python), before you open the window:

   ```python
   import os, json
   from c4submit.testing import FakeFarm, SALTMARSH, serve
   import sys; sys.path.insert(0, "/path/to/integrations/dcc/maya/tests")
   from maya_scenes import MAYA_RENDER
   farm = FakeFarm(tools=[MAYA_RENDER], projects={"saltmarsh": dict(SALTMARSH, root="/tmp/show")})
   served = serve(farm)
   os.environ.update(C4_HUD=served.url, C4_PROJECT="saltmarsh", C4_SHOT="sm0140", C4_ROOT="/tmp/show")
   ```

4. Build the scene. Set the project to `seq/sm0140/maya`. Make meshes
   `heron` and `water`, an `aiStandIn` `splash` on `splash.1001.ass` with
   "Use Frame Extension" on, an `aiVolume` `fog` on `fog.####.vdb` with
   "Use Frame Extension" on, and an `aiSkyDomeLight` with an `aiImage` on
   `sky.exr`. Give the heron a file texture on `heron_diffuse.1001.exr` with
   UV Tiling Mode "UDIM (Mari)". Give the water a file texture on
   `sourceimages/caustics/caustic.0001.exr` (relative) with "Use Image
   Sequence" on. Import the Alembic onto the heron, and reference the rig.
   Make a camera `shotCam` and make it renderable. In Render Setup, make the
   layer **beauty** (heron, water, splash, sky) and the layer **fg** (heron,
   fog, sky), with an override of the end frame to 1010 on fg. Make a layer
   **old** and turn off its renderable switch. Turn off the master layer's.
   Render settings: frames 1001-1048, padding 4. Save the scene as
   `scenes/sm0140_light_v003.ma`.

## Startup and the menu

1. Start Maya. **Expect:** no errors in the Script Editor, and a **C4** menu
   in the main menu bar with **Submit to farm...**.
   **[guess]** that Maya runs `scripts/userSetup.py` from a module's
   `scripts` folder even when another `userSetup.py` is also on the path
   (Maya runs every `userSetup.py` it finds on `sys.path`, not only the
   first). **[guess]** that `mel.eval("$c4_main_window = $gMainWindow")`
   returns the main window's name, and that `cmds.menu(parent=...,
   tearOff=True)` adds a menu bar entry after `executeDeferred`.
2. Start `mayabatch` or `mayapy` with the module installed. **Expect:** no
   menu and no errors. **[guess]** that `cmds.about(batch=True)` is true in
   both.

## The window

1. Choose **C4 > Submit to farm...**. **Expect:** a window titled "Submit to
   C4" that does not block Maya. "Reading saltmarsh's tools from the farm..."
   shows and is soon replaced. Choosing the menu item again replaces the
   window and does not add a second one.
2. **Expect**, top to bottom: Tool ("Maya render"), Project (saltmarsh), Shot
   (sm0140), Needed by (Dailies, Client review, When the farm suggests, At a
   time I type) with a greyed time field beside it, Frames "each render
   layer's own range" with "linked" ticked beside it, a "Render layers"
   frame with two ticked checkboxes "beauty  scene range" and "fg
   1001-1010  (its own range)" (no old, no masterLayer), Note, Scene (a file
   field with "...") with "linked", Render layer ("each ticked render
   layer") with "linked", Camera (shotCam) with "linked", Renderer (arnold),
   a folded **Advanced** frame (Threads, Verbose log), the buttons Check,
   Submit (greyed), Show file (greyed) and Save file... (greyed) in one row,
   and a read-only summary box in a fixed-width font.
   **[guess]**: menu items are added to an `optionMenuGrp` with
   `menuItem(parent=grp + "|OptionMenu")`, listed with `itemListLong`, and
   chosen with `optionMenuGrp(edit=True, value=label)`. `rowLayout` with
   `adjustableColumn` keeps "linked" beside its field. `frameLayout(collapse=True)`
   starts folded. `scrollField(editable=False, font="fixedWidthFont")` is
   read-only yet lets you select and copy text.
3. Hover over every control. **Expect:** no annotation says image, pod,
   namespace, priority, cluster or container, and nothing offers a priority.
4. Untick fg. **Expect:** Render layer shows `rs_beauty`, Frames shows
   1001-1048. Tick it again. **[guess]** that `checkBox`'s `changeCommand`
   runs on a click and not when the code sets the value. The window ignores
   its own edits either way.
5. Type "fri 18:00" in the time field and press Enter. **Expect:** Needed by
   switches to "At a time I type". Choose a milestone. **Expect:** the time
   field greys. **[guess]** about `textField(edit=True, enable=...)`.
6. Type 1001-1010 in Frames and press Enter. **Expect:** "linked" unticks.
   Tick it again. **Expect:** Frames shows "each render layer's own range".
7. Type a new Shot and, without pressing Enter, press Check. **Expect:** the
   check uses the new shot. **[guess]** that a text field's `changeCommand`
   runs only on Enter or when it loses focus. The window reads every text
   field before Check, Submit and Save file anyway, so either way works.
8. Press "..." beside Scene. **Expect:** a file browser filtered to "Maya
   scenes (*.ma *.mb)". Pick a file. **Expect:** Scene shows it and "linked"
   unticks. **[guess]** about `fileDialog2(fileMode=1, fileFilter=...,
   dialogStyle=2)`.

## Check

1. Move the heron (so the scene is modified), then press **Check**.
   **Expect:** the scene is saved (the title bar loses its `*`). Maya's
   progress window "Checking with the farm" shows "Reading files: n of 76",
   and Esc cancels it. Maya stays responsive.
   **[guess]**: `progressWindow` edited from `executeDeferred` calls while a
   worker thread runs updates on screen. `progressWindow(query=True,
   isCancelled=True)`, asked through `executeInMainThreadWithResult` from the
   worker, turns true after Esc. `cmds.file(save=True)` saves to the open
   path without a dialog.
2. **Expect** in the summary: "sm0140-render-beauty-v001   Maya render · 48
   frames · 12 units" and "sm0140-render-fg-v001   Maya render · 10 frames ·
   3 units", "files 76 found from sm0140_light_v003.ma … 76 to upload",
   estimate and due lines with a slack, local times with UTC beside them, and
   "Ready: press Submit to send it to the farm." Submit is now available.
   **[guess]** that `maya.utils.executeDeferred(fn, *args)` runs the update
   on the main thread soon after, even while Maya is idle.
3. Press **Check** and press Esc at once. **Expect:** "Check cancelled." The
   progress window closes, Check is available, Submit is greyed.
4. Check, then change Shot. **Expect:** Submit greys and the summary starts
   "Changed since the check: Check again before you submit." Change it back.
   **Expect:** Submit is available again.
5. Check, then move an object and save (Ctrl+S). **Expect:** Submit greys.
   **[guess]** that the `SceneSaved` scriptJob event fires on every save,
   and that `scriptJob(event=..., parent=window)` dies with the window.
6. Check, then move an object without saving. Press Submit. **Expect:**
   nothing is submitted. **[guess]** that the window's own controls do not
   mark the scene modified.
7. Add a render layer **rim** (heron), then press Check. **Expect:** the
   window redraws with a third checkbox, ticked, and the check has three jobs.
   The redraw is deferred with `executeDeferred`, so that Check's own callback
   does not delete the Check button. **[guess]** that deleting and re-making
   the window's lower column redraws it in place.

## Render layers (each one a [guess] about Maya's layers)

1. **Each layer's range.** With fg current in Render Setup, open the window.
   **Expect:** fg shows 1001-1010, beauty "scene range", Frames per layer.
   After the window reads the scene, fg is still the current layer and the
   scene is not marked modified if it was not before.
   **[guess]**: the code makes each layer current in turn with
   `editRenderLayerGlobals(currentRenderLayer=...)` to read its overrides of
   `defaultRenderGlobals.startFrame`/`endFrame`/`byFrameStep`, then makes the
   artist's layer current again. It then sets `cmds.file(modified=False)` if
   the scene was unmodified before. Watch how long this takes in a heavy scene,
   because switching layers applies their overrides.
2. **Layer names.** In Render Setup, layer beauty's node is `rs_beauty`. The
   job's `layer` is `rs_beauty`, and its key is `beauty`. **[guess]** that
   `Render -rl rs_beauty` renders the Render Setup layer beauty on the farm.
   If the farm's command needs `beauty`, the tool should strip `rs_`.
3. **The master layer.** Turn on the master layer's renderable switch.
   **Expect:** a "masterLayer" checkbox first, with job key `render-master`.
4. **Legacy layers.** Open a scene made with legacy render layers (Maya's
   preference "Legacy Render Layers"). **Expect:** the same window, the
   layers by their own names.
5. **Referenced layers.** Reference a scene that has its own layers.
   **Expect:** its layers (`ns:defaultRenderLayer`) are not listed.
6. **Outputs.** Hover over a layer's checkbox. **Expect:** "Renders
   …/images/fg/…fg.####.exr". **[guess]** that
   `renderSettings(genericFrameImageName="####", fullPath=True, layer=...)`
   gives the layer's first image path with `####` for the frame. Only the
   main image is listed, not Arnold's AOVs. Nothing depends on outputs today,
   because layers do not read each other.
7. **The camera.** **[guess]** that the renderable camera is the one whose
   shape's `renderable` is on (shotCam before persp). A layer that overrides
   which camera is renderable still gets the one camera the master layer
   says, because c4submit takes one camera per scene.

## What the scene reads (each one a [guess] about node attributes)

For each item, press Check and Show file. The summary's file count should
change as stated.

1. **Membership.** Untick beauty. **Expect:** the caustics frames and the
   splash standin are no longer collected (18 files). **[guess]** that
   `editRenderLayerMembers(rs_fg, query=True, fullNames=True)` lists a Render
   Setup layer's collection members, for every layer and not only the
   visible one. A layer that lists no members is taken to read everything,
   so a wrong answer collects too much, never too little.
2. **Shading.** **[guess]** that `listHistory(file, future=True)` reaches the
   shading group and `sets(sg, query=True)` lists its objects, including
   face assignments (`heron.f[0:99]`).
3. **UDIM textures.** **Expect:** the three heron tiles. **[guess]** that
   `uvTilingMode` is 3 for UDIM (Mari), 1 for ZBrush and 2 for Mudbox, and
   that `computedFileTextureNamePattern` holds `<UDIM>`. If it does not, the
   last four-digit number from 1001 up in the file name is the tile.
4. **ZBrush and Mudbox tiles** (`_u0_v0`, `_u1_v1`). **Expect:** every tile
   on disk is collected.
5. **Image sequences.** **Expect:** all ten caustics frames. **[guess]** that
   `useFrameExtension` marks a sequence. The frame is the last number in the
   file's name, at its padding, and every frame on disk is collected,
   whatever range the texture actually reads.
6. **Relative paths and variables.** **Expect:** `sourceimages/...` resolves
   against `workspace(query=True, rootDirectory=True)`, and `$SHOW/...`
   expands. Maya's own file-rule fallbacks (looking in `sourceimages` for a
   bare file name) are not followed.
7. **Arnold.** `aiStandIn.dso`, `aiVolume.filename` and `aiImage.filename`,
   with `useFrameExtension` on the first two. **Expect:** the 48 standin
   frames, the ten VDBs and the sky. An `aiImage` with `<tile>` is expanded
   to its `_u1_v1` tiles. `.tx` files that Arnold makes beside textures are
   not collected, so render with "Use existing .tx" off, or make them first.
8. **Alembic and GPU caches.** `AlembicNode.abc_File` (and `abc_layerFiles`
   for layered caches) and `gpuCache.cacheFileName`. **Expect:** collected.
9. **References.** **Expect:** the rig. A reference loaded twice is
   collected once (`withoutCopyNumber`). An unloaded reference is a warning
   when missing. Nested references are listed.
   **[guess]** about `referenceQuery(ref, filename=True,
   withoutCopyNumber=True)` and `isLoaded=True`.
10. **Everything else Maya lists.** `file(query=True, list=True,
    withoutCopyNumber=True)` adds what no node above accounts for (an OCIO
    config, a plug-in's files). A missing one is a warning, because Maya
    lists files a render may not read.
11. **A large scene** (thousands of nodes, hundreds of textures). **Expect:**
    ticking boxes stays instant and Check starts within a few seconds.
    Every scene read walks the shading networks and switches layers.

## Show file, Save file, Submit

1. After a check, press **Show file**. **Expect:** the summary box shows the
   submission (`c4: submit/1`, project, shot, due, tree, two jobs with
   `layer: rs_beauty` and `layer: rs_fg`). Press it again to get the summary
   back.
2. Press **Save file...**. **Expect:** a save dialog for `*.yaml`, opened in
   the scene's folder and named `sm0140-render-beauty.yaml`. **[guess]**
   that `fileDialog2`'s `startingDirectory` with a file name at its end
   fills in the name. If it only opens the folder, type the name. Save it.
   **Expect:** the summary says where it was saved and shows "c4 farm
   submit". The file reads back the same as Show file.
3. Press **Submit**. **Expect:** a progress window "Submitting to the farm"
   ("Uploading … of …"). Then the summary says "Submitted to the farm:" with
   `sm0140-render-beauty-v001` and `sm0140-render-fg-v001`, and how many files
   were sent. Submit greys until the next check. With the fake farm,
   `farm.submitted()` in the Script Editor shows one submission with two jobs.
