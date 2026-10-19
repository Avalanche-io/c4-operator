package sched

import "testing"

func TestReconsider(t *testing.T) {
	var st State
	st.init()
	if pending, _, _ := st.Reconsider(Snapshot{}, Decisions{}, "a/0"); pending {
		t.Fatal("pending without a stop")
	}
	full := Pool{Name: "gpu", Slots: 4, Used: 4}
	waiting := Job{ID: "b", RunnableTotal: 3}
	cases := []struct {
		name string
		pp   pendingPreempt
		s    Snapshot
		d    Decisions
		stop bool
	}{
		{"hold still on", pendingPreempt{job: "a", reason: ReasonHoldStop}, Snapshot{Jobs: []Job{{ID: "a", HoldStop: true}}}, Decisions{}, true},
		{"hold released", pendingPreempt{job: "a", reason: ReasonHoldStop}, Snapshot{Jobs: []Job{{ID: "a"}}}, Decisions{}, false},
		{"wrangler", pendingPreempt{job: "a", reason: ReasonWrangler}, Snapshot{}, Decisions{}, true},
		{"pool still over", pendingPreempt{job: "a", pool: "gpu", slots: 1, reason: ReasonCapacity},
			Snapshot{Pools: []Pool{{Name: "gpu", Slots: 3, Used: 4}}}, Decisions{}, true},
		{"pool has room", pendingPreempt{job: "a", pool: "gpu", slots: 1, reason: ReasonCapacity},
			Snapshot{Pools: []Pool{{Name: "gpu", Slots: 4, Used: 4}}}, Decisions{}, false},
		{"for a job still waiting", pendingPreempt{job: "a", forJob: "b", pool: "gpu", slots: 1, reason: ReasonFeasibility},
			Snapshot{Pools: []Pool{full}, Jobs: []Job{{ID: "a"}, waiting}}, Decisions{Jobs: map[string]JobState{"b": {AtRisk: true}}}, true},
		{"for a job gone", pendingPreempt{job: "a", forJob: "b", pool: "gpu", slots: 1, reason: ReasonFeasibility},
			Snapshot{Pools: []Pool{full}, Jobs: []Job{{ID: "a"}}}, Decisions{}, false},
		{"for a job on time again", pendingPreempt{job: "a", forJob: "b", pool: "gpu", slots: 1, reason: ReasonFeasibility},
			Snapshot{Pools: []Pool{full}, Jobs: []Job{{ID: "a"}, waiting}}, Decisions{Jobs: map[string]JobState{"b": {}}}, false},
		{"capacity returned", pendingPreempt{job: "a", forJob: "b", pool: "gpu", slots: 1, reason: ReasonReclaim},
			Snapshot{Pools: []Pool{{Name: "gpu", Slots: 5, Used: 4}}, Jobs: []Job{{ID: "a"}, waiting}}, Decisions{}, false},
		{"capacity returned but dispatched", pendingPreempt{job: "a", forJob: "b", pool: "gpu", slots: 1, reason: ReasonReclaim},
			Snapshot{Pools: []Pool{{Name: "gpu", Slots: 5, Used: 4}}, Jobs: []Job{{ID: "a"}, waiting}},
			Decisions{Dispatch: []Dispatch{{Job: "c", Pool: "gpu", Slots: 1}}}, true},
	}
	for _, c := range cases {
		st.pending["a/0"] = c.pp
		pending, stop, why := st.Reconsider(c.s, c.d, "a/0")
		if !pending || stop != c.stop || why == "" {
			t.Errorf("%s: pending %v stop %v (%s)", c.name, pending, stop, why)
		}
	}
	if r, f, ok := st.Pending("a/0"); !ok || r != ReasonReclaim || f != "b" || !st.CancelPreempt("a/0") || st.CancelPreempt("a/0") {
		t.Fatal("cancel")
	}
}
