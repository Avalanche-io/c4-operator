// Package order holds the unit-axis primitives of the work system: frame-set
// parsing, chunking, run-length index sets (how WorkJob status stores
// per-unit state), and the dispatch orders a job can choose (Sequential,
// Reverse, Binary, FailRate).
//
// Everything here is pure and allocation-bounded; nothing imports
// Kubernetes.
package order

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MaxFrames bounds how many frames one frame set may expand to, so a typo
// such as "1-1000000000" fails fast instead of allocating gigabytes.
const MaxFrames = 1 << 20

// ParseFrames expands a frame set into ascending frame numbers.
//
// The grammar is a comma-separated list of items, each one of:
//
//	N        a single frame
//	A-B      every frame from A to B inclusive
//	A-BxS    every S-th frame from A, not past B
//
// Frames are non-negative integers. Whitespace around items is ignored.
// The result is sorted ascending; a frame named twice is an error, because
// it would make the unit axis ambiguous.
func ParseFrames(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty frame set")
	}
	var out []int
	seen := map[int]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("frame set %q: empty item", s)
		}
		frames, err := parseItem(item)
		if err != nil {
			return nil, fmt.Errorf("frame set %q: %w", s, err)
		}
		for _, f := range frames {
			if seen[f] {
				return nil, fmt.Errorf("frame set %q: frame %d listed twice", s, f)
			}
			seen[f] = true
			out = append(out, f)
			if len(out) > MaxFrames {
				return nil, fmt.Errorf("frame set %q: more than %d frames", s, MaxFrames)
			}
		}
	}
	sort.Ints(out)
	return out, nil
}

func parseItem(item string) ([]int, error) {
	rng, stepStr, hasStep := strings.Cut(item, "x")
	step := 1
	if hasStep {
		v, err := parseNonNeg(stepStr)
		if err != nil {
			return nil, fmt.Errorf("item %q: step: %w", item, err)
		}
		if v == 0 {
			return nil, fmt.Errorf("item %q: step must be at least 1", item)
		}
		step = v
	}
	loStr, hiStr, isRange := strings.Cut(rng, "-")
	lo, err := parseNonNeg(loStr)
	if err != nil {
		return nil, fmt.Errorf("item %q: %w", item, err)
	}
	if !isRange {
		if hasStep {
			return nil, fmt.Errorf("item %q: a step needs a range", item)
		}
		return []int{lo}, nil
	}
	hi, err := parseNonNeg(hiStr)
	if err != nil {
		return nil, fmt.Errorf("item %q: %w", item, err)
	}
	if hi < lo {
		return nil, fmt.Errorf("item %q: end %d before start %d", item, hi, lo)
	}
	if (hi-lo)/step+1 > MaxFrames {
		return nil, fmt.Errorf("item %q: more than %d frames", item, MaxFrames)
	}
	out := make([]int, 0, (hi-lo)/step+1)
	for f := lo; f <= hi; f += step {
		out = append(out, f)
	}
	return out, nil
}

func parseNonNeg(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("missing number")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a non-negative integer", s)
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	return v, nil
}

// Chunk groups frames into units of at most size frames each, keeping order.
// A size below 1 is treated as 1. The unit axis of a job is the result's
// index: unit i holds Chunk(...)[i].
func Chunk(frames []int, size int) [][]int {
	if size < 1 {
		size = 1
	}
	out := make([][]int, 0, (len(frames)+size-1)/size)
	for i := 0; i < len(frames); i += size {
		j := min(i+size, len(frames))
		out = append(out, frames[i:j:j])
	}
	return out
}
