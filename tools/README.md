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

## Decided in the authoring design (2026-10-20)

The format's open points these folders raised, and how they were settled
(TOOL.md and the schema follow them):

- **Images:** `c4/<name>[:tag]` is `ghcr.io/avalanche-io/c4-<name>`, at the
  tag written or else the farm's release (`schema.ImageRef`); a local farm
  uses what `c4 farm image load` put there.
- **A whole range in one unit:** `frames: {chunk: all}`, which `sim` uses.
- **Class parameters:** a parameter named `pass` (frames), `shards`,
  `target` or `latencyTarget` (shards), or `ranks`, `steps` or
  `checkpointEvery` (train) reaches the class as well as `C4_PARAM_*`.
- **Frames for a `once` tool:** a submission's `frames` fills the tool's
  `RANGE_EXPR` parameter whose `from` is `shot.frames`, as `transcode`'s
  `range`.
- **`needs`:** declared only from a measured run; a guess is a ceiling.
- **Network:** the farm refuses a declared `needs.network` until it fences
  a unit's network; named services such as `huggingface` come then. A
  model's weights are not carried in the tool's tree or its image.
- **`writes` and `previews`** take globs and parameters, as `sim`'s
  `**/cache/` and `infer`'s `{{Param.task}}/`.
- **Values:** a `BOOL` is `true` or `false`; an argument that is one format
  string with an empty value is dropped.
- **A default that depends on another parameter** is not in tool/1:
  `infer` keeps its model explicit and refuses a mismatch.
- **Studio images:** a studio copies a folder, changes `runs.image` and
  publishes the copy as its own tool.
- **Reuse:** `ci` and `lora` `never`, the rest `exact`.
- **At `c4 farm up`:** these folders publish farm-wide with their test
  marked as run in the release they shipped in.
