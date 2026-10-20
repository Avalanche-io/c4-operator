#!/bin/sh
# The comp tool's test comp, an OpenImageIO script: the render, graded a
# little warmer, as Saltmarsh's sm0140 comp does.
set -eu
mkdir -p "$C4_TREE/comp"
for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  oiiotool "$C4_TREE/render/test.$f.exr" --mulc 1.08,1.04,0.98 --powc 0.96,0.96,0.96 \
    -d half -o "$C4_TREE/comp/test.$f.exr"
done
