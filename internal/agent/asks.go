package agent

import (
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

// Questions between blocks (DESIGN.md 10.6, Agents answer agents). A
// block that lacks a fact another unit of its job has writes .c4/ask.json
// and ends its turn; the Stop hook puts the question to the farm (c4-unit
// ask) and records it open in .c4/asking. The farm routes it to the
// answering unit as a steering message, and the answer back the same way.
// Meanwhile the block keeps its slot: the runner (or block-pod) holds it
// idle and wakes its session with the answer. An answer slower than its
// hold (C4_ASK_HOLD, default 10 minutes) turns into a wait served by the
// answerer (c4-unit wait --server unit:<name>): the block gives up its
// slot and resumes with the answer as its decision.

// DefaultAskHold is how long a block keeps its slot waiting for an answer.
const DefaultAskHold = 10 * time.Minute

// Asking is a block's open question, .c4/asking.
type Asking struct {
	ID string `json:"id"`
	// To is the unit asked (unit:<name>); "" is the job's answerer.
	To string    `json:"to,omitempty"`
	At time.Time `json:"at"`
}

func askingPath(out string) string { return filepath.Join(out, ".c4", "asking") }

// WriteAsking records a block's question as open.
func WriteAsking(out string, a Asking) error {
	b, _ := json.Marshal(a)
	return writeAtomic(askingPath(out), append(b, '\n'))
}

// ReadAsking is the block's open question, if it has one.
func ReadAsking(out string) (*Asking, bool) {
	b, err := os.ReadFile(askingPath(out))
	if err != nil {
		return nil, false
	}
	var a Asking
	if json.Unmarshal(b, &a) != nil || a.ID == "" {
		return nil, false
	}
	return &a, true
}

// ClearAsking closes the block's open question.
func ClearAsking(out string) { os.Remove(askingPath(out)) }

// SteerMessage is a message to a unit, as $C4_CONTROL/steer and c4-unit
// steer --json carry it: a person's, or a question to the unit, or an
// answer to one it asked.
type SteerMessage struct {
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor,omitempty"`
	Text   string    `json:"text"`
	Ask    *AskRef   `json:"ask,omitempty"`
	Answer *AskRef   `json:"answer,omitempty"`
}

// AskRef names a question: its ID, who it is from, and for an answer
// whether the answerer could not answer.
type AskRef struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	Cannot bool   `json:"cannot,omitempty"`
}

// readSteer reads the unit's steering messages.
func readSteer(control string) ([]SteerMessage, error) {
	b, err := os.ReadFile(filepath.Join(control, "steer"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var msgs []SteerMessage
	if err := json.Unmarshal(b, &msgs); err != nil {
		return nil, fmt.Errorf("steer: %w", err)
	}
	return msgs, nil
}

// answerFor finds the answer to question id among the unit's messages.
func answerFor(u UnitDirs, id string) (*SteerMessage, bool) {
	if u.Control == "" {
		return nil, false
	}
	msgs, err := readSteer(u.Control)
	if err != nil {
		return nil, false
	}
	for i := range msgs {
		if a := msgs[i].Answer; a != nil && a.ID == id {
			return &msgs[i], true
		}
	}
	return nil, false
}

// markSteerSeen notes messages up to seq as read (out/.c4/steer-seen, as
// c4-unit steer keeps it), so an answer the block was woken with is not
// delivered again at its next turn's end.
func markSteerSeen(u UnitDirs, seq int) {
	path := filepath.Join(u.Out, ".c4", "steer-seen")
	if b, err := os.ReadFile(path); err == nil {
		if seen, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && seen >= seq {
			return
		}
	}
	writeAtomic(path, []byte(strconv.Itoa(seq)+"\n"))
}

// answerPrompt wakes a block with the answer to its question.
func answerPrompt(a *Asking, m *SteerMessage, u UnitDirs) string {
	var b strings.Builder
	from := "the unit you asked"
	if m.Answer != nil && m.Answer.From != "" {
		from = m.Answer.From
	}
	if m.Answer != nil && m.Answer.Cannot {
		fmt.Fprintf(&b, "Your question %s could not be answered by %s: %s\n\nDecide within your brief, or if only a person could give it, report it missing.\n\n", a.ID, from, m.Text)
	} else {
		fmt.Fprintf(&b, "Your question %s was answered by %s:\n%s\n\nCarry on with your task.\n\n", a.ID, from, m.Text)
	}
	fmt.Fprintf(&b, "The output directory is still %s.", u.Out)
	return b.String()
}

// askHold is how long a block keeps its slot waiting for an answer.
func askHold(p *corev1.Pod) time.Duration {
	if d, err := time.ParseDuration(podEnv(p, "C4_ASK_HOLD")); err == nil && d > 0 {
		return d
	}
	return DefaultAskHold
}
