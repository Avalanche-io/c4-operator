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
	"github.com/Avalanche-io/c4-operator/internal/node"
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
	env, out, _, ok := unitEnv(h, input)
	if !ok {
		return
	}
	// the turn's tokens first: a stop at this safe point ends the session
	if out != "" {
		if err := hookUsage(h, input, cp, out); err != nil {
			fmt.Fprintln(stderr, "c4-agent hook: usage:", err)
		}
	}
	bin, ok := unitBin()
	if !ok {
		fmt.Fprintln(stderr, "c4-agent hook: in a farm unit but c4-unit is not on the PATH; no safe point announced")
		return
	}
	ref := safepointRef(input, cp)
	ctx, cancel := context.WithTimeout(ctx, 2*safepointWait)
	defer cancel()
	// the turn's answer to a question it was asked, and its own question
	// to another unit, go to the farm before the safe point
	if out != "" {
		hookAnswer(ctx, bin, env, out, stderr)
		hookAsk(ctx, bin, env, out, stderr)
	}
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
	hookSteer(ctx, bin, env, out, false, stdout, stderr)
}

// unitEnv is the environment c4-unit runs in for the session's unit, its
// output root and its control directory: the hook's own in a pod, the unit
// directory the host runner recorded for the session's member on a host.
// ok is false outside a unit.
func unitEnv(h *agent.Harness, input []byte) (env []string, out, control string, ok bool) {
	if os.Getenv("C4_PODINFO") != "" {
		return nil, os.Getenv("C4_OUTPUT"), os.Getenv("C4_CONTROL"), true // nil: the hook's own environment
	}
	dir := unitDirOf(h, input)
	if dir == "" {
		return nil, "", "", false
	}
	u := agent.UnitDirsAt(dir)
	return append(os.Environ(), "C4_OUTPUT="+u.Out, "C4_CONTROL="+u.Control, "C4_PODINFO="+u.PodInfo), u.Out, u.Control, true
}

// steerWaiting reports whether the unit has a message it has not seen:
// one in control/steer numbered past .c4/steer-seen. It runs after every
// tool call, so it starts no process; c4-unit still reads and marks them.
func steerWaiting(control, out string) bool {
	if control == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(control, "steer"))
	if err != nil {
		return false
	}
	var msgs []struct {
		Seq int `json:"seq"`
	}
	if json.Unmarshal(b, &msgs) != nil {
		return true // c4-unit says what is wrong with it
	}
	seen, _ := node.ReadSteerSeen(out)
	for _, m := range msgs {
		if m.Seq > seen {
			return true
		}
	}
	return false
}

// unitBin is the c4-unit the hook runs.
func unitBin() (string, bool) {
	if bin := os.Getenv("C4_UNIT_BIN"); bin != "" {
		return bin, true
	}
	bin, err := exec.LookPath("c4-unit")
	return bin, err == nil
}

// hookSteerAtTool delivers, after a tool call, what was sent to the unit
// while it worked, so a steer reaches the model at its next tool call, not
// only at the end of its turn. The messages are marked seen as they are
// read, so each is delivered once, here or at the turn's end. Outside a
// unit it does nothing.
func hookSteerAtTool(ctx context.Context, h *agent.Harness, input []byte, stdout, stderr io.Writer) {
	env, out, control, ok := unitEnv(h, input)
	if !ok || !steerWaiting(control, out) {
		return
	}
	bin, ok := unitBin()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, steerAtToolWait)
	defer cancel()
	hookSteer(ctx, bin, env, out, true, stdout, stderr)
}

// steerAtToolWait bounds the steer read after a tool call, well inside the
// hook's 10-second timeout.
const steerAtToolWait = 5 * time.Second

// hookAsk puts a turn's question to another unit (.c4/ask.json:
// {"to": "unit:<name>", "ask": "..."}; to defaults to the job's answerer)
// to the farm with c4-unit ask, and records it open (.c4/asking), so the
// runner holds the block for the answer. A question that cannot be put
// becomes the block's next-part note: it decides within its brief.
func hookAsk(ctx context.Context, bin string, env []string, out string, stderr io.Writer) {
	var q struct {
		To  string `json:"to,omitempty"`
		Ask string `json:"ask"`
	}
	ok, err := takeTurnFile(out, "ask.json", &q)
	if !ok {
		return
	}
	if err == nil && strings.TrimSpace(q.Ask) == "" {
		err = errors.New("ask.json: no question in ask")
	}
	if err == nil && q.To != "" && !strings.HasPrefix(q.To, "unit:") {
		err = fmt.Errorf("ask.json: to %q is not unit:<name>", q.To)
	}
	var id strings.Builder
	if err == nil {
		args := []string{"ask", "--ask", strings.TrimSpace(q.Ask)}
		if q.To != "" {
			args = append(args, "--to", q.To)
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stdout, cmd.Stderr, cmd.Env = &id, stderr, env
		if err = cmd.Run(); err == nil {
			err = agent.WriteAsking(out, agent.Asking{ID: strings.TrimSpace(id.String()), To: q.To, At: time.Now().UTC()})
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "c4-agent hook: the question was not put:", err)
		note := "Your question could not be put (" + err.Error() + "). Decide within your brief and carry on with your task."
		os.WriteFile(filepath.Join(out, ".c4", "more"), []byte(note+"\n"), 0o644)
	}
}

// hookAnswer gives the farm a turn's answer to a question it was asked
// (.c4/answer.json: {"ask": "<id>", "answer": "..."} or {"ask": "<id>",
// "cannot": "why"}), with c4-unit answer.
func hookAnswer(ctx context.Context, bin string, env []string, out string, stderr io.Writer) {
	var a struct {
		Ask    string `json:"ask"`
		Answer string `json:"answer,omitempty"`
		Cannot string `json:"cannot,omitempty"`
	}
	ok, err := takeTurnFile(out, "answer.json", &a)
	if !ok {
		return
	}
	if err == nil && (a.Ask == "" || (a.Answer == "") == (a.Cannot == "")) {
		err = errors.New(`answer.json: needs "ask" and one of "answer" or "cannot"`)
	}
	if err == nil {
		args := []string{"answer", "--ask", a.Ask}
		if a.Answer != "" {
			args = append(args, "--answer", a.Answer)
		} else {
			args = append(args, "--cannot", a.Cannot)
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stderr, cmd.Env = stderr, env
		err = cmd.Run()
	}
	if err != nil {
		fmt.Fprintln(stderr, "c4-agent hook: the answer was not given:", err)
	}
}

// takeTurnFile reads and consumes a document the turn left in out/.c4/,
// strictly; ok is false when there is none.
func takeTurnFile(out, name string, v any) (ok bool, err error) {
	path := filepath.Join(out, ".c4", name)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	os.Remove(path)
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return true, fmt.Errorf("%s: %v", name, err)
	}
	return true, nil
}

// hookSteer delivers what was sent to the unit while it worked (c4-unit
// steer --json: the messages it has not seen, marked seen as they are read)
// at the end of a turn that goes on: the hook keeps the session from
// stopping and gives it the messages, so it takes them into account now. A
// person's message is passed on; a question from another unit comes with
// how to answer it; an answer to the block's own open question closes it.
// A turn the farm stops leaves them for the attempt that resumes it.
// atTool is a delivery after a tool call, which the session reads before
// its next step.
func hookSteer(ctx context.Context, bin string, env []string, out string, atTool bool, stdout, stderr io.Writer) {
	var raw strings.Builder
	cmd := exec.CommandContext(ctx, bin, "steer", "--json")
	cmd.Stdout, cmd.Stderr = &raw, stderr
	cmd.Env = env
	if cmd.Run() != nil || strings.TrimSpace(raw.String()) == "" {
		return // exit 1: nothing new
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(raw.String()), "\n") {
		var m agent.SteerMessage
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		lines = append(lines, steerLine(m, out))
	}
	if len(lines) == 0 {
		return
	}
	text := "Messages for you arrived while you worked:\n" + strings.Join(lines, "\n") +
		"\nTake them into account and carry on with your task, then end your turn as before."
	if atTool {
		text = "Your last tool call ran as usual. Messages for you arrived while you worked:\n" + strings.Join(lines, "\n") +
			"\nTake them into account in what you do next, and carry on with your task."
	}
	reason, _ := json.Marshal(map[string]any{"decision": "block", "reason": text})
	fmt.Fprintln(stdout, string(reason))
}

// steerLine is one message as the session reads it.
func steerLine(m agent.SteerMessage, out string) string {
	text := m.Text
	switch {
	case m.Ask != nil:
		// the farm's text says how to answer from a shell; a block answers
		// with a file
		if i := strings.Index(text, "\n(answer with:"); i >= 0 {
			text = text[:i]
		}
		return fmt.Sprintf(`- %s. Answer it from what you know: write answer.json in %s/.c4/, {"ask": %q, "answer": "..."}, or {"ask": %q, "cannot": "why"} if you cannot.`, text, out, m.Ask.ID, m.Ask.ID)
	case m.Answer != nil:
		if a, ok := agent.ReadAsking(out); ok && a.ID == m.Answer.ID {
			agent.ClearAsking(out)
		}
		return "- " + text
	}
	who := m.Actor
	if who == "" {
		who = "someone"
	}
	return fmt.Sprintf("- %s: %s", who, text)
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
