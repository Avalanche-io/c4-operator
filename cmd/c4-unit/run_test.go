package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/schema"
)

// syncBuffer is a log the runner's goroutines write while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// toolRun is one tool, its tree and its unit's variables.
type toolRun struct {
	t         *testing.T
	tree, dir string
	vars      map[string]string
	out, errs syncBuffer
	signals   chan os.Signal
	minute    time.Duration
}

func newToolRun(t *testing.T, toolYAML string) *toolRun {
	t.Helper()
	r := &toolRun{t: t, tree: t.TempDir(), dir: t.TempDir(), signals: make(chan os.Signal, 1), minute: time.Minute}
	if err := os.WriteFile(filepath.Join(r.dir, "tool.yaml"), []byte(toolYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	r.vars = map[string]string{"C4_OUTPUT": r.tree, "C4_TOOLS": r.dir, "C4_JOB": "sm0140-comp", "C4_UNIT": "3",
		"C4_UNIT_LABEL": "1001-1015", "C4_FRAMES": "1001-1015", "C4_FRAME_START": "1001", "C4_FRAME_END": "1015",
		"C4_TERMINATION_LOG": filepath.Join(t.TempDir(), "termination-log")}
	return r
}

func (r *toolRun) env() env {
	return env{
		getenv:    func(k string) string { return r.vars[k] },
		mountinfo: "/nonexistent",
		environ: func() []string {
			out := []string{"PATH=" + os.Getenv("PATH"), "C4_PARAM_STALE=x"}
			for k, v := range r.vars {
				out = append(out, k+"="+v)
			}
			return out
		},
		stderr:    &r.errs,
		signals:   r.signals,
		minuteLen: r.minute,
		stopGrace: 300 * time.Millisecond,
		pollEvery: 20 * time.Millisecond,
	}
}

func (r *toolRun) run() int {
	r.t.Helper()
	c, err := run([]string{"run"}, &r.out, r.env())
	if err != nil {
		r.t.Logf("run: %v", err)
	}
	return c
}

func (r *toolRun) file(rel string) string {
	b, _ := os.ReadFile(filepath.Join(r.tree, rel))
	return string(b)
}

func (r *toolRun) outcome() outcome {
	r.t.Helper()
	var o outcome
	b, err := os.ReadFile(filepath.Join(r.tree, ".c4", "outcome"))
	if err != nil {
		r.t.Fatalf("no outcome: %v\nlog:\n%s%s", err, r.out.String(), r.errs.String())
	}
	if err := json.Unmarshal(b, &o); err != nil {
		r.t.Fatal(err)
	}
	return o
}

func TestRunFillsTheCommandAndEnvironment(t *testing.T) {
	r := newToolRun(t, `
c4: tool/1
name: args
kind: frames
runs:
  image: c4/runtime-base
  command: [bash, -c, 'printf "%s\n" "$@" > "$C4_TREE/args"; env | grep -E "^(C4_PARAM_|C4_TREE|SHOTVAR|NOVAR)" | sort > "$C4_TREE/env"', args,
            "{{Param.stage}}", --note, "{{Param.note}}", "{{ Unit.Frames }}", "{{Unit.FrameCount}}", "{{Unit.FrameStart}}-{{Unit.FrameEnd}}",
            "{{Unit.Name}}#{{Unit.Index}}", "{{Job.Name}}", "{{Job.Shot}}", "{{Tool.Dir}}/run.sh", "x{{Param.empty}}"]
  env: {SHOTVAR: "{{Job.Tree}}/{{Param.stage}}", NOVAR: "{{Param.note}}"}
  workdir: render
params:
- {name: stage, type: PATH}
- {name: note, type: STRING, default: ""}
- {name: empty, type: STRING, default: ""}
- {name: frameRange, type: RANGE_EXPR}
- {name: slate, type: BOOL}
- {name: count, type: INT}
- {name: plates, type: "LIST[PATH]"}
`)
	os.MkdirAll(filepath.Join(r.tree, "render"), 0o755)
	r.vars["C4_FRAME_STEP"] = "2"
	r.vars["C4_PARAMS"] = `{"stage": "/show/sm0140.usda", "empty": "", "frameRange": "1001-1015:2", "slate": true, "count": 3, "plates": ["/show/a.exr", "/show/b.exr"]}`
	if c := r.run(); c != 0 {
		t.Fatalf("exit %d\n%s", c, r.errs.String())
	}
	want := strings.Join([]string{"/show/sm0140.usda", "--note", "1001-1015:2", "8", "1001-1015", "1001-1015#3", "sm0140-comp", r.dir + "/run.sh"}, "\n") + "\n"
	if got := r.file("args"); got != want {
		t.Errorf("args:\n%s\nwant:\n%s", got, want)
	}
	env := r.file("env")
	for _, w := range []string{
		"C4_PARAM_STAGE=/show/sm0140.usda\n", "C4_PARAM_FRAME_RANGE=1001-1015:2\n", "C4_PARAM_SLATE=true\n",
		"C4_PARAM_COUNT=3\n", `C4_PARAM_PLATES=["/show/a.exr","/show/b.exr"]` + "\n", "C4_PARAM_EMPTY=\n", "C4_PARAM_NOTE=\n",
		"C4_TREE=" + r.tree + "\n", "SHOTVAR=" + r.tree + "//show/sm0140.usda\n",
	} {
		if !strings.Contains(env, w) {
			t.Errorf("env lacks %q:\n%s", w, env)
		}
	}
	for _, not := range []string{"NOVAR", "C4_PARAM_STALE"} {
		if strings.Contains(env, not) {
			t.Errorf("env has %s:\n%s", not, env)
		}
	}
	if o := r.outcome(); o.Result != "succeeded" || o.Class != "" {
		t.Errorf("outcome %+v", o)
	}
	if p := r.file(".c4/progress"); p != "1\n" {
		t.Errorf("progress %q", p)
	}
}

func TestRunRefusesWhatCannotRun(t *testing.T) {
	for name, c := range map[string]struct{ yaml, want string }{
		"farm variable": {`runs: {image: c4/runtime-base, command: ['true'], env: {C4_JOB: x}}`, "runs.env.C4_JOB: C4_ variables are the farm's own"},
		"unknown param": {`runs: {image: c4/runtime-base, command: [echo, "{{Param.nope}}"]}`, `the tool has no parameter "nope"`},
		"unknown name":  {`runs: {image: c4/runtime-base, command: [echo, "{{Task.Frame}}"]}`, "{{Task.Frame}} is not a format string"},
		"no exe":        {`runs: {image: c4/runtime-base, command: ["{{Run.Exe}}", -V]}`, "runs.command is empty once filled in"},
		"bad regex":     {"runs: {image: c4/runtime-base, command: ['true']}\nerrors: {retry: ['(']}", "errors.retry[0]"},
		"no group":      {"runs: {image: c4/runtime-base, command: ['true']}\nprogress: [{match: 'x', percent: 1}]", "group 1 is not in"},
		"no program":    {`runs: {image: c4/runtime-base, command: [/nonexistent/husk]}`, "could not start the tool"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\n"+c.yaml)
			if code := r.run(); code != setupCode {
				t.Fatalf("exit %d", code)
			}
			if o := r.outcome(); o.Result != "failed" || o.Class != "permanent" || !strings.Contains(o.Reason, c.want) {
				t.Errorf("outcome %+v, want reason with %q", o, c.want)
			}
		})
	}
}

func TestRunExeOnAHost(t *testing.T) {
	r := newToolRun(t, `
c4: tool/1
name: x
kind: once
runs:
  image: c4/runtime-base
  exe: "false"
  host: {has: [bash], exe: {linux: echo, macos: echo}}
  command: ["{{Run.Exe}}", hello]
`)
	if c := r.run(); c != 1 {
		t.Errorf("in a container runs.exe (false): exit %d", c)
	}
	r.vars["C4_RUNNER"] = "host"
	if c := r.run(); c != 0 || !strings.Contains(r.out.String(), "hello") {
		t.Errorf("on a host runs.host.exe: exit %d, log %q", c, r.out.String())
	}
}

func writePNG(t *testing.T, p string) {
	t.Helper()
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 64, 36)))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunReadsTheProtocol(t *testing.T) {
	r := newToolRun(t, `
c4: tool/1
name: x
kind: frames
runs:
  image: c4/runtime-base
  command: [bash, -c, 'echo openjd_status: loading the stage; echo openjd_progress: 12.5; echo "openjd_redacted_env: TOKEN=s3cret"; echo "using s3cret now"; echo "s3cret on stderr" >&2; echo c4_frame: 1001 f.1001.png; echo c4_frame: 1003; echo openjd_fail: will not be the reason; echo c4_fail: permanent the scene names a missing asset; exit 3']
`)
	r.vars["C4_FRAME_END"] = "1003"
	writePNG(t, filepath.Join(r.tree, "f.1001.png"))
	if c := r.run(); c != 3 {
		t.Fatalf("exit %d", c)
	}
	o := r.outcome()
	if o.Result != "failed" || o.Class != "permanent" || o.Reason != "the scene names a missing asset" || o.ExitCode != 3 {
		t.Errorf("outcome %+v", o)
	}
	if s := r.file(".c4/status"); s != "loading the stage\n" {
		t.Errorf("status %q", s)
	}
	// two of three frames is ahead of 12.5%
	if p := r.file(".c4/progress"); p != "0.6667\n" {
		t.Errorf("progress %q", p)
	}
	if _, err := os.Stat(filepath.Join(r.tree, "preview", "1001.png")); err != nil {
		t.Errorf("no preview of 1001: %v", err)
	}
	log := r.out.String() + r.errs.String()
	if strings.Contains(log, "s3cret") || !strings.Contains(log, "openjd_redacted_env: TOKEN=********") || !strings.Contains(log, "using ******** now") || !strings.Contains(log, "******** on stderr") {
		t.Errorf("the secret is not masked:\n%s", log)
	}
}

func TestRunClassifiesByPatterns(t *testing.T) {
	const tool = `
c4: tool/1
name: x
kind: frames
runs:
  image: c4/runtime-base
  command: [bash, -c, 'echo ALF_PROGRESS 40%%; echo "Warning: CUDA error in an unused pass" >&2; echo "CUDA error: out of memory" >&2; %s exit 1']
progress:
- {match: 'ALF_PROGRESS (\d+)%%', percent: 1}
errors:
  licence: ['No licenses could be found']
  retry: ['CUDA error']
  ignore: ['^Warning:']
`
	r := newToolRun(t, fmt.Sprintf(tool, ""))
	if c := r.run(); c != 1 {
		t.Fatalf("exit %d", c)
	}
	if o := r.outcome(); o.Class != "retry" || o.Reason != "CUDA error: out of memory" {
		t.Errorf("the first matching line that is not ignored gives the reason: %+v", o)
	}
	if p := r.file(".c4/progress"); p != "0.4\n" {
		t.Errorf("progress %q", p)
	}

	// a licence line on the other stream, before or after, is stronger
	r = newToolRun(t, fmt.Sprintf(tool, `echo "No licenses could be found to run this application";`))
	r.run()
	if o := r.outcome(); o.Class != "licence" || o.Reason != "No licenses could be found to run this application" {
		t.Errorf("outcome %+v", o)
	}
}

func TestRunDefaultsToRetry(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'exit 7']}\n")
	if c := r.run(); c != 7 {
		t.Fatalf("exit %d", c)
	}
	if o := r.outcome(); o.Class != "retry" || o.Reason != "exit status 7" || o.ExitCode != 7 {
		t.Errorf("outcome %+v", o)
	}
}

func TestRunEndsAStall(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'echo started; sleep 30']}\nstall: {quietMinutes: 2}\n")
	r.minute = 100 * time.Millisecond
	began := time.Now()
	if c := r.run(); c != stallCode {
		t.Fatalf("exit %d", c)
	}
	if d := time.Since(began); d > 20*time.Second {
		t.Errorf("the stall took %v to end; the tool alone runs 30 s", d)
	}
	if o := r.outcome(); o.Class != "retry" || !o.Stalled || o.Reason != "no output for 2 minutes" {
		t.Errorf("outcome %+v", o)
	}
}

func TestRunForwardsSIGTERM(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'trap \"echo stopping; exit 143\" TERM; echo ready; while :; do sleep 0.05; done']}\n")
	go func() {
		for !strings.Contains(r.out.String(), "ready") {
			time.Sleep(10 * time.Millisecond)
		}
		r.signals <- syscall.SIGTERM
	}()
	if c := r.run(); c != 143 {
		t.Fatalf("exit %d", c)
	}
	if o := r.outcome(); o.Class != "retry" || o.Reason != "stopped by terminated" {
		t.Errorf("outcome %+v", o)
	}
}

func TestRunOutlivesAChildHoldingStdout(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, '(sleep 60 &); echo done']}\n")
	began := time.Now()
	if c := r.run(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	if d := time.Since(began); d > 30*time.Second {
		t.Errorf("waited %v for the child's stdout; the child holds it 60 s", d)
	}
}

func TestRunSafePointAndNeeds(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'mkdir -p $C4_OUTPUT/.c4; echo 1 > $C4_OUTPUT/.c4/safepoint; exit 10']}\n")
	writeAtomic(filepath.Join(r.tree, ".c4", "outcome"), []byte(`{"result":"failed"}`))
	if c := r.run(); c != stopCode {
		t.Fatalf("exit %d", c)
	}
	if _, err := os.Stat(filepath.Join(r.tree, ".c4", "outcome")); !os.IsNotExist(err) {
		t.Errorf("a stop at a safe point left an outcome: %v", err)
	}

	r = newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'printf \"the frame rate\\nwhy: 24 or 25\" > $C4_TERMINATION_LOG; exit 12']}\n")
	if c := r.run(); c != needsCode {
		t.Fatalf("exit %d", c)
	}
	if o := r.outcome(); o.Class != "question" || o.Reason != "the brief lacks information: the frame rate; why: 24 or 25" {
		t.Errorf("outcome %+v", o)
	}

	// exit 10 with no safe point in this run is a failure like any other
	r = newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'exit 10']}\n")
	r.run()
	if o := r.outcome(); o.Result != "failed" || o.ExitCode != 10 {
		t.Errorf("outcome %+v", o)
	}
}

func TestRunCountsFramesInTheTree(t *testing.T) {
	r := newToolRun(t, `
c4: tool/1
name: x
kind: frames
runs:
  image: c4/runtime-base
  command: [bash, -c, 'sleep 0.2; cp "$C4_TOOLS/f.png" render/beauty/sm.1001.png; sleep 0.2; cp "$C4_TOOLS/f.png" render/beauty/sm.1003.png; cp "$C4_TOOLS/f.png" render/beauty/sm.1004.png; sleep 0.2; exit 1']
  workdir: "{{Job.Tree}}"
previews: "render/**/*.$F4.png"
`)
	r.vars["C4_FRAME_END"], r.vars["C4_FRAME_STEP"] = "1005", "2"
	writePNG(t, filepath.Join(r.dir, "f.png"))
	old := filepath.Join(r.tree, "render", "beauty", "sm.1005.png")
	writePNG(t, old)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(old, past, past)
	if c := r.run(); c != 1 {
		t.Fatalf("exit %d\n%s", c, r.errs.String())
	}
	// 1001 and 1003 of 1001, 1003, 1005; 1004 is not the unit's; 1005 was
	// there before
	if p := r.file(".c4/progress"); p != "0.6667\n" {
		t.Errorf("progress %q", p)
	}
	for f, want := range map[string]bool{"1001": true, "1003": true, "1004": false, "1005": false} {
		_, err := os.Stat(filepath.Join(r.tree, "preview", f+".png"))
		if (err == nil) != want {
			t.Errorf("preview of %s: %v, want %v", f, err == nil, want)
		}
	}
}

func TestRunWritesThePathMap(t *testing.T) {
	r := newToolRun(t, `c4: tool/1
name: x
kind: once
runs: {image: c4/runtime-base, command: [bash, -c, 'echo "$1 $HOUDINI_PATHMAP $C4_PATH_MAP"', x, "{{Run.PathMap}}"], env: {HOUDINI_PATHMAP: "{{Run.PathMap}}"}}
`)
	r.vars["C4_SOURCE_ROOTS"] = `[{"os": "windows", "path": "S:/saltmarsh"}, {"os": "macos", "path": "/Volumes/saltmarsh"}]`
	if c := r.run(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	p := filepath.Join(r.tree, ".c4", "path-map.json")
	if got := strings.TrimSpace(r.out.String()); got != p+" "+p+" "+p {
		t.Errorf("log %q", got)
	}
	var doc struct {
		Version string `json:"version"`
		Rules   []struct {
			SourcePathFormat string `json:"source_path_format"`
			SourcePath       string `json:"source_path"`
			DestinationPath  string `json:"destination_path"`
		} `json:"path_mapping_rules"`
	}
	json.Unmarshal([]byte(r.file(".c4/path-map.json")), &doc)
	if doc.Version != "pathmapping-1.0" || len(doc.Rules) != 2 || doc.Rules[0].SourcePathFormat != "WINDOWS" ||
		doc.Rules[0].SourcePath != "S:/saltmarsh" || doc.Rules[1].SourcePathFormat != "POSIX" || doc.Rules[1].DestinationPath != r.tree {
		t.Errorf("path map %+v", doc)
	}

	// no roots: no map, and the argument and variable naming it are removed
	r = newToolRun(t, `c4: tool/1
name: x
kind: once
runs: {image: c4/runtime-base, command: [bash, -c, 'echo "[$#] [${HOUDINI_PATHMAP-unset}]"', x, "{{Run.PathMap}}"], env: {HOUDINI_PATHMAP: "{{Run.PathMap}}"}}
`)
	r.run()
	if got := strings.TrimSpace(r.out.String()); got != "[0] [unset]" {
		t.Errorf("log %q", got)
	}
}

func TestFramePattern(t *testing.T) {
	for _, c := range []struct {
		pattern, path, frame string
	}{
		{"render/*.$F4.exr", "render/sm0140.1012.exr", "1012"},
		{"render/*.$F4.exr", "render/beauty/sm0140.1012.exr", ""},
		{"render/**/*.$F4.exr", "render/beauty/sm0140.1012.exr", "1012"},
		{"render/**/*.$F4.exr", "render/sm0140.1012.exr", "1012"},
		{"comp/sm0140.<F4>.exr", "comp/sm0140.0099.exr", "0099"},
		{"comp/sm0140.%04d.exr", "comp/sm0140.10012.exr", "10012"},
		{"comp/sm0140.%04d.exr", "comp/sm0140.1012.exr.tmp", ""},
		{"out/f.$F.png", "out/f.7.png", "7"},
	} {
		re, err := framePattern(c.pattern)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if m := re.FindStringSubmatch(c.path); m != nil {
			got = m[1]
		}
		if got != c.frame {
			t.Errorf("%s on %s: %q, want %q", c.pattern, c.path, got, c.frame)
		}
	}
	for _, bad := range []string{"render/*.exr", "render/*.$F4.$F4.exr"} {
		if _, err := framePattern(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if p := fixedPrefix("render/beauty/**/*.$F4.exr"); p != filepath.Join("render", "beauty") {
		t.Errorf("prefix %q", p)
	}
}

func TestEnvName(t *testing.T) {
	for in, want := range map[string]string{"stage": "C4_PARAM_STAGE", "frameRange": "C4_PARAM_FRAME_RANGE",
		"out_dir": "C4_PARAM_OUT_DIR", "LUT": "C4_PARAM_LUT", "pass2Name": "C4_PARAM_PASS2_NAME"} {
		if got := envName(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestUnitFrames(t *testing.T) {
	for _, c := range []struct {
		vars  map[string]string
		label string
		count int
	}{
		{map[string]string{"C4_FRAME_START": "1001", "C4_FRAME_END": "1008"}, "1001-1008", 8},
		{map[string]string{"C4_FRAME_START": "1001", "C4_FRAME_END": "1015", "C4_FRAME_STEP": "2"}, "1001-1015:2", 8},
		{map[string]string{"C4_FRAMES": "1"}, "1", 1},
		{map[string]string{"C4_FRAMES": "12-14"}, "12-14", 3},
		{map[string]string{}, "", 0},
	} {
		f, err := unitFrames(vars(c.vars))
		if err != nil || f.String() != c.label || f.count() != c.count {
			t.Errorf("%v: %q %d %v", c.vars, f.String(), f.count(), err)
		}
	}
	if _, err := unitFrames(vars(map[string]string{"C4_FRAME_START": "9", "C4_FRAME_END": "1"})); err == nil {
		t.Error("a backwards range accepted")
	}
}

// A parameter missing from C4_PARAMS takes its default, a relative PATH
// (or each of a LIST[PATH]) is joined to the tree, and a required one with
// no value fails as permanent, never dropped.
func TestRunResolvesParameters(t *testing.T) {
	const tool = `
c4: tool/1
name: x
kind: once
runs:
  image: c4/runtime-base
  command: [bash, -c, 'printf "%%s\n" "$@" > "$C4_TREE/args"', x, "{{Param.codec}}", "{{Param.stage}}", "{{Param.plates}}"]
params:
- {name: codec, type: STRING, default: prores-422hq}
- {name: stage, type: PATH%s}
- {name: plates, type: "LIST[PATH]", default: [plates/a.exr, /abs/b.exr]}
`
	r := newToolRun(t, fmt.Sprintf(tool, ""))
	r.vars["C4_PARAMS"] = `{"stage": "shots/sm0140.usda"}`
	if c := r.run(); c != 0 {
		t.Fatalf("exit %d\n%s", c, r.errs.String())
	}
	want := "prores-422hq\n" + filepath.Join(r.tree, "shots", "sm0140.usda") + "\n" +
		`["` + filepath.Join(r.tree, "plates", "a.exr") + `","/abs/b.exr"]` + "\n"
	if got := r.file("args"); got != want {
		t.Errorf("args:\n%s\nwant:\n%s", got, want)
	}
	for _, params := range []string{``, `{"stage": ""}`} {
		r = newToolRun(t, fmt.Sprintf(tool, ""))
		r.vars["C4_PARAMS"] = params
		if c := r.run(); c != setupCode {
			t.Fatalf("%q: exit %d", params, c)
		}
		if o := r.outcome(); o.Class != "permanent" || !strings.Contains(o.Reason, "parameter stage is required") {
			t.Errorf("%q: outcome %+v", params, o)
		}
	}
}

// With retryOn, an unclassified failure retries only on the codes it
// lists; any other is permanent. A classified one keeps its class.
func TestRunRetryOn(t *testing.T) {
	for code, class := range map[string]string{"137": "retry", "1": "permanent"} {
		r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'exit "+code+"']}\nretryOn: {exitCodes: [137, 139]}\n")
		r.run()
		if o := r.outcome(); o.Class != class {
			t.Errorf("exit %s: %+v, want %s", code, o, class)
		}
	}
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'echo c4_fail: licence no seat; exit 1']}\nretryOn: {exitCodes: [137]}\n")
	r.run()
	if o := r.outcome(); o.Class != "licence" {
		t.Errorf("classified: %+v", o)
	}
}

// A frame appearing in the tree is activity: it moves the stall clock as a
// log line does, so a frames tool that writes frames without a word to its
// log is working, not stalled. On an injected clock, not the wall's.
func TestFrameInTheTreeIsActivity(t *testing.T) {
	start := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	now := start
	tree := t.TempDir()
	r := &runner{e: env{now: func() time.Time { return now }}, tree: tree, tool: &schema.Tool{},
		frames: frames{start: 1001, end: 1008, step: 1, known: true}, start: start, last: start,
		done: map[int]bool{}, previewed: map[int]bool{}, previewQ: make(chan previewJob, 8)}
	now = start.Add(40 * time.Minute)
	if q := r.quiet(); q != 40*time.Minute {
		t.Fatalf("quiet %v before the frame", q)
	}
	r.countTreeFrame(1001, filepath.Join(tree, "render", "sm.1001.exr"))
	if q := r.quiet(); q != 0 {
		t.Fatalf("quiet %v after a frame landed", q)
	}
	if p := r.written; p != 0.125 {
		t.Errorf("progress %v, want 1 of 8", p)
	}
}

// The program is found on the PATH runs.env gives the tool.
func TestRunFindsTheProgramOnTheToolsPath(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "mytool"), []byte("#!/bin/sh\necho from mytool\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [mytool], env: {PATH: \""+bin+":/usr/bin:/bin\"}}\n")
	if c := r.run(); c != 0 || !strings.Contains(r.out.String(), "from mytool") {
		t.Fatalf("exit %d, log %q %s", c, r.out.String(), r.errs.String())
	}
}

// On a host whose OS has no runs.host.exe entry, the refusal says so.
func TestRunNamesTheMissingHostExe(t *testing.T) {
	other := "windows"
	if hostOS() == "windows" {
		other = "linux"
	}
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {host: {has: [nuke], exe: {"+other+": /bin/nuke}}, command: ['{{Run.Exe}}']}\n")
	r.vars["C4_RUNNER"] = "host"
	if c := r.run(); c != setupCode {
		t.Fatalf("exit %d", c)
	}
	if o := r.outcome(); !strings.Contains(o.Reason, "runs.host.exe has no entry for "+hostOS()) {
		t.Errorf("outcome %+v", o)
	}
}

// A secret is masked whichever of the tool's streams speaks first: here
// stdout is deep in a backlog when the tool announces the secret there and
// at once writes it to stderr, so a reader of two pipes would see stderr's
// line long before stdout's announcement.
func TestRunMasksAcrossStreams(t *testing.T) {
	r := newToolRun(t, `
c4: tool/1
name: x
kind: once
runs:
  image: c4/runtime-base
  command: [bash, -c, 'i=0; while [ $i -lt 800 ]; do echo "filler $i: a line long enough that the backlog outgrows the reader buffer, as a real tool log does"; i=$((i+1)); done; echo "openjd_redacted_env: TOKEN=s3cret"; echo "s3cret on stderr" >&2; echo "s3cret on stdout"']
`)
	// the line the tool announces the secret on is read late, behind a
	// backlog: as a loaded machine reads it. The backlog takes longer to
	// read than the stop grace, so the tool has exited well before its
	// last lines are read: they are read all the same.
	testReadPause = time.Millisecond
	t.Cleanup(func() { testReadPause = 0 })
	if c := r.run(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	log := r.out.String() + r.errs.String()
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "s3cret") {
			t.Errorf("unmasked: %q", l)
		}
	}
	if !strings.Contains(log, "filler 799:") || !strings.Contains(log, "******** on stderr") || !strings.Contains(log, "******** on stdout") {
		t.Errorf("lines are missing: %d bytes, ending %q", len(log), log[max(0, len(log)-200):])
	}
}

// A question a tool raises (c4_fail: question, or an errors.question
// line) is raised as c4-unit needs raises one: exit 12, with the question
// as the termination message, so the farm escalates it instead of spending
// a retry.
func TestRunRaisesAQuestion(t *testing.T) {
	for name, tool := range map[string]string{
		"protocol": "runs: {image: c4/runtime-base, command: [bash, -c, 'echo c4_fail: question which frame rate, 24 or 25; exit 3']}\n",
		"pattern":  "runs: {image: c4/runtime-base, command: [bash, -c, 'echo Error: which frame rate, 24 or 25; exit 3']}\nerrors: {question: ['which frame rate']}\n",
	} {
		r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\n"+tool)
		if c := r.run(); c != needsCode {
			t.Errorf("%s: exit %d, want %d", name, c, needsCode)
		}
		o := r.outcome()
		if o.Class != "question" || o.ExitCode != 3 {
			t.Errorf("%s: outcome %+v", name, o)
		}
		if b, _ := os.ReadFile(r.vars["C4_TERMINATION_LOG"]); !strings.Contains(string(b), "which frame rate, 24 or 25") {
			t.Errorf("%s: termination message %q", name, b)
		}
	}
}

// The strongest class stands, whichever source gives it: permanent, then
// question, then licence, then retry. A later, weaker c4_fail line does not
// overrule an earlier, stronger one, and a stronger class than the question
// c4-unit needs raised means it is not escalated as one.
func TestRunStrongestClassStands(t *testing.T) {
	for _, c := range []struct {
		name, script, errors string
		class                string
		code                 int
	}{
		{"protocol, stronger first", "echo c4_fail: permanent no such stage; echo c4_fail: retry flaky; exit 3", "", "permanent", 3},
		{"question over licence", "echo No licenses could be found; echo c4_fail: question 24 or 25; exit 3", "errors: {licence: ['No licenses']}\n", "question", needsCode},
		{"permanent over needs", `printf "the frame rate" > $C4_TERMINATION_LOG; echo c4_fail: permanent the stage is missing; exit 12`, "", "permanent", 1},
	} {
		r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, '"+c.script+"']}\n"+c.errors)
		if code := r.run(); code != c.code {
			t.Errorf("%s: exit %d, want %d", c.name, code, c.code)
		}
		if o := r.outcome(); o.Class != c.class {
			t.Errorf("%s: %+v, want %s", c.name, o, c.class)
		}
	}
}

// A re-check runs nothing: a tool cannot tell whether its question still
// applies, so the question stands as it was.
func TestRunDoesNotReRunOnARecheck(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'touch $C4_TREE/ran']}\n")
	r.vars["C4_RECHECK"], r.vars["C4_NEEDED"] = "1", "the delivery frame rate"
	if c := r.run(); c != needsCode {
		t.Fatalf("exit %d", c)
	}
	if _, err := os.Stat(filepath.Join(r.tree, "ran")); err == nil {
		t.Error("the tool ran on a re-check")
	}
	if b, _ := os.ReadFile(r.vars["C4_TERMINATION_LOG"]); string(b) != "the delivery frame rate" {
		t.Errorf("termination message %q", b)
	}
	if o := r.outcome(); o.Class != "question" {
		t.Errorf("outcome %+v", o)
	}
}

// An irregular chunk (C4_FRAMES 1001,1004,1010-1012) is its list of
// frames: its bounds, its count, the label as the farm wrote it, and only
// those frames counted.
func TestIrregularChunk(t *testing.T) {
	f, err := unitFrames(vars(map[string]string{"C4_FRAMES": "1001,1004,1010-1012", "C4_FRAME_START": "1001", "C4_FRAME_END": "1012"}))
	if err != nil {
		t.Fatal(err)
	}
	if f.start != 1001 || f.end != 1012 || f.count() != 5 || f.String() != "1001,1004,1010-1012" || !f.has(1004) || f.has(1005) {
		t.Fatalf("frames %+v count %d", f, f.count())
	}
	if f, err := unitFrames(vars(map[string]string{"C4_FRAMES": "1-9:4,2"})); err != nil || f.count() != 4 || !f.has(9) || f.has(3) {
		t.Fatalf("stepped list %+v %v", f, err)
	}
	if _, err := unitFrames(vars(map[string]string{"C4_FRAMES": "1001,x"})); err == nil {
		t.Error("a bad list accepted")
	}
}

// A question stands as it was asked: a tool that reports one and exits 12
// itself keeps its words, and a later openjd_fail line does not replace
// them.
func TestRunQuestionKeepsItsWords(t *testing.T) {
	for name, script := range map[string]string{
		"exit 12 itself":    "echo c4_fail: question which frame rate, 24 or 25; exit 12",
		"then openjd_fail":  "echo c4_fail: question which frame rate, 24 or 25; echo openjd_fail: the render stopped; exit 3",
		"openjd_fail first": "echo openjd_fail: the render stopped; echo c4_fail: question which frame rate, 24 or 25; exit 3",
	} {
		r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, '"+script+"']}\n")
		if c := r.run(); c != needsCode {
			t.Errorf("%s: exit %d", name, c)
		}
		if o := r.outcome(); o.Class != "question" || o.Reason != "which frame rate, 24 or 25" {
			t.Errorf("%s: outcome %+v", name, o)
		}
		if b, _ := os.ReadFile(r.vars["C4_TERMINATION_LOG"]); string(b) != "which frame rate, 24 or 25" {
			t.Errorf("%s: termination message %q", name, b)
		}
	}
}

// When a stronger class beats the question c4-unit needs raised, the
// termination message says the failure, not the question. A class the
// tool reported stands when it then stalls.
func TestRunFailureOverQuestionAndStall(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'printf \"the frame rate\" > $C4_TERMINATION_LOG; echo c4_fail: permanent the stage is missing; exit 12']}\n")
	if c := r.run(); c != 1 {
		t.Errorf("exit %d", c)
	}
	if b, _ := os.ReadFile(r.vars["C4_TERMINATION_LOG"]); string(b) != "the stage is missing" {
		t.Errorf("termination message %q", b)
	}

	r = newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'echo c4_fail: permanent no such stage; sleep 30']}\nstall: {quietMinutes: 2}\n")
	r.minute = 100 * time.Millisecond
	if c := r.run(); c != stallCode {
		t.Errorf("stalled: exit %d", c)
	}
	if o := r.outcome(); o.Class != "permanent" || o.Reason != "no such stage" || !o.Stalled {
		t.Errorf("stalled: outcome %+v", o)
	}
}

// A re-check of a tool that does not reason passes its question through
// as it was asked, why and what it is about included. A tool that reasons
// (kind agent) runs, told it is a re-check, and decides for itself.
func TestRunRecheckPassesTheQuestionThrough(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {image: c4/runtime-base, command: [bash, -c, 'touch $C4_TREE/ran']}\n")
	r.vars["C4_RECHECK"], r.vars["C4_NEEDED"] = "1", "the delivery frame rate"
	r.vars["C4_NEEDED_WHY"], r.vars["C4_NEEDED_ABOUT"] = "the plates disagree", "path:plates/a, unit:comp"
	if c := r.run(); c != needsCode {
		t.Fatalf("exit %d", c)
	}
	want := "the delivery frame rate\nwhy: the plates disagree\nabout: path:plates/a, unit:comp"
	if b, _ := os.ReadFile(r.vars["C4_TERMINATION_LOG"]); string(b) != want {
		t.Errorf("termination message %q, want %q", b, want)
	}
	if o := r.outcome(); o.Class != "question" || o.Reason != "the delivery frame rate" {
		t.Errorf("outcome %+v", o)
	}

	r = newToolRun(t, "c4: tool/1\nname: x\nkind: agent\nruns: {image: c4/runtime-base, command: [bash, -c, 'echo $C4_RECHECK > $C4_TREE/ran']}\n")
	r.vars["C4_RECHECK"], r.vars["C4_NEEDED"] = "1", "the delivery frame rate"
	if c := r.run(); c != 0 {
		t.Errorf("agent: exit %d", c)
	}
	if b, err := os.ReadFile(filepath.Join(r.tree, "ran")); err != nil || strings.TrimSpace(string(b)) != "1" {
		t.Errorf("an agent tool did not run its re-check: %q %v", b, err)
	}
}
