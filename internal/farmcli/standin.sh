# A sample scenario's stand-in unit (c4 farm samples load): it takes the
# time the real work would and writes placeholder outputs, and it keeps
# the unit contract a real image keeps with c4-unit: progress, and for a
# training run, checkpoints at safe points where the farm may ask it to
# yield. busybox sh; everything it needs is in its environment:
#
#   S_MOUNT        the writable mount (its .c4/ is the unit's control folder)
#   S_OUT          the folder under it the outputs go in
#   S_NAME         the outputs' base name (sm0150 writes sm0150.1001.exr)
#   S_EXT          the outputs' extension
#   S_SECONDS      seconds per frame, or for the whole unit without frames
#   S_FAIL_NODE    a node this unit fails on, after S_FAIL_AFTER seconds,
#   S_FAIL_MSG     printing this
#   S_STEP_SECONDS a training step's seconds (with C4_STEPS, C4_CHECKPOINT_EVERY)
#   NODE_NAME      the node it runs on
set -eu
ctl="$S_MOUNT/.c4"
out="$S_MOUNT/$S_OUT"
mkdir -p "$ctl" "$out"
ratio() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.4f\n", (b > 0 ? a / b : 1) }'; }

if [ -n "${S_FAIL_NODE:-}" ] && [ "${NODE_NAME:-}" = "$S_FAIL_NODE" ]; then
  sleep "${S_FAIL_AFTER:-30}"
  echo "${S_FAIL_MSG:-input/output error}" >&2
  exit 1
fi

if [ -n "${C4_STEPS:-}" ]; then
  # a training run: resume from the last checkpoint this job saved
  step=0
  [ -f "$out/checkpoint" ] && step=$(cat "$out/checkpoint")
  seq=0
  [ -f "$ctl/safepoint" ] && seq=$(awk '$1 == "seq" { print $2 }' "$ctl/safepoint")
  every=${C4_CHECKPOINT_EVERY:-50}
  [ "$step" -gt 0 ] && echo "resumed at step $step"
  while [ "$step" -lt "$C4_STEPS" ]; do
    sleep "$S_STEP_SECONDS"
    step=$((step + 1))
    ratio "$step" "$C4_STEPS" >"$ctl/progress"
    [ $((step % every)) -eq 0 ] && [ "$step" -lt "$C4_STEPS" ] || continue
    echo "$step" >"$out/checkpoint"
    echo "stand-in weights at step $step" >"$out/$S_NAME.step-$step.$S_EXT"
    seq=$((seq + 1))
    printf 'seq %d\nat %s\ncheckpoint step-%d\n' "$seq" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$step" >"$ctl/safepoint"
    # the farm's answer, within 20 s; with none, a yield it asked for stands
    d=""
    i=0
    while [ "$i" -lt 100 ]; do
      if [ -n "${C4_CONTROL:-}" ] && [ -f "$C4_CONTROL/safepoint.decision" ] &&
        tr -d ' ' <"$C4_CONTROL/safepoint.decision" | grep -q "\"seq\":$seq[,}]"; then
        d=$(tr -d ' ' <"$C4_CONTROL/safepoint.decision")
        break
      fi
      sleep 0.2
      i=$((i + 1))
    done
    case "$d" in
    *'"do":"stop"'*) echo "yielded at step $step"; exit 0 ;;
    *'"do":"continue"'*) ;;
    *) if [ -n "${C4_PODINFO:-}" ] && grep -q '^c4.avalanche.io/yield=' "$C4_PODINFO" 2>/dev/null; then
         echo "yielded at step $step"
         exit 0
       fi ;;
    esac
  done
  echo "stand-in weights, $C4_STEPS steps" >"$out/$S_NAME.$S_EXT"
  echo 1 >"$ctl/progress"
  exit 0
fi

if [ -z "${C4_FRAME_START:-}" ]; then
  sleep "$S_SECONDS"
  echo "stand-in output of $S_NAME" >"$out/$S_NAME.$S_EXT"
  echo 1 >"$ctl/progress"
  exit 0
fi

n=$((C4_FRAME_END - C4_FRAME_START + 1))
i=0
f=$C4_FRAME_START
while [ "$f" -le "$C4_FRAME_END" ]; do
  sleep "$S_SECONDS"
  if [ -n "${S_PER_FRAME:-}" ]; then
    echo "stand-in frame $f of $S_NAME" >"$out/$S_NAME.$f.$S_EXT"
  fi
  i=$((i + 1))
  ratio "$i" "$n" >"$ctl/progress"
  f=$((f + 1))
done
[ -n "${S_PER_FRAME:-}" ] || echo "stand-in output of $S_NAME, frames $C4_FRAME_START-$C4_FRAME_END" >"$out/$S_NAME.$S_EXT"
