package farmcli

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Clock formats times for people: local time with a short zone, and UTC
// small beside it when it helps. The farm and its APIs stay in UTC.
type Clock struct {
	Now func() time.Time
	Loc *time.Location
	// UTC prints UTC instead of local time (--utc).
	UTC bool
}

func (c Clock) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Clock) loc() *time.Location {
	if c.UTC {
		return time.UTC
	}
	if c.Loc != nil {
		return c.Loc
	}
	return time.Local
}

// At is "14:52 PDT", with the weekday when it is not today and the date
// when it is more than a week away.
func (c Clock) At(t time.Time) string {
	t = t.In(c.loc())
	now := c.now().In(c.loc())
	zone := t.Format("MST")
	switch {
	case sameDay(t, now):
		return t.Format("15:04") + " " + zone
	case t.After(now) && t.Sub(now) < 6*24*time.Hour, t.Before(now) && now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon 15:04") + " " + zone
	default:
		return t.Format("Mon 2 Jan 15:04") + " " + zone
	}
}

// AtUTC is At with the UTC time beside it, "18:00 PDT today (01:00 UTC)",
// unless the clock already shows UTC.
func (c Clock) AtUTC(t time.Time) string {
	s := c.At(t)
	if sameDay(t.In(c.loc()), c.now().In(c.loc())) {
		s += " today"
	}
	if c.loc() == time.UTC {
		return s
	}
	return s + " (" + t.UTC().Format("15:04") + " UTC)"
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// Span is a duration as people say it: "3h", "1h 25m", "8m", "40s".
func Span(d time.Duration) string {
	neg := d < 0
	if neg {
		d = -d
	}
	var s string
	switch {
	case d >= 48*time.Hour:
		s = fmt.Sprintf("%dd", int(math.Round(d.Hours()/24)))
	case d >= 10*time.Hour:
		s = fmt.Sprintf("%dh", int(math.Round(d.Hours())))
	case d >= time.Hour:
		h := int(d.Hours())
		m := int(math.Round((d - time.Duration(h)*time.Hour).Minutes()))
		if m == 60 {
			h, m = h+1, 0
		}
		s = fmt.Sprintf("%dh", h)
		if m > 0 {
			s += fmt.Sprintf(" %dm", m)
		}
	case d >= time.Minute:
		s = fmt.Sprintf("%dm", int(math.Round(d.Minutes())))
	default:
		s = fmt.Sprintf("%ds", int(math.Round(d.Seconds())))
	}
	if neg {
		return "-" + s
	}
	return s
}

var (
	clockRE  = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
	dayRE    = regexp.MustCompile(`^(mon|tue|wed|thu|fri|sat|sun)[a-z]*\s+(\d{1,2}):(\d{2})$`)
	offsetRE = regexp.MustCompile(`^\+(\d+(?:\.\d+)?)(m|h|d)$`)
)

// ParseWhen reads a deadline as a person types it, on the local clock:
// "18:00" (today, or tomorrow if that has passed), "fri 18:00" (the next
// Friday), "+3h", "+90m", "+2d", or an RFC 3339 time. "suggested" (or
// "") returns the zero time: the farm suggests one.
func (c Clock) ParseWhen(in string) (time.Time, error) {
	raw := strings.TrimSpace(in)
	s := strings.ToLower(raw)
	now := c.now().In(c.loc())
	switch {
	case s == "" || s == "suggested":
		return time.Time{}, nil
	case clockRE.MatchString(s):
		m := clockRE.FindStringSubmatch(s)
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		if h > 23 || mi > 59 {
			return time.Time{}, fmt.Errorf("%q is not a time of day", s)
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	case dayRE.MatchString(s):
		m := dayRE.FindStringSubmatch(s)
		h, _ := strconv.Atoi(m[2])
		mi, _ := strconv.Atoi(m[3])
		if h > 23 || mi > 59 {
			return time.Time{}, fmt.Errorf("%q is not a time of day", s)
		}
		want := map[string]time.Weekday{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}[m[1]]
		t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
		for t.Weekday() != want || !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	case offsetRE.MatchString(s):
		m := offsetRE.FindStringSubmatch(s)
		n, _ := strconv.ParseFloat(m[1], 64)
		unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
		return c.now().Add(time.Duration(n * float64(unit))).Truncate(time.Minute), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, strings.ToUpper(raw), c.loc()); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q: give a time like 18:00, fri 18:00, +3h, 2026-10-23T18:00:00-07:00, or suggested", s)
}
