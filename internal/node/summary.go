package node

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxSummary bounds the summary of its task a unit leaves in .c4/done, as
// the farm keeps it, in bytes.
const MaxSummary = 4 << 10

// Summary is a unit's own summary of its task (.c4/done) as the farm keeps
// it: trimmed, cut to MaxSummary at a character boundary, with any path
// under a home folder written from ~. It is the unit's words, not a fact
// the farm checked. Empty when the unit wrote nothing.
func Summary(b []byte) []byte {
	b = bytes.TrimSpace(b)
	if len(b) > MaxSummary {
		b = b[:MaxSummary]
		for len(b) > 0 && !utf8.Valid(b) {
			b = b[:len(b)-1]
		}
	}
	return []byte(Homeless(string(b)))
}

// homeRE finds a path under a person's home folder in free text.
var homeRE = regexp.MustCompile(`(?:/Users|/home)/[^/\s"'` + "`" + `]+`)

// Homeless says paths under a home folder from ~, so free text that leaves
// the machine (what a session last said, a limit's words, a unit's
// summary) carries no one's home path.
func Homeless(s string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return homeRE.ReplaceAllString(s, "~")
}
