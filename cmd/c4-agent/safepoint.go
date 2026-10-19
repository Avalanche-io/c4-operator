package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/agent"
)

// stopCode is c4-unit safepoint's exit status when the farm asked the unit
// to stop.
const stopCode = 10

// safepointWait is how long c4-unit waits for the farm's decision (the
// queue answers within a few seconds); the hook gives it this and a margin.
const safepointWait = 8 * time.Second

// hookSafepoint runs after the Stop hook's checkpoint when the session runs
// inside a farm unit (the queue sets $C4_PODINFO on every unit pod). The end
// of a turn is the unit's safe point: it announces it with
// `c4-unit safepoint --checkpoint REF`, and when the farm asked the unit to
// stop, it tells Claude Code to end the session here by printing
// {"continue": false} on stdout (which takes precedence over any other
// hook asking to continue). The safe point is a decision point: c4-unit
// waits up to safepointWait for the farm's answer, then falls back on the
// last request it can see, well inside the hook's own timeout.
//
// A member a host runner runs has no pod sandbox: the runner keeps its unit
// directory (output root, control directory, podinfo) and names it on the
// member record, so the hook points c4-unit there instead of relying on
// the session's environment. Outside a unit, or without c4-unit on the
// PATH, it does nothing. It never fails the hook.
func hookSafepoint(ctx context.Context, h *agent.Harness, input []byte, cp *agent.Checkpoint, stdout, stderr io.Writer) {
	var env []string
	if os.Getenv("C4_PODINFO") == "" {
		dir := unitDirOf(h, input)
		if dir == "" {
			return
		}
		u := agent.UnitDirsAt(dir)
		env = append(os.Environ(), "C4_OUTPUT="+u.Out, "C4_CONTROL="+u.Control, "C4_PODINFO="+u.PodInfo)
	}
	bin := os.Getenv("C4_UNIT_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("c4-unit"); err != nil {
			fmt.Fprintln(stderr, "c4-agent hook: in a farm unit but c4-unit is not on the PATH; no safe point announced")
			return
		}
	}
	ref := safepointRef(input, cp)
	ctx, cancel := context.WithTimeout(ctx, 2*safepointWait)
	defer cancel()
	args := []string{"safepoint"}
	if ref != "" {
		args = append(args, "--checkpoint", ref)
	}
	args = append(args, "--wait", safepointWait.String())
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = stderr
	cmd.Env = env // nil: the hook's own environment
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return
	case errors.As(err, &exit) && exit.ExitCode() == stopCode:
		out, _ := json.Marshal(map[string]any{
			"continue":   false,
			"stopReason": "The farm decided this unit stops at its safe point; the turn is saved.",
		})
		fmt.Fprintln(stdout, string(out))
	default:
		fmt.Fprintln(stderr, "c4-agent hook: c4-unit safepoint:", err)
	}
}

// safepointRef names the turn the safe point is at: the member's checkpoint
// as <session>#<seq>:<head> (the form the HUD reads), or, for a session that
// is not a member, its session ID.
func safepointRef(input []byte, cp *agent.Checkpoint) string {
	if cp != nil {
		return fmt.Sprintf("%s#%d:%s", cp.Session, cp.Seq, cp.Head)
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(input, &in) == nil {
		return in.SessionID
	}
	return ""
}

// unitDirOf is the unit directory a host runner recorded for the session's
// member, or "".
func unitDirOf(h *agent.Harness, input []byte) string {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if h == nil || json.Unmarshal(input, &in) != nil || in.SessionID == "" {
		return ""
	}
	m, err := h.Store.BySession(in.SessionID)
	if err != nil {
		return ""
	}
	return m.UnitDir
}
