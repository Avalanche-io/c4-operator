package rendezvous

import (
	"fmt"
	"math"
	"testing"
)

func TestBalancedAndStable(t *testing.T) {
	members := []string{"a", "b", "c", "d", "e"}
	counts := map[string]int{}
	before := map[int]string{}
	const n = 50000
	for i := range n {
		k := []byte(fmt.Sprint(i))
		w := Rank(members, k)[0]
		counts[w]++
		before[i] = w
	}
	for m, c := range counts {
		if math.Abs(float64(c)-n/5) > n/5*0.05 {
			t.Errorf("%s won %d of %d", m, c, n)
		}
	}
	// Removing one member moves only its keys.
	moved := 0
	for i := range n {
		w := Rank([]string{"a", "b", "c", "d"}, []byte(fmt.Sprint(i)))[0]
		if w != before[i] {
			moved++
			if before[i] != "e" {
				t.Fatalf("key %d moved from %s although e left", i, before[i])
			}
		}
	}
	if moved != counts["e"] {
		t.Fatalf("moved %d, e had %d", moved, counts["e"])
	}
}

func TestSpread(t *testing.T) {
	members := []string{"r0", "r1", "r2", "r3"}
	key := []byte("hot")
	top2 := Top(members, key, 2)
	seen := map[string]int{}
	for i := range 1000 {
		first := Spread(members, key, 2, fmt.Sprint("node-", i))[0]
		if first != top2[0] && first != top2[1] {
			t.Fatalf("spread chose %s outside top 2 %v", first, top2)
		}
		seen[first]++
	}
	if len(seen) != 2 || seen[top2[0]] < 400 || seen[top2[1]] < 400 {
		t.Fatalf("uneven spread %v", seen)
	}
	if len(Spread(members, key, 2, "x")) != 4 {
		t.Fatal("fallbacks dropped")
	}
}
