package tracker

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// A write the tracker answered and declined did not happen; one that timed
// out or met a 5xx may have.
func TestDefinite(t *testing.T) {
	for err, want := range map[error]bool{
		fmt.Errorf("github: 422: %w", ErrRefused):        true,
		fmt.Errorf("flow: %w: Task/4", ErrNotFound):      true,
		fmt.Errorf("mlflow: kind x: %w", ErrUnsupported): true,
		&RateLimited{Until: time.Now()}:                  true,
		errors.New("github: 502 Bad Gateway"):            false,
		errors.New("read tcp: connection reset by peer"): false,
	} {
		if got := Definite(err); got != want {
			t.Errorf("%v: %v, want %v", err, got, want)
		}
	}
}

func TestWhen(t *testing.T) {
	at := time.Date(2026, 10, 22, 0, 45, 0, 0, time.UTC)
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		loc        *time.Location
		long, shrt string
	}{
		{nil, "Thu 22 Oct 00:45 UTC", "Thu 00:45 UTC"},
		{time.UTC, "Thu 22 Oct 00:45 UTC", "Thu 00:45 UTC"},
		{la, "Wed 21 Oct 17:45 PDT (00:45 UTC)", "Wed 17:45 PDT (00:45 UTC)"},
	} {
		if got := When(at, c.loc); got != c.long {
			t.Errorf("When %v: %q", c.loc, got)
		}
		if got := WhenShort(at, c.loc); got != c.shrt {
			t.Errorf("WhenShort %v: %q", c.loc, got)
		}
	}
}
