// Command c4-plan turns a plan into a WorkJob's expansion document
// (expansion v1, the queue's own format): a block per piece of work, the
// edges between them, and where each edge came from (agent-jobs section 10).
//
//	c4-plan import [--format file|lanemap] [--milestone M1] [--extra EXTRA.yaml]
//	               [-o OUT.json] [--notes NOTES.json] [--owners OWNERS.json] PLAN
//	c4-plan edges  --since PREV.json [--format …] [--milestone …] [--extra …]
//	               [-o OUT.json] [--notes NOTES.json] PLAN
//
// PLAN is read through an adapter, and only read:
//
//   - a file (format "file"): a plan written as data, a YAML or JSON list of
//     blocks (internal/plan.File). This is the format to keep a plan in;
//   - a directory (format "lanemap"): a plan written as prose in the
//     lane-map layout (LANES.md, the milestone's file, charters/), read by
//     sentence patterns.
//
// EXTRA.yaml lists edges and waits a person adds where a plan states an
// order in words no adapter reads, each citing the plan:
//
//   - {block: C2.1, after: [F1], file: LANES.md, line: 193, text: "So no lane from C2.1 onward starts until the foundation lane is chartered and merged."}
//
// import writes the whole document. --owners also writes the paths blocks
// hold, each to its block, for an edge source that names a marker's waiting
// unit by its file (c4-edges -owners).
//
// edges is for a job already running the plan: re-run after a merge, it
// writes a document of edges alone, the orderings between the job's blocks
// that PREV (the document the job was created from, then each edges
// document since, in order: --since may repeat) does not have. A block
// the job does not have, and an ordering the plan dropped, are notes.
//
// Both print a summary, and write what they could not read (an ordering
// sentence with no pattern, an edge to a block the plan does not list) to
// NOTES.json, or to stderr: their count is how much of the plan is still
// only prose. A cycle in the edges is an error, and so is a document the
// queue would refuse.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4-operator/internal/plan"
	"github.com/Avalanche-io/c4-operator/internal/plan/lanemap"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

const usage = `usage: c4-plan import [--format file|lanemap] [--milestone M1] [--extra EXTRA.yaml] [-o OUT.json] [--notes NOTES.json] [--owners OWNERS.json] PLAN
       c4-plan edges --since PREV.json [--format file|lanemap] [--milestone M1] [--extra EXTRA.yaml] [-o OUT.json] [--notes NOTES.json] PLAN`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// sinces is a repeatable --since.
type sinces []string

func (s *sinces) String() string     { return strings.Join(*s, ",") }
func (s *sinces) Set(v string) error { *s = append(*s, v); return nil }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "import" && args[0] != "edges") {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "", "file (a plan written as data) or lanemap (a lane-map directory); default: by whether PLAN is a file or a directory")
	milestone := fs.String("milestone", "M1", "lanemap: the milestone whose charters and file to read")
	extra := fs.String("extra", "", "a YAML list of edges and waits a person adds, each citing the plan")
	extraBy := fs.String("extra-by", "", "who read the --extra entries from the plan's prose, for those that do not say (their citations' by)")
	out := fs.String("o", "", "write the document here (default stdout)")
	notes := fs.String("notes", "", "write what could not be read here (default stderr)")
	owners := fs.String("owners", "", "import: also write the paths blocks hold, each to its block (c4-edges -owners)")
	var since sinces
	fs.Var(&since, "since", "edges: the job's document, then each edges document since (repeat, in order)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "c4-plan %s: one PLAN\n", cmd)
		return 2
	}
	if cmd == "edges" && len(since) == 0 {
		fmt.Fprintln(stderr, "c4-plan edges: --since the job's document")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "c4-plan:", err)
		return 1
	}
	p, err := read(fs.Arg(0), *format, *milestone)
	if err != nil {
		return fail(err)
	}
	if *extra != "" {
		b, err := os.ReadFile(*extra)
		if err != nil {
			return fail(err)
		}
		var xs []plan.Extra
		if err := yaml.Unmarshal(b, &xs); err != nil {
			return fail(fmt.Errorf("%s: %w", *extra, err))
		}
		for i := range xs {
			if xs[i].By == "" {
				xs[i].By = *extraBy
			}
		}
		p.AddExtras(xs)
	}
	var doc *class.Doc
	allNotes := p.Notes
	if cmd == "edges" {
		prev, err := readDocs(since)
		if err != nil {
			return fail(err)
		}
		d, ns, err := p.EdgesSince(prev)
		if err != nil {
			return fail(err)
		}
		doc, allNotes = d, append(allNotes, ns...)
	} else if doc, err = p.Doc(); err != nil {
		return fail(err)
	}
	b, err := plan.JSON(doc)
	if err != nil {
		return fail(err)
	}
	if *out == "" {
		fmt.Fprintln(stdout, string(b))
	} else if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		return fail(err)
	}
	if *owners != "" {
		o, ns := p.Owners()
		allNotes = append(allNotes, ns...)
		ob, _ := json.MarshalIndent(o, "", "  ")
		if err := os.WriteFile(*owners, append(ob, '\n'), 0o644); err != nil {
			return fail(err)
		}
	}
	nb, _ := json.MarshalIndent(allNotes, "", "  ")
	if *notes != "" {
		if err := os.WriteFile(*notes, append(nb, '\n'), 0o644); err != nil {
			return fail(err)
		}
	} else if len(allNotes) > 0 {
		fmt.Fprintln(stderr, string(nb))
	}
	if *out == "" {
		return 0
	}
	if cmd == "edges" {
		fmt.Fprintf(stdout, "%d new edges, %d notes\n", len(doc.Edges), len(allNotes))
		return 0
	}
	edges := 0
	for _, u := range doc.Units {
		edges += len(u.After)
	}
	fmt.Fprintf(stdout, "%d blocks, %d edges, %d notes\n", len(doc.Units), edges, len(allNotes))
	gs, n := p.Groups()
	for _, g := range gs {
		fmt.Fprintf(stdout, "  %-12s %d\n", g, n[g])
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

// readDocs reads the job's document and folds in each edges document
// after it.
func readDocs(paths []string) (*class.Doc, error) {
	var prev *class.Doc
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var d class.Doc
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if prev == nil {
			prev = &d
			continue
		}
		prev.Units = append(prev.Units, d.Units...)
		plan.Fold(prev, &d)
	}
	return prev, nil
}
