package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// recheckDone ends a re-check on a host runner at its turn's end: Succeeded
// with the why when the block withdrew its question, else NeedsInformation
// with the text to keep.
func (r *Runner) recheckDone(ctx context.Context, p *corev1.Pod, m *Member, u UnitDirs) error {
	withdrawn, text, note := recheckEnd(p, u)
	r.logf("%s: re-checked its question and %s", m.Name, note)
	if !withdrawn {
		return r.needsInformation(ctx, p, m, text)
	}
	return r.finish(ctx, p, corev1.PodSucceeded, "Withdrawn", text, 0)
}

// A block that stopped for information its brief lacked (needs.json, exit
// 12) is an escalation that can heal: when related work lands, the farm
// runs a short extra attempt of it, a re-check, to say whether its question
// still applies. The re-check resumes the block's session for one turn,
// told to decide only, and ends at that turn's end: a withdrawal (.c4/withdraw,
// {"why": "..."}) ends it with exit 0 and the why as its message; needs.json
// again ends it with exit 12 and the new text; neither, or a turn past its
// time, is still needed, with the text it had.
const (
	// EnvRecheck marks a re-check attempt ("1").
	EnvRecheck = "C4_RECHECK"
	// EnvRecheckWhy says what fired the re-check, in plain words.
	EnvRecheckWhy = "C4_RECHECK_WHY"
	// EnvRecheckSeconds bounds a re-check's turn (default 300).
	EnvRecheckSeconds = "C4_RECHECK_SECONDS"
	// EnvWithdrawn is why the block's question was withdrawn, for the
	// attempt that resumes it.
	EnvWithdrawn = "C4_WITHDRAWN"
)

func recheck(p *corev1.Pod) bool { return strings.TrimSpace(podEnv(p, EnvRecheck)) == "1" }

// recheckLimit is how long a re-check's turn may take.
func recheckLimit(p *corev1.Pod) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(podEnv(p, EnvRecheckSeconds))); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return 300 * time.Second
}

// recheckPrompt wakes a block that stopped for information to decide,
// and only decide, whether its question still applies.
func recheckPrompt(p *corev1.Pod, u UnitDirs) string {
	var b strings.Builder
	b.WriteString("Earlier you stopped because your brief lacked information only a person could give")
	if needed := strings.TrimSpace(podEnv(p, "C4_NEEDED")); needed != "" {
		fmt.Fprintf(&b, ":\n%s\n\n", needed)
	} else {
		b.WriteString(".\n\n")
	}
	if why := strings.TrimSpace(podEnv(p, EnvRecheckWhy)); why != "" {
		fmt.Fprintf(&b, "Since then: %s.\n\n", why)
	} else {
		b.WriteString("Since then, related work has landed.\n\n")
	}
	if repo := podEnv(p, EnvBlockRepo); repo != "" {
		fmt.Fprintf(&b, "The project's repository is %s. ", repo)
	}
	b.WriteString("Look at what landed and decide one thing: does your question still apply? This turn is for that decision only; change nothing, and do not resume your task.\n")
	fmt.Fprintf(&b, "- If it no longer applies (the work answers it, or makes it moot), write withdraw in %s/.c4/, {\"why\": \"what settled it, in one line\"}.\n", u.Out)
	fmt.Fprintf(&b, "- If it still applies, write needs.json in %s/.c4/ again, {\"needs\": \"...\", \"why\": \"...\"}, sharpened by what you now know.\n", u.Out)
	b.WriteString("Then end your turn. You resume your task later, with the answer or with why the question was withdrawn.")
	return b.String()
}

// takeWithdraw reads and consumes a block's .c4/withdraw. ok is false when
// there is none; err says why a file is malformed.
func takeWithdraw(u UnitDirs) (why string, ok bool, err error) {
	path := filepath.Join(u.Out, ".c4", "withdraw")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", true, err
	}
	os.Remove(path)
	var w struct {
		Why string `json:"why"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return "", true, fmt.Errorf("withdraw: %v", err)
	}
	if strings.TrimSpace(w.Why) == "" {
		return "", true, errors.New("withdraw: no why")
	}
	return strings.TrimSpace(w.Why), true, nil
}

// recheckEnd is how a re-check's turn ended: withdrawn, with why, or still
// needed, with the text to keep. note says what the runner saw, for its log.
func recheckEnd(p *corev1.Pod, u UnitDirs) (withdrawn bool, text, note string) {
	old := strings.TrimSpace(podEnv(p, "C4_NEEDED"))
	if why, ok, err := takeWithdraw(u); ok {
		if err == nil {
			takeNeeds(u) // a withdrawal wins over a stale needs.json
			return true, why, "withdrew its question: " + why
		}
		return false, old, "wrote a withdrawal it could not mean: " + err.Error()
	}
	if msg, ok, err := takeNeeds(u); ok {
		if err == nil {
			return false, msg, "still needs it"
		}
		return false, old, "still needs it, but " + err.Error()
	}
	return false, old, "wrote no decision: still needed"
}

// withdrawnText is why the block's question was withdrawn ($C4_WITHDRAWN),
// for the attempt that resumes it.
func withdrawnText(p *corev1.Pod) string {
	why := strings.TrimSpace(podEnv(p, EnvWithdrawn))
	if why == "" {
		return ""
	}
	if needed := strings.TrimSpace(podEnv(p, "C4_NEEDED")); needed != "" {
		return fmt.Sprintf("You stopped because your brief lacked:\n%s\n\nThat question was withdrawn: %s\n\nCarry on with your task, using what settled it.\n\n", needed, why)
	}
	return fmt.Sprintf("The question you stopped on was withdrawn: %s\n\nCarry on with your task, using what settled it.\n\n", why)
}

// recheck runs a re-check's one turn in a pod, within limit, and ends the
// attempt: exit 0 with the why as its termination message when the block
// withdrew its question, else exit 12 with the text to keep.
func (b *PodBlock) recheck(ctx context.Context, m *Member, u UnitDirs, args []string, limit time.Duration) (int, error) {
	p := b.pod()
	tctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	err := b.claude(tctx, args, nil)
	if tctx.Err() != nil && ctx.Err() == nil {
		b.logf("%s: its re-check ran past %s: still needed", m.Name, limit)
		return b.needsInformation(m, neededText(p))
	}
	if err != nil {
		return 1, err
	}
	withdrawn, text, note := recheckEnd(p, u)
	b.logf("%s: re-checked its question and %s", m.Name, note)
	if !withdrawn {
		return b.needsInformation(m, text)
	}
	if err := os.WriteFile(b.terminationLog(), []byte(text), 0o644); err != nil {
		return 1, err
	}
	return PodBlockDone, nil
}

// neededText is the question a re-check was asked about ($C4_NEEDED).
func neededText(p *corev1.Pod) string { return strings.TrimSpace(podEnv(p, "C4_NEEDED")) }
