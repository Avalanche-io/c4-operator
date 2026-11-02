package main

import (
	"io"
	"os"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/unitact"
)

// A unit signs what it asks, answers, needs, withdraws and waits on as
// itself (internal/unitact): c4-unit is how a unit's own process does it.

const (
	unitActsMax  = unitact.Max
	actsSeenWait = unitact.SeenWait
)

// unit is this process's unit, for signing.
func (e env) unit() unitact.Unit {
	return unitact.Unit{Getenv: e.get, Now: e.now, Sleep: e.sleep, Issue: e.issue}
}

func unitSigns(e env) bool { return e.unit().Signs() }

func signAct(e env, out, act string, params map[string]any) (int, error) {
	return e.unit().Sign(out, act, params)
}

func awaitActsSeen(e env, n int, wait time.Duration) bool { return e.unit().AwaitSeen(n, wait) }

func signReport(e env, out, act string, params map[string]any, w io.Writer) {
	e.unit().Report(out, act, params, w)
}

func signNote(e env, out, act string, params map[string]any, w io.Writer) {
	e.unit().Note(out, act, params, w)
}

func nextNoteSeq(path string) int { return unitact.NextNoteSeq(path) }

// errw is where c4-unit says what went wrong beside its answer.
func (e env) errw() io.Writer {
	if e.stderr != nil {
		return e.stderr
	}
	return os.Stderr
}
