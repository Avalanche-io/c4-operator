# Checking the farm plug-in in a real Katana

The tests drive the plug-in through a fake `Katana` (`tests/fakekatana.py`)
and a fake Qt (`tests/fakekatanaqt.py`). They show that the logic is right.
They cannot show that real Katana behaves the way the fakes do. This list
covers what only a real Katana can show. Each item marked **[guess]** is a
place where the code assumes something about Katana's API that no test here
can confirm. If an item fails, note the Katana version and fix the code (and
the fake, so the tests keep matching Katana).

Run the whole list in **Katana 7 and 8** (Python 3.10 and 3.11), with
RenderMan (or the renderer the studio uses) loaded. Katana 5 and earlier
(Python 3.7) are not supported. There, expect no C4 menu entries and the line
"C4: submitting to the farm needs Katana 6 or later (Python 3.9); no C4 menus
added" in the terminal.

## Setup

1. Add `integrations/dcc/katana` to `KATANA_RESOURCES`, with
   `integrations/dcc/c4submit` beside it, as in the repository.
2. Build a show folder (for example `/tmp/show`) with the project from
   `tests/katana_scenes.py`. You need `anim/sm0140_heron.abc`,
   `assets/marsh/marsh_set.usda`, `assets/heron/heron.klf`, the tiles
   `assets/heron/tex/heron_diffuse.1001.tex`, `.1002` and `.1011`, and
   `fx/fog/fog.usd`. Real files Katana can open are needed here, because
   Katana reads them.
3. Run a farm that answers. Without a real HUD, use c4submit's fake farm over
   HTTP. In Katana's Python tab, before you open the dialog:

   ```python
   import os, sys
   from c4submit.testing import FakeFarm, SALTMARSH, serve
   sys.path.insert(0, "/path/to/integrations/dcc/katana/tests")
   from katana_scenes import KATANA_RENDER
   farm = FakeFarm(tools=[KATANA_RENDER], projects={"saltmarsh": dict(SALTMARSH, root="/tmp/show")})
   served = serve(farm)
   os.environ.update(C4_HUD=served.url, C4_PROJECT="saltmarsh", C4_SHOT="sm0140", C4_ROOT="/tmp/show")
   ```

4. Build the project:
   - Alembic_In `heron_abc` on the heron cache and UsdIn `marsh_usd` on the
     set, merged.
   - A LookFileAssign `heron_look` on `heron.klf`.
   - A NetworkMaterialCreate `heron_mat` with a PxrTexture `heron_diffuse_tex`
     on `heron_diffuse.<UDIM>.tex`.
   - A RenderSettings `settings`.
   - A Render node `Render_shadow` whose RenderOutputDefine writes
     `/tmp/show/render/sm0140/shadow/shadow.####.exr`.
   - A second branch adds a UsdIn `fog_usd` and a material `beauty_mat` with a
     PxrTexture `shadow_map` reading the shadow render, into `Render_beauty`.
     Katana should report that Render_beauty depends on Render_shadow.
   - A bypassed `Render_old`.

   Save the project as `/tmp/show/seq/sm0140/katana/sm0140_light_v002.katana`.

## Start-up and the menus

1. Start Katana. **Expect:** no errors in the terminal, and **Submit to
   C4...** on the farm menu (the main menu's Render farm entries).
   **[guess]** that Katana runs `Startup/init.py` from every
   `KATANA_RESOURCES` folder. **[guess]** that
   `Callbacks.addCallback(Callbacks.Type.onStartupComplete, fn)` calls `fn`
   with keyword arguments once the UI is up. **[guess]** that
   `FarmAPI.AddFarmMenuOption(name, callback)` puts the entry on the farm menu
   and calls `callback` with no arguments.
2. Right-click `Render_shadow` in the Node Graph and open its farm menu.
   **Expect:** **Submit this Render node to C4...**. **[guess]** that
   `FarmAPI.AddFarmPopupMenuOption` adds to a Render node's right-click
   menu. **[guess]** that `FarmAPI.GetCurrentNode()` returns the clicked
   node there. If it does not, the selected Render nodes are used, from
   `NodegraphAPI.GetAllSelectedNodes()`.
3. Start `katana --batch` with the folder on `KATANA_RESOURCES`.
   **Expect:** no errors. Registering farm menus in batch mode should be
   harmless.

## The dialog

1. Choose **Submit to C4...**. **Expect:** a dialog titled "Submit to C4"
   over Katana's main window, which does not block Katana. "Reading
   saltmarsh's tools from the farm..." shows and is soon replaced. Choosing
   it again closes the first dialog. **[guess]** that `from Katana import
   QtCore, QtWidgets` gives the Qt Katana runs, and that
   `UI4.App.MainWindow.GetMainWindow()` is the main window. Without these,
   PySide6, PySide2 or PyQt5 is tried, and the dialog has no parent.
2. **Expect**, top to bottom:
   - Tool ("Katana render"), Project (saltmarsh), Shot (sm0140).
   - Needed by (Dailies, Client review, When the farm suggests, At a time I
     type) with a greyed time field beside it.
   - Frames 1001-1048 with "linked" ticked.
   - A "Render nodes" box with "Render_shadow  project range" and
     "Render_beauty  project range  (after Render_shadow)", both ticked, and
     no Render_old.
   - Note, then Project (a file field with "...") and Render node ("each
     ticked Render node"), each with "linked".
   - A folded **Advanced** toggle (Threads, Verbose log).
   - Check, Submit (greyed), Show file (greyed), Save file... (greyed), and a
     read-only summary in a fixed-width font.
3. Hover over every widget. **Expect:** no tooltip says image, pod,
   namespace, priority, cluster or container, and nothing offers a priority.
4. Untick Render_beauty. **Expect:** Render node shows Render_shadow. Tick it
   again. **[guess]** that `QCheckBox.clicked`, `QLineEdit.textEdited` and
   `QComboBox.activated` fire only on the artist's own edits, as Qt documents.
   `valueChanged` and `textChanged` also fire on the code's edits, and the
   dialog ignores those.
5. Type "fri 18:00" in the time field. **Expect:** Needed by switches to "At
   a time I type". Choose a milestone. **Expect:** the time field greys.
6. Type 1001-1010 in Frames. **Expect:** "linked" unticks. Tick it again.
   **Expect:** Frames shows 1001-1048.

## Render nodes (each one a [guess] about FarmAPI)

1. **The list and its order.** **[guess]** that
   `FarmAPI.GetSortedDependencyList(nodes)` takes a list of Render nodes and
   returns, for each, a dict with `name`, `deps` (the Render nodes it depends
   on), `outputs` (dicts with `outputLocation` and `enabled`) and, when the
   node's farm settings give one, `range` (start, end). The list comes
   dependencies first. If it takes no list, it is called without one.
2. **Its own range.** In Render_shadow's farm settings, set an active frame
   range of 1001-1010. **Expect:** its checkbox reads "Render_shadow
   1001-1010", Frames reads "each Render node's own range", and its job has
   frames 1001-1010. **[guess]** that the farm settings range is what
   `range` carries.
3. **Bypassed.** Bypass Render_beauty. **Expect:** it is no longer listed.
   **[guess]** about `node.isBypassed()`.
4. **Job names.** **Expect:** Render_shadow's job is `render-shadow` (the
   `Render_` is dropped), named `sm0140-render-shadow-v001` by the farm.
   **[guess]** that the tool's `katana --batch --katana-file=...
   --render-node=Render_shadow -t 1001-1004` renders that node on the farm.
   That is the tool's to get right, not the plug-in's.

## Check

1. Move a node (so the project is dirty), then press **Check**. **Expect:**
   the project is saved. A progress dialog "Checking with the farm" shows
   "Reading files: n of 8", and its Cancel stops the check. Katana stays
   responsive. **[guess]**: `KatanaFile.IsFileDirty()` and
   `KatanaFile.Save(path)` work. A `QTimer` on the main thread takes the
   worker's answers every 50 ms, and nothing touches Qt off the main thread.
2. **Expect** in the summary:
   - "sm0140-render-shadow-v001   Katana render · 48 frames in 12 units".
   - "sm0140-render-beauty-v001 … after sm0140-render-shadow-v001", with an
     estimate line saying it follows shadow, which does not exist until this
     is submitted.
   - "files 8 found from sm0140_light_v002.katana … 8 to upload". The shadow
     render that beauty reads is not counted, because shadow renders it.
   - Due lines "Dailies, Thu 18:00 · slack …".
   - "Ready: press Submit to send it to the farm."
3. Press **Check**, then Cancel. **Expect:** "Check cancelled." Check is
   available, Submit is greyed.
4. Check, then change Shot. **Expect:** Submit greys and the summary starts
   "Changed since the check: Check again before you submit." Change it back.
   **Expect:** Submit is available again.
5. Check, then move a node. Press Submit. **Expect:** nothing is submitted.
   **[guess]** that the dialog's own edits do not mark the project dirty.
6. Add a Render node, then press Check. **Expect:** a third checkbox,
   ticked, and three jobs. **[guess]** that `deleteLater` on the lower half
   of the dialog, while its own Check button's signal runs, redraws it
   safely.
7. Submit only Render_beauty (from its farm menu) and Check. **Expect:** an
   error on `shadow_map.parameters.filename.value`, saying its
   `shadow.####.exr` files do not exist. Tick Render_shadow and Check again.
   **Expect:** it passes.

## What the project reads (each one a [guess] about parameters)

For each item, press Check and Show file.

1. **Parameters.** **[guess]**: `node.getParameters()` is the root group,
   and `getChildren()`, `getType()` (`"string"`), `getValue(frame)` and
   `getFullName()` (`node.path.to.param`) work as the fake has them.
   **[guess]** that `getHintString()` is a dict written as text, whose
   `widget` is `assetIdInput` or `fileInput` for a file to read and
   `assetIdOutput` for one to write.
2. **Alembic_In, UsdIn, LookFileAssign.** **Expect:** `abcAsset`,
   `fileName` and `args.lookfile.asset.value` are collected.
3. **Shading nodes.** **Expect:** a PxrTexture's `parameters.filename.value`
   is collected over its `<UDIM>` tiles, and a parameter whose `enable` is
   off is not. **[guess]** that shading node parameters come as
   `enable`/`value` pairs.
4. **Groups.** **Expect:** a material group upstream of a Render node counts
   for that node, with everything inside it. A node inside a group fed from
   the group's input reads what feeds that input. **[guess]** that
   `getInputPorts()`, `getConnectedPorts()`, `getNode()`, `getParent()` and
   `getInputPort(name)` behave this way. Inside a group, the port an inner
   node connects to belongs to the group itself (its send port), with the
   same name as the group's input. SuperTools (Gaffer, Importomatic) are
   groups too.
5. **Relative paths and variables.** **Expect:** `../cache/rel.abc`
   resolves against the project file's folder, and `$SHOW/...` expands.
   **[guess]** that Katana resolves relative asset paths the same way. A
   studio asset resolver (asset IDs that are not paths) is not resolved, so
   those files are reported missing.
6. **Known limits.** A USD stage's sublayers and references, the textures
   inside a look file, and files an expression names on frames after the
   first are not collected. Note any that a real project needs.
7. **A large project** (thousands of nodes). **Expect:** ticking boxes stays
   instant and Check starts within a few seconds. Each read walks every
   node's parameters.

## Show file, Save file, Submit

1. After a check, press **Show file**. **Expect:** the submission
   (`c4: submit/1`, two jobs, beauty `after: render-shadow`). Press it again
   to get the summary back.
2. Press **Save file...**. **Expect:** a save dialog for `*.yaml`, in the
   project's folder, named `sm0140-render-shadow.yaml`. Save it. **Expect:**
   the summary says where it was saved.
3. Press **Submit**. **Expect:** a progress dialog "Submitting to the farm",
   then "Submitted to the farm:" with both jobs and how many files were sent.
   With the fake farm, `farm.submitted()` shows one submission with two jobs.
