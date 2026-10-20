# Prebuilt tools

These are the farm's prebuilt tools, in the tool format (`c4: tool/1`).
`c4 farm up` publishes them farm-wide, so every project's Submit panel,
`c4 farm submit` and MCP offer them from the start. They are the eight job
types of `internal/farmcli/types.go`, one folder each: a `tool.yaml`, the
script it runs, and a `test/` tree its `test:` block runs against.

| tool        | kind     | image          | what it does                                                                                              |
| ----------- | -------- | -------------- | --------------------------------------------------------------------------------------------------------- |
| `render`    | `frames` | `c4/job-hydra` | renders a USD stage through a Hydra delegate (Cycles in the image) to OpenEXR, through its RenderSettings |
| `usd`       | `once`   | `c4/job-usd`   | flattens a USD stage into one layer and checks it with usdchecker                                         |
| `sim`       | `frames` | `c4/job-sim`   | bakes a fluid sim to OpenVDB in one unit: Blender Mantaflow for a .blend, Houdini for a .hip              |
| `comp`      | `frames` | `c4/job-oiio`  | runs a comp: a Nuke script on an image with Nuke, or an OpenImageIO shell script anywhere                 |
| `transcode` | `once`   | `c4/job-media` | an ACES-managed review movie (ProRes 422 HQ, ProRes 4444, DNxHR HQX) and a contact sheet                  |
| `ci`        | `shards` | `c4/job-ci`    | runs a repository's pytest and Go tests in shards, JUnit per shard                                        |
| `lora`      | `train`  | `c4/job-train` | fine-tunes a LoRA adapter on captioned frames, checkpointing and resuming                                 |
| `infer`     | `frames` | `c4/job-infer` | runs a model over every frame (depth, matte, 2x upscale), with its AI use declared                        |

## How the scripts are written

- Each `runs.command` is `bash {{Tool.Dir}}/<tool>.sh` with the tool's
  parameters as arguments, so the command line in `tool.yaml` is the
  script's whole interface. `PATH` parameters arrive as absolute paths in
  the job's tree. Outputs go under `$C4_TREE`, never a fixed mount path.
- A frames tool's script prints `c4_frame: <frame> <path>` for each frame it
  finished, after checking the file is there, and `previews` names the
  frames for the HUD. No script calls `c4-unit preview`.
- A failure the script can name is printed as `c4_fail: permanent <reason>`
  (a missing scene, an unknown renderer) or `c4_fail: retry <reason>` before
  it exits. `errors` patterns are only those seen from the real program.
- Sequences are given with `$F4` for the frame (`plates/sm0140/sm0140.$F4.exr`).
  `transcode` and `infer` also take `<F4>`, `%04d`, a folder of
  `<name>.####.exr` frames, or one frame's file, as the Go types did.

To run a script by hand as the runner would, mount a copy of its test tree
at `/show` and the tool folder read-only:

```sh
cp -R tools/render/test/. /tmp/t && chmod -R a+rwX /tmp/t
docker run --rm --user 65532:65532 -v /tmp/t:/show -v "$PWD/tools/render:/etc/c4/tools:ro" \
  -e C4_TREE=/show -e C4_FRAME_START=1001 -e C4_FRAME_END=1002 c4-job-hydra:dev \
  bash /etc/c4/tools/render.sh /show/shot/test.usda cycles 64
```

## What changed from types.go

- `usd` fails the job when usdchecker finds a problem. The Go script piped
  usdchecker into `tee` under `sh -e`, so a failed check passed.
- `sim` runs Blender with `--python-exit-code 1`, so an error in the bake
  fails the unit instead of exiting 0, and it takes a `.py` that builds the
  scene, which is how its test makes a scene without a binary `.blend` in git.
- `transcode` takes its OpenColorIO view as a parameter (the Go script fixed
  it), maps 23.976, 29.97 and 59.94 to exact rates, and takes its frames as
  a `range` parameter, since a `once` unit has no frames of its own.
- `infer` refuses one task's model for another (the Go type would pass
  depth-anything-v2-small to a matte).
- `lora`'s `base` is a model folder in the tree if there is one, else a
  Hugging Face model ID, as `c4-train-lora` accepts; the Go script always
  prefixed `/work/`.
- The fixed mount paths (`/show`, `/src`, `/work`) are gone: every script
  uses `$C4_TREE`, and `ci`, `comp` and `lora` run in `{{Job.Tree}}`.
- `ci`'s test repository is Python only. These folders are embedded in
  `c4-farm` for `c4 farm up`, and Go embeds no folder holding its own
  `go.mod`; `ci.sh` still runs a repository's Go tests.

## Open questions for c4-authoring

1. **Image names.** `runs.image` is `c4/job-<name>`, after TOOL.md's
   `c4/runtime-oiio`. Something must map it to
   `ghcr.io/avalanche-io/c4-job-<name>:<version>` (and `c4-job-<name>:dev`
   on a local farm) before publishing pins the digest.
2. **A whole range in one unit.** The `sim` type's chunk was `all`: one unit
   over the job's frames, because a sim cannot start mid-range. `frames.chunk`
   is a number, so `sim` says 1000, the render class's largest chunk; a sim
   longer than 1000 frames would split and the second unit would start cold.
   It needs `chunk: all` or an equivalent.
3. **Class parameters.** The classes read params of their own: render's
   `pass`, ci's `shards` and `target`, ml's `steps`, `checkpointEvery` and
   `ranks`. These tools declare them as ordinary parameters with the same
   names, and assume the lowering passes a parameter whose name the class
   reads to the class as well as to `C4_PARAM_*`. That is how `ci` takes its
   shard count, and how `lora`'s steps reach `C4_STEPS`. TOOL.md section 9
   maps only `frames` to class params. `lora.sh` falls back to
   `C4_PARAM_STEPS` and `C4_PARAM_CHECKPOINTEVERY` where the class's
   variables are absent, as under `c4 farm tool test`.
4. **Frames for a `once` tool.** A `once` job lowers with frames `"1"`
   (SUBMIT.md 7.1), so `transcode` takes its frames as a `RANGE_EXPR`
   parameter named `range` (`from: shot.frames`). It is not named `frames`,
   to keep clear of a submission's own `frames`. `c4 farm submit transcode
   ... --frames` would have to fill it.
5. **`needs`.** The Go types requested no resources, and the lowering
   writes requests and limits, so any number set here becomes a ceiling
   that throttles a render or kills a sim that runs unlimited today. Only
   `transcode` declares something (`scratch` for its intermediate TIFFs).
   cpu and memory wait for measured runs on the local farm.
6. **Network.** `infer` and `lora` download model weights from Hugging Face
   on first use, and the `lora` and `infer` tests do too. Units are unsealed
   today, but tool/1 is sealed by default and slice 1 refuses
   `needs.network`, so neither declares it. Before tools are sealed they
   need a named service (`huggingface`) that `c4 farm up` defines, or the
   weights in the tool's tree (TOOL.md 7.3) or a cache. `ci` on a real
   repository needs a module or package proxy for the same reason; its test
   repository has no dependencies.
7. **How values arrive.** These scripts assume a `BOOL` arrives as `true` or
   `false` (`usd.sh` also accepts `1` and `yes`), and that
   `{{Job.Shot}}` with no shot either drops its argument or passes `""`:
   `transcode` and `infer` take it last and handle both.
8. **`previews` and `writes` cannot follow a parameter.** `infer` writes to
   `depth/`, `matte/` or `upscale/` by its `task`, so `previews` names
   `depth/` and the other passes reach the preview only through their
   `c4_frame` paths. `sim` writes its cache beside the scene, so its `writes`
   is a glob, `**/cache/`; TOOL.md shows globs only in `previews`.
9. **A default that depends on another parameter.** `infer`'s model should
   follow its task (birefnet for a matte). TOOL.md has no way to say so, and
   `aiUsage` takes `{{Param.model}}` as given, so the model stays explicit
   and the script refuses a mismatch.
10. **Studio images for licensed branches.** `comp`'s Nuke branch and `sim`'s
    Houdini branch need images with Nuke or Houdini, and tool/1 has no
    per-submission image (the Go types' `--image`). A studio copies the
    folder and changes `runs.image`. The scripts fail with a reason when
    the program is missing.
11. **The samples' comp scripts.** `hack/samples` writes `comp.sh` and
    `comp.nk` reading `/in/render/` and writing `/show/comp/`. With one tree
    per job there is no `/in`: they should read `render/` in the tree.
12. **SUBMIT.md 5.1's movie.** Its `transcode` job gives `{codec, view,
    slate}` and no plates or range. This `transcode` needs `plates` and
    `range`, has no slate, and "ACES 1.0 - SDR Video" is not a view in
    `ocio://studio-config-latest` (its Rec.709 views are listed in
    `allowedValues`). The example and this tool should agree.
13. **Reuse.** `ci` says `never`: its JUnit files record times, so no two
    runs write the same bytes, and real repositories have flaky tests (the
    sample has one on purpose, left out of this test tree). `sim` says
    `exact`, though a threaded Mantaflow bake may not repeat bit for bit; the
    farm's re-run sampling would show it.
