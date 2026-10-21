# Checking the Blender add-on in a real Blender

The tests drive the add-on through a fake `bpy` (`tests/fakebpy.py`). They show
that the logic is right. They cannot show that real Blender behaves the way the
fake does. This list covers what only a real Blender can show. Each item marked
**[guess]** is a place where the code assumes something about Blender's API
that no test here can confirm. If an item fails, note the Blender version and
fix the code (and the fake, so the tests keep matching Blender).

Run the whole list in **Blender 4.2 LTS** and **Blender 4.5 LTS**.

## Setup

1. Build a show folder (for example `/tmp/show`) like `tests/blender_scenes.py`:
   - `assets/tex/wood.1001.exr`, `wood.1002.exr` and `wood.1011.exr`;
   - `fx/smoke/smoke_0001.png` to `smoke_0060.png`;
   - `assets/props/props.blend`, holding a material with an Image Texture on
     `//tex/props_diffuse.png` (that file beside it);
   - `assets/cache/sh010_anim.abc`;
   - `fx/vdb/explosion_0001.vdb` to `explosion_0012.vdb`;
   - `assets/fonts/title.ttf`.

   Any small files of the right kind will do.
2. Run a farm that answers. Without a real HUD, use c4submit's fake farm over
   HTTP, from Blender's Python Console, before you open the panel:

   ```python
   import os, sys
   sys.path.insert(0, "<repo>/integrations/dcc/c4submit/src")
   sys.path.insert(0, "<repo>/integrations/dcc/blender/tests")
   from c4submit.testing import FakeFarm, SALTMARSH, serve
   from blender_scenes import BLENDER_RENDER
   farm = FakeFarm(tools=[BLENDER_RENDER], projects={"saltmarsh": dict(SALTMARSH, root="/tmp/show")})
   served = serve(farm)
   os.environ.update(C4_HUD=served.url, C4_PROJECT="saltmarsh", C4_SHOT="sh010", C4_ROOT="/tmp/show")
   ```

   (`blender_scenes` imports `fakebpy` only to build test files. Importing it
   does not replace Blender's `bpy`.)
3. Build the file `/tmp/show/sh010/lighting/sh010_lighting_v003.blend`:
   - Scene `sh010`, frames 1001-1048, a camera `CAM_sh010`, view layers
     `bg`, `fg` and `old` (`old` with Use for Rendering off), output
     `//render/sh010_` as OpenEXR.
   - A material with an Image Texture on the wood UDIM set (tiled).
   - A material with an Image Texture on the smoke sequence, its Start Frame
     at 1001 and its Frames at 48.
   - A World with an Environment Texture on `//tex/sh010_matte.png`.
   - A linked (not appended) material from `props.blend`.
   - A Mesh Sequence Cache on the Alembic file, a Volume object on the VDB
     sequence (Sequence on, Start 1001, Frames 10), and a Text object in
     `title.ttf`.
   - A packed texture (Image > Pack) and a reference image on an Empty.
   - A second scene, `sh010_comp`, with view layer `comp`, Use Nodes on, and
     an Image node on `//render/sh010_0001.exr` as a sequence.

## Installing

1. In Preferences > File Paths > Script Directories, add
   `<repo>/integrations/dcc/blender`. Restart Blender. In Preferences >
   Add-ons, find **C4 Farm** and enable it. **Expect:** it enables with no
   error in the system console. **[guess]** that Blender loads an add-on from
   the `addons/` folder of a script directory in 4.2 and 4.5.
2. Disable it, then run `python integrations/dcc/blender/build_addon.py` and
   choose Preferences > Add-ons > **Install from Disk...** on the zip.
   **Expect:** it installs as a legacy add-on (not an extension) and enables.
   In the Python Console, `import c4submit; c4submit.__file__` is inside
   the installed add-on's `vendor` folder. **[guess]** that 4.2 and 4.5 still
   install a zip with `bl_info` and no `blender_manifest.toml` as a legacy
   add-on, and allow it to add its `vendor` folder to `sys.path`.
3. With the add-on enabled, choose Blender > System > Reload Scripts.
   **Expect:** no error, and the panel still works.

## The panel

1. Open the file and go to Properties > Render. **Expect:** a **C4 Farm**
   panel. "Reading saltmarsh's tools from the farm..." shows and is soon
   replaced. **[guess]**: a panel's `draw` may register a `bpy.app.timers`
   function. The timer starts the panel. A `draw` must not write to the
   file's data.
2. **Expect**, top to bottom: Tool (Blender render), Project (saltmarsh),
   Shot (sh010), Needed by (Dailies, Client review, When the farm suggests,
   At a time I type), Frames 1001-1048 with a link toggle beside it, "View
   layers" with a refresh button, and checkboxes "bg  1001-1048" and
   "fg  1001-1048" (both ticked) and "sh010_comp / comp  1001-1048  (scene
   sh010_comp · after bg, fg)" (unticked). There is no `old`. Then Size,
   Note, Blender file (a file field, linked), Scene ("sh010", linked), View
   layer ("each ticked view layer", linked), Render engine (a menu), a folded
   **Advanced** section,
   Check and Submit (greyed), Show file and Save file... (greyed), and the
   summary box.
   **[guess]**: `UILayout.panel(idname, default_closed=True)` (Blender 4.1
   and later) returns `(header, body)`, with `body` None while it is folded.
   The icon names `LINKED`, `UNLINKED`, `FILE_REFRESH`, `VIEWZOOM`, `EXPORT`,
   `CANCEL`, `COPYDOWN`, `ERROR` and `INFO` exist; an unknown icon raises
   in `draw`.
3. Open the menus. **Expect:** each lists its choices, and choosing one
   sticks. **[guess]**: an `EnumProperty` whose `items` is a function, with
   `get` and `set`, shows the right choice, and Python keeping the item
   strings alive (`panel._keep`) is enough.
4. Read every label and tooltip. **Expect:** none says image, pod,
   namespace, priority, cluster or container, and nothing offers a priority.
5. Type "fri 18:00" in Needed by's time field (choose "At a time I type"
   first). **Expect:** the field stays, and Needed by shows "At a time I type".
6. Type `1001-1010` into Frames. **Expect:** the link toggle turns off.
   Turn it on. **Expect:** 1001-1048 again. **[guess]**: a property's
   `update` callback runs when Python sets the property, not only when the
   artist does (the panel hushes its own writes for this reason).
7. Open **Advanced**. **Expect:** Camera (CAM_sh010, linked), Samples and
   Denoise. Type into Camera. **Expect:** its link toggle turns off.
8. Add a view layer `fx` in the View Layer menu, then move the mouse over
   the panel. **Expect:** "fx" appears, ticked, without pressing refresh.
   Rename it. **Expect:** the panel follows.

## Check

1. Make an edit (move an object) so the title shows unsaved changes, then
   press **Check**. **Expect:** the file is saved (the unsaved mark goes),
   the summary shows "Checking with the farm..." then "Reading files: n of
   72", the cursor shows progress, and Blender stays usable (orbit the
   viewport) while it runs. **[guess]**: `bpy.ops.wm.save_mainfile()` saves
   in place from an operator's `execute` and returns `{'FINISHED'}`. The
   check operator has no UNDO flag, so finishing it does not mark the file
   unsaved again. `window_manager.progress_begin`/`progress_update`/
   `progress_end` work while the operator is modal.
2. **Expect** the summary: "To fix" is absent. It lists 72 files found from
   `sh010_lighting_v003.blend` and 72 to upload, then
   "sh010-render-bg-v001   Blender render · 48 frames in 12 units" and the
   same for fg, each with an estimate line and, with Dailies picked, a due
   line ending "Dailies, Thu 18:00 · slack ...". There are no licence lines.
   It ends "Ready: press Submit to send it to the farm."
   **[guess]** in reading the file:
   - `bpy.utils.blend_paths(absolute=True)` lists a sequence by its first
     frame's file, a tiled texture with `<UDIM>` or a tile number, and a
     cache by its folder. It includes no output paths, or only ones the
     panel recognises as outputs.
   - `bpy.path.abspath(path, library=lib)` resolves a linked data-block's
     `//` from its library's folder. A library inside a library resolves
     from `Library.parent`.
   - `Image.source` is `FILE`, `SEQUENCE`, `MOVIE`, `TILED`, `GENERATED` or
     `VIEWER`. `Image.type` `RENDER_RESULT`/`COMPOSITING` are not files.
     `packed_file` or `packed_files` marks a packed texture.
   - An Image Texture node's `image_user` has `frame_start`,
     `frame_duration`, `frame_offset` and `use_cyclic`. Before its first
     frame it holds file 1 (the code's guess) and after its last it holds
     the last. The compositor's Image node keeps the same four on the node.
   - `Volume.is_sequence`, `frame_start`, `frame_duration`, `frame_offset`
     and `sequence_mode` (`CLIP`, `EXTEND`, `REPEAT`, `PING_PONG`), and
     `CacheFile.is_sequence`, mean what their names say.
   - The sequencer's strips are `sequence_editor.sequences_all` (4.2) or
     `strips_all` (later versions). An image strip has `directory` and
     `elements[].filename`, a movie strip `filepath`.
   - The compositor is `scene.node_tree` with `use_nodes` on (4.x), or
     `scene.compositing_node_group` (5.x).
   - A File Output node has `base_path`, `format.file_format` and
     `file_slots[].path`/`use_node_format`, and writes `####` and an
     extension when the slot's path has none.
   - The render output gets four `#` when its name has none, and the
     format's extension when "File Extensions" is on.
   - `ViewLayer.use` is "Use for Rendering".
3. Compare the collected files with the folder. **Expect:** smoke frames
   1-48 only (not 49-60), VDB frames 1-10 only, the three wood tiles,
   `props_diffuse.png` from `assets/props/tex`, no packed texture.
4. Press **Check** and then Esc at once. **Expect:** "Check cancelled.", and
   Check is available again. **[guess]**: a modal operator returning
   `PASS_THROUGH` leaves Blender usable while Esc reaches it, and
   `event_timer_add` keeps calling `modal` with nothing else happening.
   Opening another file while it runs may be refused while the operator is
   modal; that is acceptable.
5. Tick `sh010_comp / comp`, Check. **Expect:** three jobs. The comp job's
   heading ends "after sh010-render-bg-v001, sh010-render-fg-v001". In place
   of its estimate, a warning says it follows a job that does not exist until
   this is submitted. The check still passes. Untick bg
   and fg, Check. **Expect:** an error on `sh010_render` saying the render's
   files are missing, with its fix on the line under it.

## Submit

1. After a passing check, move an object. **Expect:** the summary starts
   "Changed since the check: Check again before you submit." and Submit is
   greyed. **[guess]**: `bpy.data.is_dirty` becomes true on the artist's
   edits and false again after `save_mainfile`. Writing the panel's own
   properties from a timer, outside an undo step, does not set it.
2. Check again, then press **Submit**. **Expect:** "Submitting to the
   farm...", then "Submitted to the farm:" with one line per job and "72
   files sent". In the Python Console, `farm.jobs[-1]["jobs"]` has
   `render-bg` and `render-fg` with `scene` `sh010` and `view_layer` `bg` and `fg`.
   Submit is greyed until the next check.
3. Check and Submit again. **Expect:** "Nothing to send: the farm had every
   file." and `-v002` jobs.

## The file

1. After a check, press **Show file**. **Expect:** the summary box shows the
   submission (`c4: submit/1`...), the button reads "Show summary", and a
   copy button appears beside it. Press copy, paste in a Text Editor.
   **Expect:** the same text. **[guess]**: `window_manager.clipboard` can be
   written.
2. Press **Save file...**. **Expect:** Blender's file browser, filtered to
   `.yaml`, with `sh010-render-bg.yaml` in the file's folder. Save as
   `test` in another folder. **Expect:** `test.yaml` is written and the
   summary says "Saved .../test.yaml." and how to submit it.
   **[guess]**: `window_manager.fileselect_add(op)` with `filepath`,
   `filter_glob` and `check_existing` behaves as an export operator's
   browser does.
3. Save the .blend, quit, and open it again. **Expect:** the panel keeps the
   ticked view layers, Note, Needed by and the typed parameters.
   **[guess]**: `PropertyGroup.id_data` is the scene for items in the
   panel's collections, and `Scene.as_pointer()` stays the same for the
   session. A `load_post` handler marked `@persistent` runs on every file
   open, and timers from the previous file are dropped.
4. File > New, then look at the panel. **Expect:** it starts again for the
   new file. With the untitled file, Check shows "could not save the file:
   the file has never been saved; save it with File > Save As first".

## Farm problems

1. Unset `C4_HUD` (restart Blender without it). **Expect:** the summary says
   "the farm's address is not set: set C4_HUD to the HUD's URL", and a
   button reads the farm's tools again.
2. With a farm that does not serve tools, and `C4_TOOL_PATH` set to a folder
   holding `blender-render/tool.yaml`. **Expect:** the form is drawn. Check
   says "Worth knowing: this farm does not take submissions yet...", Submit
   stays greyed, and Save file works.
3. Make the summary's errors long (a long path). **Expect:** rows wrap to the
   panel's width, roughly. **[guess]**: about 7 pixels per character at UI
   scale 1.

## The farm's tool (`tools/blender-render`)

The tool's tests (`tests/test_blender_tool.py`) run its script against a fake
`bpy`. Its own test (`c4 farm tool test`) passes in `c4/job-sim`'s Blender
5.2.2. This is what is left to check in a studio's Blender images and on the
farm.

1. In a studio image with Blender 4.2 LTS, then 4.5 LTS, run
   `c4 farm tool test --image <image> integrations/dcc/blender/tools/blender-render`.
   **Expect:** "expect: 2 of 2 files" and `c4_frame` lines for
   `shot/render/fg/sh010_1001.png` and `sh010_1002.png`. Then
   `--with engine=EEVEE` on a GPU image. **Expect:** the same files.
   **[guess]**: EEVEE is `BLENDER_EEVEE_NEXT` from 4.2 until 5.0 and
   `BLENDER_EEVEE` from 5.0; EEVEE renders headless (`-b`) with a GPU and no
   display.
2. `--with scene=sh020`, then `--with view_layer=fx`, then
   `--with camera=Cube`. **Expect:** each fails at once, as permanent, naming
   what the file has ("its scenes are sh010", "its view layers are bg, fg",
   "the file has no camera Cube").
3. Submit the Setup's lighting file from the panel (bg and fg) to a farm
   with the tool published. **Expect:** bg's frames in
   `sh010/lighting/render/bg/sh010_1001.exr` and so on, fg's in `.../fg/`,
   and each bg frame shows only bg's collections. **[guess]**:
   - turning a view layer's `use` off for the run renders only the job's;
   - `bpy.ops.render.render(animation=True, scene=name)` renders the named
     scene in background mode even when it is not the file's active scene;
   - a one-frame animation (frame start and end set to the frame) names its
     file as the whole animation would, and `RenderSettings.frame_path(frame=f)`
     is that file.
4. Tick `sh010_comp / comp` too and submit. **Expect:** the comp job waits for
   bg and fg. **[guess]**: its compositor's Image node finds the renders it
   reads; it reads `//render/sh010_####.exr`, which a scene with two view
   layers now writes per view layer, so a comp reading a multi-layer scene's
   render may need its path pointed at one view layer's folder.
5. A scene with a compositor File Output node. **Expect:** its files are
   written where the node says (inside the job's tree when the path is `//`),
   not per view layer. This is a known gap, listed in the README.
6. A file whose rig uses a Python driver. **Expect:** it renders as Blender
   does with "Auto Run Python Scripts" off, the default. **[guess]**: that is
   how the artist sees it too; a studio that runs scripts adds `-y` in its
   copy of `blender-render.sh`.
