package authoring

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/when"
)

// A Due is a submission's or a job's due, read: a milestone it joins, a
// pinned time, a date at the project's due time, or the farm's to suggest.
type Due struct {
	Milestone string
	At        time.Time
	Date      string
	Suggested bool
}

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// readDue reads a due: "suggested" (and ""), a milestone of the
// namespace, a date (2026-10-23), or a time as a person writes one
// (fri 18:00, 18:00, +3h, RFC 3339), read in loc at now.
func readDue(s string, milestones map[string]time.Time, now time.Time, loc *time.Location) (Due, bool) {
	s = strings.TrimSpace(s)
	if _, ok := milestones[s]; ok && s != "" {
		return Due{Milestone: s}, true
	}
	switch {
	case s == "" || s == "suggested":
		return Due{Suggested: true}, true
	case dateRE.MatchString(s):
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return Due{}, false
		}
		return Due{Date: s}, true
	}
	t, err := when.Parse(s, now, loc)
	if err != nil || t.IsZero() {
		return Due{}, false
	}
	return Due{At: t}, true
}

// dueError is the refusal of a due that is none of the forms, naming the
// project's milestones, soonest first.
func dueError(path, s, project string, milestones map[string]time.Time) Error {
	names := make([]string, 0, len(milestones))
	for n := range milestones {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := milestones[names[i]], milestones[names[j]]
		return a.Before(b) || a.Equal(b) && names[i] < names[j]
	})
	fix := "use a time (fri 18:00, +3h), a date (2026-10-23), or suggested"
	if len(names) > 0 {
		fix = "use a milestone (" + strings.Join(names, ", ") + "), " + strings.TrimPrefix(fix, "use ")
	}
	return errf(path, fix, "%q is not a milestone in %s, a time or a date", s, project)
}

// String is the due as the explanation says it.
func (d Due) String() string {
	switch {
	case d.Milestone != "":
		return fmt.Sprintf("milestone %s (label c4.avalanche.io/milestone=%s)", d.Milestone, d.Milestone)
	case !d.At.IsZero():
		return "pinned " + d.At.Format(time.RFC3339)
	case d.Date != "":
		return "pinned " + d.Date + ", at the project's due time"
	}
	return "suggested by the farm"
}
