#!/usr/bin/env bash
# The whole e2e suite (`make e2e`): the cluster, then the phases in the
# order they depend on each other (P5 sets up the pools P4, P7 and P6 use;
# P6 reads what P5 and P7 left), stopping at the first that fails.
#
#   hack/e2e/run.sh              every phase
#   PHASES="p1 p3" hack/e2e/run.sh
set -euo pipefail
dir=$(cd "$(dirname "$0")" && pwd)
"$dir/up.sh"
for p in ${PHASES:-p1 p2 p3 p5 p4 p7 p6}; do
  printf '\n=== %s\n' "$p" >&2
  "$dir/$p.sh"
done
