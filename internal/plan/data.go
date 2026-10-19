package plan

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// File is a plan written as data: the format a project keeps when its plan
// is a file rather than prose (YAML or JSON). It is expansion v1 with
// defaults a person would otherwise repeat: every block is planned (a
// generator), exists for certain and takes one slot, unless it says
// otherwise or is a wait.
//
//	version: 1
//	blocks:
//	  - name: text/scan
//	    group: leaves
//	    key: module
//	    holds: [text/scan]
//	    params: {size: "14"}
//	  - name: text/format
//	    group: leaves
//	    after: [text/scan]
//	    why: {text/scan: "format calls scan's tokenizer"}
//	  - name: api-break
//	    wait: {server: "person:owner", ask: "keep the old signature?", expect: 7200}
//	    before: [text/format]
type File struct {
	Version int         `json:"version"`
	Blocks  []FileBlock `json:"blocks"`
}

// FileBlock is one block of a plan file: a DocUnit with Before, Why and
// defaults.
type FileBlock struct {
	Name    string            `json:"name"`
	Label   string            `json:"label,omitempty"`
	Key     string            `json:"key,omitempty"`
	Group   string            `json:"group,omitempty"`
	After   []string          `json:"after,omitempty"`
	Work    float64           `json:"work,omitempty"`
	WorkSD  float64           `json:"workSD,omitempty"`
	Exists  *float64          `json:"exists,omitempty"`
	Planned *bool             `json:"planned,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Inputs  []class.DocInput  `json:"inputs,omitempty"`
	Slots   int               `json:"slots,omitempty"`
	Holds   []string          `json:"holds,omitempty"`
	Claims  []class.DocClaim  `json:"claims,omitempty"`
	Gate    string            `json:"gate,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	Wait    *class.DocWait    `json:"wait,omitempty"`
	// Before lists blocks that run after this one: the same edges as their
	// After, written where the reason is (a wait before what it holds up).
	Before []string `json:"before,omitempty"`
	// Why gives the reason for an edge, by the block it runs after or
	// before; it becomes the edge's citation.
	Why map[string]string `json:"why,omitempty"`
}

// Read reads a plan file. A block named twice is an error; an edge to a
// block the file does not list is a Note, as in any adapter.
func Read(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.UnmarshalStrict(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%s: plan version %d, want 1", path, f.Version)
	}
	return FromFile(&f, filepath.Base(path))
}

// FromFile builds a plan from a plan file's contents; file names it in the
// citations.
func FromFile(f *File, file string) (*Plan, error) {
	p := New()
	for i, fb := range f.Blocks {
		if fb.Name == "" {
			return nil, fmt.Errorf("%s: block %d has no name", file, i+1)
		}
		if p.Unit(fb.Name) != nil {
			return nil, fmt.Errorf("%s: block %s is listed twice", file, fb.Name)
		}
		u := p.Block(fb.Name, fb.Group)
		if fb.Label != "" {
			u.Label = fb.Label
		}
		if fb.Key != "" {
			u.Key = fb.Key
		}
		if fb.Exists != nil {
			u.Exists = *fb.Exists
		}
		if fb.Slots > 0 {
			u.Slots = fb.Slots
		}
		u.Work, u.WorkSD, u.Env, u.Inputs, u.Holds, u.Claims, u.Gate = fb.Work, fb.WorkSD, fb.Env, fb.Inputs, fb.Holds, fb.Claims, fb.Gate
		for k, v := range fb.Params {
			u.Params[k] = v
		}
		if fb.Wait != nil {
			u.Wait, u.Planned = fb.Wait, false
			if fb.Key == "" {
				u.Key = "wait"
			}
			if fb.Label == "" && fb.Wait.Ask != "" {
				u.Label = fb.Name + ": " + fb.Wait.Ask
			}
		}
		if fb.Planned != nil {
			u.Planned = *fb.Planned
		}
	}
	cite := func(fb FileBlock, other string) Cite {
		t := fb.Why[other]
		if t == "" {
			t = "listed in the plan file"
		}
		return Cite{File: file, Line: 0, Text: t}
	}
	for _, fb := range f.Blocks {
		for _, d := range fb.After {
			if p.Unit(d) == nil {
				p.Note(file, 0, fb.Name+" after "+d, "names "+d+", which the plan does not list")
				continue
			}
			p.Edge(fb.Name, d, cite(fb, d))
		}
		for _, b := range fb.Before {
			if p.Unit(b) == nil {
				p.Note(file, 0, fb.Name+" before "+b, "names "+b+", which the plan does not list")
				continue
			}
			p.Edge(b, fb.Name, cite(fb, b))
		}
		// a wait with no row of its own sits in the row of the first block
		// it holds up, as a person's added wait does (AddExtras)
		if u := p.Unit(fb.Name); fb.Wait != nil && u.Group == "" {
			for _, b := range fb.Before {
				if v := p.Unit(b); v != nil {
					u.Group = v.Group
					break
				}
			}
		}
	}
	return p, nil
}
