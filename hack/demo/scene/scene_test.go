// Package scene holds the dailies demo's render script (render.sh); this
// test keeps its expansion document valid under the queue's own, strict
// reading, so the demo cannot drift from the operator.
package scene

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

func TestPlanExpandsAsTheQueueReadsIt(t *testing.T) {
	out := t.TempDir()
	cmd := exec.Command("sh", "render.sh")
	cmd.Env = append(os.Environ(), "C4_OUTPUT="+out, "C4_UNIT_NAME=plan")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render.sh plan: %v\n%s", err, b)
	}
	doc, err := os.ReadFile(filepath.Join(out, ".c4", "expand.json"))
	if err != nil {
		t.Fatal(err)
	}
	job := &c4v1.WorkJob{Spec: c4v1.WorkJobSpec{Class: "graph", Params: map[string]string{"root": "plan"}}}
	gen := class.Unit{Index: 0, Name: "plan", Generator: true}
	x, err := class.ExpandDocument(job, gen, doc, map[string]int{"plan": 0}, 1)
	if err != nil {
		t.Fatalf("the queue refuses the demo's expansion: %v", err)
	}
	if len(x.Units) != 4 {
		t.Fatalf("units: %d, want three frame chunks and the dailies wait", len(x.Units))
	}
	w := x.Units[3]
	if w.Name != "dailies" || w.Wait == nil || len(w.After) != 3 {
		t.Fatalf("the dailies wait: %+v", w)
	}
}
