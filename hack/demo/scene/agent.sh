#!/bin/sh
# The GitHub demo's agent work (hack/demo/github.sh). The agent is a
# script: this demo shows the tracker loop, not a model. A graph job: its
# root, plan, lays out one build block and a review wait for the
# maintainer; the build writes a patch into the job's output view. A
# version made from review comments (mounted at /c4/notes/note.md by
# c4-sync) writes a patch that answers them.
set -eu
out="${C4_OUTPUT:-/show}"
case "${C4_UNIT_NAME:-}" in
plan)
  mkdir -p "$out/.c4"
  cat >"$out/.c4/expand.json" <<'EOF'
{"version": 1,
 "units": [
  {"name": "build", "label": "port the tier fan-in test", "key": "builder", "work": 30},
  {"name": "review", "label": "review: the pull request", "after": ["build"],
   "wait": {"server": "person:dana", "ask": "Review the pull request", "expect": 3600, "choices": ["approve", "rework"]}}
 ]}
EOF
  ;;
build)
  mkdir -p "$out/work"
  if [ -f /c4/notes/note.md ]; then
    {
      echo "--- a/tier/fanin_test.go"
      echo "+++ b/tier/fanin_test.go"
      echo "@@ the order of the fan-in is asserted, per review @@"
      echo "+	if !slices.Equal(got, want) { t.Fatalf(\"order %v, want %v\", got, want) }"
      echo
      echo "# answers:"
      sed 's/^/# /' /c4/notes/note.md
    } >"$out/work/patch.diff"
  else
    {
      echo "--- a/tier/fanin_test.go"
      echo "+++ b/tier/fanin_test.go"
      echo "@@ port the fan-in test @@"
      echo "+	if len(got) != len(want) { t.Fatalf(\"%d, want %d\", len(got), len(want)) }"
    } >"$out/work/patch.diff"
  fi
  sleep 20 # work takes a moment, so the HUD and the check run show it running
  ;;
*)
  echo "agent.sh: unknown unit ${C4_UNIT_NAME:-}" >&2
  exit 2
  ;;
esac
