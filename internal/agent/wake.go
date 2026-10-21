package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/Avalanche-io/c4-operator/internal/node"
)

// A member idle between turns does not see a message until it takes a
// turn, and the CLI cannot put one into a running session. So a member
// with unread messages (a person or the lead steering it, a question to it,
// an answer to its own) is woken: its idle session is stopped at that turn
// boundary, with nothing in flight, and resumed by its ID with the messages
// as its prompt. A session with background work still open is left to it:
// it takes a turn when the work reports, and is woken after.

// unreadMessages are the unit's messages past what it last read.
func unreadMessages(u UnitDirs) ([]SteerMessage, error) {
	if u.Control == "" {
		return nil, nil
	}
	msgs, err := readSteer(u.Control)
	if err != nil {
		return nil, err
	}
	seen, _ := node.ReadSteerSeen(u.Out)
	var out []SteerMessage
	for _, m := range msgs {
		if m.Seq > seen {
			out = append(out, m)
		}
	}
	return out, nil
}

// wakeOnMessages wakes an idle member that has unread messages, and
// reports whether it did.
func (r *Runner) wakeOnMessages(ctx context.Context, p *corev1.Pod, m *Member, u UnitDirs) (bool, error) {
	unread, err := unreadMessages(u)
	if err != nil || len(unread) == 0 {
		return false, err
	}
	res, err := r.Harness.Wake(ctx, m.Name, wakePrompt(unread, u))
	if errors.Is(err, ErrBusy) {
		return false, nil // it took a turn, or has background work open: next tick
	}
	if err != nil {
		return false, err
	}
	markSteerSeen(u, unread[len(unread)-1].Seq)
	r.since[p.UID] = r.now()
	r.logf("%s: woke for %d message(s) (epoch %d)", m.Name, len(unread), res.Member.Epoch)
	return true, r.annotateEpoch(ctx, p, res.Member)
}

// Wake stops a member's idle session at its turn boundary and resumes it
// by its ID with prompt. It is refused (ErrBusy) while the session is
// mid-turn or has background work open, and when it is not running.
func (h *Harness) Wake(ctx context.Context, name, prompt string) (ResumeResult, error) {
	unlock, err := h.Store.Lock(name)
	if err != nil {
		return ResumeResult{}, err
	}
	m, err := h.Store.Get(name)
	if err != nil {
		unlock()
		return ResumeResult{}, err
	}
	live, a, err := h.live(ctx, m)
	switch {
	case err != nil:
		unlock()
		return ResumeResult{}, err
	case !live:
		unlock()
		return ResumeResult{}, fmt.Errorf("member %s is not running: resume it instead", name)
	case !a.Idle():
		unlock()
		return ResumeResult{}, fmt.Errorf("member %s: %w (mid-turn); it reads its messages when it is idle", name, ErrBusy)
	}
	if bg, err := OpenBackground(m.Transcript, a.Started()); err != nil || len(bg) > 0 {
		unlock()
		if err != nil {
			return ResumeResult{}, err
		}
		return ResumeResult{}, fmt.Errorf("member %s: %w: %d background task(s) open; it takes a turn when they report", name, ErrBusy, len(bg))
	}
	if _, err := waitStable(m.Transcript, h.SettleQuiet, h.SettleMax); err != nil {
		unlock()
		return ResumeResult{}, err
	}
	if _, _, err := h.checkpointLocked(ctx, m, "wake"); err != nil {
		unlock()
		return ResumeResult{}, err
	}
	if err := h.Claude.Stop(ctx, m.ConfigDir, m.Session); err != nil {
		unlock()
		return ResumeResult{}, err
	}
	unlock()
	return h.Resume(ctx, name, prompt)
}

// wakePrompt gives a woken member its messages, and how to answer a
// question among them.
func wakePrompt(msgs []SteerMessage, u UnitDirs) string {
	var b strings.Builder
	b.WriteString("Messages for you arrived while you were idle:\n")
	asks := false
	for _, m := range msgs {
		from := m.Actor
		if from == "" {
			from = "the farm"
		}
		switch {
		case m.Ask != nil:
			asks = true
			fmt.Fprintf(&b, "- a question from %s (%s): %s\n", orElse(m.Ask.From, from), m.Ask.ID, m.Text)
		case m.Answer != nil && m.Answer.Cannot:
			fmt.Fprintf(&b, "- %s could not answer your question %s: %s\n", orElse(m.Answer.From, from), m.Answer.ID, m.Text)
		case m.Answer != nil:
			fmt.Fprintf(&b, "- the answer to your question %s, from %s: %s\n", m.Answer.ID, orElse(m.Answer.From, from), m.Text)
		default:
			fmt.Fprintf(&b, "- from %s: %s\n", from, m.Text)
		}
	}
	if asks {
		fmt.Fprintf(&b, "Answer each question from what you know: c4-unit answer --dir %s --ask <its id> --answer \"...\" (or --cannot \"why\").\n", u.Out)
	}
	b.WriteString("Act on them within your brief, then carry on.")
	return b.String()
}

func orElse(s, otherwise string) string {
	if strings.TrimSpace(s) == "" {
		return otherwise
	}
	return s
}
