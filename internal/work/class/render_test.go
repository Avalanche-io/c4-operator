package class

import (
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

func envOf(u Unit, name string) (string, bool) {
	for _, e := range u.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// A unit over stepped frames says the step, so it renders 1001, 1003 and
// 1005 rather than every frame from 1001 to 1005; a unit over every frame
// or one frame says none, and one over irregular frames names them.
func TestRenderFrameStep(t *testing.T) {
	render, err := Lookup("render")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		frames, chunk string
		step          string // "" for none
		first         string
	}{
		{"1001-1012x2", "3", "2", "1001-1005"},
		{"1001-1012", "3", "", "1001-1003"},
		{"1001-1012x2", "1", "", "1001"},
		{"1001,1003,1010", "3", "", "1001,1003,1010"},
		{"1001,1050-1052", "4", "", "1001,1050-1052"},
	} {
		job := &c4v1.WorkJob{Spec: c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": c.frames, "chunk": c.chunk}}}
		p, err := render.Plan(job)
		if err != nil {
			t.Fatal(err)
		}
		u := p.Units[0]
		step, ok := envOf(u, "C4_FRAME_STEP")
		if step != c.step || ok != (c.step != "") {
			t.Errorf("%s by %s: C4_FRAME_STEP %q %v, want %q", c.frames, c.chunk, step, ok, c.step)
		}
		if got, _ := envOf(u, "C4_FRAMES"); got != c.first {
			t.Errorf("%s by %s: C4_FRAMES %q, want %q", c.frames, c.chunk, got, c.first)
		}
	}
}
