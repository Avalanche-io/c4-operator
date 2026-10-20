package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestParseFixtures holds the parsers to the fixtures: every passing case
// parses, every by: schema case gives exactly its expected errors, and
// every by: check case passes (only the farm can refuse it).
func TestParseFixtures(t *testing.T) {
	for _, c := range cases(t) {
		t.Run(c.Format+"/"+c.Name+map[bool]string{true: "", false: " (fail)"}[c.Pass], func(t *testing.T) {
			var errs []Error
			var parsed any
			if c.Format == "submit" {
				s, e := ParseSubmission(c.Doc)
				parsed, errs = s, e
			} else {
				tl, e := ParseTool(c.Doc)
				parsed, errs = tl, e
			}
			switch {
			case c.Pass || c.Expect.By == "check":
				if errs != nil {
					t.Fatalf("refused: %+v", errs)
				}
				if reflect.ValueOf(parsed).IsNil() {
					t.Fatal("no document and no errors")
				}
			default:
				if !reflect.DeepEqual(errs, c.Expect.Errors) {
					got, _ := json.MarshalIndent(errs, "", "  ")
					want, _ := json.MarshalIndent(c.Expect.Errors, "", "  ")
					t.Fatalf("errors\n got %s\nwant %s", got, want)
				}
			}
		})
	}
}

// TestParsersAgreeWithTheSchema: the parsers and a full JSON Schema
// validator accept and refuse the same fixtures.
func TestParsersAgreeWithTheSchema(t *testing.T) {
	for _, c := range cases(t) {
		rs := resolve(t, map[string][]byte{"submit": SubmitSchema(), "tool": ToolSchema()}[c.Format])
		validErr := rs.Validate(value(t, c.Doc))
		var errs []Error
		if c.Format == "submit" {
			_, errs = ParseSubmission(c.Doc)
		} else {
			_, errs = ParseTool(c.Doc)
		}
		if (validErr == nil) != (errs == nil) {
			t.Errorf("%s/%s: the validator says %v, the parser %v", c.Format, c.Name, validErr, errs)
		}
	}
}

func TestJobsKeepTheirOrder(t *testing.T) {
	s, errs := ParseSubmission([]byte("c4: submit/1\njobs:\n  zeta: {tool: karma}\n  alpha: {tool: nuke-comp, after: zeta}\n  mid: {tool: transcode, after: [alpha, zeta]}\n"))
	if errs != nil {
		t.Fatal(errs)
	}
	var keys []string
	for _, j := range s.Jobs {
		keys = append(keys, j.Key)
	}
	if strings.Join(keys, " ") != "zeta alpha mid" {
		t.Fatalf("jobs %v", keys)
	}
	if got := s.Jobs.Job("mid").After; !reflect.DeepEqual(got, After{{Job: "alpha"}, {Job: "zeta"}}) {
		t.Fatalf("after %+v", got)
	}
}

func TestShapesReadAsTheirTypes(t *testing.T) {
	s, errs := ParseSubmission([]byte(`c4: submit/1
due: 2026-10-23
jobs:
  sim: {tool: pyro-sim, frames: 1001}
  light:
    tool: karma
    after: {sim: previous, fx: all}
    approve: {by: role:lighting-supervisor, expect: 7200}
    over: {seed: [1, 2], dissipation: [0.05]}
    combine: seed * dissipation
`))
	if errs != nil {
		t.Fatal(errs)
	}
	if s.Due != "2026-10-23" {
		t.Errorf("a date stays the text written, got %q", s.Due)
	}
	if s.Jobs.Job("sim").Frames != "1001" {
		t.Errorf("frames %q", s.Jobs.Job("sim").Frames)
	}
	l := s.Jobs.Job("light")
	if !reflect.DeepEqual(l.After, After{{"sim", "previous"}, {"fx", "all"}}) {
		t.Errorf("after %+v", l.After)
	}
	if l.Approve.Expect != "7200s" {
		t.Errorf("expect %q", l.Approve.Expect)
	}
	if len(l.Over) != 2 || l.Over[0].Name != "seed" || l.Over[1].Name != "dissipation" {
		t.Errorf("over %+v", l.Over)
	}
}

func TestToolTypes(t *testing.T) {
	var karma []byte
	for _, c := range cases(t) {
		if c.Format == "tool" && c.Pass && c.Name == "karma" {
			karma = c.Doc
		}
	}
	tl, errs := ParseTool(karma)
	if errs != nil {
		t.Fatal(errs)
	}
	if tl.Runs.Host == nil || !strings.HasSuffix(tl.Runs.Host.Exe["windows"], "husk.exe") {
		t.Errorf("host %+v", tl.Runs.Host)
	}
	if len(tl.Progress) != 1 || tl.Progress[0].Percent != 1 || tl.Errors.Licence == nil {
		t.Errorf("progress %+v errors %+v", tl.Progress, tl.Errors)
	}
	if tl.Needs.Licences["sesi/karma"] != 1 || tl.Needs.Sizes["heavy"].Memory != "128G" || *tl.Needs.GPU != 1 {
		t.Errorf("needs %+v", tl.Needs)
	}
	if p := tl.Param("stage"); p == nil || !p.Required() || p.UserInterface.Control != "CHOOSE_INPUT_FILE" {
		t.Errorf("stage %+v", p)
	}
	if p := tl.Param("renderer"); p == nil || p.Required() {
		t.Errorf("renderer %+v", p)
	}
}

func TestParseLock(t *testing.T) {
	good := `{"c4": "lock/1", "images": {"c4/runtime-oiio": "ghcr.io/avalanche-io/runtime-oiio@sha256:` + strings.Repeat("ab", 32) + `"}}`
	if l, errs := ParseLock([]byte(good)); errs != nil || len(l.Images) != 1 {
		t.Fatalf("%+v %+v", l, errs)
	}
	_, errs := ParseLock([]byte(`{"c4": "lock/1", "images": {"c4/runtime-oiio": "c4/runtime-oiio:1"}}`))
	if len(errs) != 1 || errs[0].Path != "images.c4/runtime-oiio" {
		t.Fatalf("an unpinned image: %+v", errs)
	}
}

func TestToJSON(t *testing.T) {
	j, err := ToJSON([]byte("b: 1\na: &x {k: [true, null, 1.5, '07']}\nc: *x\nd: 2026-10-20T15:00:00-07:00\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"b":1,"a":{"k":[true,null,1.5,"07"]},"c":{"k":[true,null,1.5,"07"]},"d":"2026-10-20T15:00:00-07:00"}`
	if string(j) != want {
		t.Fatalf("got %s", j)
	}
	if _, err := ToJSON([]byte("a: 1\na: 2\n")); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("a key written twice: %v", err)
	}
	if _, errs := ParseSubmission([]byte("c4: submit/1\njobs: [\n")); len(errs) != 1 || !strings.HasPrefix(errs[0].Message, "this is not YAML or JSON") {
		t.Fatalf("bad YAML: %+v", errs)
	}
}
