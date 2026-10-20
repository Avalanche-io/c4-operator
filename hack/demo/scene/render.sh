#!/bin/sh
# The dailies demo's render (hack/demo/dailies.sh): a graph job whose root
# unit, plan, lays out three chunks of four frames and a dailies wait for
# the supervisor; each chunk renders its frames with oiiotool into the
# job's output view. A version made from a supervisor's note (mounted at
# /c4/notes/note.md by c4-sync) renders with the fill light brought down.
set -eu
out="${C4_OUTPUT:-/show}"
case "${C4_UNIT_NAME:-}" in
plan)
  mkdir -p "$out/.c4"
  cat >"$out/.c4/expand.json" <<'EOF'
{"version": 1,
 "units": [
  {"name": "frames/1001-1004", "label": "frames 1001-1004", "key": "frames", "work": 20, "env": {"FRAMES": "1001 1002 1003 1004"}},
  {"name": "frames/1005-1008", "label": "frames 1005-1008", "key": "frames", "work": 20, "env": {"FRAMES": "1005 1006 1007 1008"}},
  {"name": "frames/1009-1012", "label": "frames 1009-1012", "key": "frames", "work": 20, "env": {"FRAMES": "1009 1010 1011 1012"}},
  {"name": "dailies", "label": "dailies: sh040 lighting",
   "after": ["frames/1001-1004", "frames/1005-1008", "frames/1009-1012"],
   "wait": {"server": "person:dana", "ask": "Dailies: approve sh040 lighting?", "expect": 1800, "choices": ["approve", "rework", "accept"]}}
 ]}
EOF
  ;;
frames/*)
  mkdir -p "$out/render"
  color="0.98,0.62,0.26" label="fill light hot"
  if [ -f /c4/notes/note.md ]; then
    color="0.62,0.48,0.36" label="fill light down, per the note"
  fi
  for f in $FRAMES; do
    oiiotool --pattern constant:color="$color" 960x540 3 \
      --text:x=40:y=90:size=44 "sh040 lighting  $f" \
      --text:x=40:y=150:size=28 "$label" \
      -o "$out/render/sh040_lgt.$f.png"
    sleep 2 # a frame takes a moment, so the HUD shows units running
  done
  ;;
*)
  echo "render.sh: unknown unit ${C4_UNIT_NAME:-}" >&2
  exit 2
  ;;
esac
