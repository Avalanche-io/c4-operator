# Checking the Nuke panel in a real Nuke

The tests drive the panel through a fake `nuke` module (`tests/fakenuke.py`).
They show that the logic is right. They cannot show that real Nuke behaves the
way the fake does. This list covers what only a real Nuke can show. Each item
marked **[guess]** is a place where the code assumes something about Nuke's API
that no test here can confirm. If an item fails, note the Nuke version and fix
the code (and the fake, so the tests keep matching Nuke).

Run the whole list in **Nuke 14, 15 and 16** (Python 3.9, 3.10 and 3.11).
**Nuke 13 and earlier (Python 3.7) are not supported**, because c4submit
needs Python 3.9. On Nuke 13, expect no C4 menu, the line "C4: submitting to
the farm needs Nuke 14 or later (Python 3.9); no C4 menu added" in the
terminal, and an otherwise normal start.

## Setup

1. Put `integrations/dcc/nuke` on `NUKE_PATH`. Leave `integrations/dcc/c4submit`
   beside it, as it is in the repository.
2. Build a show folder (for example `/tmp/show`) with the comp from
   `tests/nuke_scripts.py`: 48 plate frames `plates/sm0140.1001.exr`…, 48 light
   renders `render/light/sm0140_beauty.1001.exr`…, and `luts/show.cube`. Any
   small EXRs will do.
3. Run a farm that answers. Without a real HUD, use c4submit's fake farm over
   HTTP. In Nuke's Script Editor, before you open the panel:

   ```python
   import os
   from c4submit.testing import FakeFarm, SALTMARSH, serve
   farm = FakeFarm(projects={"saltmarsh": dict(SALTMARSH, root="/tmp/show")})
   served = serve(farm)
   os.environ.update(C4_HUD=served.url, C4_PROJECT="saltmarsh", C4_SHOT="sm0140", C4_ROOT="/tmp/show")
   ```

4. Build the script: `Read_plate` (`plates/sm0140.####.exr`) and
   `Read_beauty` (`render/light/sm0140_beauty.%04d.exr`) into a Merge, then
   `Write_bg` writing `comp/out/bg.%04d.exr`. Separately, `Read_bg` reading
   `comp/out/bg.####.exr`, into a Grade, into an OCIOFileTransform on
   `luts/show.cube`, then `Write_main` writing `comp/out/main.####.exr`, with
   "limit to range" set to 1001-1048. Add a disabled `Write_old`. Save it as
   `/tmp/show/comp/sm0140_comp_v004.nk`.

## Startup and menus

1. Start Nuke. **Expect:** no errors in the terminal or Script Editor. The
   menu bar has **C4 > Submit to farm...**. **[guess]** that `menu.py` gets a
   `__file__`. If it does not, the code falls back to `nuke.pluginPath()`.
2. Right-click in the Node Graph. **Expect:** **C4 > Submit this Write to C4**.
   **[guess]** that the `"Node Graph"` menu is the right-click menu, in every
   version.
3. Choose that item with nothing selected. **Expect:** the message "Select a
   Write node first, then choose Submit this Write to C4." and no panel.

## The panel

1. Choose **C4 > Submit to farm...**. **Expect:** a floating panel titled
   "Submit to C4" that does not block Nuke. "Reading saltmarsh's tools from
   the farm..." shows and is soon replaced. **[guess]** that `show()` opens
   it non-modal, and that it stays open while the module holds it.
2. **Expect**, top to bottom: Tool ("Nuke comp"), Project (saltmarsh), Shot
   (sm0140), Needed by (Dailies, Client review, When the farm suggests, At a
   time I type) with a greyed time field beside it, Frames 1001-1048 with
   "linked" ticked beside it, the heading "Write nodes" with two ticked
   checkboxes "Write_bg  script range" and "Write_main  1001-1048  (after
   Write_bg)" (no Write_old), Note, Script (a file field) and Write node
   ("each ticked Write node"), each with "linked" beside it, a closed
   **Advanced** group (Proxy mode, Threads), the buttons Check, Submit
   (greyed), Show file (greyed) and Save file... (greyed) on one row, and an
   empty summary box.
   **[guess]**: `Tab_Knob` with `TABBEGINCLOSEDGROUP`/`TABENDGROUP` draws a
   closed, collapsible group inside a `PythonPanel`. `Text_Knob(name, label)`
   draws a labelled divider. `clearFlag(STARTLINE)` puts "linked" and the
   buttons on the same row.
3. Read every label and tooltip. **Expect:** none says image, pod,
   namespace, priority, cluster or container, and nothing offers a priority.
4. Open **Advanced**, set Threads to 8. Untick and tick a Write. **Expect:**
   no errors. "linked" beside Write node stays ticked. **[guess]**:
   `Int_Knob.setRange` exists. If it does not, it is skipped.
5. Type "fri 18:00" in the time field. **Expect:** Needed by switches to "At
   a time I type" and the field stays enabled. **[guess]**:
   `Enumeration_Knob.values()` and `setValues()` exist and behave as the fake
   does. A milestone label holding "/" is not split into a submenu.
6. Type 1001-1010 in Frames. **Expect:** "linked" unticks. Tick it again.
   **Expect:** Frames shows 1001-1048. **[guess]** that the code's own
   `setValue` calls may fire `knobChanged`. The panel ignores them while it
   updates, so typed values are never lost. Watch for a field that will not
   keep what you type.

## Check

1. Make a small edit to the script (so it is modified), then press **Check**.
   **Expect:** the script is saved (the title bar loses its modified mark).
   A Nuke progress bar "Checking with the farm" shows "Reading files: n of
   98" and has a Cancel button. Nuke stays responsive.
   **[guess]**: a `ProgressTask` made on a worker thread shows and closes
   when the thread deletes it. `nuke.scriptSave()` saves to the open path
   without a dialog.
2. **Expect** in the summary: "sm0140-comp-bg-v001   Nuke comp · 48 frames ·
   12 units", then a line for comp-main ending "after comp-bg", "files 98
   found from sm0140_comp_v004.nk … 98 to upload", estimate and due lines
   with a slack, local times with UTC beside them, and "Ready: press Submit
   to send it to the farm." Submit is now available. **[guess]** that
   `nuke.executeInMainThread(fn, args=(...))` runs the update on the main
   thread soon after. If it does not, the summary never fills.
   **[guess]** that the summary box is read-only (the `READ_ONLY` flag on a
   `Multiline_Eval_String_Knob`) yet still lets you select and copy text.
3. Press **Check** again and Cancel at once. **Expect:** "Check cancelled."
   Check is available, Submit is greyed.
4. Check again, then change Shot. **Expect:** Submit greys and the summary
   starts "Changed since the check: Check again before you submit." Change it
   back. **Expect:** Submit is available again.
5. Check, then move a node in the script (so it is modified). Press Submit.
   **Expect:** nothing is submitted. **[guess]** that the panel's own knobs
   do not mark the script modified. If they do, Submit would always be
   refused after a check.
6. Add a new Write node, then press Check. **Expect:** the panel redraws with
   a third checkbox, ticked, and the check has three jobs. **[guess]** that
   removing and re-adding knobs while the panel is open redraws it. The
   redraw is deferred with `executeInMainThread` so that Check's own
   callback does not remove the Check button.

## Show file, Save file, Submit

1. After a check, press **Show file**. **Expect:** the summary box shows the
   submission (`c4: submit/1`, project, shot, due, tree, jobs). Press it again
   to get the summary back.
2. Press **Save file...**. **Expect:** a save dialog for `*.yaml`, opened in
   the script's folder, named `sm0140-comp-bg.yaml`. Save it. **Expect:** the
   summary says where it was saved and shows "c4 farm submit". The file reads
   back the same as Show file. **[guess]** about `nuke.getFilename`'s
   `default=` and `type="save"` arguments.
3. Press **Submit**. **Expect:** a progress bar "Submitting to the farm"
   ("Uploading … of …"). Then the summary says "Submitted to the farm:" with
   `sm0140-comp-bg-v001` and `sm0140-comp-main-v001`, and how many files were
   sent. Submit greys until the next check. With the fake farm,
   `farm.submitted()` in the Script Editor shows one submission with two jobs,
   main `after` bg.

## Right-click on a Write

1. Select `Write_bg` and choose **C4 > Submit this Write to C4**. **Expect:**
   the panel opens with only Write_bg ticked, and Write node shows
   "Write_bg".
2. Do the same with `Write_main` alone, then Check. **Expect:** an error on
   `Read_bg` saying its `bg.####.exr` files do not exist (nothing renders
   them in this submission). Tick Write_bg and Check. **Expect:** it passes.

## What the script reads (each one a [guess] about knob names)

For each item, press Check and Show file. The tree's file count in the
summary should change as stated.

1. **Writes in a Group.** Put a Write inside a Group fed by `Read_bg`.
   **Expect:** a checkbox `Group1.Write1 … (after Write_bg)`. On the farm,
   `nuke -X Group1.Write1` must render it. **[guess]** that `-X` takes a
   dotted name; confirm on a render node.
2. **Gizmos.** A gizmo holding a Read. **Expect:** the Read's files are
   collected. **[guess]** that `allNodes(recurseGroups=True)` goes inside
   gizmos. Gizmos themselves must be installed on the farm, because they
   are not collected.
3. **DeepRead/DeepWrite, ReadGeo/WriteGeo.** **Expect:** DeepWrite and
   WriteGeo get checkboxes. A DeepRead's or ReadGeo's file is collected over
   its own frame range. **[guess]** that the range knobs are `first`/`last`
   (and `range_first`/`range_last` on ReadGeo), and that DeepWrite/WriteGeo
   use `use_limit`/`first`/`last` as Write does.
4. **Camera from file.** A Camera (Camera2/3/4, whatever the version makes)
   with "read from file" on an `.abc`. **Expect:** collected. With it off:
   not collected. **[guess]** that the knob is `read_from_file` in every
   version, including Nuke 15's new 3D camera.
5. **Vectorfield, OCIOFileTransform.** **Expect:** their LUT files are
   collected. The code takes every knob whose `Class()` is `"File_Knob"` on
   any node that is not a Write, GenerateLUT or Viewer. **[guess]** that no
   other common node has a File_Knob that is an output. If one does, add it
   to `NOT_INPUTS` in `host.py`.
6. **Expressions in paths.** A Read whose file is
   `[file dirname [value root.name]]/../plates/sm0140.####.exr`. **Expect:**
   collected, from the evaluated path, with `####` kept. **[guess]** that
   `nuke.filename(node)` evaluates expressions and keeps frame tokens. For
   other file knobs, `knob.evaluate()` is used only when the value has `[`,
   because it may fill in the current frame.
7. **Relative paths.** Set Project Settings > project directory to the show
   and a Read to `plates/sm0140.####.exr`. **Expect:** collected. **[guess]**
   that relative paths resolve against `project_directory` (evaluated), or
   else against the script's folder.
8. **Read file on a Write.** Turn on "read file" on `Write_bg` and feed a
   new Write from it. **Expect:** the new Write is "after Write_bg".
   **[guess]** that the knob is named `reading`.
9. **Expression links.** A node linked to a Read only by an expression.
   **Expect:** the Read is collected for that Write. **[guess]** about
   `node.dependencies(nuke.EXPRESSIONS)`.
10. **Stereo.** Views left/right and a Read on `%V/sm0140_%v.####.exr`.
    **Expect:** both views' files are collected. **[guess]** about
    `nuke.views()`.
11. **Custom OCIO config.** Project Settings > Color > OCIO config "custom",
    with a config whose `search_path` names a `luts` folder. **Expect:** the
    config and every file under `luts` are collected. **[guess]** that the
    root knobs are `OCIO_config` (value `"custom"`) and
    `customOCIOConfigPath` in Nuke 14 to 16. A Windows config's
    `search_path` separator is not checked either.
12. **Precomp.** A Precomp node. **Expect:** its `.nk` is collected. Its own
    Reads are **not** collected (a known limit), so they must already be on
    the farm.
13. **Disabled by expression.** A Write whose disable knob holds an
    expression. **Expect:** it is left out or included by the value at the
    current frame.
14. **A large script** (a few thousand nodes). **Expect:** ticking boxes
    stays instant and Check starts within a second or two. Every scene read
    walks the whole graph.

## The nuke-render tool (each one a [guess] about Nuke's command line)

Run these on the studio's Nuke image (or a render machine) after setting
`runs.image` in `tools/nuke-render/tool.yaml`. `c4 farm tool test
tools/nuke-render` runs the tool's test as a unit runs it.

1. **The test.** **Expect:** `c4 farm tool test` passes, with
   `comp/out/test.1001.exr` and `test.1002.exr` written, and two
   `c4_frame:` lines in the log. **[guess]** that `test/comp/test.nk`, a
   hand-written script (a Constant into a Write whose file is
   `[file dirname [value root.name]]/out/test.####.exr`), opens in Nuke 14
   to 16.
2. **Terminal mode.** **[guess]** that `nuke -m 4 -t nuke_render.py SCRIPT
   WRITE PROXY` runs the script with `sys.argv` as `[nuke_render.py, SCRIPT,
   WRITE, PROXY]`, as `__main__`, and exits with its `sys.exit` code.
   **[guess]** that `-t` takes a render licence (`nuke_r`) as `-x` does,
   not an interactive one; check the licence server's log. **[guess]**
   that `-m` sets the threads `nuke.execute` uses.
3. **One Write, frame by frame.** **[guess]** that `nuke.scriptOpen(path)`
   opens the script without a dialog, and `nuke.execute(write, f, f, 1)`
   renders that Write alone, as `-X` does. **Expect:** the other Writes
   write nothing. If one `execute` per frame is much slower than one per
   unit, note how much: the script can render a unit's run of frames at
   once and report the frames after.
4. **The file each frame writes.** **[guess]** that `nuke.filename(write)`
   keeps `####` and `%04d` (and gives the proxy file in proxy mode), so the
   script can check each frame's file. **Expect** a `c4_frame:` line per
   frame with the real path. A Write with "create directories" off still
   renders, because the script makes the folder first.
5. **A Write inside a Group.** Submit `Group1.Write1`. **Expect:** it
   renders. **[guess]** that `nuke.toNode("Group1.Write1")` finds it.
6. **A missing input.** Delete a plate frame and render. **Expect:** the
   unit fails with `c4_fail: permanent Nuke failed on frame …`, not
   retried. **[guess]** that `nuke.execute` raises with Nuke's "Read error"
   or "No such file or directory" text. Note the exact text; if it differs,
   update `MISSING_INPUT` in `nuke_render.py`, and add it to the tool's
   `errors.permanent`.
7. **Path mapping.** Submit from a Windows machine whose project folder is
   `S:\show`. **Expect:** Reads and the Write resolve under the job's tree.
   **[guess]** that `nuke.addFilenameFilter(fn)` applies to every file knob
   Nuke reads and writes, from before `scriptOpen`.
8. **A home folder it cannot write.** Units run as an unprivileged user.
   **[guess]** that Nuke starts with a read-only `$HOME` (its `.nuke`
   folder). If it does not, set `NUKE_TEMP_DIR` and `HOME` in `runs.env`.
9. **The licence error.** Run with no licence reachable. Note Nuke's exact
   message and add it to the tool's `errors.licence`, so the farm waits
   for a seat instead of spending a retry.
