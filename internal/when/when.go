// Package when reads a time as a person writes one: "18:00", "fri 18:00",
// "+3h", or an RFC 3339 time. The CLI's --due and a submission's due read
// alike.
package when

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	clockRE  = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
	dayRE    = regexp.MustCompile(`^(mon|tue|wed|thu|fri|sat|sun)[a-z]*\s+(\d{1,2}):(\d{2})$`)
	offsetRE = regexp.MustCompile(`^\+(\d+(?:\.\d+)?)(m|h|d)$`)
)

// Parse reads in on the clock of loc at now: "18:00" (today, or tomorrow
// if that has passed), "fri 18:00" (the next Friday), "+3h", "+90m",
// "+2d", or an RFC 3339 time (or one without its zone, in loc).
// "suggested" (or "") returns the zero time: the farm suggests one.
func Parse(in string, now time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	raw := strings.TrimSpace(in)
	s := strings.ToLower(raw)
	local := now.In(loc)
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
		t := time.Date(local.Year(), local.Month(), local.Day(), h, mi, 0, 0, loc)
		if !t.After(local) {
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
		t := time.Date(local.Year(), local.Month(), local.Day(), h, mi, 0, 0, loc)
		for t.Weekday() != want || !t.After(local) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	case offsetRE.MatchString(s):
		m := offsetRE.FindStringSubmatch(s)
		n, _ := strconv.ParseFloat(m[1], 64)
		unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
		return now.Add(time.Duration(n * float64(unit))).Truncate(time.Minute), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, strings.ToUpper(raw), loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q: give a time like 18:00, fri 18:00, +3h, 2026-10-23T18:00:00-07:00, or suggested", s)
}
