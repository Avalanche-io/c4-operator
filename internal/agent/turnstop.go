package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A turn can run for most of an hour, and the farm stops a member only at
// a turn's end. So the stop lands within a step instead: after each tool
// call (Claude Code's PostToolUse hook, the same c4-agent checkpoint --hook),
// the member's unit is checked for a stop the farm has asked for (the
// c4.avalanche.io/yield annotation in its podinfo), and if there is one,
// the model is told to finish the step, leave its work where it can be
// picked up, and end its turn. The turn's end is then the safe point the
// stop waits for.

// StopAsked reads a PostToolUse hook's input and, when the farm has asked
// the member's unit to stop, returns what to tell the model. A session that
// is not a member, or one with no unit, is never told anything.
func (h *Harness) StopAsked(input []byte) (string, bool, error) {
	var in hookInput
	if err := json.NewDecoder(bytes.NewReader(input)).Decode(&in); err != nil {
		return "", false, fmt.Errorf("hook input: %w", err)
	}
	if in.SessionID == "" {
		return "", false, nil
	}
	m, err := h.Store.BySession(in.SessionID)
	if errors.Is(err, ErrNoMember) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	// a unit pod's own podinfo, else the host runner's for the member
	info := os.Getenv("C4_PODINFO")
	if info == "" && m.UnitDir != "" {
		info = UnitDirsAt(m.UnitDir).PodInfo
	}
	if info == "" {
		return "", false, nil
	}
	why, ok := yieldAsked(info)
	if !ok {
		return "", false, nil
	}
	return stopText(why), true, nil
}

// yieldAsked reads a podinfo file (key="value" lines, the downward API's
// form) for the farm's soft stop.
func yieldAsked(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.TrimSpace(k) != c4v1.AnnotationYield {
			continue
		}
		if u, err := strconv.Unquote(strings.TrimSpace(v)); err == nil {
			v = u
		}
		return strings.TrimSpace(v), true
	}
	return "", false
}

func stopText(why string) string {
	s := "The farm has asked this session to stop"
	if why != "" {
		s += " (" + why + ")"
	}
	return s + ". Finish the step you are on, leave your work where it can be picked up (commit it, or note where you are in your progress file), and end your turn now. You will be resumed later, with your conversation intact."
}

// HookEvent is the hook event a hook's input names (Stop, PostToolUse).
func HookEvent(input []byte) string {
	var in hookInput
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	return in.HookEventName
}

// HookDecision is what a PostToolUse hook prints to stop a turn early:
// Claude Code gives the reason to the model.
func HookDecision(reason string) []byte {
	b, _ := json.Marshal(map[string]string{"decision": "block", "reason": reason})
	return b
}
