#!/bin/bash
# Render this unit's frames of a USD stage to OpenEXR with usdrecord.
#   render.sh STAGE RENDERER WIDTH
# STAGE is an absolute path in the job's tree. Frames go to
# $C4_TREE/render/<stage>.<frame>.exr, one c4_frame line each.
set -euo pipefail
stage="$1" renderer="$2" width="$3"
here="$(cd "$(dirname "$0")" && pwd)"

case "$renderer" in
  cycles) delegate=Cycles ;;
  karma-xpu) delegate=BRAY_HdKarmaXPU ;;
  karma-cpu) delegate=BRAY_HdKarma ;;
  prman) delegate=HdPrmanLoaderRendererPlugin ;;
  arnold) delegate=HdArnoldRendererPlugin ;;
  vray) delegate=HdVRayRendererPlugin ;;
  *)
    echo "c4_fail: permanent renderer $renderer: use cycles, karma-xpu, karma-cpu, prman, arnold or vray"
    exit 2
    ;;
esac

if [ ! -f "$stage" ]; then
  echo "c4_fail: permanent no stage at $stage"
  exit 2
fi
name="$(basename "$stage")"
stem="${name%.*}"
out="$C4_TREE/render"
mkdir -p "$out"

# the stage's render settings (its camera and frame), if it has any
rs="$(python3 "$here/render_settings.py" "$stage" 2>/dev/null || true)"

echo "openjd_status: rendering frames $C4_FRAME_START-$C4_FRAME_END with $delegate"
# --disableGpu keeps usdrecord to CPU delegates and opens no GL context
set -- --disableGpu --renderer "$delegate" --frames "$C4_FRAME_START:$C4_FRAME_END" --imageWidth "$width"
if [ -n "$rs" ]; then set -- "$@" --renderSettingsPrimPath "$rs"; fi
usdrecord "$@" "$stage" "$out/$stem.####.exr"

for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  frame="$out/$stem.$(printf %04d "$f").exr"
  if [ ! -f "$frame" ]; then
    echo "c4_fail: retry usdrecord exited 0 but did not write $frame"
    exit 1
  fi
  echo "c4_frame: $f $frame"
done
