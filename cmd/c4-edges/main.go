// Command c4-edges runs an edge source over a tree and writes the edges it
// finds as an expansion document (DESIGN 10.6), for a procedural unit that
// keeps a job's graph in step with its code:
//
//	c4-edges -source marker -root /src -include '**/*.go' \
//	    -pattern 'Pending\{[^}]*Lane: *"(?P<from>[^"]+)"' -owners owners.json
//
// writes $C4_OUTPUT/.c4/expand.json (or -o FILE, "-" for stdout) with one
// edge per ordering, each citing the file and line it was read from, and
// prints what it could not read on stderr. -owners is a JSON object from
// a path (a directory ending in "/") to the unit that owns it, such as a
// lane's claimed paths; a marker in a file waits as its owner, else as
// -to. -source json reads a list of edges from -file.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/edges"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "c4-edges:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("c4-edges", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "marker", "the edge source: marker or json")
	root := fs.String("root", ".", "the tree to read")
	pattern := fs.String("pattern", "", "marker: a regular expression with a (?P<from>...) group and optionally (?P<to>...)")
	include := fs.String("include", "**/*", "marker: comma-separated globs of the files to read")
	exclude := fs.String("exclude", "", "marker: comma-separated globs of files to skip (test fixtures)")
	owners := fs.String("owners", "", "marker: a JSON file mapping paths to the units that own them")
	to := fs.String("to", "", "marker: the waiting unit when neither the pattern nor -owners says")
	file := fs.String("file", "", "json: the file of edges")
	out := fs.String("o", "", "where to write the document (default $C4_OUTPUT/.c4/expand.json; - for stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := edges.Config{Root: *root, Pattern: *pattern, Include: list(*include), Exclude: list(*exclude), To: *to, File: *file}
	if *owners != "" {
		b, err := os.ReadFile(*owners)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &cfg.Owners); err != nil {
			return fmt.Errorf("-owners: %w", err)
		}
	}
	src, err := edges.Open(*source, cfg)
	if err != nil {
		return err
	}
	es, notes, err := src.Edges(context.Background())
	if err != nil {
		return err
	}
	doc, err := edges.Document(es)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Fprintf(stderr, "%s:%d: %s: %s\n", n.File, n.Line, n.Why, n.Text)
	}
	fmt.Fprintf(stderr, "%d markers read as edges, %d not\n", len(es), len(notes))
	dest := *out
	if dest == "" {
		o := getenv("C4_OUTPUT")
		if o == "" {
			return errors.New("no -o and no $C4_OUTPUT")
		}
		dest = filepath.Join(o, ".c4", "expand.json")
	}
	if dest == "-" {
		_, err := stdout.Write(append(doc, '\n'))
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, append(doc, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func list(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
