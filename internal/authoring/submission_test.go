package authoring

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/schema"
)

func lowerDoc(t *testing.T, doc string) (*Plan, []Error) {
	t.Helper()
	sub, errs := ParseSubmission([]byte(doc))
	if len(errs) > 0 {
		t.Fatalf("parse: %v", errs)
	}
	return Lower(sub, loadFarm(t))
}

func mustLower(t *testing.T, doc string) *Plan {
	t.Helper()
	p, errs := lowerDoc(t, doc)
	if len(errs) > 0 {
		t.Fatalf("lower: %v", errs)
	}
	return p
}

func specOf(t *testing.T, j PlannedJob) c4v1.WorkJobSpec {
	t.Helper()
	var s c4v1.WorkJobSpec
	if err := json.Unmarshal(j.Request.Spec, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func envVal(s c4v1.WorkJobSpec, name string) (string, bool) {
	for _, e := range s.Template.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// proxies-movie lowers to two jobs named for the whole submission: the
// movie follows the proxies on their output, fills its range from its
// frames as one unit, and both join the milestone and say their reuse.
func TestLowerProxiesMovie(t *testing.T) {
	b, err := fs.ReadFile(schema.Fixtures(), "submit/pass/proxies-movie.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p := mustLower(t, string(b))
	if p.Namespace != "saltmarsh" || len(p.Jobs) != 2 {
		t.Fatalf("plan %+v", p)
	}
	px, mv := p.Jobs[0], p.Jobs[1]
	if px.Request.Name != "sm0140-proxies-v001" || mv.Request.Name != "sm0140-movie-v001" || px.Request.Stage != "proxies" || mv.Request.Type != "transcode" {
		t.Fatalf("names %s %s", px.Request.Name, mv.Request.Name)
	}
	ps, ms := specOf(t, px), specOf(t, mv)
	if ps.Params["frames"] != "1001-1048" || ps.Params["chunk"] != "8" || ps.Params["order"] != "binary" || px.Units != 6 {
		t.Fatalf("proxies params %v units %d", ps.Params, px.Units)
	}
	if ms.Params["frames"] != "1" || mv.Units != 1 {
		t.Fatalf("movie params %v units %d", ms.Params, mv.Units)
	}
	params, _ := envVal(ms, "C4_PARAMS")
	if !strings.Contains(params, `"range":"1001-1048"`) || !strings.Contains(params, `"plates":"/show/plates/sm0140.$F4.exr"`) {
		t.Fatalf("movie C4_PARAMS %s", params)
	}
	in := mv.Request.Inputs
	if len(in) != 2 || in[1].FromJob != "sm0140-proxies-v001" || in[1].MountPath != "/show" || in[1].ReadOnly ||
		len(mv.After) != 1 || mv.After[0] != "sm0140-proxies-v001" {
		t.Fatalf("movie inputs %+v after %v", in, mv.After)
	}
	if px.Request.Inputs[1].View != "." {
		t.Fatalf("a folder tree is left for the front end: %+v", px.Request.Inputs)
	}
	for _, j := range p.Jobs {
		if j.Labels[c4v1.LabelMilestone] != "dailies" || j.Request.Due != "" || j.Annotations["c4.avalanche.io/derivation-class"] != "exact" {
			t.Fatalf("%s labels %v annotations %v due %q", j.Key, j.Labels, j.Annotations, j.Request.Due)
		}
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "frame by frame") {
		t.Fatalf("warnings %v", p.Warnings)
	}
	if w := mv.What(); w != "1 unit over 1001-1048, after sm0140-proxies-v001" {
		t.Fatalf("what %q", w)
	}
	x := Explain(p)
	for _, want := range []string{
		"sm0140-movie-v001  WorkJob saltmarsh/sm0140-movie-v001",
		"class render · params {frames: 1}",
		"inputs   fromJob sm0140-proxies-v001 at /show (writable)   dependsOn [sm0140-proxies-v001]",
		"command c4-unit run · tool transcode@v1",
		"units    48 frames in 6 units",
		"deadline milestone dailies (label c4.avalanche.io/milestone=dailies)",
		"note: movie follows proxies frame by frame; for now it waits for the whole of sm0140-proxies-v001",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("explain lacks %q:\n%s", want, x)
		}
	}
}

// The forms a submission's fields take, each lowered: a version past the
// jobs on the farm, stepped frames, frames from the shot's cut, a job's
// output as the tree, a read tree, a pinned time in the project's zone, a
// date, the submitter's root, and needs limited.
func TestLowerForms(t *testing.T) {
	defer func(f func() time.Time) { now = f }(now)
	ny, _ := time.LoadLocation("America/New_York")
	now = func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, ny) } // a Tuesday
	lut := c4.Identify(strings.NewReader("lut")).String()

	p := mustLower(t, `c4: submit/1
project: saltmarsh
shot: sm0140
due: fri 18:00
tree: job:sm0140-light
read: {lut: `+lut+`}
root: S:/shows/saltmarsh
jobs:
  light: {tool: depth, frames: "1001-1048:2", with: {plates: plates/sm0140.$F4.exr}, size: cpu-only}
`)
	j := p.Jobs[0]
	s := specOf(t, j)
	if j.Request.Name != "sm0140-light-v004" || s.Params["frames"] != "1001-1048x2" || j.Request.Due != "2026-10-23T18:00:00-04:00" {
		t.Fatalf("name %s frames %s due %s", j.Request.Name, s.Params["frames"], j.Request.Due)
	}
	in := j.Request.Inputs
	if len(in) != 3 || in[1].FromJob != "sm0140-light-v003" || in[1].MountPath != "/show" || in[2].View != lut ||
		in[2].MountPath != "/c4/read/lut" || !in[2].ReadOnly || len(j.After) != 1 || j.After[0] != "sm0140-light-v003" {
		t.Fatalf("inputs %+v after %v", in, j.After)
	}
	if v, _ := envVal(s, "C4_SOURCE_ROOTS"); v != `[{"os":"windows","path":"S:/shows/saltmarsh"}]` {
		t.Fatalf("C4_SOURCE_ROOTS %q", v)
	}
	if v, _ := envVal(s, "C4_SHOT"); v != "sm0140" {
		t.Fatalf("C4_SHOT %q", v)
	}
	res := s.Template.Spec.Containers[0].Resources
	if q := res.Limits["memory"]; q.String() != "24Gi" || len(res.Limits) != 1 || res.Requests.Cpu().String() != "4" {
		t.Fatalf("the cpu-only size: %+v", res)
	}

	// frames from the shot's cut; a date; suggested
	p = mustLower(t, `c4: submit/1
project: saltmarsh
shot: sm0140
due: "2026-10-23"
tree: .
jobs:
  proxies: {tool: proxies, with: {plates: plates/sm0140.$F4.exr}}
  later: {tool: proxies, due: suggested, with: {plates: plates/sm0140.$F4.exr}}
`)
	if s := specOf(t, p.Jobs[0]); s.Params["frames"] != "1001-1048" || p.Jobs[0].Request.Due != "2026-10-23" {
		t.Fatalf("frames %s due %q", s.Params["frames"], p.Jobs[0].Request.Due)
	}
	if d := p.Jobs[1]; d.Request.Due != "" || !d.Due.Suggested || len(d.Labels) != 0 {
		t.Fatalf("suggested: %+v", d)
	}

	// a once tool's frames fill its range; with no frame parameter it is refused
	_, errs := lowerDoc(t, `c4: submit/1
project: saltmarsh
shot: sm0140
tree: .
jobs:
  merge: {tool: merge, frames: 1001-1048}
`)
	if len(errs) != 1 || errs[0].Path != "jobs.merge.frames" {
		t.Fatalf("once without a frame parameter: %v", errs)
	}

	// a whole-range unit over more than the class's most frames
	_, errs = lowerDoc(t, `c4: submit/1
project: saltmarsh
shot: sm0140
tree: .
jobs:
  proxies: {tool: proxies, frames: 1-1500, chunk: all, with: {plates: p.$F4.exr}}
`)
	if len(errs) != 1 || errs[0].Path != "jobs.proxies.chunk" || errs[0].Message != "a whole-range unit over more than 1000 frames cannot run on this farm yet" {
		t.Fatalf("chunk all: %v", errs)
	}
}

// A job lowered from a tool with no image pinned is refused, not run on
// the image as written.
func TestLowerNeedsPinnedImage(t *testing.T) {
	f := loadFarm(t)
	tl := f.tools["proxies"]
	_, errs := lowerTool(toolJob{tool: tl, ref: ToolRef{Name: "proxies", Version: "v1", View: "c4x"}, frames: "1-4",
		with: map[string]any{"plates": "p.exr"}, inputs: []api.SubmitInput{}})
	if len(errs) != 1 || errs[0].Path != "tool" || !strings.Contains(errs[0].Message, "no image pinned") {
		t.Fatalf("%v", errs)
	}
}

// answers: {by: unit:<name>} must name a unit of the job that reasons: a
// frames or once tool's units (<key>/<n> from 0) each run a procedure, so
// each is refused at the check, as is a name that is no unit of the job;
// a lead answers only its own job, which a submission's never is, and a
// tool answering stays refused, in the farm's words.
func TestLowerAnswers(t *testing.T) {
	for by, want := range map[string]string{
		"unit:lead":      `no unit "lead" in proxies`,
		"unit:proxies/9": `no unit "proxies/9" in proxies`,
		"unit:proxies/2": "proxies/2 cannot answer questions: it runs a procedure",
		"unit:4":         "proxies/4 cannot answer questions: it runs a procedure",
		"lead":           "this job has no lead: it was not made by a lead's c4_agent",
		"tool:shot-lead": "a tool answering a job's questions cannot run on this farm yet",
	} {
		_, errs := lowerDoc(t, `c4: submit/1
project: saltmarsh
shot: sm0140
tree: .
jobs:
  proxies: {tool: proxies, answers: {by: "`+by+`"}, with: {plates: plates/sm0140.$F4.exr}}
`)
		if len(errs) != 1 || errs[0].Path != "jobs.proxies.answers.by" || errs[0].Message != want {
			t.Errorf("answers by %s: %v", by, errs)
		}
	}
}

// trackerFarm is the fixture farm with flow connected to saltmarsh.
type trackerFarm struct{ *fixtureFarm }

func (trackerFarm) Trackers(string) []string { return []string{"flow"} }

// The defaults and refusals added after review: follows defaults to the
// previous version; frames come from the frames job followed (one written
// after it too), then the cut, for a once job's range as well; for: and
// tracker: are checked against the farm; preview orders fml and none
// only.
func TestLowerReviewed(t *testing.T) {
	p := mustLower(t, `c4: submit/1
project: saltmarsh
shot: sm0140
tree: .
jobs:
  light: {tool: depth, with: {plates: p.$F4.exr}}
`)
	if j := p.Jobs[0].Request; j.Name != "sm0140-light-v004" || j.Follows != "sm0140-light-v003" {
		t.Fatalf("follows: %s follows %q", j.Name, j.Follows)
	}
	// the movie follows a frames job written after it, and fills its range
	p = mustLower(t, `c4: submit/1
project: saltmarsh
shot: sm0999
tree: .
jobs:
  movie: {tool: transcode, after: plates, with: {plates: p.$F4.exr}}
  plates: {tool: proxies, frames: 1-24, with: {plates: p.$F4.exr}}
`)
	if v, _ := envVal(specOf(t, p.Jobs[0]), "C4_PARAMS"); !strings.Contains(v, `"range":"1-24"`) {
		t.Fatalf("movie's range: %s", v)
	}
	farm := loadFarm(t)
	for _, c := range []struct{ doc, path, msg string }{
		{"for: nor\n", "for", `no person or party "nor" on this farm`},
		{"tracker: flwo/Task/1\n", "tracker", `no tracker "flwo" in saltmarsh`},
	} {
		sub, _ := ParseSubmission([]byte("c4: submit/1\nproject: saltmarsh\nshot: sm0140\ntree: .\n" + c.doc +
			"jobs:\n  proxies: {tool: proxies, frames: 1-4, with: {plates: p.exr}}\n"))
		errs := Check(sub, trackerFarm{farm})
		if len(errs) != 1 || errs[0].Path != c.path || errs[0].Message != c.msg {
			t.Errorf("%s: %v", c.path, errs)
		}
	}
	_, errs := lowerDoc(t, `c4: submit/1
project: saltmarsh
shot: sm0140
tree: .
jobs:
  proxies: {tool: proxies, frames: 1-4, preview: "every:10", with: {plates: p.exr}}
`)
	if len(errs) != 1 || errs[0].Path != "jobs.proxies.preview" || !strings.Contains(errs[0].Message, "orders only fml and none") {
		t.Fatalf("preview: %v", errs)
	}
}

// Counts say "1 unit" and "1 frame"; a one-unit frames job says its frames.
func TestWhatSingular(t *testing.T) {
	for _, c := range []struct{ job, want string }{
		{"{tool: proxies, frames: 1001, with: {plates: p.exr}}", "1 frame in 1 unit"},
		{"{tool: proxies, frames: 1001-1010, chunk: all, with: {plates: p.exr}}", "10 frames in 1 unit"},
		{"{tool: proxies, frames: 1001-1048, with: {plates: p.exr}}", "48 frames in 6 units"},
		{"{tool: transcode, frames: 1001-1048, with: {plates: p.exr}}", "1 unit over 1001-1048"},
	} {
		p := mustLower(t, "c4: submit/1\nproject: saltmarsh\nshot: sm0140\ntree: .\njobs:\n  k: "+c.job+"\n")
		if w := p.Jobs[0].What(); w != c.want {
			t.Errorf("%s: %q, want %q", c.job, w, c.want)
		}
	}
	if w := (PlannedJob{Once: true, Units: 1}).What(); w != "1 unit" {
		t.Errorf("a once job with no frames: %q", w)
	}
	if Count(1, "frame") != "1 frame" || Count(0, "unit") != "0 units" {
		t.Fatal(Count(1, "frame"), Count(0, "unit"))
	}
}

// A shot names jobs as its stem lowercased, with anything outside
// [a-z0-9-] made -, repeats collapsed and the ends trimmed; one with
// nothing left is refused at shot.
func TestJobStem(t *testing.T) {
	for in, want := range map[string]string{"sm0220_fx": "sm0220-fx", "SM0220__FX ": "sm0220-fx", "-a.b-": "a-b", "___": "", "日本": ""} {
		if got := JobStem(in); got != want {
			t.Errorf("JobStem(%q) = %q, want %q", in, got, want)
		}
	}
	p := mustLower(t, "c4: submit/1\nproject: saltmarsh\nshot: sm0220_FX\ntree: .\njobs:\n  proxies: {tool: proxies, frames: 1-4, with: {plates: p.exr}}\n")
	if n := p.Jobs[0].Request.Name; n != "sm0220-fx-proxies-v001" || p.Jobs[0].Request.Shot != "sm0220_FX" {
		t.Fatalf("name %s shot %s", n, p.Jobs[0].Request.Shot)
	}
	_, errs := lowerDoc(t, "c4: submit/1\nproject: saltmarsh\nshot: ___\ntree: .\njobs:\n  proxies: {tool: proxies, frames: 1-4, with: {plates: p.exr}}\n")
	if len(errs) != 1 || errs[0].Path != "shot" || errs[0].Message != "___ can't name a job: use letters, digits and -" || errs[0].Fix != "set name: to override" {
		t.Fatalf("%v", errs)
	}
}

// A tool pinned to a version older than the one in use is refused: a form
// or c4_run_<tool> drawn from it has the old parameters. A version past
// the latest is no version at all.
func TestStalePin(t *testing.T) {
	for pin, want := range map[string]string{
		"karma@v3": "karma is v4 now; its form changed: draw it again",
		"karma@v9": "no version v9 of karma in saltmarsh",
	} {
		sub, _ := ParseSubmission([]byte("c4: submit/1\nproject: saltmarsh\nshot: sm0140\ntree: .\njobs:\n  light: {tool: " + pin + ", frames: 1-4, with: {stage: s.usda}}\n"))
		errs := Check(sub, loadFarm(t))
		if len(errs) != 1 || errs[0].Path != "jobs.light.tool" || errs[0].Message != want {
			t.Errorf("%s: %v", pin, errs)
		}
	}
}

// A job's version names it, and must be past every job of its name.
func TestVersionGiven(t *testing.T) {
	p := mustLower(t, "c4: submit/1\nproject: saltmarsh\nshot: sm0140\ntree: .\njobs:\n  light: {tool: depth, version: 7, frames: 1-4, with: {plates: p.exr}}\n")
	if j := p.Jobs[0]; j.Request.Name != "sm0140-light-v007" || !j.Pinned {
		t.Fatalf("%s pinned %v", j.Request.Name, j.Pinned)
	}
	for v, want := range map[string]string{
		"3":    "sm0140-light-v003 exists; leave version out for the next one, v004",
		"v002": "sm0140-light-v002 is below the latest, sm0140-light-v003; leave version out for the next one, v004",
	} {
		_, errs := lowerDoc(t, "c4: submit/1\nproject: saltmarsh\nshot: sm0140\ntree: .\njobs:\n  light: {tool: depth, version: "+v+", frames: 1-4, with: {plates: p.exr}}\n")
		if len(errs) != 1 || errs[0].Path != "jobs.light.version" || errs[0].Message != want || errs[0].Fix != "leave version out, or set version: 4 or later" {
			t.Errorf("version %s: %v", v, errs)
		}
	}
}
