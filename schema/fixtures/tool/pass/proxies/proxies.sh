#!/bin/bash
# Proxies for this unit's frames: plates in, half-size sRGB JPEGs out.
set -euo pipefail
plates="$1" width="$2" colorspace="$3"
mkdir -p "$C4_TREE/proxy"
for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  src="${plates/\$F4/$(printf %04d "$f")}"
  out="$C4_TREE/proxy/$(basename "${src%.exr}").jpg"
  oiiotool "$src" --colorconvert "$colorspace" sRGB --resize "${width}x0" -o "$out"
  echo "c4_frame: $f $out"
done
