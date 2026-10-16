// Package rendezvous ranks members for a key by highest random weight
// (HRW hashing). Every caller that knows the same members computes the
// same ranking with no coordination, and adding or removing one member
// moves only the keys that member wins: about 1/n of them.
package rendezvous

import (
	"hash/fnv"
	"sort"
)

// Score is a member's weight for key.
func Score(member string, key []byte) uint64 {
	h := fnv.New64a()
	h.Write([]byte(member))
	h.Write([]byte{0})
	h.Write(key)
	// FNV's low bits mix poorly for short inputs; finish with a
	// SplitMix64 round so scores are uniform.
	x := h.Sum64()
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// Rank returns members ordered from highest to lowest score for key.
// Ties (vanishingly rare) break by name so the order is total.
func Rank(members []string, key []byte) []string {
	type scored struct {
		m string
		s uint64
	}
	ss := make([]scored, len(members))
	for i, m := range members {
		ss[i] = scored{m, Score(m, key)}
	}
	sort.Slice(ss, func(i, j int) bool {
		if ss[i].s != ss[j].s {
			return ss[i].s > ss[j].s
		}
		return ss[i].m < ss[j].m
	})
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.m
	}
	return out
}

// Top returns the k highest-ranked members for key.
func Top(members []string, key []byte, k int) []string {
	r := Rank(members, key)
	if k < len(r) {
		r = r[:k]
	}
	return r
}

// Spread picks one of the top k members for key, chosen by caller, so
// that different callers asking for the same hot key land on different
// members of its top k, and the same caller always lands on the same one.
// The rest of the ranking follows as fallbacks.
func Spread(members []string, key []byte, k int, caller string) []string {
	r := Rank(members, key)
	if k <= 1 || len(r) <= 1 {
		return r
	}
	k = min(k, len(r))
	i := int(Score(caller, key) % uint64(k))
	out := append([]string{r[i]}, r[:i]...)
	return append(out, r[i+1:]...)
}
