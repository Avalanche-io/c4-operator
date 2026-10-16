package tier

import (
	"context"
	"crypto/sha512"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/fill"
	"github.com/Avalanche-io/c4-operator/internal/metrics"
)

// ErrNoRoute means there is nowhere to fetch from.
var ErrNoRoute = errors.New("no upstream for this object")

// Route returns the sources for an object in preference order. It is
// called again on every retry round, so membership changes and breaker
// state are picked up.
type Route func() []Source

// Fetcher fills objects from upstream sources: it tries candidates in
// order, resumes a partial transfer at the next candidate from the bytes
// already written, skips sources whose breaker is open, backs off with
// jitter when every candidate refuses, and never invents a fallback
// outside the route it was given (a node never falls through to the
// store because the regional tier is busy).
type Fetcher struct {
	Tier     string // metrics label of this process: node or regional
	Breakers *Breakers
	Backoff  Backoff
	// Stall aborts an upstream read that delivers no byte for this long,
	// so a hung connection is resumed elsewhere. Zero means 60 s.
	Stall time.Duration
	// Verify hashes the object as it is written and fails the fill if the
	// bytes do not hash to the ID (a project's verifyFill).
	Verify bool
	// MaxRounds bounds retry rounds (0 means until ctx ends).
	MaxRounds int
}

// Fill writes id into f, then returns. The caller commits or fails the
// flight.
func (fe *Fetcher) Fill(ctx context.Context, f *fill.Flight, project string, id c4.ID, route Route) error {
	var h hash.Hash
	if fe.Verify {
		h = sha512.New()
	}
	stall := fe.Stall
	if stall <= 0 {
		stall = 60 * time.Second
	}
	quiet := 0 // rounds without progress, for backoff
	for round := 0; fe.MaxRounds == 0 || round < fe.MaxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		sources := route()
		if len(sources) == 0 {
			return ErrNoRoute
		}
		progressed := false
		missing := false
		for _, s := range sources {
			if !fe.Breakers.Allow(s.Name()) {
				continue
			}
			done, moved, err := fe.try(ctx, f, project, id, s, h, stall)
			if moved {
				progressed = true
			}
			if done {
				fe.Breakers.Ok(s.Name())
				return nil
			}
			switch {
			case errors.Is(err, ErrNotFound):
				if s.Authoritative() {
					missing = true
				}
			case errors.Is(err, ErrBusy):
			case errors.Is(err, errCorrupt):
				return err
			case Unreachable(err):
				fe.Breakers.Fail(s.Name())
			}
			if missing {
				break
			}
		}
		if missing {
			return fmt.Errorf("%s: %w", id, ErrNotFound)
		}
		if progressed {
			quiet = 0
		} else {
			quiet++
		}
		select {
		case <-time.After(fe.Backoff.Delay(quiet)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("%s: gave up after %d rounds", id, fe.MaxRounds)
}

var errCorrupt = errors.New("upstream bytes do not hash to the requested ID")

// try reads from one source. done means the object is complete.
func (fe *Fetcher) try(ctx context.Context, f *fill.Flight, project string, id c4.ID, s Source, h hash.Hash, stall time.Duration) (done, moved bool, err error) {
	off := f.Written()
	body, total, start, err := s.Get(ctx, project, id, off)
	if err != nil {
		metrics.TierUpstream.WithLabelValues(fe.Tier, s.Kind(), resultOf(err)).Inc()
		return false, false, err
	}
	if start != off {
		// The source restarted the object; so do we.
		if err := f.Truncate(); err != nil {
			body.Close()
			return false, false, err
		}
		if h != nil {
			h.Reset()
		}
		off = 0
	}
	if off > 0 {
		metrics.TierResumes.WithLabelValues(fe.Tier, s.Kind()).Inc()
	}
	if total >= 0 {
		f.SetSize(total)
	}
	var dst io.Writer = f
	if h != nil {
		dst = io.MultiWriter(f, h)
	}
	n, cerr := io.Copy(dst, newStallReader(body, stall))
	body.Close()
	metrics.TierUpstreamBytes.WithLabelValues(fe.Tier, s.Kind()).Add(float64(n))
	complete := cerr == nil && (total < 0 || f.Written() == total)
	if !complete {
		if cerr == nil {
			cerr = fmt.Errorf("short read from %s: %d of %d bytes", s.Name(), f.Written(), total)
		}
		metrics.TierUpstream.WithLabelValues(fe.Tier, s.Kind(), "short").Inc()
		return false, n > 0, cerr
	}
	metrics.TierUpstream.WithLabelValues(fe.Tier, s.Kind(), "ok").Inc()
	if h == nil {
		return true, n > 0, nil
	}
	metrics.NodeHashedBytes.WithLabelValues("verify-fill").Add(float64(f.Written()))
	var got c4.ID
	copy(got[:], h.Sum(nil))
	if got != id {
		return false, n > 0, fmt.Errorf("%w: %s gave bytes of %s", errCorrupt, s.Name(), got)
	}
	return true, n > 0, nil
}

func resultOf(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "notfound"
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, ErrDenied):
		return "denied"
	}
	return "error"
}

// stallReader closes the body when no byte arrives for d, which unblocks
// the read with an error so the fill resumes elsewhere.
type stallReader struct {
	r io.ReadCloser
	d time.Duration
	t *time.Timer
}

func newStallReader(r io.ReadCloser, d time.Duration) *stallReader {
	s := &stallReader{r: r, d: d}
	s.t = time.AfterFunc(d, func() { r.Close() })
	return s
}

func (s *stallReader) Read(p []byte) (int, error) {
	s.t.Reset(s.d)
	n, err := s.r.Read(p)
	if err != nil {
		s.t.Stop()
	}
	return n, err
}
