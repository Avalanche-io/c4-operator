package syncer

import (
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// verdicting is an adapter with its own default decisions.
type verdicting struct {
	tracker.Adapter
	words map[string]string
}

func (v verdicting) Verdict(native string) string { return v.words[native] }

// plain is an adapter without them.
type plain struct{ tracker.Adapter }

// The order is the Tracker's verdictsIn, then the adapter's own default,
// then, only for an adapter without one, DefaultVerdicts.
func TestVerdictOrder(t *testing.T) {
	tr := &c4v1.Tracker{Spec: c4v1.TrackerSpec{VerdictsIn: map[string]string{"status:cbb": "accept", "APPROVED": "accept"}}}
	kitsu := verdicting{words: map[string]string{"status:cbb": "approve", "status:done": "approve"}}
	for _, c := range []struct {
		name   string
		a      tracker.Adapter
		native string
		want   string
	}{
		{"verdictsIn wins over the adapter", kitsu, "status:cbb", "accept"},
		{"the adapter's default", kitsu, "status:done", "approve"},
		{"the adapter's no answer decides nothing", kitsu, "CHANGES_REQUESTED", ""},
		{"verdictsIn wins over DefaultVerdicts", plain{}, "APPROVED", "accept"},
		{"an adapter without one uses DefaultVerdicts", plain{}, "CHANGES_REQUESTED", "rework"},
	} {
		if got := verdictFor(tr, c.a, c.native); got != c.want {
			t.Errorf("%s: %s gave %q, want %q", c.name, c.native, got, c.want)
		}
	}
}
