#!/bin/sh
# The Kitsu demo's render (hack/demo/kitsu.sh): a render-class job's units,
# each a chunk of frames (C4_FRAME_START to C4_FRAME_END), written with
# oiiotool into the job's output view with a preview proxy per frame
# (preview/<frame>.png), which the HUD plays and c4-sync gives Kitsu as the
# version's preview. A version made from a supervisor's note (mounted at
# /c4/notes/note.md by c4-sync) renders with the fill light brought down.
set -eu
out="${C4_OUTPUT:-/show}"
mkdir -p "$out/render" "$out/preview"
color="0.98,0.62,0.26" label="fill light hot"
if [ -f /c4/notes/note.md ]; then
  color="0.62,0.48,0.36" label="fill light down, per the note"
fi
f=$C4_FRAME_START
while [ "$f" -le "$C4_FRAME_END" ]; do
  oiiotool --pattern constant:color="$color" 960x540 3 \
    --text:x=40:y=90:size=44 "sh040 lighting  $f" \
    --text:x=40:y=150:size=28 "$label" \
    -o "$out/render/sh040_lgt.$f.png"
  oiiotool "$out/render/sh040_lgt.$f.png" --resize 480x270 -o "$out/preview/$f.png"
  sleep 2 # a frame takes a moment, so the HUD shows units running
  f=$((f + 1))
done
