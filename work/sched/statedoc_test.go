package sched

import (
	"reflect"
	"testing"
	"time"
)

// A State written down and read back is the same State: every field of
// the hysteresis a decision depends on survives, and the document is the
// same however the maps iterate.
func TestStateDocRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	st := &State{}
	st.init()
	st.pending["a/x#3"] = pendingPreempt{job: "a/x", forJob: "a/y", pool: "cpu", slots: 2, at: now, reason: ReasonReclaim}
	st.pending["a/x#1"] = pendingPreempt{job: "a/x", pool: "gpu", slots: 1, at: now.Add(time.Minute), reason: ReasonHoldStop}
	st.preemptLog["cpu"] = []time.Time{now, now.Add(time.Second)}
	st.surplus["burst"] = now.Add(-time.Hour)
	st.guard["a/y#0"] = now
	st.guardHit["a/y"] = now.Add(2 * time.Minute)
	st.manual.lastCheck = now
	st.manual.bumped["a/z"] = true
	st.took[[2]string{"a/x", "a/y"}] = now.Add(3 * time.Minute)

	d := st.Doc()
	back := StateFrom(d)
	if !reflect.DeepEqual(back, st) {
		t.Fatalf("round trip changed the state:\n%+v\n%+v", back, st)
	}
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(st.Doc(), d) {
			t.Fatal("the same state gave two documents")
		}
	}
	if got := (*State)(nil).Doc(); !reflect.DeepEqual(got, StateDoc{}) {
		t.Fatalf("nil state: %+v", got)
	}
}
