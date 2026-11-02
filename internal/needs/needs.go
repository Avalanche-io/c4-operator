// Package needs is what a unit that stops for information its brief
// lacked says about its question, shared by c4-unit and c4-agent's blocks.
package needs

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// About checks what a question is about: the work whose landing should
// re-check it. Each is path:<prefix in the job's tree> (a bare value is a
// path), unit:<a unit of this job> or job:<namespace>/<name>. It returns
// them with their kind spelled out.
func About(about []string) ([]string, error) {
	want := map[string]string{"path": "a path in the job's tree", "unit": "a unit's name", "job": "namespace/name"}
	var out []string
	for _, a := range about {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		kind, v, ok := strings.Cut(a, ":")
		if !ok {
			kind, v = "path", a
		}
		if want[kind] == "" {
			return nil, fmt.Errorf("about %q: path:, unit: or job:", a)
		}
		v = strings.TrimSpace(v)
		if v == "" || strings.ContainsAny(v, ",\n") || kind == "job" && !strings.Contains(v, "/") {
			return nil, fmt.Errorf("about %q: want %s", a, want[kind])
		}
		out = append(out, kind+":"+v)
	}
	return out, nil
}

// Message is the termination message of a unit that needs information:
// the text, then a "why: " line and an "about: " line when there are any.
// The farm reads it as the question.
func Message(text, why string, about []string) string {
	msg := strings.TrimSpace(text)
	if why = strings.TrimSpace(why); why != "" {
		msg += "\nwhy: " + why
	}
	if len(about) > 0 {
		msg += "\nabout: " + strings.Join(about, ", ")
	}
	return msg
}

// MessageMax is the most a needs message may be: Kubernetes cuts a
// container's termination message at 4096 bytes.
const MessageMax = 4096

// CutMark ends a field Fit shortened.
const CutMark = " [cut]"

// Fit shortens a question so its Message fits in MessageMax: the why
// first, then the text, each cut at a character boundary and ending with
// CutMark, and only if those can't make room, the last abouts. The unit
// signs exactly what Fit returns and writes exactly its Message, so the
// farm reads the same question it signed.
func Fit(text, why string, about []string) (string, string, []string) {
	text, why = strings.TrimSpace(text), strings.TrimSpace(why)
	const keep = 64 // a field is cut no shorter than this
	for {
		over := len(Message(text, why, about)) - MessageMax
		switch {
		case over <= 0:
			return text, why, about
		case len(why) > keep+len(CutMark):
			why = cut(why, max(keep, len(why)-over-len(CutMark)))
		case len(text) > keep+len(CutMark):
			text = cut(text, max(keep, len(text)-over-len(CutMark)))
		case len(about) > 0:
			about = about[:len(about)-1]
		default:
			return text, why, about
		}
	}
}

// cut is s's first n bytes or fewer, at a character boundary, without
// trailing space, and CutMark.
func cut(s string, n int) string {
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimRightFunc(s[:min(n, len(s))], unicode.IsSpace) + CutMark
}
