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
