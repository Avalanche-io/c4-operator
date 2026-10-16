#!/usr/bin/env bash
# Capture HUD screenshots into docs/hud/ from a frozen mock farm.
#
#   hack/hudmock/screenshots.sh            # all shots
#   hack/hudmock/screenshots.sh queue cost # only these
#
# Uses chrome-headless-shell (a plain binary, not an .app bundle). The
# browser exits by itself after each capture; it is never killed.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
out="$root/docs/hud"
port="${HUD_PORT:-8741}"
freeze="${HUD_FREEZE:-50m}"
seed="${HUD_SEED:-7}"
mkdir -p "$out"

chrome="${CHROME:-}"
if [[ -z "$chrome" ]]; then
  dir="$(ls -d "$HOME"/.cache/puppeteer/chrome-headless-shell/mac_arm-*/ 2>/dev/null | sort -V | tail -1)"
  chrome="${dir}chrome-headless-shell-mac-arm64/chrome-headless-shell"
fi
[[ -x "$chrome" ]] || { echo "chrome-headless-shell not found; set CHROME" >&2; exit 1; }

tmp="$(mktemp -d)"
bin="$tmp/hudmock"
(cd "$root" && go build -o "$bin" ./hack/hudmock)
"$bin" --addr "127.0.0.1:$port" --freeze "$freeze" --seed "$seed" --web "$root/internal/hud/web" >"$tmp/mock.log" 2>&1 &
mock=$!
cleanup() { kill -TERM "$mock" 2>/dev/null || true; wait "$mock" 2>/dev/null || true; rm -rf "$tmp"; }
trap cleanup EXIT
for _ in $(seq 1 100); do
  curl -sf "http://127.0.0.1:$port/api/v1/snapshot" >/dev/null && break
  sleep 0.1
done

base="http://127.0.0.1:$port/"
api() { curl -sf "http://127.0.0.1:$port/api/v1/$1"; }

# Pick jobs and units from the frozen farm, so the shots stay meaningful if
# the scenario changes.
ci_job="$(api snapshot | python3 -I -c 'import json,sys
d=json.load(sys.stdin)
js=[j for j in d["jobs"] if j["class"]=="ci-presubmit"]
run=[j for j in js if j["phase"]=="Running"] or js
print(run[0]["namespace"]+"/"+run[0]["name"])')"
retry_unit="$(api jobs/harbor-render/sh040-fx-v003 | python3 -I -c 'import json,sys
d=json.load(sys.stdin)
u=[u for u in d["unitList"] if u["state"]=="retrying"] or [u for u in d["unitList"] if u["state"]=="failed"]
print(u[0]["i"] if u else 0)')"

shot() { # name url width height
  local name="$1" url="$2" w="$3" hgt="$4"
  "$chrome" --headless --disable-gpu --hide-scrollbars --no-first-run --no-default-browser-check \
    --user-data-dir="$tmp/profile-$name" --force-device-scale-factor=2 --window-size="$w,$hgt" \
    --virtual-time-budget=9000 --run-all-compositor-stages-before-draw \
    --screenshot="$out/$name.png" "$url" >/dev/null 2>&1
  echo "docs/hud/$name.png"
}

want() { [[ $# -eq 0 ]] && return 0; local n; for n in "${ARGS[@]}"; do [[ "$n" == "$1" ]] && return 0; done; return 1; }
ARGS=("$@")
sel() { [[ ${#ARGS[@]} -eq 0 ]] || want "$1"; }

W="?as=wrangler&static=1"
A="?as=artist&static=1"
sel queue && shot queue "$base$W#/queue" 1600 1240
sel render && shot job-render-binary-order "$base$W#/job/harbor-render/sh040-fx-v003" 1600 1460
sel unit && shot job-render-unit-retrying "$base$W#/job/harbor-render/sh040-fx-v003/unit/$retry_unit" 1600 1460
sel generator && shot job-generator-expanding "$base$W#/job/harbor-render/sh050-light-v002" 1600 1340
sel ci && shot lens-ci "$base$W#/job/$ci_job" 1600 1500
sel ml && shot lens-ml "$base$W#/job/harbor-ml/harbor-lora-lookdev-v2" 1600 1300
sel compact && shot job-density-compact "$base$W#/job/kestrel-render/ep104-crowd-v006" 1600 1300
sel aggregate && shot job-density-aggregate "$base$W#/job/kestrel-render/ep104-flood-wedge-v002" 1600 1000
sel workers && shot workers "$base$W#/workers" 1600 1500
sel tiers && shot tiers "$base$W#/tiers" 1600 900
sel graph && shot graph "$base$W#/graph" 1600 1100
sel cost && shot cost-capacity "$base$W#/cost" 1600 1700
sel artist && shot artist-scoped-queue "$base$A#/queue" 1600 1000
sel laptop && shot laptop-job-view "$base$W#/job/harbor-render/sh040-fx-v003" 1280 800
exit 0
