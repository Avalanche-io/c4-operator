#!/bin/bash
# Run a model over this unit's frames with c4-infer, OpenEXR in and out.
#   infer.sh PLATES TASK MODEL [SHOT]
# PLATES is an absolute path in the job's tree: a sequence with $F4, <F4>
# or %04d for the frame, a folder of <name>.####.exr frames, or one frame's
# file. Frames go to $C4_TREE/<task>/<shot>.<frame>.exr, one c4_frame line
# each; SHOT defaults to the plates' name.
set -euo pipefail
plates="$1" task="$2" model="$3" shot="${4:-}"

# seq_pattern PATH [NAME]: the sequence as a printf pattern (%04d)
seq_pattern() {
  local p="$1"
  if [ -d "$p" ]; then
    printf '%s/%s.%%04d.exr\n' "${p%/}" "${2:-$(basename "$p")}"
    return
  fi
  p="${p//\$F4/%04d}"
  p="${p//<F4>/%04d}"
  if [[ "$p" != *%04d* && "$p" =~ ^(.*)\.[0-9]+(\.[A-Za-z0-9]+)$ ]]; then
    p="${BASH_REMATCH[1]}.%04d${BASH_REMATCH[2]}"
  fi
  printf '%s\n' "$p"
}

case "$task" in
  depth | matte | upscale) ;;
  *)
    echo "c4_fail: permanent task $task: use depth, matte or upscale"
    exit 2
    ;;
esac
# c4-infer's short names belong to one task each
case "$task:$model" in
  depth:birefnet | depth:swin2sr-x2 | matte:depth-anything-v2-small | matte:swin2sr-x2 | upscale:depth-anything-v2-small | upscale:birefnet)
    echo "c4_fail: permanent model $model is not a $task model: depth takes depth-anything-v2-small, matte birefnet, upscale swin2sr-x2"
    exit 2
    ;;
esac

pattern="$(seq_pattern "$plates" "$shot")"
if [ -z "$shot" ]; then
  shot="$(basename "$pattern")"
  shot="${shot%%.*}"
fi
out="$C4_TREE/$task"
mkdir -p "$out"

echo "openjd_status: $task with $model, frames $C4_FRAME_START-$C4_FRAME_END"
c4-infer --task "$task" --model "$model" \
  --in "$pattern" --out "$out/$shot.%04d.exr" \
  --frames "$C4_FRAME_START-$C4_FRAME_END"

for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  frame="$out/$shot.$(printf %04d "$f").exr"
  if [ ! -f "$frame" ]; then
    echo "c4_fail: retry c4-infer exited 0 but did not write $frame"
    exit 1
  fi
  echo "c4_frame: $f $frame"
done
