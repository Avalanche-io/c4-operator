package forecast

import (
	"math"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/work/estimate"
)

var la = mustLoc("America/Los_Angeles")

func mustLoc(n string) *time.Location {
	l, err := time.LoadLocation(n)
	if err != nil {
		panic(err)
	}
	return l
}

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, la)
	if err != nil {
		panic(err)
	}
	return t
}

func TestSessions(t *testing.T) {
	p := Policy{Kind: Sessions, Sessions: []string{"14:00", "08:00"}, Location: la, Quantile: 0.8,
		Weekdays: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ finish, want string }{
		{"2026-10-21 03:10", "2026-10-21 08:00"}, // Wednesday night: the morning session
		{"2026-10-21 08:00", "2026-10-21 08:00"}, // exactly on a session
		{"2026-10-21 09:30", "2026-10-21 14:00"}, // missed the morning: the afternoon
		{"2026-10-23 15:00", "2026-10-26 08:00"}, // Friday after the last session: Monday
		{"2026-10-31 23:00", "2026-11-02 08:00"}, // across the DST change, local 08:00
	}
	for _, c := range cases {
		now := at(c.finish).Add(-6 * time.Hour)
		got := p.Earliest(at(c.finish), now, now, now)
		if !got.Equal(at(c.want)) {
			t.Errorf("finish %s: got %s, want %s", c.finish, got.In(la).Format("Mon 2006-01-02 15:04"), c.want)
		}
	}
}

func TestAchievableRounding(t *testing.T) {
	p := DefaultPolicy()
	p.Location = la
	now := at("2026-10-21 10:00")
	if got := p.Earliest(at("2026-10-21 13:07"), now, now, now); !got.Equal(at("2026-10-21 13:15")) {
		t.Errorf("13:07 rounds to %s", got.In(la).Format("15:04"))
	}
	if got := p.Earliest(at("2026-10-21 13:15"), now, now, now); !got.Equal(at("2026-10-21 13:15")) {
		t.Errorf("13:15 stays, got %s", got.In(la).Format("15:04"))
	}
	// Minimum lead: a finish in five minutes is still 15 minutes away.
	if got := p.Earliest(now.Add(5*time.Minute), now, now, now); !got.Equal(at("2026-10-21 10:15")) {
		t.Errorf("min lead: got %s", got.In(la).Format("15:04"))
	}
	ist := mustLoc("Asia/Kolkata") // +05:30
	p.Location = ist
	f := time.Date(2026, 10, 21, 13, 7, 0, 0, ist)
	if got := p.Earliest(f, f.Add(-time.Hour), f, f); !got.Equal(time.Date(2026, 10, 21, 13, 15, 0, 0, ist)) {
		t.Errorf("half-hour offset: got %s", got.In(ist).Format("15:04"))
	}
}

func TestFixedPolicies(t *testing.T) {
	submit, ready := at("2026-10-21 09:00"), at("2026-10-21 09:20")
	tp := Policy{Kind: Turnaround, Turnaround: 24 * time.Hour, Quantile: 0.8}
	if got := tp.Earliest(at("2026-10-23 00:00"), submit, submit, ready); !got.Equal(submit.Add(24 * time.Hour)) {
		t.Errorf("turnaround: %v", got)
	}
	lp := Policy{Kind: Latency, Turnaround: 15 * time.Minute, Quantile: 0.8}
	if !lp.Hard() || !lp.Earliest(submit, submit, submit, ready).Equal(ready.Add(15*time.Minute)) {
		t.Errorf("latency")
	}
	if !(Policy{Kind: Tracking}).Earliest(submit, submit, submit, ready).IsZero() {
		t.Errorf("tracking has no policy deadline")
	}
	if (Policy{Kind: "weekly", Quantile: 0.8}).Validate() == nil {
		t.Errorf("unknown kind validates")
	}
}

func TestDisplay(t *testing.T) {
	now := at("2026-10-21 10:00")
	s := estimate.Summary{W: 36000, W90: 72000, TPath: 3600, Calibrated: true,
		W50: 40000, W80: 60000, W95: 90000}
	eta50, eta90 := at("2026-10-21 12:00"), at("2026-10-21 14:00") // 36000 slot-s per 2h
	b := Display(now, s, eta50, eta90)
	// P50: W50 is 4000 slot-s past W, 1/9 of the way to W90: 12:13:20,
	// less half the 1h tail.
	want := at("2026-10-21 11:43").Add(20 * time.Second)
	if !b.P50.Equal(want) {
		t.Errorf("P50 %s, want %s", b.P50.In(la).Format("15:04:05"), want.In(la).Format("15:04:05"))
	}
	if !b.P90.Equal(at("2026-10-21 13:30")) {
		t.Errorf("P90 %s", b.P90.In(la).Format("15:04:05"))
	}
	if !(b.P50.Before(b.P80) && b.P80.Before(b.P90) && b.P90.Before(b.P95)) {
		t.Errorf("band not ordered: %+v", b)
	}
	if !Display(now, s, time.Time{}, time.Time{}).IsZero() {
		t.Errorf("no ETA, want no band")
	}
	// Never earlier than now plus the critical path.
	b = Display(now, estimate.Summary{W: 10, W90: 20, W50: 10, W80: 15, W95: 25, TPath: 7200}, now.Add(time.Minute), now.Add(2*time.Minute))
	if b.P50.Before(now.Add(2 * time.Hour)) {
		t.Errorf("P50 %s before now + the critical path", b.P50.In(la).Format("15:04"))
	}
}

func TestInfoPoints(t *testing.T) {
	var got []int
	for n := 0; n <= 40; n++ {
		if InfoPoint(n) {
			got = append(got, n)
		}
	}
	want := []int{1, 3, 5, 9, 17, 33}
	if len(got) != len(want) {
		t.Fatalf("info points %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("info points %v, want %v", got, want)
		}
	}
}

func band(start time.Time, p50h, p80h float64) Band {
	h := func(x float64) time.Time { return start.Add(time.Duration(x * float64(time.Hour))) }
	return Band{P50: h(p50h), P80: h(p80h), P90: h(p80h * 1.1), P95: h(p80h * 1.2)}
}

func TestSuggestionRevisesOnlyOnInformation(t *testing.T) {
	p := DefaultPolicy()
	start := at("2026-10-21 10:00")
	var s Suggestion
	if !s.Observe(p, Event{Now: start, Total: 200, Candidate: at("2026-10-21 16:00"), Band: band(start, 4, 6)}) {
		t.Fatal("first suggestion not set")
	}
	if s.State != Estimating || s.Width != 0.5 {
		t.Fatalf("state %s width %v", s.State, s.Width)
	}
	t1 := start.Add(20 * time.Minute)
	// Not an information event: no move.
	if s.Observe(p, Event{Now: t1, Finished: 2, Total: 200, Candidate: at("2026-10-21 19:00"), Band: band(start, 6, 7)}) {
		t.Error("moved between information events")
	}
	// An information event with a big move but no narrowing: no move.
	if s.Observe(p, Event{Now: t1, Finished: 3, Total: 200, Info: true, Candidate: at("2026-10-21 19:00"), Band: band(start, 5, 8)}) {
		t.Error("moved without the band narrowing")
	}
	// Narrowed by more than 20%, moved by more than the threshold: moves.
	if !s.Observe(p, Event{Now: t1, Finished: 5, Total: 200, Info: true, Candidate: at("2026-10-21 19:00"), Band: band(start, 6, 7)}) {
		t.Fatal("did not revise")
	}
	if s.State != Settling || s.Revisions != 1 {
		t.Fatalf("after one revision: %s, %d", s.State, s.Revisions)
	}
	// A small move is ignored even with narrowing.
	if s.Observe(p, Event{Now: t1, Finished: 9, Total: 200, Info: true, Candidate: at("2026-10-21 19:10"), Band: band(start, 6, 6.2)}) {
		t.Error("moved by less than the threshold")
	}
	// Second revision, then settled: nothing moves it again.
	if !s.Observe(p, Event{Now: t1, Finished: 17, Total: 200, Info: true, Candidate: at("2026-10-21 17:00"), Band: band(start, 6, 6.2)}) {
		t.Fatal("second revision")
	}
	if s.State != Settled {
		t.Fatalf("after %d revisions state %s", s.Revisions, s.State)
	}
	if s.Observe(p, Event{Now: t1, Finished: 33, Total: 200, Info: true, Candidate: at("2026-10-21 12:00"), Band: band(start, 1, 1.01)}) {
		t.Error("a settled suggestion moved")
	}
	if len(s.History) != 3 {
		t.Errorf("history %d entries, want 3", len(s.History))
	}
}

func TestSuggestionSettlesOnTimeAndUnits(t *testing.T) {
	p := DefaultPolicy()
	start := at("2026-10-21 10:00")
	var s Suggestion
	s.Observe(p, Event{Now: start, Total: 200, Candidate: at("2026-10-21 18:00"), Band: band(start, 6, 8)})
	// A quarter of the way to the deadline: settled.
	s.Observe(p, Event{Now: at("2026-10-21 12:00"), Finished: 2, Total: 200, Band: band(start, 6, 8), Candidate: at("2026-10-21 18:00")})
	if s.State != Settled {
		t.Errorf("not settled at a quarter of the time: %s", s.State)
	}
	var u Suggestion
	u.Observe(p, Event{Now: start, Total: 200, Candidate: at("2026-10-22 18:00"), Band: band(start, 30, 32)})
	u.Observe(p, Event{Now: start.Add(time.Hour), Finished: SettleUnits, Total: 200, Band: band(start, 30, 32), Candidate: at("2026-10-22 18:00")})
	if u.State != Settled {
		t.Errorf("not settled after %d units: %s", SettleUnits, u.State)
	}
	z := DefaultPolicy()
	z.MaxRevisions = 0
	var v Suggestion
	v.Observe(z, Event{Now: start, Total: 200, Candidate: at("2026-10-21 18:00"), Band: band(start, 6, 8)})
	if v.State != Settled {
		t.Errorf("no revisions allowed, want settled at once: %s", v.State)
	}
}

func TestAchievable(t *testing.T) {
	lo, hi := at("2026-10-21 10:00"), at("2026-10-22 10:00")
	truth := at("2026-10-21 14:23")
	calls := 0
	got := FirstAchievable(lo, hi, time.Minute, func(d time.Time) bool { calls++; return !d.Before(truth) })
	if got.Before(truth) || got.Sub(truth) > time.Minute {
		t.Errorf("achievable %s, want %s within a minute", got.In(la).Format("15:04:05"), truth.In(la).Format("15:04"))
	}
	if calls > 14 {
		t.Errorf("%d policy runs", calls)
	}
	if !FirstAchievable(lo, hi, time.Minute, func(time.Time) bool { return false }).Equal(hi) {
		t.Error("never achievable: want hi")
	}
	if !FirstAchievable(lo, hi, time.Minute, func(time.Time) bool { return true }).Equal(lo) {
		t.Error("always achievable: want lo")
	}
}

func TestFinishCalibration(t *testing.T) {
	c := NewFinishCalibration(0)
	c.Min = 10
	k := FinishKey{Class: "render", Stage: 1}
	now := at("2026-10-21 10:00")
	b := Band{P50: now.Add(time.Hour), P80: now.Add(90 * time.Minute), P90: now.Add(2 * time.Hour), P95: now.Add(3 * time.Hour)}
	if got := c.Apply(k, now, b); got != b {
		t.Fatal("an empty calibration changed the band")
	}
	// Jobs took 1x to 2x what their median said, evenly.
	for i := range 101 {
		f := 1 + float64(i)/100
		c.Add(k, FinishScore(now, now.Add(time.Hour), now.Add(time.Duration(f*float64(time.Hour)))))
	}
	got := c.Apply(k, now, b)
	near := func(x time.Time, h float64) bool { return math.Abs(x.Sub(now).Hours()-h) < 0.03 }
	if !near(got.P50, 1.5) || !near(got.P80, 1.8) || !near(got.P95, 1.95) {
		t.Fatalf("calibrated %v %v %v", got.P50.Sub(now), got.P80.Sub(now), got.P95.Sub(now))
	}
	if c.Apply(FinishKey{Class: "ci"}, now, b) != b {
		t.Fatal("a key with no scores borrowed another's")
	}
	if got := c.Apply(FinishKey{Class: "render", Stage: 3}, now, b); !near(got.P50, 1.5) {
		t.Fatal("a stage with no scores did not fall back to its class")
	}
	if !c.Covers(k) || !c.Covers(FinishKey{Class: "render", Stage: 3}) || c.Covers(FinishKey{Class: "ci"}) || (*FinishCalibration)(nil).Covers(k) {
		t.Fatal("Covers")
	}
	if StageOf(0, 10) != 0 || StageOf(2, 10) != 1 || StageOf(5, 10) != 2 || StageOf(8, 10) != 3 {
		t.Fatal("stages")
	}
}
