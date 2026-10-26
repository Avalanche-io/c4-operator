package forecast

import "time"

// Clock renders a time for a person: local time first, with UTC alongside,
// for example "Thu 08:00 PDT (15:00 UTC)". When loc is UTC (or nil) the UTC
// time is shown once. A different UTC day is spelled out, "Thu 20:00 PDT
// (Fri 03:00 UTC)". Times in API fields stay RFC 3339; this is only for
// sentences and labels.
func Clock(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	ut := t.UTC()
	if loc == time.UTC || loc.String() == "UTC" {
		return ut.Format("Mon 15:04 UTC")
	}
	lt := t.In(loc)
	s := lt.Format("Mon 15:04 MST")
	if lt.Weekday() != ut.Weekday() {
		return s + " (" + ut.Format("Mon 15:04") + " UTC)"
	}
	return s + " (" + ut.Format("15:04") + " UTC)"
}

// NoHistory is what a person is shown in place of a finish or a slack
// while a job's estimate rests on its class default
// (estimate.Summary.NoHistory): "no history yet: the first frames set the
// estimate" for a render, "units" for other classes.
func NoHistory(class string) string {
	units := "units"
	if class == "render" {
		units = "frames"
	}
	return "no history yet: the first " + units + " set the estimate"
}
