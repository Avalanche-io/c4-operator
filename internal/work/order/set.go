package order

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// EncodeSet writes a set of unit indexes as a run-length string such as
// "0-99,102,104-180". Input order and duplicates do not matter; the output
// is canonical (ascending, maximal runs, single indexes without a dash), so
// two equal sets always encode to the same string. Negative indexes are
// not unit indexes and cause a panic.
func EncodeSet(idx []int) string {
	if len(idx) == 0 {
		return ""
	}
	s := append([]int(nil), idx...)
	sort.Ints(s)
	if s[0] < 0 {
		panic(fmt.Sprintf("order.EncodeSet: negative index %d", s[0]))
	}
	var b strings.Builder
	lo, hi := s[0], s[0]
	flush := func() {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(lo))
		if hi > lo {
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(hi))
		}
	}
	for _, v := range s[1:] {
		if v <= hi+1 {
			hi = max(hi, v)
			continue
		}
		flush()
		lo, hi = v, v
	}
	flush()
	return b.String()
}

// DecodeSet parses a run-length index set. It accepts exactly what a
// careful writer could produce: ascending, non-overlapping items. Adjacent
// runs ("0-4,5-9") are accepted and merged; overlapping or descending items
// are rejected, because they indicate corrupt status rather than a style
// choice.
func DecodeSet(s string) ([]int, error) {
	runs, err := parseRuns(s)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, r := range runs {
		n += r[1] - r[0] + 1
	}
	if n > MaxFrames*16 {
		return nil, fmt.Errorf("index set %q: %d indexes is too many", s, n)
	}
	out := make([]int, 0, n)
	for _, r := range runs {
		for v := r[0]; v <= r[1]; v++ {
			out = append(out, v)
		}
	}
	return out, nil
}

// CountSet returns the number of indexes in a run-length set without
// expanding it.
func CountSet(s string) (int, error) {
	runs, err := parseRuns(s)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range runs {
		n += r[1] - r[0] + 1
	}
	return n, nil
}

func parseRuns(s string) ([][2]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var runs [][2]int
	prev := -1
	for _, item := range strings.Split(s, ",") {
		loStr, hiStr, isRange := strings.Cut(item, "-")
		lo, err := parseNonNeg(loStr)
		if err != nil {
			return nil, fmt.Errorf("index set %q: %w", s, err)
		}
		hi := lo
		if isRange {
			hi, err = parseNonNeg(hiStr)
			if err != nil {
				return nil, fmt.Errorf("index set %q: %w", s, err)
			}
			if hi < lo {
				return nil, fmt.Errorf("index set %q: run %q is descending", s, item)
			}
		}
		if lo <= prev {
			return nil, fmt.Errorf("index set %q: %q overlaps or precedes the previous run", s, item)
		}
		prev = hi
		runs = append(runs, [2]int{lo, hi})
	}
	return runs, nil
}
