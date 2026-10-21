#!/bin/sh
# Build the c4 farm sample archive for a release:
#   hack/samples/build.sh VERSION [OUT]
# writes OUT/c4-samples-VERSION.tar.gz and its C4 ID in
# OUT/c4-samples-VERSION.tar.gz.c4, the two files `c4 farm samples get`
# downloads. The text, the plates and the repository come from
# saltmarsh.py (with oiiotool); fog.blend and the LoRA style frames are
# made in the job images, so this needs Docker and the images from
# `make job-images` (see images/README.md).
set -eu
VERSION=${1:?usage: build.sh VERSION [OUT]}
OUT=${2:-bin/samples}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
REG=${REGISTRY:-ghcr.io/avalanche-io}
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

python3 -I "$ROOT/hack/samples/saltmarsh.py" "$STAGE" --plates

# the job images run as this machine's user, who owns the stage it
# writes into (as 65532 they could not write a runner's folders), with a
# HOME of their own
AS="--user $(id -u):$(id -g) -e HOME=/tmp"

# the marsh fog's .blend, built by Blender in the sim image
docker run --rm $AS -v "$STAGE/saltmarsh/sm0220_fx:/work" -w /work \
  "$REG/c4-job-sim:$VERSION" blender -b --python fog_setup.py -- /work/fog.blend

# style frames for the LoRA set: the sm0140 stage rendered small, one frame
# per caption, in the hydra image
docker run --rm $AS -v "$STAGE:/samples" "$REG/c4-job-hydra:$VERSION" sh -euc '
  cd /samples/lora-saltmarsh/frames
  i=0
  for cap in style_*.txt; do
    f=$((1001 + i % 48))
    usdrecord --disableGpu --renderer Cycles --frames "$f" --imageWidth 512 \
      /samples/saltmarsh/sm0140/sm0140.usda "/tmp/r.####.png"
    mv "/tmp/r.$f.png" "${cap%.txt}.png"
    i=$((i + 1))
  done'

mkdir -p "$OUT"
ARCHIVE="$OUT/c4-samples-$VERSION.tar.gz"
# a stable archive: sorted, no owners, fixed times
tar -C "$STAGE" --sort=name --owner=0 --group=0 --numeric-owner --mtime='2026-10-01 00:00Z' \
  -czf "$ARCHIVE" saltmarsh pipeline-tools lora-saltmarsh deadline 2>/dev/null ||
  (cd "$STAGE" && COPYFILE_DISABLE=1 tar -czf "$ARCHIVE" saltmarsh pipeline-tools lora-saltmarsh deadline)
c4 -x < "$ARCHIVE" > "$ARCHIVE.c4"  # the bare ID (c4 id prints a c4m line)
echo "$ARCHIVE $(cat "$ARCHIVE.c4")"
