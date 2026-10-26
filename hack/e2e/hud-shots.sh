#!/usr/bin/env bash
# Screenshots of the HUD serving the live e2e cluster (P5), into
# docs/hud/live/ of the c4-hud checkout beside this one (HUD_SHOTS_OUT
# overrides). Each shot loads one snapshot (static=1), so it shows the
# farm at the moment it is taken.
#
#   hack/e2e/hud-shots.sh PREFIX shot...     shots: queue job:NS/NAME pools map tiers cost graph artist wall widget:NS/NAME
#
# Uses chrome-headless-shell (a plain binary, not an .app bundle). The
# browser exits by itself after each capture; it is never killed.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
out="${HUD_SHOTS_OUT:-$root/../c4-hud/docs/hud/live}"
base="${HUD_URL:-http://127.0.0.1:31080}/"
mkdir -p "$out"
chrome="${CHROME:-}"
if [[ -z "$chrome" ]]; then
  dir="$(ls -d "$HOME"/.cache/puppeteer/chrome-headless-shell/mac_arm-*/ 2>/dev/null | sort -V | tail -1)"
  chrome="${dir}chrome-headless-shell-mac-arm64/chrome-headless-shell"
fi
[[ -x "$chrome" ]] || { echo "chrome-headless-shell not found; set CHROME" >&2; exit 1; }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

prefix="$1"
shift
shot() { # name url width height
  "$chrome" --headless --disable-gpu --hide-scrollbars --no-first-run --no-default-browser-check \
    --user-data-dir="$tmp/profile-$1" --force-device-scale-factor=2 --window-size="$3,$4" \
    --virtual-time-budget=9000 --run-all-compositor-stages-before-draw \
    --screenshot="$out/$1.png" "$2" >/dev/null 2>&1
  echo "docs/hud/live/$1.png"
}
W="?as=wrangler&static=1"
for s in "$@"; do
  case "$s" in
    queue) shot "$prefix-queue" "$base$W#/queue" 1600 1100 ;;
    workers|pools) shot "$prefix-pools" "$base$W#/pools" 1600 1200 ;;
    map) shot "$prefix-map" "$base$W#/map" 1600 1000 ;;
    wall) shot "$prefix-wall" "$base$W#/wall" 1920 1080 ;;
    widget:*)
      j="${s#widget:}"
      shot "$prefix-widget-${j##*/}" "$base$W#/widget/$j" 420 560
      ;;
    tiers) shot "$prefix-tiers" "$base$W#/tiers" 1600 900 ;;
    cost) shot "$prefix-cost" "$base$W#/cost" 1600 1300 ;;
    graph) shot "$prefix-graph" "$base$W#/graph" 1600 1000 ;;
    artist) shot "$prefix-artist-queue" "$base?as=artist&static=1#/queue" 1600 900 ;;
    job:*)
      j="${s#job:}"
      shot "$prefix-job-${j##*/}" "$base$W#/job/$j" 1600 1400
      ;;
    *) echo "unknown shot $s" >&2 ;;
  esac
done
