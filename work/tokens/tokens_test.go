package tokens

import (
	"math"
	"testing"
)

func TestWeighted(t *testing.T) {
	// The session agent-jobs measured: cache reads dwarf the rest.
	c := Counts{Input: 2112, Output: 1060000, CacheRead: 514000000, CacheWrite: 9233}
	w := Weighted("claude-sonnet-5-5", c)
	want := 2112 + 5*1060000.0 + 1.25*9233 + 0.1*514000000
	if math.Abs(w-want) > 1e-6 || c.Raw() != 515071345 {
		t.Fatalf("weighted %v (want %v), raw %d", w, want, c.Raw())
	}
	if CacheReadWeight("claude-opus-5-5") != 0.05 || CacheReadWeight("claude-fable-5-1") != 0.025 || CacheReadWeight("someone-else") != 0.1 {
		t.Fatal("cache read weights")
	}
	// Opus 5.5 at $4 a million input: a million output tokens cost $20.
	if cost, ok := Estimate("claude-opus-5-5", Counts{Output: 1e6}); !ok || math.Abs(cost-20) > 1e-9 {
		t.Fatalf("estimate %v %v", cost, ok)
	}
	for _, m := range []string{"anthropic.claude-sonnet-5-5", "claude-opus-4-5@20251101"} {
		if _, ok := InputPrice(m); ok != (m == "anthropic.claude-sonnet-5-5") {
			t.Errorf("%s: known %v", m, ok)
		}
	}
	if _, ok := Estimate("gpt-x", Counts{Input: 5}); ok {
		t.Fatal("priced an unknown model")
	}
	// The longest known prefix wins: Opus 5.5, not Opus 5.
	if p, _ := InputPrice("claude-opus-5-5"); math.Abs(p*1e6-4) > 1e-9 {
		t.Fatalf("opus 5.5 at %v", p*1e6)
	}
}
