package effect

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errAway = errors.New("connection reset")
var errRefused = errors.New("422: refused")

// service counts the writes it took, and fails or "loses the answer" on
// request: the write happens, but the caller gets an error.
type service struct {
	sent, probes int
	fail         error
	lose         bool
}

func (s *service) call(g Guard, key, intent string) Call {
	return Call{Key: key, Intent: intent, Guard: g,
		Send: func(context.Context) (string, error) {
			if s.fail != nil {
				return "", s.fail
			}
			s.sent++
			if s.lose {
				return "", errAway
			}
			return "comment-1", nil
		},
		Probe: func(context.Context) (bool, string, error) {
			s.probes++
			return s.sent > 0, "comment-1", nil
		},
		Definite: func(err error) bool { return errors.Is(err, errRefused) },
	}
}

var t0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// A done call is answered from the record; a new intent under the same
// key is a new call; a probe runs once, for the key the journal had no
// entry for, and not before every write.
func TestDoOnce(t *testing.T) {
	ctx := context.Background()
	j, s := &Memory{}, &service{}
	for i := 0; i < 3; i++ {
		ref, err := Do(ctx, j, s.call(Probed, "comment", "running"), t0)
		if err != nil || ref != "comment-1" {
			t.Fatalf("try %d: %q %v", i, ref, err)
		}
	}
	if s.sent != 1 || s.probes != 1 {
		t.Fatalf("sent %d, probed %d: want one send and one probe", s.sent, s.probes)
	}
	if _, err := Do(ctx, j, s.call(Probed, "comment", "failed"), t0); err != nil || s.sent != 2 || s.probes != 1 {
		t.Fatalf("a new intent: %v, sent %d, probed %d", err, s.sent, s.probes)
	}
	// A journal that lost its entries probes before sending, and finds it.
	j = &Memory{}
	if ref, err := Do(ctx, j, s.call(Probed, "comment", "failed"), t0); err != nil || ref != "comment-1" || s.sent != 2 {
		t.Fatalf("after a lost journal: %q %v, sent %d", ref, err, s.sent)
	}
}

// An interrupted call (the write happened, the answer was lost) is probed
// before it is sent again, sent again when safe, and held when nothing can
// tell.
func TestDoInterrupted(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		guard      Guard
		sent, held bool
		probes     int
	}{
		{Probed, false, false, 2}, // once for the key it had no entry for, once after the interruption
		{Idempotent, true, false, 0},
		{Keyed, true, false, 0},
		{Unguarded, false, true, 0},
	} {
		j, s := &Memory{}, &service{lose: true}
		if _, err := Do(ctx, j, s.call(c.guard, "w", "x"), t0); !errors.Is(err, errAway) {
			t.Fatalf("%s: the lost answer: %v", c.guard, err)
		}
		s.lose = false
		_, err := Do(ctx, j, s.call(c.guard, "w", "x"), t0.Add(time.Minute))
		var held *HeldError
		if errors.As(err, &held) != c.held || (err != nil && !c.held) {
			t.Fatalf("%s: %v", c.guard, err)
		}
		if (s.sent == 2) != c.sent || s.probes != c.probes {
			t.Fatalf("%s: sent %d, probed %d", c.guard, s.sent, s.probes)
		}
		if c.held {
			// Held, it stays held: never sent on a guess.
			if _, err := Do(ctx, j, s.call(c.guard, "w", "x"), t0.Add(time.Hour)); !errors.As(err, &held) || s.sent != 1 {
				t.Fatalf("still held: %v, sent %d", err, s.sent)
			}
			if !held.Since.Equal(t0.Add(time.Minute)) {
				t.Fatalf("held since %v", held.Since)
			}
		}
	}
}

// A person confirms a held call: it happened (answered from the record
// after), or it did not (sent on the next call).
func TestConfirm(t *testing.T) {
	ctx := context.Background()
	for _, happened := range []bool{true, false} {
		j, s := &Memory{}, &service{lose: true}
		Do(ctx, j, s.call(Unguarded, "w", "x"), t0)
		s.lose = false
		Do(ctx, j, s.call(Unguarded, "w", "x"), t0)
		if err := Confirm(ctx, j, "w", happened, "", t0); err != nil {
			t.Fatal(err)
		}
		if _, err := Do(ctx, j, s.call(Unguarded, "w", "x"), t0); err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{true: 1, false: 2}[happened]; s.sent != want {
			t.Fatalf("happened %v: sent %d, want %d", happened, s.sent, want)
		}
	}
	if err := Confirm(ctx, &Memory{}, "w", true, "", t0); err == nil {
		t.Fatal("confirmed what was never held")
	}
}

// A refusal is definite: the call is fresh next time, and the last done
// write under the key is kept. An uncertain error leaves it interrupted.
func TestDoRefused(t *testing.T) {
	ctx := context.Background()
	j, s := &Memory{}, &service{}
	if _, err := Do(ctx, j, s.call(Unguarded, "status", "running"), t0); err != nil {
		t.Fatal(err)
	}
	s.fail = errRefused
	if _, err := Do(ctx, j, s.call(Unguarded, "status", "failed"), t0); !errors.Is(err, errRefused) {
		t.Fatal(err)
	}
	if e, _, _ := j.Load(ctx, "status"); e.State != Done || e.Intent != "running" {
		t.Fatalf("after a refusal, the last done write stands: %+v", e)
	}
	s.fail = nil
	if _, err := Do(ctx, j, s.call(Unguarded, "status", "failed"), t0); err != nil || s.sent != 2 {
		t.Fatalf("sent fresh after a refusal: %v, sent %d", err, s.sent)
	}
}
