package schema

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"sigs.k8s.io/yaml"
)

func resolve(t *testing.T, b []byte) *jsonschema.Resolved {
	t.Helper()
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// value reads a YAML document as the JSON value a schema validates.
func value(t *testing.T, doc []byte) any {
	t.Helper()
	j, err := yaml.YAMLToJSON(doc)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func cases(t *testing.T) []Case {
	t.Helper()
	cs, err := Cases()
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestFixtures(t *testing.T) {
	schemas := map[string]*jsonschema.Resolved{"submit": resolve(t, Submit()), "tool": resolve(t, Tool())}
	counts := map[string]int{}
	for _, c := range cases(t) {
		t.Run(c.Format+"/"+map[bool]string{true: "pass", false: "fail"}[c.Pass]+"/"+c.Name, func(t *testing.T) {
			err := schemas[c.Format].Validate(value(t, c.Doc))
			switch {
			case c.Pass:
				counts[c.Format+" pass"]++
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
			case c.Expect.By == "schema":
				counts[c.Format+" fail by schema"]++
				if err == nil {
					t.Fatal("accepted; the schema should refuse it")
				}
				if !strings.Contains(err.Error(), c.Expect.SchemaMentions) {
					t.Fatalf("refused for another reason: %v (want it to mention %q)", err, c.Expect.SchemaMentions)
				}
			case c.Expect.By == "check":
				counts[c.Format+" fail by check"]++
				if err != nil {
					t.Fatalf("the schema refuses a case meant for the farm's check: %v", err)
				}
			default:
				t.Fatalf("by is %q, want schema or check", c.Expect.By)
			}
			if c.Pass {
				return
			}
			if len(c.Expect.Errors) == 0 {
				t.Fatal("no errors expected")
			}
			for _, e := range c.Expect.Errors {
				if e.Path == "" || e.Message == "" || e.Fix == "" {
					t.Fatalf("an expected error needs a path, a message and a fix: %+v", e)
				}
			}
		})
	}
	t.Logf("%v", counts)
}

// farm is fixtures/farm.yaml.
type farm struct {
	Projects map[string]struct {
		Milestones map[string]string `json:"milestones"`
		Jobs       []string          `json:"jobs"`
	} `json:"projects"`
	People  []string          `json:"people"`
	Roles   []string          `json:"roles"`
	Parties []string          `json:"parties"`
	Tools   map[string]string `json:"tools"`
}

// TestFarmContext keeps the fixtures whole: every tool farm.yaml publishes
// has a passing fixture of that name, and every tool a passing submission
// names is published.
func TestFarmContext(t *testing.T) {
	b, err := fs.ReadFile(Fixtures(), "farm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var f farm
	if err := yaml.UnmarshalStrict(b, &f); err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	for _, c := range cases(t) {
		if c.Format == "tool" && c.Pass {
			var d struct {
				Name string `json:"name"`
			}
			if err := yaml.Unmarshal(c.Doc, &d); err != nil {
				t.Fatal(err)
			}
			if c.Name != "glance" && d.Name != c.Name {
				t.Errorf("tool/pass/%s defines %q; a tool's folder is its name", c.Name, d.Name)
			}
			defined[d.Name] = true
		}
	}
	for name := range f.Tools {
		if !defined[name] {
			t.Errorf("farm.yaml publishes %q, which no tool/pass fixture defines", name)
		}
	}
	for _, c := range cases(t) {
		if c.Format != "submit" || !c.Pass {
			continue
		}
		var d struct {
			Jobs map[string]struct {
				Tool    string `json:"tool"`
				Check   *struct{ Tool string } `json:"check"`
				Each    *struct{ Tool string } `json:"each"`
				Uses    map[string]any `json:"uses"`
				Answers *struct{ By string } `json:"answers"`
			} `json:"jobs"`
		}
		if err := yaml.Unmarshal(c.Doc, &d); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, j := range d.Jobs {
			names = append(names, j.Tool)
			if j.Check != nil {
				names = append(names, j.Check.Tool)
			}
			if j.Each != nil {
				names = append(names, j.Each.Tool)
			}
			for u := range j.Uses {
				names = append(names, u)
			}
			if j.Answers != nil && strings.HasPrefix(j.Answers.By, "tool:") {
				names = append(names, strings.TrimPrefix(j.Answers.By, "tool:"))
			}
		}
		for _, n := range names {
			if n = strings.SplitN(n, "@", 2)[0]; n != "" && f.Tools[n] == "" {
				t.Errorf("submit/pass/%s names tool %q, which farm.yaml does not publish", c.Name, n)
			}
		}
	}
}
