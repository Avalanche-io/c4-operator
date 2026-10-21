package main

import (
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Every decision of a simulated week, redacted as a diagnostic bundle
// redacts it, replays to the same decisions: names become pseudonyms whose
// synthetic replay names sort as the originals did, times are shifted to
// another epoch, and money is scaled by a secret factor, and none of it
// changes what the policy decides. Run on the main farm and on an
// overloaded GPU farm that bursts and preempts.
func TestRedactedSnapshotsReplay(t *testing.T) {
	key, err := telemetry.NewKey(make32(7), 1)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		policy  string
		farm    farm
		dailies map[string]float64
	}{
		{"main", "objective", mainFarm, nil},
		{"gpu-tight", "objective+burst", gpuTightFarm(), map[string]float64{"showA": 14, "showB": 10, "showC": 8}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := lightParams()
			if c.dailies != nil {
				p.DailiesPerEvening = c.dailies
			}
			w := generate(p, 2)
			h := buildHistory(p, w.targets)
			var pol policySpec
			for _, x := range policies {
				if x.name == c.policy {
					pol = x
				}
			}
			var decisions, unfaithful, preempts, bursts int
			byKind := map[string]int{}
			observeDecide = func(_ policySpec, s sched.Snapshot, before sched.StateDoc, cfg sched.Config, d sched.Decisions) {
				decisions++
				preempts += len(d.Preempt)
				bursts += len(d.Bursts)
				r := telemetry.NewRedactor(key)
				rec, _ := r.Redact(s, sched.StateFrom(before), cfg, d)
				telemetry.CheckFaithful(&rec, pol.fn)
				if !rec.Faithful {
					unfaithful++
					for _, dc := range rec.Divergences {
						byKind[telemetry.DivergenceName(dc.Kind)] += int(dc.Count)
					}
				}
			}
			defer func() { observeDecide = nil }()
			newEngine(w, h, 2, pol, c.farm).run()
			if decisions == 0 {
				t.Fatal("no decisions observed")
			}
			t.Logf("%d decisions (%d preemptions, %d bursts), %d unfaithful %v", decisions, preempts, bursts, unfaithful, byKind)
			if unfaithful > 0 {
				t.Fatalf("%d of %d redacted snapshots replayed differently: %v", unfaithful, decisions, byKind)
			}
		})
	}
}

func make32(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b + byte(i)
	}
	return k
}
