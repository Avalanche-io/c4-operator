#!/bin/bash
# Flatten a USD stage into one layer, and check it.
#   usd.sh STAGE VALIDATE
# STAGE is an absolute path in the job's tree. Writes
# $C4_TREE/flat/<stage>.flat.usda and, when VALIDATE is true,
# $C4_TREE/flat/usdchecker.txt.
set -euo pipefail
stage="$1" validate="${2:-true}"

if [ ! -f "$stage" ]; then
  echo "c4_fail: permanent no stage at $stage"
  exit 2
fi
name="$(basename "$stage")"
stem="${name%.*}"
out="$C4_TREE/flat"
mkdir -p "$out"

echo "openjd_status: flattening $name"
usdcat --flatten "$stage" -o "$out/$stem.flat.usda"

case "$validate" in
  true | True | TRUE | 1 | yes) ;;
  *) exit 0 ;;
esac
echo "openjd_status: checking $stem.flat.usda"
if ! usdchecker "$out/$stem.flat.usda" 2>&1 | tee "$out/usdchecker.txt"; then
  echo "c4_fail: permanent usdchecker found problems in $stem.flat.usda: see flat/usdchecker.txt"
  exit 1
fi
