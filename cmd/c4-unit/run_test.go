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
- {name: note, type: STRING}
- {name: empty, type: STRING}
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
		"C4_PARAM_COUNT=3\n", `C4_PARAM_PLATES=["/show/a.exr","/show/b.exr"]` + "\n", "C4_PARAM_EMPTY=\n",
		"C4_TREE=" + r.tree + "\n", "SHOTVAR=" + r.tree + "//show/sm0140.usda\n",
	} {
		if !strings.Contains(env, w) {
			t.Errorf("env lacks %q:\n%s", w, env)
		}
	}
	for _, not := range []string{"NOVAR", "C4_PARAM_STALE", "C4_PARAM_NOTE"} {
		if strings.Contains(env, not) {
			t.Errorf("env has %s:\n%s", not, env)
		}
	}
	if o := r.outcome(); o.Outcome != "succeeded" || o.Class != "" {
		t.Errorf("outcome %+v", o)
	}
	if p := r.file(".c4/progress"); p != "1\n" {
		t.Errorf("progress %q", p)
	}
}

func TestRunRefusesWhatCannotRun(t *testing.T) {
	for name, c := range map[string]struct{ yaml, want string }{
		"farm variable": {`runs: {command: [true], env: {C4_JOB: x}}`, "runs.env.C4_JOB: the farm's C4_ variables cannot be set"},
		"unknown param": {`runs: {command: [echo, "{{Param.nope}}"]}`, `the tool has no parameter "nope"`},
		"unknown name":  {`runs: {command: [echo, "{{Task.Frame}}"]}`, "{{Task.Frame}} is not a format string"},
		"no exe":        {`runs: {command: ["{{Run.Exe}}", -V]}`, "runs.command is empty once filled in"},
		"bad regex":     {"runs: {command: [true]}\nerrors: {retry: ['(']}", "errors.retry[0]"},
		"no group":      {"runs: {command: [true]}\nprogress: [{match: 'x', percent: 1}]", "group 1 is not in"},
		"no program":    {`runs: {command: [/nonexistent/husk]}`, "could not start the tool"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\n"+c.yaml)
			if code := r.run(); code != setupCode {
				t.Fatalf("exit %d", code)
			}
			if o := r.outcome(); o.Outcome != "failed" || o.Class != "permanent" || !strings.Contains(o.Reason, c.want) {
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
  command: [bash, -c, 'echo openjd_status: loading the stage; echo openjd_progress: 12.5; echo "openjd_redacted_env: TOKEN=s3cret"; echo "using s3cret now"; sleep 0.1; echo "s3cret on stderr" >&2; echo c4_frame: 1001 f.1001.png; echo c4_frame: 1003; echo openjd_fail: will not be the reason; echo c4_fail: permanent the scene names a missing asset; exit 3']
`)
	r.vars["C4_FRAME_END"] = "1003"
	writePNG(t, filepath.Join(r.tree, "f.1001.png"))
	if c := r.run(); c != 3 {
		t.Fatalf("exit %d", c)
	}
	o := r.outcome()
	if o.Outcome != "failed" || o.Class != "permanent" || o.Reason != "the scene names a missing asset" || o.ExitCode != 3 {
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
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {command: [bash, -c, 'exit 7']}\n")
	if c := r.run(); c != 7 {
		t.Fatalf("exit %d", c)
	}
	if o := r.outcome(); o.Class != "retry" || o.Reason != "exit status 7" || o.ExitCode != 7 {
		t.Errorf("outcome %+v", o)
	}
}

func TestRunEndsAStall(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {command: [bash, -c, 'echo started; sleep 30']}\nstall: {quietMinutes: 2}\n")
	r.minute = 100 * time.Millisecond
	began := time.Now()
	if c := r.run(); c != stallCode {
		t.Fatalf("exit %d", c)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Errorf("the stall took %v to end", d)
	}
	if o := r.outcome(); o.Class != "retry" || !o.Stalled || o.Reason != "no output for 2 minutes" {
		t.Errorf("outcome %+v", o)
	}
}

func TestRunForwardsSIGTERM(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {command: [bash, -c, 'trap \"echo stopping; exit 143\" TERM; echo ready; while :; do sleep 0.05; done']}\n")
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
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {command: [bash, -c, '(sleep 5 &); echo done']}\n")
	began := time.Now()
	if c := r.run(); c != 0 {
		t.Fatalf("exit %d", c)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("waited %v for the child's stdout", d)
	}
}

func TestRunSafePointAndNeeds(t *testing.T) {
	r := newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {command: [bash, -c, 'mkdir -p $C4_OUTPUT/.c4; echo 1 > $C4_OUTPUT/.c4/safepoint; exit 10']}\n")
	writeAtomic(filepath.Join(r.tree, ".c4", "outcome"), []byte(`{"outcome":"failed"}`))
	if c := r.run(); c != stopCode {
		t.Fatalf("exit %d", c)
	}
	if _, err := os.Stat(filepath.Join(r.tree, ".c4", "outcome")); !os.IsNotExist(err) {
		t.Errorf("a stop at a safe point left an outcome: %v", err)
	}

	r = newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {command: [bash, -c, 'printf \"the frame rate\\nwhy: 24 or 25\" > $C4_TERMINATION_LOG; exit 12']}\n")
	if c := r.run(); c != needsCode {
		t.Fatalf("exit %d", c)
	}
	if o := r.outcome(); o.Class != "question" || o.Reason != "the brief lacks information: the frame rate; why: 24 or 25" {
		t.Errorf("outcome %+v", o)
	}

	// exit 10 with no safe point in this run is a failure like any other
	r = newToolRun(t, "c4: tool/1\nname: x\nkind: once\nruns: {command: [bash, -c, 'exit 10']}\n")
	r.run()
	if o := r.outcome(); o.Outcome != "failed" || o.ExitCode != 10 {
		t.Errorf("outcome %+v", o)
	}
}

func TestRunCountsFramesInTheTree(t *testing.T) {
	r := newToolRun(t, `
c4: tool/1
name: x
kind: frames
runs:
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
runs: {command: [bash, -c, 'echo "$1 $HOUDINI_PATHMAP $C4_PATH_MAP"', x, "{{Run.PathMap}}"], env: {HOUDINI_PATHMAP: "{{Run.PathMap}}"}}
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
runs: {command: [bash, -c, 'echo "[$#] [${HOUDINI_PATHMAP-unset}]"', x, "{{Run.PathMap}}"], env: {HOUDINI_PATHMAP: "{{Run.PathMap}}"}}
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
