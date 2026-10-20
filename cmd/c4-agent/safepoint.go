package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	out := os.Getenv("C4_OUTPUT")
	if os.Getenv("C4_PODINFO") == "" {
		dir := unitDirOf(h, input)
		if dir == "" {
			return
		}
		u := agent.UnitDirsAt(dir)
		out = u.Out
		env = append(os.Environ(), "C4_OUTPUT="+u.Out, "C4_CONTROL="+u.Control, "C4_PODINFO="+u.PodInfo)
	}
	// the turn's tokens first: a stop at this safe point ends the session
	if out != "" {
		if err := hookUsage(h, input, cp, out); err != nil {
			fmt.Fprintln(stderr, "c4-agent hook: usage:", err)
		}
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
	case errors.As(err, &exit) && exit.ExitCode() == stopCode:
		out, _ := json.Marshal(map[string]any{"continue": false,
			"stopReason": "The farm decided this unit stops at its safe point; the turn is saved."})
		fmt.Fprintln(stdout, string(out))
		return // its messages wait for the attempt that resumes it
	case err != nil:
		fmt.Fprintln(stderr, "c4-agent hook: c4-unit safepoint:", err)
	}
	hookSteer(ctx, bin, env, stdout, stderr)
}

// hookSteer delivers what people sent the unit while it worked (c4-unit
// steer: the messages it has not seen, marked seen as they are read) at
// the end of a turn that goes on: the hook keeps the session from stopping
// and gives it the messages, so it takes them into account now rather
// than when it next thinks to look. A turn the farm stops leaves them for
// the attempt that resumes it.
func hookSteer(ctx context.Context, bin string, env []string, stdout, stderr io.Writer) {
	var msgs strings.Builder
	cmd := exec.CommandContext(ctx, bin, "steer")
	cmd.Stdout, cmd.Stderr = &msgs, stderr
	cmd.Env = env
	if cmd.Run() != nil || strings.TrimSpace(msgs.String()) == "" {
		return // exit 1: nothing new
	}
	out, _ := json.Marshal(map[string]any{
		"decision": "block",
		"reason": "Messages for you arrived while you worked:\n" + strings.TrimSpace(msgs.String()) +
			"\nTake them into account and carry on with your task, then end your turn as before.",
	})
	fmt.Fprintln(stdout, string(out))
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

// hookUsage writes the attempt's token use so far to the unit's
// .c4/usage.json, per model (the queue's model spend; c4-unit usage's
// shape), totalled from the session's transcript, with what it reasoned
// with (models, effort, thinking, tools; the member's declared profile). The attempt starts where
// the checkpoint it resumed from ends, recorded in .c4/usage.from at its
// first turn, so a resumed member's earlier attempts are not counted
// again. The file is rewritten whole each turn, so a hook that runs twice
// counts nothing twice; for a member session the hook owns it.
func hookUsage(h *agent.Harness, input []byte, cp *agent.Checkpoint, out string) error {
	var in struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil
	}
	m, _ := h.Store.BySession(in.SessionID)
	if in.TranscriptPath == "" && m != nil {
		in.TranscriptPath = m.Transcript
	}
	if in.TranscriptPath == "" {
		return nil
	}
	dir := filepath.Join(out, ".c4")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fromFile := filepath.Join(dir, "usage.from")
	var from int64
	if b, err := os.ReadFile(fromFile); err == nil {
		from, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	} else {
		if m != nil && cp != nil {
			from = h.Store.AttemptStart(m.Name, cp)
		}
		if err := os.WriteFile(fromFile, []byte(strconv.FormatInt(from, 10)+"\n"), 0o644); err != nil {
			return err
		}
	}
	var to int64
	if cp != nil {
		to = cp.Offset
	}
	d, err := agent.TranscriptUsage(in.TranscriptPath, from, to)
	if err != nil {
		return err
	}
	if m != nil && d.Reasoning != nil {
		d.Reasoning.Tools = m.Profile
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".usage.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "usage.json"))
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
