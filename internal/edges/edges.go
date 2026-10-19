// Package edges finds a plan's orderings in the work itself: edge sources
// read a tree (code markers, a list a tool wrote) and give the edges as an
// expansion document of edges alone (DESIGN 10.6), which a procedural
// unit re-run after each merge writes to .c4/expand.json. The plan's
// edges are then computed, not kept by hand (LLM-OR-PROCEDURE 5 and 7.2:
// Ra's Pending markers).
//
// A source is a plugin: Register adds one by name, and c4-edges runs one.
// Two are built in:
//
//   - marker: a regular expression over the files of a tree, with a named
//     group from (the unit waited on) and, optionally, to (the unit that
//     waits). Without to, the waiting unit is the owner of the file (the
//     unit whose claimed path covers it, from an owners map), else a
//     default.
//   - json: a file of edges, [{from, to, file?, line?, text?}].
package edges

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Edge is one ordering: To runs after From, read at File:Line.
type Edge struct {
	From, To string
	File     string
	Line     int
	Text     string
}

// Config configures a source.
type Config struct {
	// Root is the tree to read.
	Root string
	// marker: Pattern, the files to read (Include, Exclude: globs on the
	// slash path from Root; "**/" matches any directories), Owners (a
	// path, or a directory ending in "/", to the unit that owns it) and To,
	// the waiting unit when neither the pattern nor Owners says.
	Pattern          string
	Include, Exclude []string
	Owners           map[string]string
	To               string
	// json: File, the list of edges.
	File string
}

// Source finds edges.
type Source interface {
	Edges(ctx context.Context) ([]Edge, []Note, error)
}

// Note is something a source read and could not turn into an edge.
type Note struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
	Why  string `json:"why"`
}

var sources = map[string]func(Config) (Source, error){}

// Register adds a source by name.
func Register(name string, open func(Config) (Source, error)) { sources[name] = open }

// Open makes the named source.
func Open(name string, cfg Config) (Source, error) {
	open, ok := sources[name]
	if !ok {
		var names []string
		for n := range sources {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("no edge source %q (have %s)", name, strings.Join(names, ", "))
	}
	return open(cfg)
}

func init() {
	Register("marker", newMarker)
	Register("json", newJSON)
}

type marker struct {
	cfg Config
	re  *regexp.Regexp
	to  int
}

func newMarker(cfg Config) (Source, error) {
	re, err := regexp.Compile(cfg.Pattern)
	if err != nil {
		return nil, fmt.Errorf("pattern: %w", err)
	}
	if re.SubexpIndex("from") < 0 {
		return nil, fmt.Errorf("pattern %q has no (?P<from>...) group", cfg.Pattern)
	}
	if len(cfg.Include) == 0 {
		cfg.Include = []string{"**/*"}
	}
	return &marker{cfg: cfg, re: re, to: re.SubexpIndex("to")}, nil
}

func (m *marker) Edges(ctx context.Context) ([]Edge, []Note, error) {
	var out []Edge
	var notes []Note
	err := filepath.WalkDir(m.cfg.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(m.cfg.Root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".c4" {
				return filepath.SkipDir
			}
			return nil
		}
		if !matchAny(m.cfg.Include, rel) || matchAny(m.cfg.Exclude, rel) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for line := 1; sc.Scan(); line++ {
			text := sc.Text()
			for _, g := range m.re.FindAllStringSubmatch(text, -1) {
				from := g[m.re.SubexpIndex("from")]
				to := ""
				if m.to >= 0 {
					to = g[m.to]
				}
				if to == "" {
					to = owner(m.cfg.Owners, rel)
				}
				if to == "" {
					to = m.cfg.To
				}
				switch {
				case from == "":
					notes = append(notes, Note{File: rel, Line: line, Text: strings.TrimSpace(text), Why: "the marker names no unit"})
				case to == "":
					notes = append(notes, Note{File: rel, Line: line, Text: strings.TrimSpace(text), Why: "no unit owns this file, and no default was given"})
				case to == from:
					// a unit's own marker naming itself: no ordering
				default:
					out = append(out, Edge{From: from, To: to, File: rel, Line: line, Text: strings.TrimSpace(text)})
				}
			}
		}
		return sc.Err()
	})
	return out, notes, err
}

// owner is the unit owning file rel: the longest owned path covering it.
func owner(owners map[string]string, rel string) string {
	best, who := -1, ""
	for p, u := range owners {
		q := strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
		if rel == q || strings.HasPrefix(rel, q+"/") || q == "" {
			if len(q) > best {
				best, who = len(q), u
			}
		}
	}
	return who
}

// matchAny matches a slash path against globs; "**/" matches any number
// of directories, including none.
func matchAny(globs []string, rel string) bool {
	for _, g := range globs {
		if match(g, rel) {
			return true
		}
	}
	return false
}

func match(g, rel string) bool {
	if rest, ok := strings.CutPrefix(g, "**/"); ok {
		parts := strings.Split(rel, "/")
		for i := range parts {
			if match(rest, strings.Join(parts[i:], "/")) {
				return true
			}
		}
		return false
	}
	ok, _ := path.Match(g, rel)
	return ok
}

type jsonSource struct{ cfg Config }

func newJSON(cfg Config) (Source, error) {
	if cfg.File == "" {
		return nil, fmt.Errorf("the json source needs a file")
	}
	return jsonSource{cfg}, nil
}

func (s jsonSource) Edges(context.Context) ([]Edge, []Note, error) {
	b, err := os.ReadFile(s.cfg.File)
	if err != nil {
		return nil, nil, err
	}
	var raw []struct {
		From, To, File, Text string
		Line                 int
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", s.cfg.File, err)
	}
	var out []Edge
	var notes []Note
	for _, r := range raw {
		if r.From == "" || r.To == "" {
			notes = append(notes, Note{File: r.File, Line: r.Line, Text: r.Text, Why: "an edge without both ends"})
			continue
		}
		out = append(out, Edge{From: r.From, To: r.To, File: r.File, Line: r.Line, Text: r.Text})
	}
	return out, notes, nil
}

// Document is edges as an expansion document, version 1, of edges alone:
// each ordering once, citing the first place it was read, in a stable
// order.
func Document(es []Edge) ([]byte, error) {
	type cite struct {
		File string `json:"file,omitempty"`
		Line int    `json:"line,omitempty"`
		Text string `json:"text,omitempty"`
	}
	type edge struct {
		From string `json:"from"`
		To   string `json:"to"`
		Cite *cite  `json:"cite,omitempty"`
	}
	seen := map[[2]string]bool{}
	var out []edge
	for _, e := range es {
		k := [2]string{e.From, e.To}
		if seen[k] {
			continue
		}
		seen[k] = true
		x := edge{From: e.From, To: e.To}
		if e.File != "" {
			x.Cite = &cite{File: e.File, Line: e.Line, Text: e.Text}
		}
		out = append(out, x)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].To != out[b].To {
			return out[a].To < out[b].To
		}
		return out[a].From < out[b].From
	})
	return json.MarshalIndent(map[string]any{"version": 1, "units": []any{}, "edges": out}, "", "  ")
}
