package authoring

import (
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4-operator/schema"
)

// fixtureFarm is schema/fixtures/farm.yaml with the tools under tool/pass.
type fixtureFarm struct {
	Projects map[string]struct {
		Milestones map[string]string `json:"milestones"`
		Jobs       []string          `json:"jobs"`
	} `json:"projects"`
	PeopleL  []string          `json:"people"`
	RolesL   []string          `json:"roles"`
	PartiesL []string          `json:"parties"`
	ToolsV   map[string]string `json:"tools"`
	tools    map[string]*Tool
}

func loadFarm(t *testing.T) *fixtureFarm {
	t.Helper()
	b, err := fs.ReadFile(schema.Fixtures(), "farm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var f fixtureFarm
	if err := yaml.UnmarshalStrict(b, &f); err != nil {
		t.Fatal(err)
	}
	f.tools = map[string]*Tool{}
	for name := range f.ToolsV {
		b, err := fs.ReadFile(schema.Fixtures(), path.Join("tool/pass", name, "tool.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		tl, errs := ParseTool(b)
		if len(errs) > 0 {
			t.Fatalf("tool %s: %v", name, errs)
		}
		f.tools[name] = tl
	}
	return &f
}

func (f *fixtureFarm) Tools(project string) []ToolRef {
	var out []ToolRef
	for _, n := range sortedKeys(f.ToolsV) {
		out = append(out, ToolRef{Name: n, Version: f.ToolsV[n], View: "c4view" + n, Image: "img/" + n + "@sha256:0", Scope: "project"})
	}
	return out
}

func (f *fixtureFarm) Tool(project, name, version string) (*Tool, ToolRef, error) {
	v, ok := f.ToolsV[name]
	if !ok || version != "" && version != v {
		return nil, ToolRef{}, fmt.Errorf("%s@%s: %w", name, version, ErrNoTool)
	}
	return f.tools[name], ToolRef{Name: name, Version: v, View: "c4view" + name, Image: "img/" + name + "@sha256:0", Scope: "project"}, nil
}

func (f *fixtureFarm) Jobs(ns string) []string { return f.Projects[ns].Jobs }

func (f *fixtureFarm) Milestones(ns string) map[string]time.Time {
	out := map[string]time.Time{}
	for k, v := range f.Projects[ns].Milestones {
		t, _ := time.Parse(time.RFC3339, v)
		out[k] = t
	}
	return out
}

func (f *fixtureFarm) People() []string          { return f.PeopleL }
func (f *fixtureFarm) Roles() []string           { return f.RolesL }
func (f *fixtureFarm) Parties() []string         { return f.PartiesL }
func (f *fixtureFarm) Root(string) string        { return "/show" }
func (f *fixtureFarm) Namespace(p string) string { return p }

func cases(t *testing.T) []schema.Case {
	t.Helper()
	cs, err := schema.Cases()
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// Every fixture parses as it should: the passing ones without an error,
// the failing ones the schema refuses with exactly their errors.
func TestParseFixtures(t *testing.T) {
	for _, c := range cases(t) {
		t.Run(c.Format+"/"+c.Name, func(t *testing.T) {
			var errs []Error
			if c.Format == "submit" {
				_, errs = ParseSubmission(c.Doc)
			} else {
				_, errs = ParseTool(c.Doc)
			}
			switch {
			case c.Pass && len(errs) > 0:
				t.Fatalf("refused: %v", errs)
			case !c.Pass && c.Expect.By == "schema" && !reflect.DeepEqual(errs, c.Expect.Errors):
				t.Fatalf("\n got  %v\n want %v", errs, c.Expect.Errors)
			case !c.Pass && c.Expect.By == "check" && len(errs) > 0:
				t.Fatalf("refused before the check: %v", errs)
			}
		})
	}
}

// The tools' failing check fixtures are refused by CheckTool with exactly
// their errors, and the passing tools are not.
func TestCheckToolFixtures(t *testing.T) {
	farm := loadFarm(t)
	for _, c := range cases(t) {
		if c.Format != "tool" || !c.Pass && c.Expect.By != "check" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			tl, errs := ParseTool(c.Doc)
			if len(errs) > 0 {
				t.Fatalf("parse: %v", errs)
			}
			errs = CheckTool(tl, "saltmarsh", farm)
			if c.Pass {
				if len(errs) > 0 {
					t.Fatalf("refused: %v", errs)
				}
				return
			}
			if !reflect.DeepEqual(errs, c.Expect.Errors) {
				t.Fatalf("\n got  %v\n want %v", errs, c.Expect.Errors)
			}
		})
	}
}

// A tool's test lowers to one job: the runner in the pinned image, the
// tool's tree read-only, the test tree writable at the root, the
// parameters resolved.
func TestLowerTest(t *testing.T) {
	farm := loadFarm(t)
	lowered := 0
	for name, tl := range farm.tools {
		if tl.Test == nil || (tl.Kind != "frames" && tl.Kind != "once") || len(refusals(tl)) > 0 {
			continue
		}
		_, ref, _ := farm.Tool("saltmarsh", name, "")
		req, err := LowerTest(tl, ref, "c4testtree", "saltmarsh", "saltmarsh", "/show")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if req.Name != name+"-test-"+ref.Version || len(req.Inputs) != 2 || req.Inputs[0].MountPath != ToolsMount ||
			!req.Inputs[0].ReadOnly || req.Inputs[1].View != "c4testtree" || req.Inputs[1].MountPath != "/show" || req.Inputs[1].ReadOnly {
			t.Errorf("%s: %+v", name, req)
		}
		s := string(req.Spec)
		lowered++
		if !strings.Contains(s, `"command":["c4-unit","run"]`) || !strings.Contains(s, ref.Image) || !strings.Contains(s, `"name":"C4_PARAMS"`) {
			t.Errorf("%s spec %s", name, s)
		}
	}
	if lowered == 0 {
		t.Fatal("no tool's test was lowered")
	}
	t.Logf("%d tools' tests lowered", lowered)
}
