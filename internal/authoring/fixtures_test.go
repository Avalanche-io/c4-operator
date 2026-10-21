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
		Shots      map[string]struct {
			Cut string `json:"cut"`
		} `json:"shots"`
		Jobs []string `json:"jobs"`
	} `json:"projects"`
	PeopleL  []string               `json:"people"`
	RolesL   []string               `json:"roles"`
	PartiesL []string               `json:"parties"`
	ToolsV   map[string]string      `json:"tools"`
	Policies map[string]*FarmPolicy `json:"policies"`
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

func (f *fixtureFarm) People() []string   { return f.PeopleL }
func (f *fixtureFarm) Roles() []string    { return f.RolesL }
func (f *fixtureFarm) Parties() []string  { return f.PartiesL }
func (f *fixtureFarm) Root(string) string { return "/show" }
func (f *fixtureFarm) Namespace(p string) string {
	if _, ok := f.Projects[p]; !ok {
		return ""
	}
	return p
}
func (f *fixtureFarm) Cut(p, shot string) string   { return f.Projects[p].Shots[shot].Cut }
func (f *fixtureFarm) Policy(p string) *FarmPolicy { return f.Policies[p] }
func (f *fixtureFarm) Trackers(string) []string    { return nil }

// The fixtures' milestones are on New York time.
func (f *fixtureFarm) Location(string) *time.Location {
	loc, _ := time.LoadLocation("America/New_York")
	return loc
}

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
		if c.Format == "chain" { // the accountability chain is not a lowering input
			continue
		}
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
		if tl.Test == nil || (tl.Kind != "frames" && tl.Kind != "once") || len(ToolRefusals(tl)) > 0 {
			continue
		}
		_, ref, _ := farm.Tool("saltmarsh", name, "")
		req, errs := LowerTest(tl, ref, "c4testtree", "saltmarsh", "saltmarsh", "/show")
		if len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
			continue
		}
		if req.Name != TestJobName(name, ref) || len(req.Inputs) != 2 || req.Inputs[0].MountPath != ToolsMount ||
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

// A test job's name tells two trees of the same version apart (a farm
// tool's v1 and a project's), and stays within a WorkJob's 50 characters
// however long the tool's name, two long names still apart.
func TestTestJobName(t *testing.T) {
	a := TestJobName("proxies", ToolRef{Version: "v1", View: "c41AbCdEfGhJk"})
	b := TestJobName("proxies", ToolRef{Version: "v1", View: "c42ZyXwVuTsRq"})
	if a != "proxies-test-1abcdefghj" || a == b {
		t.Fatalf("%q %q", a, b)
	}
	if n := TestJobName("proxies", ToolRef{Version: "v3"}); n != "proxies-test-v3" {
		t.Fatalf("no view: %q", n)
	}
	long := strings.Repeat("usd-passes-with-a-name-far-too-long", 2)
	l1 := TestJobName(long+"-a", ToolRef{Version: "v1"})
	l2 := TestJobName(long+"-b", ToolRef{Version: "v1"})
	if len(l1) > MaxJobName || len(l2) > MaxJobName || l1 == l2 {
		t.Fatalf("%q (%d) %q (%d)", l1, len(l1), l2, len(l2))
	}
}

// The submissions' failing check fixtures are refused by Check with
// exactly their errors, and the passing submissions are not.
func TestCheckFixtures(t *testing.T) {
	farm := loadFarm(t)
	n := 0
	for _, c := range cases(t) {
		if c.Format != "submit" || !c.Pass && c.Expect.By != "check" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			sub, errs := ParseSubmission(c.Doc)
			if len(errs) > 0 {
				t.Fatalf("parse: %v", errs)
			}
			errs = Check(sub, farm)
			if c.Pass {
				if len(errs) > 0 {
					t.Fatalf("refused: %v", errs)
				}
				return
			}
			if !reflect.DeepEqual(errs, c.Expect.Errors) {
				t.Fatalf("\n got  %v\n want %v", errs, c.Expect.Errors)
			}
			n++
		})
	}
	t.Logf("%d check fixtures refused exactly", n)
}

// Every passing submission lowers exactly as slice1.yaml says: refused
// with its errors when listed, else to one request per job.
func TestLowerFixtures(t *testing.T) {
	farm := loadFarm(t)
	b, err := fs.ReadFile(schema.Fixtures(), "submit/slice1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Refusals map[string][]Error `json:"refusals"`
	}
	if err := yaml.UnmarshalStrict(b, &want); err != nil {
		t.Fatal(err)
	}
	refused, ran, errs := 0, 0, 0
	for _, c := range cases(t) {
		if c.Format != "submit" || !c.Pass {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			sub, perrs := ParseSubmission(c.Doc)
			if len(perrs) > 0 {
				t.Fatalf("parse: %v", perrs)
			}
			plan, lerrs := Lower(sub, farm)
			w, listed := want.Refusals[c.Name]
			switch {
			case listed && !reflect.DeepEqual(lerrs, w):
				t.Fatalf("\n got  %v\n want %v", lerrs, w)
			case listed:
				refused++
				errs += len(lerrs)
			case len(lerrs) > 0:
				t.Fatalf("refused: %v", lerrs)
			case plan == nil || len(plan.Jobs) != len(sub.Jobs):
				t.Fatalf("plan %+v", plan)
			default:
				ran++
			}
		})
	}
	if refused != len(want.Refusals) {
		t.Errorf("%d submissions refused, slice1.yaml lists %d", refused, len(want.Refusals))
	}
	t.Logf("%d refused with %d errors, %d lowered", refused, errs, ran)
}
