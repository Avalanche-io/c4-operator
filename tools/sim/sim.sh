#!/bin/bash
# Bake this unit's frames of a fluid sim to OpenVDB, all in one unit.
#   sim.sh SCENE DOMAIN NODE
# SCENE is an absolute path in the job's tree: a .blend (Mantaflow, the
# DOMAIN object's Fluid modifier, cached to cache/ beside the scene), a
# .hip (hython renders NODE, which writes where it is set to), or a .py
# that Blender runs to build the .blend (it is given the .blend's path
# after --). Prints a c4_frame line per frame once the bake is done.
set -euo pipefail
scene="$1" domain="$2" node="$3"
here="$(cd "$(dirname "$0")" && pwd)"

if [ ! -f "$scene" ]; then
  echo "c4_fail: permanent no scene at $scene"
  exit 2
fi
cache="$(dirname "$scene")/cache"
start="$C4_FRAME_START" end="$C4_FRAME_END"

case "$scene" in
  *.hip | *.hipnc | *.hiplc)
    if ! command -v hython >/dev/null; then
      echo "c4_fail: permanent this image has no hython; bake .hip scenes on a farm image with Houdini"
      exit 2
    fi
    echo "openjd_status: baking $node, frames $start-$end, with Houdini"
    hython "$here/bake_houdini.py" "$scene" "$node" "$start" "$end"
    ;;
  *)
    blend="$scene"
    if [ "${scene##*.}" = py ]; then
      tmp="$(mktemp -d)"
      trap 'rm -rf "$tmp"' EXIT
      blend="$tmp/scene.blend"
      echo "openjd_status: building the scene with $(basename "$scene")"
      blender -b --python-exit-code 1 --python "$scene" -- "$blend"
    fi
    echo "openjd_status: baking $domain, frames $start-$end, with Mantaflow"
    blender -b "$blend" --python-exit-code 1 --python "$here/bake_blender.py" -- "$domain" "$cache" "$start" "$end"
    ;;
esac

for f in $(seq "$start" "$end"); do
  echo "c4_frame: $f"
done
