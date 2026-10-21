#!/bin/bash
# Render one view layer of a Blender file over this unit's frames.
#   blender-render.sh BLEND scene=S view_layer=L camera=C engine=E samples=N
# BLEND is an absolute path in the job's tree: a .blend, or a .py that
# Blender runs to build the .blend first (it is given the .blend's path,
# beside the .py, after --), which is how the test makes a scene without a
# binary .blend in git. render.py prints a c4_frame line per frame.
# BLENDER names the program, if it is not blender on the PATH.
set -euo pipefail
blend="$1"
shift
here="$(cd "$(dirname "$0")" && pwd)"
blender="${BLENDER:-blender}"

if [ ! -f "$blend" ]; then
  echo "c4_fail: permanent no Blender file at $blend"
  exit 2
fi
case "$blend" in
  *.blend) ;;
  *.py)
    built="${blend%.py}.blend"
    echo "openjd_status: building the scene with $(basename "$blend")"
    "$blender" -b --python-exit-code 1 --python "$blend" -- "$built"
    blend="$built"
    ;;
  *)
    echo "c4_fail: permanent $(basename "$blend") is not a .blend file"
    exit 2
    ;;
esac

exec "$blender" -b "$blend" --python-exit-code 1 --python "$here/render.py" -- "$@"
