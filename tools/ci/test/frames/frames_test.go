package frames

import "testing"

func TestCount(t *testing.T) {
	for in, want := range map[string]int{"1001-1048": 48, "1001": 1, "1-1": 1} {
		if got, err := Count(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	if _, err := Count("48-1"); err == nil {
		t.Error("a backwards range counted")
	}
}
