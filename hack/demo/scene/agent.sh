#!/bin/sh
# The GitHub demo's agent work (hack/demo/github.sh). By default the
# agent is a script: the demo shows the tracker loop, not a model. With
# DEMO_BLOCK=1 (set in the job's template) the build is a real block,
# Claude Code run by c4-agent block-pod on the sign-in Joshua lends. A graph job: its
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
  if [ -n "${DEMO_BLOCK:-}" ]; then
    # a block reasons with a model; declared, so the HUD shows it beside
    # what the attempt actually used
    sed 's/"work": 30}/"work": 30, "reasoning": {"does": "model", "effort": "low"}}/' "$out/.c4/expand.json" >"$out/.c4/expand.tmp"
    mv "$out/.c4/expand.tmp" "$out/.c4/expand.json"
  fi
  ;;
build)
  mkdir -p "$out/work"
  if [ -n "${DEMO_BLOCK:-}" ]; then
    # The block reads only its own folder, so a review it answers comes
    # in its brief.
    brief="Port the tier fan-in test for acme/widgets issue #7. Write the change as a
unified diff to work/patch.diff in this folder, adding a test in
tier/fanin_test.go that checks a fan-in of three producers delivers every
item exactly once. Keep it short. Write only that file."
    if [ -f /c4/notes/note.md ]; then
      brief="$brief

This is version 2. The maintainer's review of version 1, which the new
diff must answer:
$(cat /c4/notes/note.md)"
    fi
    export C4_AGENT_BLOCK=1 C4_BLOCK_PROMPT="$brief" C4_BLOCK_TOOLS="Read Edit Write Glob Grep" C4_BLOCK_EFFORT=low
    exec c4-agent block-pod --cwd /private/tmp
  fi
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
