// Package schema holds the two formats of the authoring layers, the
// submission (c4: submit/1) and the tool definition (c4: tool/1), as JSON
// Schema 2020-12, and the fixtures every checker of them must agree with.
//
// The schemas check a document's shape. The farm's own check (POST
// /submissions, POST /tools) adds what needs the farm: that an after names a
// job, that a tool exists and takes the parameters given, that a format
// string names a parameter. Each failing fixture says which of the two
// refuses it, and the exact errors the farm reports: path, message and fix.
//
// Every worked example in the formats' design is a passing fixture here.
package schema

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
)

//go:embed submit-1.schema.json
var submit []byte

//go:embed tool-1.schema.json
var tool []byte

//go:embed fixtures
var fixtures embed.FS

// SubmitSchema is the JSON Schema of a submission, as GET /schema/submit
// serves it.
func SubmitSchema() []byte { return slices.Clone(submit) }

// ToolSchema is the JSON Schema of a tool definition, as GET /schema/tool
// serves it.
func ToolSchema() []byte { return slices.Clone(tool) }

// Fixtures is the fixture tree: farm.yaml (the farm the checks run against),
// submit/pass, submit/fail, tool/pass (one folder per tool) and tool/fail.
func Fixtures() fs.FS {
	f, err := fs.Sub(fixtures, "fixtures")
	if err != nil {
		panic(err) // the directory is embedded, so this cannot happen
	}
	return f
}

// Expect is what a failing fixture must be refused with.
type Expect struct {
	// By is "schema" when the shape alone refuses the document, and "check"
	// when only the farm's check can (the shape is valid).
	By string `json:"by"`
	// SchemaMentions is a token the schema validator's error must contain,
	// for By "schema": proof it was refused for the right reason.
	SchemaMentions string `json:"schemaMentions,omitempty"`
	// Note explains a choice the errors embody.
	Note string `json:"note,omitempty"`
	// Errors are the errors the farm reports, in order.
	Errors []Error `json:"errors"`
}

// Error is one refusal as the farm reports it. Path is dotted, with list
// indexes in brackets: jobs.comp.after, params[1].type.
type Error struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

// Case is one fixture document.
type Case struct {
	Format string // "submit", "tool" or "chain"
	Name   string // the file's name without .yaml, or the tool's folder
	Pass   bool
	Doc    []byte  // YAML, as written
	Expect *Expect // for failing cases
}

// Cases lists every fixture document, sorted by format, then pass before
// fail, then name. Plan files beside the submissions are not cases.
func Cases() ([]Case, error) {
	fsys := Fixtures()
	var out []Case
	for _, format := range []string{"submit", "tool", "chain"} {
		for _, pass := range []bool{true, false} {
			dir := path.Join(format, map[bool]string{true: "pass", false: "fail"}[pass])
			entries, err := fs.ReadDir(fsys, dir)
			if err != nil {
				return nil, err
			}
			var names []string
			for _, e := range entries {
				n := e.Name()
				switch {
				case format == "tool" && pass && e.IsDir():
					names = append(names, n)
				case !e.IsDir() && strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".plan.yaml"):
					names = append(names, strings.TrimSuffix(n, ".yaml"))
				case !e.IsDir() && strings.HasSuffix(n, ".json") && !strings.HasSuffix(n, ".expect.json"):
					names = append(names, strings.TrimSuffix(n, ".json")+".json")
				}
			}
			sort.Strings(names)
			for _, n := range names {
				file := path.Join(dir, n+".yaml")
				if strings.HasSuffix(n, ".json") {
					file = path.Join(dir, n)
					n = strings.TrimSuffix(n, ".json")
				}
				if format == "tool" && pass {
					file = path.Join(dir, n, "tool.yaml")
				}
				doc, err := fs.ReadFile(fsys, file)
				if err != nil {
					return nil, err
				}
				c := Case{Format: format, Name: n, Pass: pass, Doc: doc}
				if !pass {
					b, err := fs.ReadFile(fsys, path.Join(dir, n+".expect.json"))
					if err != nil {
						return nil, fmt.Errorf("%s has no expect file: %w", file, err)
					}
					c.Expect = new(Expect)
					if err := json.Unmarshal(b, c.Expect); err != nil {
						return nil, fmt.Errorf("%s.expect.json: %w", path.Join(dir, n), err)
					}
				}
				out = append(out, c)
			}
		}
	}
	return out, nil
}
