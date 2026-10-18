# Job images

The images the prebuilt job types run (`internal/farmcli/types.go`:
`ghcr.io/avalanche-io/c4-job-<name>:<version>`). Each one is built from
the repository root for `linux/amd64` and `linux/arm64`. Each runs as the
non-root user 65532 with `HOME=/home/c4`, so it is admitted under Pod
Security `restricted`. Each carries `c4-unit` (`/usr/local/bin`), with
`c4unit.py` on `PYTHONPATH`.

```sh
make job-images                      # every image, OCI archives in bin/
make job-images JOB_IMAGES=oiio      # one
make job-images PUSH=1 REGISTRY=ghcr.io/avalanche-io VERSION=v0.1.0
make job-image-hydra-blender         # the amd64 fallback for hydra
```

Each Dockerfile has a `Dockerfile.dockerignore` beside it. The root
`.dockerignore` admits only the operator's binaries, and each job image
needs only `go.mod`, `go.sum`, `cmd/c4-unit` and its own folder.

| image   | job types          | contents                                                                                                                                       | base                       | size class |
| ------- | ------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------- | ---------- |
| `oiio`  | comp (OIIO script) | oiiotool from the OpenImageIO 3.2 wheel, with OpenColorIO's built-in configs                                                                   | Debian 13.7                | small      |
| `media` | transcode          | oiiotool (as `oiio`), FFmpeg 7.1 (`prores_ks`, `dnxhd`)                                                                                        | Debian 13.7                | medium     |
| `usd`   | usd                | OpenUSD 26.08 built from source, without imaging: usdcat, usdchecker, usdzip, usdtree, usddiff, usdresolve                                     | Debian 13.7                | medium     |
| `hydra` | render             | OpenUSD 26.08 with imaging and usdrecord, OpenSubdiv 3.7, Cycles 5.2's Hydra delegate (hdCycles), oiiotool for previews; all built from source | Debian 13.7                | large      |
| `sim`   | sim                | Blender 5.2.2 (Debian package, with Mantaflow and OpenVDB), oiiotool                                                                           | Debian sid, dated snapshot | large      |
| `ci`    | ci                 | Go 1.26.9, Python 3.13 with pytest and pytest-shard, gotestsum, `c4-test`                                                                      | Debian 13.7                | medium     |
| `train` | lora               | PyTorch 2.14 (CPU), diffusers, PEFT, transformers, `c4-train-lora`                                                                             | Debian 13.7                | large      |
| `infer` | infer              | PyTorch 2.14 (CPU), transformers, OpenImageIO, `c4-infer` (depth, matte, upscale)                                                              | Debian 13.7                | large      |

Model weights are never in an image. `train` and `infer` download them on
first use into `$HOME/.cache/huggingface` (`HF_HOME`). Mount a cache
volume at `/home/c4/.cache` to keep them between units. `ci` keeps Go's
module and build caches under the same folder.

## Choices, and why

- **hydra: usdrecord with Cycles' Hydra delegate is the primary recipe.**
  It is what Joshua approved, and it is the same shape as every studio
  renderer: USD in, a Hydra delegate, EXR out. It is also the only Cycles
  path for arm64. Nothing ships it prebuilt: the usd-core wheel has no
  imaging, no command-line tools and no Linux arm64 build (26.8 is
  manylinux x86_64 only); Debian packages no OpenUSD; and the delegate
  must be built against the OpenUSD it loads into. So OpenUSD, OpenSubdiv
  and hdCycles are built from source. OpenUSD is built with GL support so
  that usdrecord and UsdAppUtils exist, and it runs with `--disableGpu`,
  which keeps it to CPU delegates and opens no GL context.
- **The fallback is `Dockerfile.blender`, amd64 only.** blender.org
  publishes Linux builds for x64 alone, and Debian's Blender is built
  without USD. The fallback installs `c4-render-usd` as `usdrecord`, with
  the same command line, so templates don't change. It cannot run on an
  Apple-silicon local farm.
- **oiiotool comes from the OpenImageIO wheel.** Debian 13 packages
  OpenColorIO 2.1, and `ocio://studio-config-latest` needs 2.2 or later.
  The OpenImageIO 3.2 wheel has amd64 and arm64 builds and carries its own
  libraries.
- **sim is built on Debian sid.** Mantaflow writes OpenVDB only when
  Blender is built with it. Debian 13 and forky build Blender with OpenVDB
  on amd64 alone, while sid's 5.2.2 has it on both architectures. The
  packages come from snapshot.debian.org at a fixed date, so a rebuild
  installs the same versions.

## What the first real build must verify

Build each image once per architecture, on a machine with Docker and
power to spare, then smoke-test each type on the local farm with one frame
(or one shard, or a few steps).

1. **hydra**, the riskiest:
   - OpenUSD 26.08's CMake builds usdrecord and UsdAppUtils with these
     flags.
   - The system oneTBB from Debian 13 (`libtbb-dev`) is accepted by both
     OpenUSD and Cycles.
   - Cycles 5.2 builds its Hydra delegate with `WITH_LIBS_PRECOMPILED=OFF`
     against Debian's OpenImageIO 2.5, OpenEXR 3.1 and pugixml.
   - The `find` at the end of the Cycles stage prints the plugin folder.
     Set `PXR_PLUGINPATH_NAME` to it if it is not `/opt/cycles/hydra`.
   - `usdrecord --help` lists `Cycles` among the renderers (the name
     types.go passes).
   - The runtime package names resolve: `libopenimageio2.5`,
     `libopenexr-3-1-30`, `libpugixml1v5`, `libtbb12`.
   - A one-frame render of the sample shot writes an EXR, under
     `--disableGpu`, on both architectures.
2. **usd**: `usdcat --flatten`, `usdchecker` and `usdzip` run from
   `/opt/usd/bin` with the system Python 3.13.
3. **oiio and media**: `oiiotool --colorconfig ocio://studio-config-latest
   --colorconfiginfo` lists ACES2065-1 and the "ACES 1.0 - SDR Video" view.
   FFmpeg encodes one frame each of ProRes 422 HQ, ProRes 4444 and DNxHR
   HQX.
4. **sim**: the snapshot date resolves, and
   `blender -b --python-expr "import bpy; print(bpy.app.build_options.fluid, bpy.app.build_options.openvdb)"`
   prints `True True` on both architectures. A ten-frame bake writes
   `.vdb` files.
5. **ci**: `c4-test` on a sample repository with Python and Go tests, two
   shards, writes JUnit files, and a failing test fails the unit.
6. **train**: twenty steps with `C4_CHECKPOINT_EVERY=10` on a tiny model.
   Then a yield, by writing the annotation into the downward-API file,
   saves and exits 0, and a second run resumes at the saved step.
7. **infer**: one frame of each task writes an EXR with the expected
   channel (Z, A, or RGB at twice the size). The BiRefNet matte needs
   `trust_remote_code` and its extra packages (timm, einops, kornia).
8. **Every image**: it runs as 65532 under the local farm's restricted
   project namespace. `c4-unit preview` writes `preview/<frame>.png` from
   an EXR. Image sizes go into this table.

## Known gaps in the job types

`internal/farmcli/types.go` needs two changes before these images run its
templates as written:

- The render script should pass `--disableGpu` to usdrecord on CPU pools.
  Without it, usdrecord tries to open a GL context.
- The infer script passes the asset's path as `--in`. `c4-infer` takes a
  printf pattern (`sm0140.%04d.exr`), as the docs' template does.
