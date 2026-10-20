package schema

import (
	"io/fs"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type fixtureNames struct {
	Parties     map[string]string `json:"parties"`
	Records     map[string]string `json:"records"`
	Deployments map[string]string `json:"deployments"`
}

func (n fixtureNames) Party(s string) string      { return n.Parties[s] }
func (n fixtureNames) Record(s string) string     { return n.Records[s] }
func (n fixtureNames) Deployment(s string) string { return n.Deployments[s] }

// TestChainLines reads every passing chain aloud and compares it with its
// .line.txt, the words the HUD and c4 farm show.
func TestChainLines(t *testing.T) {
	b, err := fs.ReadFile(Fixtures(), "chain/names.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var names fixtureNames
	if err := yaml.UnmarshalStrict(b, &names); err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, c := range cases(t) {
		if c.Format != "chain" || !c.Pass {
			continue
		}
		want, err := fs.ReadFile(Fixtures(), "chain/pass/"+c.Name+".line.txt")
		if err != nil {
			t.Errorf("%s has no .line.txt", c.Name)
			continue
		}
		ch, errs := ParseChain(c.Doc)
		if errs != nil {
			t.Fatalf("%s: %+v", c.Name, errs)
		}
		if got := ch.Line(names); got != strings.TrimSpace(string(want)) {
			t.Errorf("%s:\n got  %s\n want %s", c.Name, got, strings.TrimSpace(string(want)))
		}
		read++
	}
	if read == 0 {
		t.Fatal("no chains read")
	}
	// Without names, a chain reads with its own.
	ch, _ := ParseChain([]byte(`{"act": "disable", "by": {"person": "maya", "grade": "direct"}, "for": {"party": "studio-x", "grade": "direct"}, "on": [{"deployment": "saltmarsh/component/hud", "owner": "avalanche"}], "accountable": true}`))
	if got, want := ch.Line(nil), "disable by maya, for studio-x, on hud (avalanche's)"; got != want {
		t.Errorf("no names:\n got  %s\n want %s", got, want)
	}
}
