package localfarm

import (
	"strings"
	"time"
)

// Timings are how long each of up's steps took, so a slow first run says
// where its time went (load on the machine, or a step that grew).
type Timings struct {
	// Now is the clock (default time.Now).
	Now   func() time.Time
	start time.Time
	cur   string
	curAt time.Time
	order []string
	spent map[string]time.Duration
}

func (t *Timings) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Mark ends the step running and starts the one named; a name used again
// adds to its time.
func (t *Timings) Mark(name string) {
	if t == nil {
		return
	}
	now := t.now()
	if t.start.IsZero() {
		t.start = now
		t.spent = map[string]time.Duration{}
	}
	t.stop(now)
	t.cur, t.curAt = name, now
}

func (t *Timings) stop(now time.Time) {
	if t.cur == "" {
		return
	}
	if _, ok := t.spent[t.cur]; !ok {
		t.order = append(t.order, t.cur)
	}
	t.spent[t.cur] += now.Sub(t.curAt)
	t.cur = ""
}

// String is the whole and each step a second or more, in the order they
// first ran, to the second: "up in 4m38s: cluster 2m10s, operator 1m5s, tools 40s".
func (t *Timings) String() string {
	if t == nil || t.start.IsZero() {
		return ""
	}
	now := t.now()
	t.stop(now)
	var parts []string
	for _, name := range t.order {
		if d := t.spent[name].Truncate(time.Second); d >= time.Second {
			parts = append(parts, name+" "+d.String())
		}
	}
	s := "up in " + now.Sub(t.start).Truncate(time.Second).String()
	if len(parts) > 0 {
		s += ": " + strings.Join(parts, ", ")
	}
	return s
}
