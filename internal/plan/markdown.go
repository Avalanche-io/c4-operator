package plan

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// line is one line of a plan file, with its 1-based number.
type line struct {
	n    int
	text string
}

func readLines(path string) ([]line, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []line
	for i, t := range strings.Split(string(b), "\n") {
		out = append(out, line{i + 1, t})
	}
	return out, nil
}

// section returns the lines under the "## " heading that starts with
// prefix, up to the next heading of the same level.
func section(ls []line, prefix string) []line {
	var out []line
	in := false
	for _, l := range ls {
		if strings.HasPrefix(l.text, "## ") {
			if in {
				break
			}
			in = strings.HasPrefix(strings.TrimSpace(l.text[3:]), prefix)
			continue
		}
		if in {
			out = append(out, l)
		}
	}
	return out
}

// clean strips Markdown emphasis, code and wiki-link marks.
func clean(s string) string {
	r := strings.NewReplacer("**", "", "*", "", "`", "", "[[", "", "]]", "")
	return r.Replace(s)
}

// sentence is a sentence of prose, at the line it starts on.
type sentence struct {
	n    int
	text string
}

// sentences joins each paragraph of prose (not table rows, not code) and
// splits it into sentences.
func sentences(ls []line) []sentence {
	var out []sentence
	var buf []string
	start := 0
	flush := func() {
		if len(buf) == 0 {
			return
		}
		para := clean(strings.Join(buf, " "))
		for _, s := range splitSentences(para) {
			out = append(out, sentence{start, s})
		}
		buf = buf[:0]
	}
	code := false
	for _, l := range ls {
		t := strings.TrimSpace(l.text)
		if strings.HasPrefix(t, "```") {
			code = !code
			flush()
			continue
		}
		if code || t == "" || strings.HasPrefix(t, "|") || strings.HasPrefix(t, "#") {
			flush()
			continue
		}
		// a list item starts its own paragraph
		if listItem.MatchString(t) {
			flush()
		}
		if len(buf) == 0 {
			start = l.n
		}
		buf = append(buf, t)
	}
	flush()
	return out
}

var (
	sentenceEnd = regexp.MustCompile(`[.!?]\s+`)
	listItem    = regexp.MustCompile(`^(?:[-*] |\d+\. )`)
)

func splitSentences(p string) []string {
	var out []string
	last := 0
	for _, m := range sentenceEnd.FindAllStringIndex(p, -1) {
		out = append(out, strings.TrimSpace(p[last:m[0]+1]))
		last = m[1]
	}
	if rest := strings.TrimSpace(p[last:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// cells splits a Markdown table row into its cells.
func cells(row string) []string {
	t := strings.TrimSpace(row)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// laneRe matches a lane's name: C2.3, C1.3b, A1p, A12, B2a, F1, M2-4,
// optionally written M1-C2.3; and a bare chapter reference, C3.
var laneRe = regexp.MustCompile(`\b(?:M1-)?(M2-\d+|[ABCFZ]\d+(?:\.\d+[a-z]?|[a-z])?)\b`)

// token is a lane name found in text, at its byte offsets.
type token struct {
	name       string
	start, end int
}

func tokens(s string) []token {
	var out []token
	for _, m := range laneRe.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, token{s[m[2]:m[3]], m[0], m[1]})
	}
	return out
}

// isChapterRef says whether name is a bare chapter reference (C3).
func isChapterRef(name string) bool {
	return len(name) >= 2 && name[0] == 'C' && !strings.ContainsAny(name, ".") && isDigits(name[1:])
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// chapterOf is the chapter number of a chapter lane (C2.3 → 2), or 0.
func chapterOf(name string) int {
	if !strings.HasPrefix(name, "C") {
		return 0
	}
	num := name[1:]
	if i := strings.IndexByte(num, '.'); i >= 0 {
		num = num[:i]
	}
	n, _ := strconv.Atoi(num)
	return n
}

// laneNum is the number after the chapter's dot (C4.6 → 6), for ranges.
func laneNum(name string) (int, bool) {
	i := strings.IndexByte(name, '.')
	if i < 0 {
		return 0, false
	}
	d := strings.TrimRight(name[i+1:], "abcdefghijklmnopqrstuvwxyz")
	n, err := strconv.Atoi(d)
	return n, err == nil && d == name[i+1:]
}

// leadingInt reads a cell's leading integer ("14 (+2 sites)" → 14).
func leadingInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	return n, err == nil
}

func rel(dir, path string) string {
	if r, err := filepath.Rel(dir, path); err == nil {
		return r
	}
	return path
}
