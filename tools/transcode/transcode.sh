#!/bin/bash
# An ACES-managed review movie and a contact sheet of a frame sequence.
#   transcode.sh PLATES RANGE CODEC FPS [SHOT]
# PLATES is an absolute path in the job's tree: a sequence with $F4, <F4>
# or %04d for the frame, a folder of <name>.####.exr frames, or one frame's
# file. Writes $C4_TREE/review/<shot>_review.mov and <shot>_contact.jpg;
# SHOT defaults to the frames' name.
set -euo pipefail
plates="$1" range="$2" codec="$3" fps="$4" shot="${5:-}"

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

case "$codec" in
  prores-422hq) codec_args=(-c:v prores_ks -profile:v 3 -pix_fmt yuv422p10le) ;;
  prores-4444) codec_args=(-c:v prores_ks -profile:v 4 -pix_fmt yuva444p10le) ;;
  dnxhr-hqx) codec_args=(-c:v dnxhd -profile:v dnxhr_hqx -pix_fmt yuv422p10le) ;;
  *)
    echo "c4_fail: permanent codec $codec: use prores-422hq, prores-4444 or dnxhr-hqx"
    exit 2
    ;;
esac
case "$fps" in
  23.976) rate=24000/1001 ;;
  29.97) rate=30000/1001 ;;
  59.94) rate=60000/1001 ;;
  *) rate="$fps" ;;
esac
if [[ "$range" =~ ^([0-9]+)-([0-9]+)$ ]]; then
  start="${BASH_REMATCH[1]}" end="${BASH_REMATCH[2]}"
elif [[ "$range" =~ ^[0-9]+$ ]]; then
  start="$range" end="$range"
else
  echo "c4_fail: permanent frames $range: a movie takes one run of frames, first-last, as 1001-1048"
  exit 2
fi
if [ "$end" -lt "$start" ]; then
  echo "c4_fail: permanent frames $range run backwards"
  exit 2
fi

pattern="$(seq_pattern "$plates" "$shot")"
first="${pattern//%04d/$(printf %04d "$start")}"
if [ ! -f "$first" ]; then
  echo "c4_fail: permanent no first frame at $first"
  exit 2
fi
if [ -z "$shot" ]; then
  shot="$(basename "$pattern")"
  shot="${shot%%.*}"
fi

out="$C4_TREE/review"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$out"

echo "openjd_status: converting frames $start-$end to Rec.709"
oiiotool --frames "$start-$end" "${pattern//%04d/#}" \
  --colorconfig ocio://studio-config-latest --iscolorspace ACES2065-1 \
  --ociodisplay "Rec.1886 Rec.709 - Display" "ACES 2.0 - SDR 100 nits (Rec.709)" \
  -d uint16 -o "$tmp/f.#.tif"
echo "openjd_progress: 50"
echo "openjd_status: encoding ${shot}_review.mov ($codec, $fps fps)"
ffmpeg -nostdin -y -loglevel error -framerate "$rate" -start_number "$start" -i "$tmp/f.%04d.tif" \
  "${codec_args[@]}" "$out/${shot}_review.mov"
echo "openjd_progress: 90"
echo "openjd_status: contact sheet"
oiiotool --frames "$start-$end" "$tmp/f.#.tif" --resize 320x0 \
  --mosaic:pad=4 8x6 -o "$out/${shot}_contact.jpg"
echo "openjd_progress: 100"
