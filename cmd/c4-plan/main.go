// Command c4-plan turns a written development plan into a WorkJob's
// expansion document: a planned block per lane, the edges between them,
// and the sentence each edge came from (agent-jobs section 10).
//
//	c4-plan import [--milestone M1] [--extra EDGES.yaml] [-o OUT.json]
//	               [--notes NOTES.json] PLAN_DIR
//
// PLAN_DIR is a plan in the lane-map layout (LANES.md, the milestone's
// file, charters/). It is only read. EDGES.yaml lists edges a person adds
// where the plan states an order in words the importer does not read, each
// citing the plan:
//
//   - {block: C2.1, after: [F1], file: LANES.md, line: 193, text: "So no lane from C2.1 onward starts until the foundation lane is chartered and merged."}
//
// The importer prints a summary, and writes what it could not read (an
// ordering sentence with no pattern, an edge to a lane the plan does not
// list) to NOTES.json, or to stderr. A cycle in the edges is an error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4-operator/internal/plan"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "import" {
		fmt.Fprintln(stderr, "usage: c4-plan import [--milestone M1] [--extra EDGES.yaml] [-o OUT.json] [--notes NOTES.json] PLAN_DIR")
		return 2
	}
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	milestone := fs.String("milestone", "M1", "the milestone whose charters and file to read")
	extra := fs.String("extra", "", "a YAML list of edges a person adds, each citing the plan")
	out := fs.String("o", "", "write the expansion document here (default stdout)")
	notes := fs.String("notes", "", "write what the importer could not read here (default stderr)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "c4-plan import: one PLAN_DIR")
		return 2
	}
	p, err := plan.ImportLanes(fs.Arg(0), *milestone)
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
	b, err := doc.JSON()
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
	groups := map[string]int{}
	for _, u := range doc.Units {
		edges += len(u.After)
		groups[u.Group]++
	}
	gs := make([]string, 0, len(groups))
	for g := range groups {
		gs = append(gs, g)
	}
	sort.Strings(gs)
	if *out != "" {
		fmt.Fprintf(stdout, "%d blocks, %d edges, %d notes\n", len(doc.Units), edges, len(p.Notes))
		for _, g := range gs {
			fmt.Fprintf(stdout, "  %-12s %d\n", g, groups[g])
		}
	}
	return 0
}
