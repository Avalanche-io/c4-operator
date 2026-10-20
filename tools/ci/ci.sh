#!/bin/bash
# Run this unit's shard of the repository's tests with c4-test.
#   ci.sh TARGET
# The job's tree is the repository. The ci class gives each unit
# C4_SHARD and C4_SHARDS; JUnit goes to $C4_TREE/results/shard-<n>.*.xml.
set -euo pipefail
target="${1:-//...}"
shard="${C4_SHARD:?the ci class sets C4_SHARD}"
shards="${C4_SHARDS:-${C4_PARAM_SHARDS:-1}}"

cd "$C4_TREE"
mkdir -p results
echo "openjd_status: shard $shard of $shards, target $target"
c4-test --shard "$shard" --shards "$shards" --target "$target" --junit "results/shard-$shard.xml"
