// Command c4-plan turns a plan into a WorkJob's expansion document
// (expansion v1, the queue's own format): a block per piece of work, the
// edges between them, and where each edge came from (agent-jobs section 10).
//
//	c4-plan import [--format file|lanemap] [--milestone M1] [--extra EXTRA.yaml]
//	               [-o OUT.json] [--notes NOTES.json] PLAN
//
// PLAN is read through an adapter, and only read:
//
//   - a file (format "file"): a plan written as data, a YAML or JSON list of
//     blocks (internal/plan.File). This is the format to keep a plan in;
//   - a directory (format "lanemap"): a plan written as prose in the
//     lane-map layout (LANES.md, the milestone's file, charters/), read by
//     sentence patterns. Ra's plan is one such plan.
//
// EXTRA.yaml lists edges and waits a person adds where a plan states an
// order in words no adapter reads, each citing the plan:
//
//   - {block: C2.1, after: [F1], file: LANES.md, line: 193, text: "So no lane from C2.1 onward starts until the foundation lane is chartered and merged."}
//
// The importer prints a summary, and writes what it could not read (an
// ordering sentence with no pattern, an edge to a block the plan does not
// list) to NOTES.json, or to stderr: their count is how much of the plan is
// still only prose. A cycle in the edges is an error, and so is a document
// the queue would refuse.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4-operator/internal/plan"
	"github.com/Avalanche-io/c4-operator/internal/plan/lanemap"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "import" {
		fmt.Fprintln(stderr, "usage: c4-plan import [--format file|lanemap] [--milestone M1] [--extra EXTRA.yaml] [-o OUT.json] [--notes NOTES.json] PLAN")
		return 2
	}
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "", "file (a plan written as data) or lanemap (a lane-map directory); default: by whether PLAN is a file or a directory")
	milestone := fs.String("milestone", "M1", "lanemap: the milestone whose charters and file to read")
	extra := fs.String("extra", "", "a YAML list of edges and waits a person adds, each citing the plan")
	out := fs.String("o", "", "write the expansion document here (default stdout)")
	notes := fs.String("notes", "", "write what the importer could not read here (default stderr)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "c4-plan import: one PLAN")
		return 2
	}
	p, err := read(fs.Arg(0), *format, *milestone)
	if err != nil {
		fmt.Fprintln(stderr, "c4-plan:", err)
		return 1
	}
	if *extra != "" {
		b, err := os.ReadFile(*extra)
		if err != nil {
			fmt.Fprintln(stderr, "c4-plan:", err)
			return 1
		}
		var xs []plan.Extra
		if err := yaml.Unmarshal(b, &xs); err != nil {
			fmt.Fprintln(stderr, "c4-plan:", *extra+":", err)
			return 1
		}
		p.AddExtras(xs)
	}
	doc, err := p.Doc()
	if err != nil {
		fmt.Fprintln(stderr, "c4-plan:", err)
		return 1
	}
	b, err := plan.JSON(doc)
	if err != nil {
		fmt.Fprintln(stderr, "c4-plan:", err)
		return 1
	}
	if *out == "" {
		fmt.Fprintln(stdout, string(b))
	} else if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintln(stderr, "c4-plan:", err)
		return 1
	}
	nb, _ := json.MarshalIndent(p.Notes, "", "  ")
	if *notes != "" {
		if err := os.WriteFile(*notes, append(nb, '\n'), 0o644); err != nil {
			fmt.Fprintln(stderr, "c4-plan:", err)
			return 1
		}
	} else if len(p.Notes) > 0 {
		fmt.Fprintln(stderr, string(nb))
	}
	edges := 0
	for _, u := range doc.Units {
		edges += len(u.After)
	}
	if *out != "" {
		fmt.Fprintf(stdout, "%d blocks, %d edges, %d notes\n", len(doc.Units), edges, len(p.Notes))
		gs, n := p.Groups()
		for _, g := range gs {
			fmt.Fprintf(stdout, "  %-12s %d\n", g, n[g])
		}
	}
	return 0
}

// read reads PLAN through the adapter its format names.
func read(path, format, milestone string) (*plan.Plan, error) {
	if format == "" {
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		format = "file"
		if st.IsDir() {
			format = "lanemap"
		}
	}
	switch format {
	case "file":
		return plan.Read(path)
	case "lanemap":
		return lanemap.Import(path, milestone)
	}
	return nil, fmt.Errorf("--format %q: want file or lanemap", format)
}
