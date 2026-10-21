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
make job-images-dev                  # this machine's architecture, loaded as c4-job-<name>:dev

make job-image-hydra-blender         # the amd64 fallback for hydra
```

Each Dockerfile has a `Dockerfile.dockerignore` beside it. The root
`.dockerignore` admits only the operator's binaries, and each job image
needs only `go.mod`, `go.sum`, `cmd/c4-unit` and its own folder.

| image   | job types          | contents                                                                                                                                                          | base                       | size class |
| ------- | ------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------- | ---------- |
| `oiio`  | comp (OIIO script) | oiiotool from the OpenImageIO 3.2 wheel, with OpenColorIO's built-in configs                                                                                      | Debian 13.7                | small      |
| `media` | transcode          | oiiotool (as `oiio`), FFmpeg 7.1 (`prores_ks`, `dnxhd`)                                                                                                           | Debian 13.7                | medium     |
| `usd`   | usd                | OpenUSD 26.08 built from source, without imaging: usdcat, usdchecker, usdzip, usdtree, usddiff, usdresolve                                                        | Debian 13.7                | medium     |
| `hydra` | render             | OpenUSD 26.03 with imaging and usdrecord, OpenSubdiv 3.7, OpenColorIO 2.4.2, Cycles 5.2's Hydra delegate (hdCycles), oiiotool for previews; all built from source | Debian 13.7                | large      |
| `sim`   | sim                | Blender 5.2.2 (Debian package, with Mantaflow and OpenVDB), oiiotool                                                                                              | Debian sid, dated snapshot | large      |
| `ci`    | ci                 | Go 1.26.9, Python 3.13 with pytest and pytest-shard, gotestsum, `c4-test`                                                                                         | Debian 13.7                | medium     |
| `train` | lora               | PyTorch 2.14 (CPU), diffusers, PEFT, transformers, `c4-train-lora`                                                                                                | Debian 13.7                | large      |
| `infer` | infer              | PyTorch 2.14 (CPU), transformers, OpenImageIO, `c4-infer` (depth, matte, upscale)                                                                                 | Debian 13.7                | large      |

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

## Verified on arm64

Each image was built on an Apple-silicon Mac (linux/arm64, `JOBS=2`) and
run as 65532 against its sample; amd64 has not been built yet.

| image   | size    | checked                                                                                                                                            |
| ------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `oiio`  | 232 MB  | `ocio://studio-config-latest` lists ACES2065-1 and the "ACES 2.0 - SDR 100 nits (Rec.709)" view                                                    |
| `media` | 622 MB  | ProRes 422 HQ and DNxHR HQX movies and a contact sheet from the sample plates                                                                      |
| `ci`    | 713 MB  | `c4-test` on the pipeline-tools sample: 28 Python tests in 4 shards with JUnit; on the local farm, 8 shards with the Go tests too                  |
| `train` | 1168 MB | a short run with checkpoints, a yield through the downward-API file, and a resume at the saved step                                                |
| `infer` | 1170 MB | depth mattes from the sample plates                                                                                                                |
| `sim`   | 767 MB  | Blender 5.2.2 from the pinned sid snapshot with fluid and OpenVDB; twelve frames of the fog sample baked to `.vdb`                                 |
| `usd`   | 315 MB  | `usdcat --flatten` and `usdchecker` on sm0140 pass                                                                                                 |
| `hydra` | 1180 MB | `usdrecord --disableGpu --renderer Cycles` lists Cycles and Storm; sm0140 frames 1001-1002 render to EXR through the stage's RenderSettings camera |

What the hydra recipe needed, beyond the first draft:

- **OpenUSD 26.03**, the version Blender 5.2 builds Cycles 5.2 against:
  26.08 drops an `IsSupported` override Cycles 5.2's delegate still
  declares.
- **GL on, with OpenSubdiv's osdGPU.** hdCycles links hgiGL, which a GL-off
  OpenUSD does not build; nothing opens a GL context under `--disableGpu`.
- **OpenColorIO 2.4.2 from source**: Cycles 5.2 needs 2.2 or later and
  Debian 13 has 2.1.
- **sse2neon on arm64**: without it Cycles' half-float code uses NEON
  intrinsics with nothing including them.
- **hdsi in the link of Cycles' own app**, which the Cycles repository
  builds alongside the delegate.
- **libpython3.13 at run time**, which OpenUSD's libraries link.
- **The stage's RenderSettings.** usdrecord uses the render settings it is
  given, else a camera named `main_cam`, else a free camera. The render
  type passes the stage's settings prim (the stage's `renderSettingsPrimPath`,
  else the first RenderSettings prim), so the shot camera and its frame
  are used.
