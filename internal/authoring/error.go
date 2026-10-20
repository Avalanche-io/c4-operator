// Package authoring reads the two authoring formats, submissions (c4:
// submit/1) and tools (c4: tool/1), checks them against the farm, and
// lowers them to the jobs the operator runs (schema/README.md; DESIGN.md
// 10.6, Tools). The HUD's POST /submissions and POST /tools use it; so do
// the CLI and the tool runner, so that every reader parses the files
// alike.
package authoring

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Avalanche-io/c4-operator/schema"
)

// Error is one refusal as the farm reports it: where (a dotted path, list
// indexes in brackets: jobs.comp.after, params[1].type), what, and what to
// do about it. It is the schema package's, so the fixtures' errors and
// the farm's are one type.
type Error = schema.Error

// Errors is a list of refusals as one error, for callers that want one.
type Errors []Error

func (es Errors) Error() string {
	var b strings.Builder
	for i, e := range es {
		if i > 0 {
			b.WriteString("; ")
		}
		if e.Path != "" {
			b.WriteString(e.Path + ": ")
		}
		b.WriteString(e.Message)
	}
	return b.String()
}

func errf(path, fix, format string, args ...any) Error {
	return Error{Path: path, Message: fmt.Sprintf(format, args...), Fix: fix}
}

// closest is the candidate nearest to s by edit distance, if near enough
// to be a slip (at most a third of its length, and at least one edit), or
// sharing a prefix of three letters, or holding most of its letters in
// order.
func closest(s string, candidates []string) (string, bool) {
	best, bestD := "", -1
	for _, c := range candidates {
		d := distance(strings.ToLower(s), strings.ToLower(c))
		if bestD < 0 || d < bestD || d == bestD && c < best {
			best, bestD = c, d
		}
	}
	if bestD < 0 {
		return "", false
	}
	limit := max(2, (len(s)+2)/3)
	if bestD <= limit {
		return best, true
	}
	if len(s) >= 3 && len(best) >= 3 && strings.HasPrefix(strings.ToLower(best), strings.ToLower(s[:3])) {
		return best, true
	}
	// a word spelt by ear (lite for light): most of its letters, in order
	sim, simBest := 0.0, ""
	for _, c := range candidates {
		l := lcs(strings.ToLower(s), strings.ToLower(c))
		if r := 2 * float64(l) / float64(len(s)+len(c)); r > sim || r == sim && c < simBest {
			sim, simBest = r, c
		}
	}
	if sim >= 0.65 && len(s) >= 3 {
		return simBest, true
	}
	return "", false
}

// lcs is the length of the longest common subsequence of a and b.
func lcs(a, b string) int {
	prev := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else {
				cur[j] = max(prev[j], cur[j-1])
			}
		}
		prev = cur
	}
	return prev[len(b)]
}

// distance is the edit distance between a and b (insertions, deletions,
// substitutions, and transpositions of neighbours).
func distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	d := make([][]int, len(ra)+1)
	for i := range d {
		d[i] = make([]int, len(rb)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(ra)][len(rb)]
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func quoted(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}

func sortedStrings(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
