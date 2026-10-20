// Package effect makes a write to an outside service (a tracker, a
// registry, a git remote) happen once (integrations GAPS 11; the effect
// gateway of agent-jobs 03.5, as a library). It is the transactional
// outbox at the call boundary:
//
//  1. the intent is journaled under its key before it is sent;
//  2. a call the journal shows done is answered from the record, not sent;
//  3. a call the journal shows interrupted (sent, with no answer kept) is
//     sent again only when that is safe: the write is idempotent by
//     itself, or the service takes the key and drops a repeat, or a probe
//     says it did not happen;
//  4. otherwise it is held as unconfirmed, for a person to say whether it
//     happened; it is never sent twice on a guess.
//
// A probe runs only when the journal cannot vouch for a key (after an
// interruption, or with no entry for it at all), never before every write.
package effect

import (
	"context"
	"fmt"
	"time"
)

// Guard is how a write is kept from happening twice.
type Guard int

const (
	// Unguarded: nothing the service offers tells a repeat apart. An
	// interrupted call is held for a person.
	Unguarded Guard = iota
	// Idempotent: sending it again changes nothing (setting a field to a
	// value, editing a comment in place, updating a check run).
	Idempotent
	// Keyed: the service takes the idempotency key and drops a repeat.
	Keyed
	// Probed: a probe tells whether it already happened (a comment or
	// version carrying the key's marker).
	Probed
)

func (g Guard) String() string {
	return [...]string{"unguarded", "idempotent", "keyed", "probed"}[g]
}

// State is where a journaled call stands.
type State string

const (
	// Sending: journaled and sent, or about to be; no answer kept yet.
	Sending State = "sending"
	// Done: it happened; Ref is what it made.
	Done State = "done"
	// Unconfirmed: interrupted, and nothing can tell whether it happened;
	// held for a person.
	Unconfirmed State = "unconfirmed"
)

// Entry is one call as the journal keeps it.
type Entry struct {
	Key string
	// Intent fingerprints what is sent: a new intent under the same key is
	// a new call.
	Intent string
	State  State
	// Ref is what the call made or changed, once done.
	Ref string
	At  time.Time
}

// Journal keeps entries by key. Save must not lose a write it returned
// nil for: the next call trusts it.
type Journal interface {
	Load(ctx context.Context, key string) (Entry, bool, error)
	Save(ctx context.Context, e Entry) error
	// Drop takes back a Sending entry whose call is known not to have
	// happened (the service refused it), so the next call is fresh.
	Drop(ctx context.Context, key string) error
}

// Call is one write.
type Call struct {
	Key    string
	Intent string
	Guard  Guard
	// Send makes the write and returns what it made (an ID), if anything.
	Send func(ctx context.Context) (ref string, err error)
	// Probe says whether the write already happened, and what it made;
	// required for Probed.
	Probe func(ctx context.Context) (done bool, ref string, err error)
	// Definite says an error from Send means the write did not happen
	// (the service answered and refused it, or asked to wait); any other
	// error leaves the call interrupted. Nil: no error is definite.
	Definite func(error) bool
}

// HeldError is a call held as unconfirmed: interrupted, unguarded, and
// waiting for a person to say whether it happened (Confirm).
type HeldError struct {
	Key, Intent string
	Since       time.Time
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("effect %s: unconfirmed since %s: it was interrupted, and nothing can tell whether it happened; a person confirms it",
		e.Key, e.Since.UTC().Format(time.RFC3339))
}

// Do makes the call once. It returns what the call made, from the record
// when it was made already, or a *HeldError when it is held.
func Do(ctx context.Context, j Journal, c Call, now time.Time) (string, error) {
	e, ok, err := j.Load(ctx, c.Key)
	if err != nil {
		return "", err
	}
	if !ok && c.Guard == Probed {
		// No entry at all: the journal cannot vouch that this was never
		// sent (a journal restored without it, or newly kept). One probe,
		// then the entry vouches from here on.
		done, ref, err := c.Probe(ctx)
		if err != nil {
			return "", err
		}
		if done {
			return ref, j.Save(ctx, Entry{Key: c.Key, Intent: c.Intent, State: Done, Ref: ref, At: now})
		}
	}
	if ok && e.Intent == c.Intent {
		switch e.State {
		case Done:
			return e.Ref, nil
		case Unconfirmed:
			return "", &HeldError{Key: c.Key, Intent: c.Intent, Since: e.At}
		case Sending:
			// Interrupted: sent, perhaps, with no answer kept.
			switch c.Guard {
			case Idempotent, Keyed:
			case Probed:
				done, ref, err := c.Probe(ctx)
				if err != nil {
					return "", err
				}
				if done {
					return ref, j.Save(ctx, Entry{Key: c.Key, Intent: c.Intent, State: Done, Ref: ref, At: now})
				}
			default:
				held := Entry{Key: c.Key, Intent: c.Intent, State: Unconfirmed, Ref: e.Ref, At: now}
				if err := j.Save(ctx, held); err != nil {
					return "", err
				}
				return "", &HeldError{Key: c.Key, Intent: c.Intent, Since: now}
			}
		}
	}
	if err := j.Save(ctx, Entry{Key: c.Key, Intent: c.Intent, State: Sending, Ref: e.Ref, At: now}); err != nil {
		return "", err
	}
	ref, err := c.Send(ctx)
	if err != nil {
		if c.Definite != nil && c.Definite(err) {
			if derr := j.Drop(ctx, c.Key); derr != nil {
				return "", fmt.Errorf("%w (and dropping its journal entry: %v)", err, derr)
			}
		}
		return "", err
	}
	return ref, j.Save(ctx, Entry{Key: c.Key, Intent: c.Intent, State: Done, Ref: ref, At: now})
}

// Confirm is a person's answer for a held call: it happened (ref is what
// it made, if known), or it did not, and the next Do sends it.
func Confirm(ctx context.Context, j Journal, key string, happened bool, ref string, now time.Time) error {
	e, ok, err := j.Load(ctx, key)
	if err != nil {
		return err
	}
	if !ok || e.State != Unconfirmed {
		return fmt.Errorf("effect %s: nothing is held for confirmation", key)
	}
	if !happened {
		return j.Drop(ctx, key)
	}
	if ref == "" {
		ref = e.Ref
	}
	return j.Save(ctx, Entry{Key: key, Intent: e.Intent, State: Done, Ref: ref, At: now})
}

// Memory is a Journal in memory, for tests and short-lived callers.
type Memory struct {
	entries map[string]Entry
	prev    map[string]Entry
}

func (m *Memory) Load(_ context.Context, key string) (Entry, bool, error) {
	e, ok := m.entries[key]
	return e, ok, nil
}

func (m *Memory) Save(_ context.Context, e Entry) error {
	if m.entries == nil {
		m.entries, m.prev = map[string]Entry{}, map[string]Entry{}
	}
	if old, ok := m.entries[e.Key]; ok && old.State == Done {
		m.prev[e.Key] = old
	}
	m.entries[e.Key] = e
	return nil
}

// Drop restores the key's last done entry, if any.
func (m *Memory) Drop(_ context.Context, key string) error {
	if old, ok := m.prev[key]; ok {
		m.entries[key] = old
		return nil
	}
	delete(m.entries, key)
	return nil
}
