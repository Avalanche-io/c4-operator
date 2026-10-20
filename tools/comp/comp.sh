#!/bin/bash
# Composite this unit's frames with a Nuke script or an OpenImageIO script.
#   comp.sh SCRIPT ENGINE
# SCRIPT is an absolute path in the job's tree. It writes
# comp/<shot>.<frame>.exr; each frame found there gets a c4_frame line.
set -euo pipefail
script="$1" engine="${2:-auto}"

if [ ! -f "$script" ]; then
  echo "c4_fail: permanent no comp script at $script"
  exit 2
fi
case "$engine" in
  auto) case "$script" in *.nk) engine=nuke ;; *) engine=oiio ;; esac ;;
  nuke | oiio) ;;
  *)
    echo "c4_fail: permanent engine $engine: use auto, nuke or oiio"
    exit 2
    ;;
esac

mkdir -p "$C4_TREE/comp"
echo "openjd_status: compositing frames $C4_FRAME_START-$C4_FRAME_END with $engine"
if [ "$engine" = nuke ]; then
  if ! command -v nuke >/dev/null; then
    echo "c4_fail: permanent this image has no nuke; run the comp tool on a farm image with Nuke, or use an OpenImageIO script"
    exit 2
  fi
  nuke -x -F "$C4_FRAME_START-$C4_FRAME_END" "$script"
else
  sh "$script"
fi

shopt -s nullglob
for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  found=("$C4_TREE"/comp/*."$(printf %04d "$f")".exr "$C4_TREE"/comp/*."$f".exr)
  if [ ${#found[@]} -eq 0 ]; then
    echo "c4_fail: retry the comp wrote no comp/*.$f.exr"
    exit 1
  fi
  echo "c4_frame: $f ${found[0]}"
done
