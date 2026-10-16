package view

import (
	"fmt"
	"path"
	"strings"
)

// Filter is the sparse part of a Materialization: path patterns. A file is
// included when the file path itself, or any of its ancestor directory
// paths, matches a pattern under path.Match. An empty Filter includes
// everything.
//
//	shots/sh010/        everything under shots/sh010
//	shots/*/plates      the plates directory of every shot
//	weights/model.bin   one file
//	*.exr               every .exr at the top level only (path.Match does
//	                    not cross "/")
//
// Filters compose by intersection (Intersect), which is how a project's
// grant paths limit what a Materialization's include list can reach.
type Filter struct {
	patterns [][]string // each pattern split into components
	and      *Filter    // also required, when set
}

// NewFilter validates and compiles patterns.
func NewFilter(patterns []string) (Filter, error) {
	var f Filter
	for _, raw := range patterns {
		p := strings.Trim(strings.TrimPrefix(strings.TrimSpace(raw), "./"), "/")
		if p == "" {
			return Filter{}, fmt.Errorf("include pattern %q is empty; omit include to take the whole tree", raw)
		}
		parts := strings.Split(p, "/")
		for _, c := range parts {
			if c == "" || c == "." || c == ".." {
				return Filter{}, fmt.Errorf("include pattern %q: invalid component %q", raw, c)
			}
			if _, err := path.Match(c, ""); err != nil {
				return Filter{}, fmt.Errorf("include pattern %q: %w", raw, err)
			}
		}
		f.patterns = append(f.patterns, parts)
	}
	return f, nil
}

// Intersect returns a filter that includes what both include.
func Intersect(a, b Filter) Filter {
	if b.All() {
		return a
	}
	if a.All() {
		return b
	}
	out := a
	if out.and != nil {
		inner := Intersect(*out.and, b)
		out.and = &inner
		return out
	}
	out.and = &b
	return out
}

// All reports whether the filter includes everything.
func (f Filter) All() bool { return len(f.patterns) == 0 && f.and == nil }

// Includes reports whether a file or directory path (slash-separated,
// relative, directories with or without a trailing slash) is covered.
func (f Filter) Includes(p string) bool {
	if f.and != nil && !f.and.Includes(p) {
		return false
	}
	if len(f.patterns) == 0 {
		return true
	}
	comps := strings.Split(strings.Trim(p, "/"), "/")
	for _, pat := range f.patterns {
		if len(pat) > len(comps) {
			continue
		}
		if prefixMatch(pat, comps[:len(pat)]) {
			return true
		}
	}
	return false
}

// MayContain reports whether anything under directory dir could be
// included. It keeps resolution sparse: a directory record the filter can
// never reach is not fetched.
func (f Filter) MayContain(dir string) bool {
	if f.and != nil && !f.and.MayContain(dir) {
		return false
	}
	if len(f.patterns) == 0 {
		return true
	}
	comps := strings.Split(strings.Trim(dir, "/"), "/")
	for _, pat := range f.patterns {
		n := min(len(pat), len(comps))
		if prefixMatch(pat[:n], comps[:n]) {
			return true
		}
	}
	return false
}

func prefixMatch(pat, comps []string) bool {
	for i := range pat {
		ok, _ := path.Match(pat[i], comps[i])
		if !ok {
			return false
		}
	}
	return true
}
