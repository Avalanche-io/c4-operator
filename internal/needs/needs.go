// Package needs is what a unit that stops for information its brief
// lacked says about its question, shared by c4-unit and c4-agent's blocks.
package needs

import (
	"fmt"
	"strings"
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
