// Package forecast turns the estimator's remaining work and the
// scheduler's plan into what a person is shown and offered: a calibrated
// finish band ("ready by ~14:20, 80%"), a suggested deadline from the
// class's deadline policy, and the rules by which that suggestion is
// revised over a job's first binary-order units and then settles.
//
// The package is pure. It reads estimate.Summary values and times the
// scheduler computed; it does not plan. Whatever needs a plan (the
// earliest deadline a job can meet without making another job late) is
// passed in as a function, so the caller decides how to run the policy.
package forecast

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind names how a policy chooses the times a deadline may take.
type Kind string

const (
	// Sessions: a calendar of times of day (dailies at 08:00 and 14:00)
	// on chosen weekdays.
	Sessions Kind = "sessions"
	// Achievable: any time, rounded up to Round, at least MinLead ahead.
	Achievable Kind = "achievable"
	// Turnaround: submission plus Turnaround.
	Turnaround Kind = "turnaround"
	// Latency: ready time plus Turnaround (a latency SLO's p95 target).
	Latency Kind = "latency"
	// Tracking: the due time production tracking sets.
	Tracking Kind = "tracking"
)

// Policy is a class's (or a project's) deadline policy.
type Policy struct {
	Kind Kind
	// Sessions are times of day "HH:MM" in Location, on Weekdays (all
	// days when empty).
	Sessions []string
	Weekdays []time.Weekday
	Location *time.Location
	// Turnaround is the duration for Turnaround and Latency policies.
	Turnaround time.Duration
	// Quantile is the finish percentile a suggestion must meet (0.8 for
	// dailies and soft classes, 0.9 for deliveries).
	Quantile float64
	// Round and MinLead shape Achievable deadlines.
	Round, MinLead time.Duration
	// Revision rules (see Suggestion).
	MaxRevisions int
	MinMoveFrac  float64
	MinMove      time.Duration
	MinNarrowing float64
}

// DefaultPolicy returns the soft "as soon as reasonable" policy:
// achievable at 80%, rounded to 15 minutes, at most two revisions.
func DefaultPolicy() Policy {
	return Policy{
		Kind: Achievable, Quantile: 0.8, Round: 15 * time.Minute, MinLead: 15 * time.Minute,
		MaxRevisions: 2, MinMoveFrac: 0.1, MinMove: 15 * time.Minute, MinNarrowing: 0.2,
		Location: time.UTC,
	}
}

// Hard reports whether the policy's deadlines are commitments made outside
// the farm (tracking, latency SLOs): they are never suggested or moved,
// only checked.
func (p Policy) Hard() bool { return p.Kind == Tracking || p.Kind == Latency }

func (p Policy) loc() *time.Location {
	if p.Location != nil {
		return p.Location
	}
	return time.UTC
}

// clock is a time of day in minutes.
type clock int

func parseClock(s string) (clock, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("session %q: want HH:MM", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("session %q: want HH:MM", s)
	}
	return clock(hh*60 + mm), nil
}

// Validate checks the policy's fields.
func (p Policy) Validate() error {
	switch p.Kind {
	case Sessions:
		if len(p.Sessions) == 0 {
			return fmt.Errorf("sessions policy with no sessions")
		}
		for _, s := range p.Sessions {
			if _, err := parseClock(s); err != nil {
				return err
			}
		}
	case Turnaround, Latency:
		if p.Turnaround <= 0 {
			return fmt.Errorf("%s policy needs a turnaround", p.Kind)
		}
	case Achievable, Tracking:
	default:
		return fmt.Errorf("unknown deadline policy %q", p.Kind)
	}
	if p.Quantile <= 0 || p.Quantile >= 1 {
		return fmt.Errorf("quantile %v: want between 0 and 1", p.Quantile)
	}
	return nil
}

func (p Policy) dayAllowed(d time.Weekday) bool {
	if len(p.Weekdays) == 0 {
		return true
	}
	for _, w := range p.Weekdays {
		if w == d {
			return true
		}
	}
	return false
}

// NextSession returns the first session at or after t, or the zero time
// if the policy has none in the next two weeks.
func (p Policy) NextSession(t time.Time) time.Time {
	var cs []clock
	for _, s := range p.Sessions {
		if c, err := parseClock(s); err == nil {
			cs = append(cs, c)
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i] < cs[j] })
	loc := p.loc()
	lt := t.In(loc)
	y, mo, d := lt.Date()
	for day := 0; day < 15; day++ {
		date := time.Date(y, mo, d+day, 0, 0, 0, 0, loc)
		if !p.dayAllowed(date.Weekday()) {
			continue
		}
		for _, c := range cs {
			at := time.Date(date.Year(), date.Month(), date.Day(), int(c)/60, int(c)%60, 0, 0, loc)
			if !at.Before(t) {
				return at
			}
		}
	}
	return time.Time{}
}

// Earliest returns the earliest deadline the policy allows that is no
// earlier than finish: the first session at or after it, or finish
// rounded up. submit and ready anchor Turnaround and Latency policies,
// whose deadline is fixed whatever the finish. It returns the zero time
// for Tracking, whose deadline comes from tracking.
func (p Policy) Earliest(finish, now, submit, ready time.Time) time.Time {
	switch p.Kind {
	case Sessions:
		return p.NextSession(maxTime(finish, now.Add(p.MinLead)))
	case Achievable:
		t := maxTime(finish, now.Add(p.MinLead))
		if p.Round > 0 {
			// Round up in the policy's location so 15-minute steps land
			// on the quarter hour of local time, also across offsets that
			// are not whole hours.
			lt := t.In(p.loc())
			_, off := lt.Zone()
			r := int64(p.Round / time.Second)
			sec := lt.Unix() + int64(off)
			if m := sec % r; m != 0 || lt.Nanosecond() != 0 {
				sec += r - m
			}
			t = time.Unix(sec-int64(off), 0).In(p.loc())
		}
		return t
	case Turnaround:
		return submit.Add(p.Turnaround)
	case Latency:
		return ready.Add(p.Turnaround)
	}
	return time.Time{}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
