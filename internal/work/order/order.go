package order

import (
	"fmt"
	"math"
	"math/bits"
	"sort"
)

// Kind names a dispatch order over a job's unit axis.
type Kind string

const (
	Sequential Kind = "Sequential"
	Reverse    Kind = "Reverse"
	Binary     Kind = "Binary"
	FailRate   Kind = "FailRate"
)

// Units returns the dispatch order for n units as a permutation of 0..n-1.
// scores is used only by FailRate (one historical failure score per unit)
// and may be nil otherwise.
func Units(k Kind, n int, scores []float64) ([]int, error) {
	if n < 0 {
		return nil, fmt.Errorf("negative unit count %d", n)
	}
	switch k {
	case Sequential, "":
		return SequentialOrder(n), nil
	case Reverse:
		return ReverseOrder(n), nil
	case Binary:
		return BinaryOrder(n), nil
	case FailRate:
		if len(scores) != n {
			return nil, fmt.Errorf("FailRate order needs %d scores, got %d", n, len(scores))
		}
		return FailRateOrder(scores), nil
	}
	return nil, fmt.Errorf("unknown order %q", k)
}

// SequentialOrder is 0, 1, ..., n-1.
func SequentialOrder(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// ReverseOrder is n-1, ..., 1, 0.
func ReverseOrder(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = n - 1 - i
	}
	return out
}

// BinaryOrder is the coarse-to-fine order over n units.
//
// Level 0 is the first and the last unit. Level k (k >= 1) is the odd
// multiples of 1/2^k of the axis, (2j+1)/2^k for j < 2^(k-1), taken in
// bit-reversed order of j, which yields the van der Corput sequence
// 1/2, 1/4, 3/4, 1/8, 5/8, 3/8, 7/8, ... A fraction f maps to the unit
// nearest f*(n-1) (halves round up). A unit already taken by an earlier
// fraction is skipped, so the result is a permutation of 0..n-1.
//
// Spread guarantee (tested for every n up to 2000 in order_test.go): for
// every k with 2^k <= n-1, the first 2^k+1 units of the order are exactly
// round(m*(n-1)/2^k) for m = 0..2^k, all distinct, and the largest
// difference between consecutive sorted indexes among them is at most
// ceil((n-1)/2^k). Proof sketch: the step d = (n-1)/2^k is at least 1, so
// round(x+d) >= round(x)+1 (distinct, no skips through level k), and
// round(x+d)-round(x) < d+1 bounds every gap by ceil(d).
func BinaryOrder(n int) []int {
	out, _ := binaryOrder(n)
	return out
}

// binaryOrder also reports how many indexes the defensive sweep had to
// append; the tests assert it is always zero.
func binaryOrder(n int) ([]int, int) {
	if n <= 0 {
		return []int{}, 0
	}
	out := make([]int, 0, n)
	taken := make([]bool, n)
	add := func(i int) {
		if taken[i] {
			return
		}
		taken[i] = true
		out = append(out, i)
	}
	add(0)
	add(n - 1)
	span := float64(n - 1)
	for k := 1; len(out) < n; k++ {
		if k > 62 {
			break // unreachable for any int n; guards the shift below
		}
		half := uint64(1) << (k - 1) // number of odd numerators at this level
		den := float64(uint64(1) << k)
		for j := uint64(0); j < half; j++ {
			r := reverseBits(j, k-1)
			f := float64(2*r+1) / den
			add(int(math.Floor(f*span + 0.5)))
		}
		if den >= 2*span {
			break // the step is at most 1/2: every index has been taken
		}
	}
	// Defensive sweep: by the argument in the doc comment the loop above
	// has taken every index once the step falls below one, so this appends
	// nothing. Tests assert that it never does.
	before := len(out)
	for i := range n {
		add(i)
	}
	return out, len(out) - before
}

// reverseBits reverses the low w bits of v.
func reverseBits(v uint64, w int) uint64 {
	if w == 0 {
		return 0
	}
	return bits.Reverse64(v) >> (64 - w)
}

// CoarsePrefix returns how many leading units of BinaryOrder(n) must exist
// before every every-th unit of the axis does, that is, before the largest
// gap between consecutive taken indexes is at most every. It is 2^k+1 for
// the least k with ceil((n-1)/2^k) <= every, capped at n. The scheduler
// treats this prefix as information-bearing (section 9.9 of DESIGN.md).
func CoarsePrefix(n, every int) int {
	if n <= 2 || every < 1 {
		return max(n, 0)
	}
	for k := 0; k < 62; k++ {
		p := 1 << k
		if p > n-1 {
			return n
		}
		if (n-1+p-1)/p <= every {
			return min(p+1, n)
		}
	}
	return n
}

// FailRateOrder orders units by descending score (historical failure
// rate), breaking ties by index so the order is deterministic.
func FailRateOrder(scores []float64) []int {
	out := SequentialOrder(len(scores))
	sort.SliceStable(out, func(a, b int) bool {
		return scores[out[a]] > scores[out[b]]
	})
	return out
}

// MaxGap returns the largest difference between consecutive sorted values
// of idx, where idx must include 0 and n-1 for the result to describe
// coverage of the whole axis. It returns 0 for fewer than two values.
func MaxGap(idx []int) int {
	if len(idx) < 2 {
		return 0
	}
	s := append([]int(nil), idx...)
	sort.Ints(s)
	g := 0
	for i := 1; i < len(s); i++ {
		g = max(g, s[i]-s[i-1])
	}
	return g
}
