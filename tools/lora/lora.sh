#!/bin/bash
# Fine-tune a LoRA adapter with c4-train-lora, checkpointing as it goes.
#   lora.sh DATA BASE RANK
# DATA is an absolute path in the job's tree. BASE is a model folder in the
# tree (relative to it) or a Hugging Face model ID. Checkpoints go to
# $C4_TREE/out; a later attempt resumes from out/latest.
set -euo pipefail
data="$1" base="$2" rank="$3"

if [ ! -d "$data" ]; then
  echo "c4_fail: permanent no training frames at $data"
  exit 2
fi
# a model folder in the job's tree, else a Hugging Face model ID
if [ -d "$C4_TREE/$base" ]; then base="$C4_TREE/$base"; fi

# the ml class gives the unit C4_STEPS and C4_CHECKPOINT_EVERY from the
# job's params; the tool's own parameters stand in where it does not
export C4_STEPS="${C4_STEPS:-${C4_PARAM_STEPS:-400}}"
export C4_CHECKPOINT_EVERY="${C4_CHECKPOINT_EVERY:-${C4_PARAM_CHECKPOINTEVERY:-50}}"

echo "openjd_status: training a rank-$rank adapter for $C4_STEPS steps"
c4-train-lora --base "$base" --data "$data" --out "$C4_TREE/out" --rank "$rank"
