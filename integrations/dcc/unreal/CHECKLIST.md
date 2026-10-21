# Checking the Unreal executor in a real Unreal

The tests drive C4 through a fake `unreal` (`tests/fakeunreal.py`). They show
that the logic is right. They cannot show that Unreal behaves the way the fake
does. This list covers what only Unreal can show. Each item marked
**[guess]** is a place where the code assumes something about Unreal's
Python API that no test here can confirm. If an item fails, note the version
and fix the code (and the fake, so the tests keep matching Unreal).

Run the whole list in **Unreal Engine 5.4, 5.5 and 5.6**.

## Setup

1. Make a project `Saltmarsh` under a show folder (for example
   `/tmp/show/unreal/Saltmarsh`), like `tests/unreal_projects.py`:
   - a World Partition map `Maps/Sh010` with built lighting, and a plain
     map `Maps/Sh020`;
   - level sequences `Cinematics/sh010_seq` (playback 1001-1048) and
     `Cinematics/sh020_seq` (1001-1024);
   - a mesh `Env/Rock` in both maps, an unused asset `Env/Unused`, and a
     game mode Blueprint set in Project Settings > Maps & Modes;
   - a content plug-in `SaltTools`, with a `Props/Lamp` placed in Sh010.
2. Run a farm that answers. Without a real HUD, use c4submit's fake farm over
   HTTP, from the Output Log's Python console, before rendering:

   ```python
   import os, sys
   sys.path.insert(0, "<repo>/integrations/dcc/unreal/tests")
   from c4submit.testing import FakeFarm, SALTMARSH, serve
   from unreal_projects import UNREAL_RENDER
   farm = FakeFarm(tools=[UNREAL_RENDER], projects={"saltmarsh": dict(SALTMARSH, root="/tmp/show")})
   served = serve(farm)
   os.environ.update(C4_HUD=served.url, C4_PROJECT="saltmarsh", C4_ROOT="/tmp/show",
                     C4_SHOT_PATTERN=r"/(?P<shot>sh\d{3})_")
   ```

   (`unreal_projects` imports `fakeunreal` only to build test projects.
   Importing it does not replace Unreal's `unreal`.)
3. Queue three jobs in Window > Cinematics > Movie Render Queue:
   - `sh010_lighting` (Sh010, sh010_seq), with a custom range 1001 to
     1011 in its Output settings;
   - `sh020_lighting` (Sh020, sh020_seq);
   - a third job, disabled.

## Installing

1. Enable the Python Editor Script Plugin, Editor Scripting Utilities and
   Movie Render Queue. Add `<repo>/integrations/dcc/unreal/python` to
   Project Settings > Plugins > Python > Additional Paths. Restart.
   **Expect:** the Output Log says "C4: C4FarmExecutor is ready...".
   **[guess]**: Unreal runs `init_unreal.py` from an Additional Path (or
   from `UE_PYTHONPATH`) at startup.
2. In Project Settings > Plugins > Movie Render Pipeline, open **Default
   Remote Executor**. **Expect:** `C4FarmExecutor` is in the list. Pick it.
   **[guess]**:
   - a Python class made with `@unreal.uclass()` from
     `unreal.MoviePipelineExecutorBase` is offered there, as
     `/Engine/PythonTypes.C4FarmExecutor`;
   - Render (Remote) creates it and calls its `execute(pipeline_queue)`
     override;
   - the setting is saved as `DefaultRemoteExecutor` in `Config/DefaultEditor.ini`
     under the Movie Render Pipeline project settings' section.

## Render (Remote)

1. Move the rock (so Sh010 is unsaved) and press **Render (Remote)**.
   **Expect:** a details panel "C4 Farm: Needed by", with Project
   (saltmarsh), Shot (sh010), Tool (Unreal render), Needed by (When the
   farm suggests), Or at a time, Size (Standard) and Note.
   **[guess]**:
   - `unreal.EditorDialog.show_object_details_view(title, object, options)`
     exists, is modal, and returns True for OK;
   - `EditorDialogLibraryObjectDetailsViewOptions` has `show_object_name` and
     `allow_search`;
   - a `uproperty(str, meta={"GetOptions": "tool_options"})` draws as a menu
     filled by the `ufunction` of that name. If it does not, the field is
     plain text, and typing a milestone's label or name works the same;
   - `meta={"MultiLine": "true"}` makes Note a multi-line box.
2. Pick "Dailies, Thu 18:00" and press OK. **Expect:** Unreal's own Save
   dialog for Sh010. Save it. **[guess]**:
   - `EditorLoadingAndSavingUtils.get_dirty_map_packages()` and
     `get_dirty_content_packages()` list unsaved packages;
   - `save_dirty_packages_with_dialog(True, True)` shows the usual dialog.
3. **Expect** a progress dialog "Checking with the farm" with "Reading
   files: n of m" and a Cancel button. Editor windows stay drawn while it
   runs. **[guess]**:
   - `unreal.ScopedSlowTask` with `make_dialog(True)`, `enter_progress_frame`
     and `should_cancel()` keeps the editor responsive enough while a Python
     worker thread runs;
   - the worker gets the interpreter while the main thread waits in
     `Thread.join(0.05)`.
4. Then the summary, in a message box with Yes, No and Cancel. **Expect:**
   - "sh010-render-sh010-lighting-v001   Unreal render · 10 frames in 1
     unit" and the sh020 job with 24 frames, each with an estimate and
     "Dailies, Thu 18:00 · slack";
   - the files found from `Saltmarsh.uproject`;
   - the legend "Yes: submit to the farm. No: show the submission as a file,
     to save it. Cancel: send nothing."

   **[guess]**: `EditorDialog.show_message(title, message, AppMsgType)`
   returns an `AppReturnType` that compares equal to `AppReturnType.YES`,
   `NO`, `CANCEL` and `OK`.
5. Press **No**. **Expect:** the submission as a file (starting
   `# Saved from unreal`). Press No again. **Expect:** the summary again.
   Press No, then **Yes**. **Expect:** "Saved .../Saved/C4/sh010-render-
   sh010-lighting.yaml." and the file there.
6. Render (Remote) again, press **Yes**. **Expect:** "Submitting to the
   farm", then "Submitted to the farm:" with both jobs. In the console,
   `farm.jobs[-1]["jobs"]["render-sh010-lighting"]` has frames
   1001-1010, `map` `/Game/Maps/Sh010`, `sequence`
   `/Game/Cinematics/sh010_seq.sh010_seq` and `config`
   `C4/queue/01-sh010-lighting.utxt`. The queue is free to render again
   (not stuck "rendering"). **[guess]**:
   - `self.on_executor_finished_impl()` is the call that tells the queue
     the executor is done;
   - `is_rendering()` returning False is right for an executor that only
     submits.
7. Open `C4/queue/01-sh010-lighting.utxt`. **Expect:** a queue manifest with
   only sh010_lighting. **[guess]**:
   - `unreal.new_object(unreal.MoviePipelineQueue)`,
     `MoviePipelineQueue.duplicate_job(job)`,
     `MoviePipelineEditorLibrary.save_queue_to_manifest_file(queue)`
     (returning its path, perhaps in a tuple with the queue) and
     `convert_manifest_file_to_string(path)` behave as their names say;
   - a farm render of the manifest with `-MoviePipelineConfig` renders that
     job.
8. Change only the rock, Render (Remote), Yes. **Expect:** "1 files sent":
   only the changed package.

## Reading the queue

1. **Expect** (from step 6 of Render (Remote)) no job for the disabled one.
   **[guess]**:
   - `MoviePipelineExecutorJob.is_enabled()` exists in 5.4 to 5.6;
   - `job.job_name`, `job.map` and `job.sequence` (soft object paths, with
     `export_text()`) and `job.get_configuration()` are as used;
   - `find_setting_by_class(unreal.MoviePipelineOutputSetting)` gives the
     Output settings, with `use_custom_playback_range`, `custom_start_frame`
     and `custom_end_frame`.
2. **Expect** the sh010 job's frames to be 1001-1010 (the custom range,
   whose end frame is not rendered) and sh020's 1001-1024. **[guess]**:
   - the custom end frame and `LevelSequence.get_playback_end()` are both one
     past the last frame rendered;
   - `unreal.load_asset(object_path)` loads the sequence.
3. **Expect** `Paths.get_project_file_path()` and
   `convert_relative_path_to_full` to give the `.uproject`'s absolute path
   (the summary says "found from Saltmarsh.uproject"). **[guess]**: that
   the project path may be relative to the engine and needs converting.

## What is sent

1. After a check, compare `files` in the summary with the project. **Expect:**
   the `.uproject`, `Config/`, all of `Content/`, `Plugins/SaltTools`
   without its `Binaries/` and `Intermediate/`, and `C4/queue/`. There is
   nothing from `Saved/`, `Intermediate/`, `DerivedDataCache/` or `Binaries/`.
2. Set `C4_UNREAL_CONTENT=referenced` and check again. **Expect:** no
   `Env/Unused`; Sh010's `__ExternalActors__` packages, `Sh010_BuiltData`,
   the rock, the lamp from SaltTools and the game mode are all there.
   **[guess]**:
   - `AssetRegistryHelpers.get_asset_registry().get_dependencies(package,
     AssetRegistryDependencyOptions(...))` returns the package's
     dependencies as names, or None;
   - external actors are not among a World Partition map's dependencies,
     but are packages of their own under `__ExternalActors__/<map path>`;
   - a project plug-in's content mounts at `/<PluginName>/`;
   - editor packages are single `.uasset`/`.umap` files, perhaps with
     `.ubulk`, `.uexp` or `.uptnl` beside them.
3. Add a C++ class to the project (so the `.uproject` has `Modules`) and
   check again. **Expect:** "Worth knowing: Saltmarsh has C++ code; Binaries
   are never sent...".
4. Render the referenced submission on the farm. **Expect:** frames that
   match a local render. A missing asset here means something loaded it by
   name; send all of Content (the default) for such a project.

## Problems

1. Press Cancel in the details panel. **Expect:** nothing else happens.
2. Unset `C4_HUD` and restart. **Expect:** "the farm's address is not set:
   set C4_HUD to the HUD's URL".
3. With a farm that does not serve tools and `C4_TOOL_PATH` set. **Expect:**
   the summary begins "Worth knowing: this farm does not take submissions
   yet", with Yes showing the file to save.
4. Refuse to save in Unreal's Save dialog. **Expect:** a "not ready" box
   saying the maps or assets were not saved, with only Yes (show the file) and
   No.
5. Read every label, tooltip and dialog. **Expect:** none says image, pod,
   namespace, priority, cluster or container, and nothing offers a priority.

## The farm's tool (`tools/unreal-render`)

The tool's tests (`tests/test_unreal_tool.py`) run its wrapper against a
stand-in for `UnrealEditor-Cmd` and a queue manifest written in the shape
Unreal's export text is taken to have. No real Unreal has run it yet, so
every item here is to be checked, in Unreal 5.4, 5.5 and 5.6.

1. Set `runs.image` and `runs.exe` to the studio's Unreal image, then run
   `c4 farm tool test integrations/dcc/unreal/tools/unreal-render` on a
   machine with a GPU. **Expect:** "building the job with 01-c4test.py", then
   "rendering c4test, frames 1001-1002", two `c4_frame` lines and
   `render/c4test/C4Test_seq.1001.png` and `.1002.png`. **[guess]**:
   - the bundled Python is at `Engine/Binaries/ThirdParty/Python3/Linux/bin/python3`
     and `UnrealEditor-Cmd` at `Engine/Binaries/Linux/` (the wrapper falls
     back to `UnrealEditor` when there is no `-Cmd`);
   - `-run=pythonscript -script=<file>` runs the builder in the editor with
     the `.uproject`'s plug-ins, and `-nullrhi` is enough for it;
   - in the builder: `LevelEditorSubsystem.new_level`, the
     `EditorActorSubsystem` spawns, `LevelSequenceFactoryNew`, the camera
     cut's `set_camera_binding_id` with `get_portable_binding_id` (or
     `get_binding_id` before it), `MoviePipelineQueue.allocate_new_job`,
     `find_or_add_setting_by_class` for the deferred pass and PNG output, and
     `output_resolution` and `file_name_format` on the Output settings.
2. Open a manifest Render (Remote) wrote (`C4/queue/01-sh010-lighting.utxt`)
   beside `MANIFEST` in `tests/test_unreal_tool.py`. **Expect:** the same
   shape. **[guess]**:
   - the Output settings are declared with
     `Class=/Script/MovieRenderPipelineCore.MoviePipelineOutputSetting`, and
     their properties are in a later `Begin Object Name="..."` block (or in
     the declaration when there is only one block);
   - the properties are `bUseCustomPlaybackRange`, `CustomStartFrame`,
     `CustomEndFrame` (one past the last frame), `OutputFrameStep` and
     `OutputDirectory=(Path="...")`, and a property left at its default is
     not written;
   - the job's map appears as `/Game/Maps/Sh010.Sh010`.

   If not, fix `set_output` and `MANIFEST` together.
3. Submit sh010_lighting (1001-1010) to a farm with the tool published.
   **Expect:** ten frames under `render/sh010-lighting/` in the job's tree,
   named as the job's file name format says, and a copy of the manifest under
   the project's `Saved/MovieRenderPipeline/C4/` with the unit's range.
   **[guess]**:
   - `-MoviePipelineConfig` takes a manifest's path from the project's
     `Saved` folder, as the engine's own new-process executor passes
     `MovieRenderPipeline/QueueManifest.utxt`;
   - `MoviePipelineEntryMap?game=/Script/MovieRenderPipelineCore.MoviePipelineGameMode`
     is the startup map that executor uses, and the job's map is loaded from
     the manifest;
   - `-RenderOffscreen` renders with no display in a container, and
     `-windowed -ResX=1280 -ResY=720` do not change the output resolution;
   - an absolute `OutputDirectory` is used as given.
4. Edit the submission's frames to `1001-1005` and submit. **Expect:** five
   frames, not ten. Then `1001-1010:2`. **Expect:** 1001, 1003 ... 1009.
   **[guess]**: `OutputFrameStep` renders every Nth frame and names files by
   their own frame numbers.
5. Keep a unit's log (the job's log in the HUD). **Expect:** lines of
   `LogMovieRenderPipeline` that give progress. **[guess]**: `progress_of`'s
   two patterns (a percentage, or "frame N/M" or "N of M") are written from
   no real log; replace them, and the sample lines in the tests, with what
   Unreal prints. `c4 farm tool test --log <file>` replays a captured log
   through the tool's patterns.
6. **Expect** each `c4_frame` line soon after its file appears, and none for
   a frame still being written. **[guess]**: Movie Render Queue writes a
   frame's files (every pass and format) before it starts the next frame's,
   and the last digits of a file name are its frame (`{frame_number}`, with
   `FrameNumberOffset` 0).
7. Make a job fail (a missing map), and one that renders no frames. **Expect:**
   each unit fails and is retried, saying how many frames were written.
   **[guess]**: Unreal exits non-zero on a failed render with `-Unattended`;
   the wrapper checks the files either way.
8. A job whose config is a Movie Render Graph. **Expect:** a permanent
   failure saying the farm renders Movie Render Queue presets.
9. On a Windows workstation with Unreal 5.5 at the default place, run the
   wrapper with `runs.host.exe.windows` by hand (`python.exe unreal_render.py
   <project> map=... sequence=... config=...`, with `C4_TREE` and
   `C4_FRAME_START`/`C4_FRAME_END` set). **Expect:** the same frames, and
   `UnrealEditor-Cmd.exe` found from `Engine\Binaries\Win64`.
