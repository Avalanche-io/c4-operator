package agent

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A usage limit (the subscription's session, weekly or spend window, or a
// model's own) stops a session's turns until it resets. On a host runner
// every member signs in as the same person, so a limit one member hits is a
// capacity drop for the machine: the runner offers no slots until the
// window resets, says why, and the queue yields and holds members as it does
// for any drop in capacity, and brings them back after.

// UsageLimit is a usage limit a session hit.
type UsageLimit struct {
	// Kind is session, weekly, spend or model.
	Kind string
	// Text is the CLI's own words.
	Text string
	// At is when the session hit it.
	At time.Time
	// Resets is when the window resets, when the CLI said; zero when it did
	// not (a spend limit raised by a person, a model's limit).
	Resets time.Time
}

// Why says it as a capacity reason.
func (l UsageLimit) Why() string {
	s := "usage limit: " + l.Text
	if !l.Resets.IsZero() {
		s += " (until " + l.Resets.UTC().Format(time.RFC3339) + ")"
	}
	return s
}

var resetsRE = regexp.MustCompile(`resets (?:([A-Z][a-z]{2}) (\d{1,2}) at )?(\d{1,2})(?::(\d{2}))?\s*(am|pm) \(([^)]+)\)`)

// limitKind is the kind of a rate-limit message, from its words.
func limitKind(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "session limit"):
		return "session"
	case strings.Contains(t, "weekly limit"):
		return "weekly"
	case strings.Contains(t, "spend limit"):
		return "spend"
	}
	return "model"
}

// resetTime reads "resets 1:10am (America/Los_Angeles)" or "resets Sep 27
// at 12pm (America/Los_Angeles)": the first such time after at, in that
// zone. ok is false when the text gives none.
func resetTime(text string, at time.Time) (time.Time, bool) {
	m := resetsRE.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(m[6])
	if err != nil {
		return time.Time{}, false
	}
	hour, _ := strconv.Atoi(m[3])
	minute := 0
	if m[4] != "" {
		minute, _ = strconv.Atoi(m[4])
	}
	if hour == 12 {
		hour = 0
	}
	if m[5] == "pm" {
		hour += 12
	}
	local := at.In(loc)
	if m[1] != "" {
		month, err := time.Parse("Jan", m[1])
		if err != nil {
			return time.Time{}, false
		}
		day, _ := strconv.Atoi(m[2])
		t := time.Date(local.Year(), month.Month(), day, hour, minute, 0, 0, loc)
		if t.Before(at) {
			t = t.AddDate(1, 0, 0)
		}
		return t, true
	}
	t := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if t.Before(at) {
		t = t.AddDate(0, 0, 1)
	}
	return t, true
}

// scanUsageLimit reads a transcript from offset for the CLI's rate-limit
// errors, and returns the last one, if any, with the offset it read to (a
// line not yet ended is read next time).
func scanUsageLimit(transcript string, offset int64) (*UsageLimit, int64, error) {
	f, err := os.Open(transcript)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() < offset {
		offset = 0 // rewritten: read it again
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	var last *UsageLimit
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			break // a partial line is read again next time
		}
		offset += int64(len(line))
		if !strings.Contains(string(line), `"rate_limit"`) {
			continue
		}
		var rec struct {
			Timestamp         time.Time `json:"timestamp"`
			IsAPIErrorMessage bool      `json:"isApiErrorMessage"`
			Error             string    `json:"error"`
			Message           struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil || !rec.IsAPIErrorMessage || rec.Error != "rate_limit" {
			continue
		}
		var text []string
		for _, c := range rec.Message.Content {
			if c.Type == "text" {
				text = append(text, c.Text)
			}
		}
		l := &UsageLimit{Text: strings.TrimSpace(strings.Join(text, " ")), At: rec.Timestamp}
		l.Kind = limitKind(l.Text)
		if t, ok := resetTime(l.Text, rec.Timestamp); ok {
			l.Resets = t
		}
		last = l
	}
	return last, offset, nil
}

// EnvUsageLimitProbe is how long the runner holds its slots for a usage
// limit that gives no reset time before it tries again (a Go duration).
const EnvUsageLimitProbe = "C4_USAGE_LIMIT_PROBE"

const defaultLimitProbe = 30 * time.Minute

// noteUsageLimits reads the new part of a member's transcript for a usage
// limit and, on one, holds the machine's slots until it resets.
func (r *Runner) noteUsageLimits(m *Member) {
	if m.Transcript == "" {
		return
	}
	l, off, err := scanUsageLimit(m.Transcript, r.limitRead[m.Name])
	if err != nil {
		return
	}
	r.limitRead[m.Name] = off
	if l == nil {
		return
	}
	until := l.Resets.Add(time.Minute)
	if l.Resets.IsZero() {
		probe := defaultLimitProbe
		if d, err := time.ParseDuration(os.Getenv(EnvUsageLimitProbe)); err == nil && d > 0 {
			probe = d
		}
		until = r.now().Add(probe)
	}
	if !until.After(r.now()) || !until.After(r.limitUntil) {
		return
	}
	r.limitUntil, r.limitWhy = until, l.Why()
	r.logf("%s hit a %s usage limit (%q): this machine offers no slots until %s", m.Name, l.Kind, l.Text, until.Local().Format("Jan 2 15:04"))
}

// limitHold is the slots a usage limit leaves the machine, and why.
func (r *Runner) limitHold() (bool, string) {
	if r.limitUntil.IsZero() || !r.now().Before(r.limitUntil) {
		return false, ""
	}
	return true, r.limitWhy
}
