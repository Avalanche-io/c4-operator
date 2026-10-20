package main

// c4-unit run, the tool runner (proposals/authoring/TOOL.md section 4).
// Every unit that runs a tool starts it. It reads the tool's tool.yaml from
// $C4_TOOLS and the resolved parameters from $C4_PARAMS, fills in the
// command's format strings, and starts the command. It reads every line the
// tool prints for the progress protocol (openjd_*, c4_*) and the tool's
// patterns, writes .c4/progress, .c4/status and preview/<frame>.png, masks
// redacted values in the log, ends the tool after a stall, and on exit
// writes .c4/outcome and exits with the tool's code.
//
// What the lowering gives it: C4_TOOLS (the tool's tree), C4_PARAMS (a
// JSON object, defaults filled and PATHs absolute), C4_OUTPUT (the job's
// tree), the unit's frames (C4_FRAME_START, C4_FRAME_END and C4_FRAME_STEP,
// else C4_FRAMES), C4_JOB, C4_SHOT, C4_UNIT, C4_UNIT_NAME, and the
// submitter's roots for a path map (C4_SOURCE_ROOTS, [{os, path}]). On a
// host runner C4_RUNNER is host.
//
// What the tool gets besides: C4_TREE, a C4_PARAM_<NAME> per parameter, and
// C4_PATH_MAP when there is a map. Its exit status is the runner's, except
// 124 after a stall and 2 when it could not be started. A run that stops at
// a safe point (exit 10 after c4-unit safepoint or wait) leaves no outcome.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"sigs.k8s.io/yaml"
)

const (
	// stallCode is the runner's exit status after it ended a stalled tool,
	// as timeout(1)'s.
	stallCode = 124
	// setupCode is its exit status when the tool could not be started.
	setupCode = 2
	// stopGrace is how long a tool has between SIGTERM and SIGKILL.
	stopGrace = 10 * time.Second
	masked    = "********"
)

// tool is the part of a tool.yaml (c4: tool/1) the runner reads. Publish
// has validated the whole document, so this decodes leniently.
// TODO(agent-jobs): use schema.ParseTool once c4-authoring lands it.
type tool struct {
	Kind string `json:"kind"`
	Runs struct {
		Exe  string `json:"exe"`
		Host *struct {
			Exe map[string]string `json:"exe"`
		} `json:"host"`
		Command []string          `json:"command"`
		Env     map[string]string `json:"env"`
		Workdir string            `json:"workdir"`
	} `json:"runs"`
	Params []struct {
		Name string `json:"name"`
	} `json:"params"`
	Previews string `json:"previews"`
	Progress []struct {
		Match   string `json:"match"`
		Percent int    `json:"percent"`
		Frame   int    `json:"frame"`
	} `json:"progress"`
	Errors struct {
		Licence   []string `json:"licence"`
		Permanent []string `json:"permanent"`
		Question  []string `json:"question"`
		Retry     []string `json:"retry"`
		Ignore    []string `json:"ignore"`
	} `json:"errors"`
	Stall *struct {
		QuietMinutes int `json:"quietMinutes"`
	} `json:"stall"`
}

// outcome is .c4/outcome, which the node daemon relays and the queue acts
// on (slice 3): succeeded, or failed with a class and a reason.
type outcome struct {
	Outcome  string `json:"outcome"`
	Class    string `json:"class,omitempty"`
	Reason   string `json:"reason,omitempty"`
	ExitCode int    `json:"exitCode"`
	Stalled  bool   `json:"stalled,omitempty"`
}

// failClasses are the classes of a failure, strongest first: a missing
// seat explains the errors that follow it, and a permanent error means a
// retry cannot help.
var failClasses = []string{"licence", "permanent", "question", "retry"}

// frames is the unit's chunk: start to end by step.
type frames struct {
	start, end, step int
	known            bool
}

func (f frames) count() int {
	if !f.known {
		return 0
	}
	return (f.end-f.start)/f.step + 1
}

func (f frames) has(n int) bool {
	return f.known && n >= f.start && n <= f.end && (n-f.start)%f.step == 0
}

func (f frames) String() string {
	switch {
	case !f.known:
		return ""
	case f.start == f.end:
		return strconv.Itoa(f.start)
	case f.step > 1:
		return fmt.Sprintf("%d-%d:%d", f.start, f.end, f.step)
	}
	return fmt.Sprintf("%d-%d", f.start, f.end)
}

// unitFrames reads the chunk from C4_FRAME_START, C4_FRAME_END and
// C4_FRAME_STEP, else from the label in C4_FRAMES.
func unitFrames(e env) (frames, error) {
	s, en := e.get("C4_FRAME_START"), e.get("C4_FRAME_END")
	if s == "" {
		label := e.get("C4_FRAMES")
		if label == "" {
			return frames{}, nil
		}
		s, en, _ = strings.Cut(label, "-")
	}
	if en == "" {
		en = s
	}
	f := frames{step: 1, known: true}
	var err1, err2, err3 error
	f.start, err1 = strconv.Atoi(s)
	f.end, err2 = strconv.Atoi(en)
	if st := e.get("C4_FRAME_STEP"); st != "" {
		f.step, err3 = strconv.Atoi(st)
	}
	if err := errors.Join(err1, err2, err3); err != nil || f.step < 1 || f.end < f.start {
		return frames{}, fmt.Errorf("the unit's frames %s-%s (step %q) are not a range", s, en, e.get("C4_FRAME_STEP"))
	}
	return f, nil
}

// runner is one run of a tool.
type runner struct {
	e      env
	tree   string
	tool   tool
	params map[string]any
	frames frames
	start  time.Time

	progress []progressRule
	errs     map[string][]*regexp.Regexp
	ignore   []*regexp.Regexp
	previews *regexp.Regexp // over paths relative to the tree; group 1 is the frame
	previewQ chan previewJob

	mu        sync.Mutex
	masks     []string
	last      time.Time    // the tool's last line
	written   float64      // the progress last written
	done      map[int]bool // frames finished
	previewed map[int]bool // frames whose preview is queued
	class     string       // from c4_fail
	reason    string       // from c4_fail or openjd_fail
	patClass  string       // from the tool's error patterns
	patLine   string
}

type progressRule struct {
	re           *regexp.Regexp
	percent, frm int
}

type previewJob struct {
	src   string
	frame int
}

// runTool is `c4-unit run`.
func runTool(e env, stdout, stderr io.Writer) (int, error) {
	tree, err := output(e, "")
	if err != nil {
		return setupCode, err
	}
	r := &runner{e: e, tree: tree, start: e.clock(), done: map[int]bool{}, previewed: map[int]bool{}}
	cmd, err := r.prepare()
	if err != nil {
		r.writeOutcome(outcome{Outcome: "failed", Class: "permanent", Reason: err.Error(), ExitCode: setupCode})
		return setupCode, err
	}
	return r.run(cmd, stdout, stderr)
}

// prepare reads the tool and its parameters and builds the command.
func (r *runner) prepare() (*exec.Cmd, error) {
	dir := r.e.get("C4_TOOLS")
	if dir == "" {
		return nil, errors.New("C4_TOOLS is not set: the runner needs the tool's tree")
	}
	b, err := os.ReadFile(filepath.Join(dir, "tool.yaml"))
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, &r.tool); err != nil {
		return nil, fmt.Errorf("tool.yaml: %w", err)
	}
	r.params = map[string]any{}
	if p := r.e.get("C4_PARAMS"); p != "" {
		d := json.NewDecoder(strings.NewReader(p))
		d.UseNumber()
		if err := d.Decode(&r.params); err != nil {
			return nil, fmt.Errorf("C4_PARAMS is not a JSON object: %w", err)
		}
	}
	if r.frames, err = unitFrames(r.e); err != nil {
		return nil, err
	}
	if err := r.compile(); err != nil {
		return nil, err
	}

	// The path map comes first: {{Run.PathMap}} names it.
	pathMap, err := r.writePathMap()
	if err != nil {
		return nil, err
	}
	vals := r.values(dir, pathMap)
	var argv []string
	for i, a := range r.tool.Runs.Command {
		v, ok, err := expand(a, vals)
		if err != nil {
			return nil, fmt.Errorf("runs.command[%d]: %w", i, err)
		}
		if ok {
			argv = append(argv, v)
		} else if i == 0 {
			argv = append(argv, "")
		}
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("runs.command is empty once filled in (is runs.exe set?)")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = r.childEnv(pathMap)
	keys := make([]string, 0, len(r.tool.Runs.Env))
	for k := range r.tool.Runs.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if strings.HasPrefix(k, "C4_") {
			return nil, fmt.Errorf("runs.env.%s: the farm's C4_ variables cannot be set", k)
		}
		v, ok, err := expand(r.tool.Runs.Env[k], vals)
		if err != nil {
			return nil, fmt.Errorf("runs.env.%s: %w", k, err)
		}
		if ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if w := r.tool.Runs.Workdir; w != "" {
		v, ok, err := expand(w, vals)
		if err != nil || !ok {
			return nil, fmt.Errorf("runs.workdir %q: %v", w, orElse(err, "empty once filled in"))
		}
		if !filepath.IsAbs(v) {
			v = filepath.Join(r.tree, v)
		}
		cmd.Dir = v
	}
	return cmd, nil
}

func orElse(err error, otherwise string) string {
	if err != nil {
		return err.Error()
	}
	return otherwise
}

// compile reads the tool's patterns.
func (r *runner) compile() error {
	for i, p := range r.tool.Progress {
		re, err := regexp.Compile(p.Match)
		if err != nil {
			return fmt.Errorf("progress[%d].match: %w", i, err)
		}
		g := max(p.Percent, p.Frame)
		if g < 1 || g > re.NumSubexp() {
			return fmt.Errorf("progress[%d]: group %d is not in %q", i, g, p.Match)
		}
		r.progress = append(r.progress, progressRule{re: re, percent: p.Percent, frm: p.Frame})
	}
	lists := map[string][]string{
		"licence": r.tool.Errors.Licence, "permanent": r.tool.Errors.Permanent,
		"question": r.tool.Errors.Question, "retry": r.tool.Errors.Retry, "ignore": r.tool.Errors.Ignore,
	}
	r.errs = map[string][]*regexp.Regexp{}
	for class, pats := range lists {
		for i, p := range pats {
			re, err := regexp.Compile(p)
			if err != nil {
				return fmt.Errorf("errors.%s[%d]: %w", class, i, err)
			}
			if class == "ignore" {
				r.ignore = append(r.ignore, re)
			} else {
				r.errs[class] = append(r.errs[class], re)
			}
		}
	}
	if r.tool.Previews != "" {
		re, err := framePattern(r.tool.Previews)
		if err != nil {
			return fmt.Errorf("previews: %w", err)
		}
		r.previews = re
	}
	return nil
}

// framePattern turns a previews pattern (render/**/*.$F4.exr) into a
// regular expression over slash paths relative to the tree, its first
// group the frame number. A frame token is $F, $F4, <F>, <F4>, %d or %04d.
func framePattern(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	frame := false
	for i := 0; i < len(p); {
		rest := p[i:]
		if n := frameToken(rest); n > 0 {
			if frame {
				return nil, fmt.Errorf("%q has two frame tokens", p)
			}
			frame = true
			b.WriteString(`(-?[0-9]+)`)
			i += n
			continue
		}
		switch {
		case strings.HasPrefix(rest, "**/"):
			b.WriteString(`(?:[^/]+/)*`)
			i += 3
		case strings.HasPrefix(rest, "**"):
			b.WriteString(`.*`)
			i += 2
		case rest[0] == '*':
			b.WriteString(`[^/]*`)
			i++
		case rest[0] == '?':
			b.WriteString(`[^/]`)
			i++
		default:
			b.WriteString(regexp.QuoteMeta(rest[:1]))
			i++
		}
	}
	if !frame {
		return nil, fmt.Errorf("%q has no frame token ($F4, <F4> or %%04d)", p)
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

var frameTokenRE = regexp.MustCompile(`^(?:\$F[0-9]?|<F[0-9]?>|%0?[0-9]?d)`)

func frameToken(s string) int {
	return len(frameTokenRE.FindString(s))
}

// values are the format strings' values. A name that is known but has no
// value (an optional parameter left out, a job with no shot) is absent.
func (r *runner) values(toolDir, pathMap string) func(string) (string, bool, error) {
	declared := map[string]bool{}
	for _, p := range r.tool.Params {
		declared[p.Name] = true
	}
	f := r.frames
	n := func(i int) string { return strconv.Itoa(i) }
	return func(name string) (string, bool, error) {
		if p, ok := strings.CutPrefix(name, "Param."); ok {
			v, has := r.params[p]
			if !has && !declared[p] {
				return "", false, fmt.Errorf("{{%s}}: the tool has no parameter %q", name, p)
			}
			s, ok := scalar(v)
			return s, ok && s != "", nil
		}
		var v string
		switch name {
		case "Unit.Frames":
			v = f.String()
		case "Unit.FrameStart":
			v = ifKnown(f.known, n(f.start))
		case "Unit.FrameEnd":
			v = ifKnown(f.known, n(f.end))
		case "Unit.FrameCount":
			v = ifKnown(f.known, n(f.count()))
		case "Unit.Name":
			v = r.e.get("C4_UNIT_NAME")
			if v == "" {
				v = r.e.get("C4_UNIT_LABEL")
			}
		case "Unit.Index":
			v = r.e.get("C4_UNIT")
		case "Job.Tree":
			v = r.tree
		case "Job.Shot":
			v = r.e.get("C4_SHOT")
		case "Job.Name":
			v = r.e.get("C4_JOB")
		case "Tool.Dir":
			v = toolDir
		case "Run.Exe":
			v = r.exe()
		case "Run.PathMap":
			v = pathMap
		default:
			return "", false, fmt.Errorf("{{%s}} is not a format string the runner knows", name)
		}
		return v, v != "", nil
	}
}

func ifKnown(known bool, s string) string {
	if known {
		return s
	}
	return ""
}

// exe is {{Run.Exe}}: runs.exe in a container, runs.host.exe for the
// host's operating system on a host runner.
func (r *runner) exe() string {
	if r.e.get("C4_RUNNER") != "host" {
		return r.tool.Runs.Exe
	}
	if r.tool.Runs.Host == nil {
		return ""
	}
	goos := runtime.GOOS
	if goos == "darwin" {
		goos = "macos"
	}
	return r.tool.Runs.Host.Exe[goos]
}

var formatRE = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}\}`)

// expand fills in a string's format strings. ok is false when a value it
// names is absent, which removes the argument (or the variable).
func expand(s string, val func(string) (string, bool, error)) (string, bool, error) {
	ok := true
	var err error
	out := formatRE.ReplaceAllStringFunc(s, func(m string) string {
		v, has, e := val(formatRE.FindStringSubmatch(m)[1])
		if e != nil && err == nil {
			err = e
		}
		ok = ok && has
		return v
	})
	if err == nil && strings.Contains(out, "{{") {
		err = fmt.Errorf("%q has a malformed format string", s)
	}
	return out, ok, err
}

// scalar is a parameter's value as one string: numbers as written, booleans
// as true or false, lists and objects as JSON. A missing or null value is
// absent.
func scalar(v any) (string, bool) {
	switch v := v.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	}
	b, _ := json.Marshal(v)
	return string(b), true
}

// envName is a parameter's variable: C4_PARAM_ and its name in upper snake
// case (frameRange is C4_PARAM_FRAME_RANGE).
func envName(p string) string {
	var b strings.Builder
	b.WriteString("C4_PARAM_")
	rs := []rune(p)
	for i, c := range rs {
		if unicode.IsUpper(c) && i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1])) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(c))
	}
	return b.String()
}

// childEnv is the tool's environment: the unit's, plus C4_TREE, a
// variable per parameter, and the path map.
func (r *runner) childEnv(pathMap string) []string {
	environ := os.Environ
	if r.e.environ != nil {
		environ = r.e.environ
	}
	out := slices.DeleteFunc(environ(), func(kv string) bool { return strings.HasPrefix(kv, "C4_PARAM_") })
	out = append(out, "C4_TREE="+r.tree)
	names := make([]string, 0, len(r.params))
	for k := range r.params {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		if s, ok := scalar(r.params[k]); ok {
			out = append(out, envName(k)+"="+s)
		}
	}
	if pathMap != "" {
		out = append(out, "C4_PATH_MAP="+pathMap)
	}
	return out
}

// writePathMap writes OpenJD's pathmapping-1.0 rules from each root the
// submitter recorded ($C4_SOURCE_ROOTS) to the tree, and returns its path,
// or "" with no roots.
func (r *runner) writePathMap() (string, error) {
	src := r.e.get("C4_SOURCE_ROOTS")
	if src == "" {
		return "", nil
	}
	var roots []struct{ OS, Path string }
	if err := json.Unmarshal([]byte(src), &roots); err != nil {
		return "", fmt.Errorf("C4_SOURCE_ROOTS: %w", err)
	}
	type rule struct {
		SourcePathFormat string `json:"source_path_format"`
		SourcePath       string `json:"source_path"`
		DestinationPath  string `json:"destination_path"`
	}
	doc := struct {
		Version string `json:"version"`
		Rules   []rule `json:"path_mapping_rules"`
	}{Version: "pathmapping-1.0", Rules: []rule{}}
	for _, root := range roots {
		format := "POSIX"
		switch strings.ToLower(root.OS) {
		case "windows":
			format = "WINDOWS"
		case "linux", "macos", "darwin", "posix":
		default:
			return "", fmt.Errorf("C4_SOURCE_ROOTS: os %q is not windows, linux or macos", root.OS)
		}
		if root.Path == "" || root.Path == r.tree && format == "POSIX" {
			continue
		}
		doc.Rules = append(doc.Rules, rule{format, root.Path, r.tree})
	}
	if len(doc.Rules) == 0 {
		return "", nil
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	p := filepath.Join(r.tree, ".c4", "path-map.json")
	return p, writeAtomic(p, append(b, '\n'))
}

// run starts the tool and reads it until it exits.
func (r *runner) run(cmd *exec.Cmd, stdout, stderr io.Writer) (int, error) {
	// Pipes of our own, not cmd's: Wait would close cmd's at the tool's
	// exit, and a child it left behind holding stdout would hang its own.
	outR, outW, err := os.Pipe()
	if err != nil {
		return setupCode, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return setupCode, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r.last = r.e.clock()
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		r.writeOutcome(outcome{Outcome: "failed", Class: "permanent", Reason: "could not start the tool: " + err.Error(), ExitCode: setupCode})
		return setupCode, err
	}

	r.previewQ = make(chan previewJob, 1024)
	var previewsDone sync.WaitGroup
	previewsDone.Add(1)
	go func() { defer previewsDone.Done(); r.makePreviews() }()

	var reading sync.WaitGroup
	reading.Add(2)
	go func() { defer reading.Done(); r.read(outR, stdout, true) }()
	go func() { defer reading.Done(); r.read(errR, stderr, false) }()
	readDone := make(chan struct{})
	go func() { reading.Wait(); close(readDone) }()

	exited := make(chan struct{})
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
		close(exited)
	}()

	sigs := r.e.signals
	if sigs == nil {
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(ch)
		sigs = ch
	}
	var watching sync.WaitGroup
	stopWatch := make(chan struct{})
	if r.previews != nil {
		watching.Add(1)
		go func() { defer watching.Done(); r.watchTree(stopWatch) }()
	}

	var stalled bool
	var signalled os.Signal
	tick := time.NewTicker(r.tickEvery())
	defer tick.Stop()
	pgid := cmd.Process.Pid
wait:
	for {
		select {
		case <-exited:
			break wait
		case s := <-sigs:
			signalled = s
			signalGroup(pgid, s)
		case <-tick.C:
			if stalled || r.tool.Stall == nil || r.tool.Stall.QuietMinutes < 1 {
				continue
			}
			r.mu.Lock()
			quiet := r.e.clock().Sub(r.last)
			r.mu.Unlock()
			if quiet < time.Duration(r.tool.Stall.QuietMinutes)*r.e.minute() {
				continue
			}
			stalled = true
			fmt.Fprintf(stderr, "c4-unit: no output for %d minutes: ending the tool\n", r.tool.Stall.QuietMinutes)
			signalGroup(pgid, syscall.SIGTERM)
			go func() {
				select {
				case <-exited:
				case <-time.After(r.e.grace()):
					signalGroup(pgid, syscall.SIGKILL)
				}
			}()
		}
	}
	// The last lines, unless something the tool left behind keeps the
	// pipes open.
	select {
	case <-readDone:
	case <-time.After(r.e.grace()):
		outR.Close()
		errR.Close()
		<-readDone
	}
	close(stopWatch)
	watching.Wait()
	if r.previews != nil {
		r.scanTree(nil, true)
	}
	close(r.previewQ)
	previewsDone.Wait()

	code := exitCode(cmd.ProcessState, <-waitErr)
	o := r.result(code, stalled, signalled)
	r.writeOutcome(o)
	if o.Outcome == "succeeded" {
		r.setProgress(1)
	}
	if stalled {
		return stallCode, nil
	}
	return code, nil
}

func signalGroup(pgid int, s os.Signal) {
	if sig, ok := s.(syscall.Signal); ok {
		_ = syscall.Kill(-pgid, sig)
	}
}

func exitCode(ps *os.ProcessState, err error) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if c := ps.ExitCode(); c >= 0 {
		return c
	}
	if err != nil {
		return 1
	}
	return 0
}

// result is the run's outcome. A tool that stopped at a safe point (exit
// 10 after c4-unit safepoint or wait, in this run) has none: it neither
// finished nor failed, and runs again.
func (r *runner) result(code int, stalled bool, signalled os.Signal) outcome {
	if code == stopCode && !stalled && r.touched(filepath.Join(r.tree, ".c4", "safepoint")) {
		return outcome{}
	}
	if code == 0 && !stalled {
		return outcome{Outcome: "succeeded"}
	}
	needs, needed := "", false
	if code == needsCode {
		needs, needed = r.needs()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	o := outcome{Outcome: "failed", ExitCode: code, Stalled: stalled}
	switch {
	case stalled:
		o.Class, o.Reason = "retry", fmt.Sprintf("no output for %d minutes", r.tool.Stall.QuietMinutes)
	case needed:
		o.Class, o.Reason = "question", "the brief lacks information: "+needs
	case r.class != "":
		o.Class = r.class
	case r.patClass != "":
		o.Class = r.patClass
	default:
		o.Class = "retry"
	}
	if o.Reason == "" {
		o.Reason = r.reason
	}
	if o.Reason == "" {
		o.Reason = r.patLine
	}
	if o.Reason == "" && signalled != nil {
		o.Reason = "stopped by " + signalled.String()
	}
	if o.Reason == "" {
		o.Reason = "exit status " + strconv.Itoa(code)
	}
	o.Reason = r.maskLocked(o.Reason)
	return o
}

// touched reports whether the file was written during this run.
func (r *runner) touched(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.ModTime().Before(r.start.Add(-time.Second))
}

// needs is what `c4-unit needs` left in the termination log in this run.
func (r *runner) needs() (string, bool) {
	p := r.e.get("C4_TERMINATION_LOG")
	if p == "" {
		p = "/dev/termination-log"
	}
	if !r.touched(p) {
		return "", false
	}
	b, _ := os.ReadFile(p)
	s := strings.TrimSpace(string(b))
	return strings.ReplaceAll(s, "\n", "; "), s != ""
}

func (r *runner) writeOutcome(o outcome) {
	p := filepath.Join(r.tree, ".c4", "outcome")
	if o.Outcome == "" {
		os.Remove(p)
		return
	}
	b, _ := json.Marshal(o)
	if err := writeAtomic(p, append(b, '\n')); err != nil {
		fmt.Fprintln(r.e.errOut(), "c4-unit: outcome:", err)
	}
}

// read copies one of the tool's streams to the log, a line at a time,
// masked, and reads each line. The protocol is read on stdout only; the
// patterns on both.
func (r *runner) read(src io.Reader, dst io.Writer, protocol bool) {
	br := bufio.NewReaderSize(src, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			r.line(strings.TrimRight(line, "\r\n"), dst, protocol)
		}
		if err != nil {
			return
		}
	}
}

func (r *runner) line(l string, dst io.Writer, protocol bool) {
	r.mu.Lock()
	r.last = r.e.clock()
	if protocol {
		if v, ok := cutProto(l, "openjd_redacted_env"); ok {
			if _, secret, ok := strings.Cut(v, "="); ok && secret != "" {
				r.masks = append(r.masks, secret)
			}
		}
	}
	shown := r.maskLocked(l)
	r.mu.Unlock()
	io.WriteString(dst, shown+"\n")

	if protocol && r.protocol(shown) {
		return
	}
	for _, re := range r.ignore {
		if re.MatchString(l) {
			return
		}
	}
	r.patterns(l, shown)
}

func cutProto(l, key string) (string, bool) {
	v, ok := strings.CutPrefix(l, key+":")
	return strings.TrimSpace(v), ok
}

// protocol reads one protocol line, and reports whether it was one.
func (r *runner) protocol(l string) bool {
	if v, ok := cutProto(l, "openjd_progress"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			r.setProgress(f / 100)
		}
		return true
	}
	if v, ok := cutProto(l, "openjd_status"); ok {
		if err := writeAtomic(filepath.Join(r.tree, ".c4", "status"), []byte(v+"\n")); err != nil {
			fmt.Fprintln(r.e.errOut(), "c4-unit: status:", err)
		}
		return true
	}
	if v, ok := cutProto(l, "openjd_fail"); ok {
		r.mu.Lock()
		r.reason = v
		r.mu.Unlock()
		return true
	}
	if _, ok := cutProto(l, "openjd_redacted_env"); ok {
		return true
	}
	if v, ok := cutProto(l, "c4_frame"); ok {
		num, path, _ := strings.Cut(v, " ")
		n, err := strconv.Atoi(num)
		if err != nil {
			return true
		}
		r.frameDone(n, strings.TrimSpace(path))
		return true
	}
	if v, ok := cutProto(l, "c4_fail"); ok {
		class, reason, _ := strings.Cut(v, " ")
		r.mu.Lock()
		if slices.Contains(failClasses, class) {
			r.class, r.reason = class, strings.TrimSpace(reason)
		} else {
			r.class, r.reason = "retry", v
		}
		r.mu.Unlock()
		return true
	}
	return false
}

// patterns reads a line with the tool's progress and error patterns. The
// strongest class any line matched is the failure's, and the first line
// of it the reason. Strongest, not first: stdout and stderr are read
// apart, so which of two streams' lines came first is not known.
func (r *runner) patterns(raw, shown string) {
	for _, p := range r.progress {
		m := p.re.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		if p.percent > 0 {
			if f, err := strconv.ParseFloat(m[p.percent], 64); err == nil {
				r.setProgress(f / 100)
			}
		} else if n, err := strconv.Atoi(m[p.frm]); err == nil {
			r.frameDone(n, "")
		}
	}
	for rank, class := range failClasses {
		for _, re := range r.errs[class] {
			if !re.MatchString(raw) {
				continue
			}
			r.mu.Lock()
			if r.patClass == "" || rank < slices.Index(failClasses, r.patClass) {
				r.patClass, r.patLine = class, truncate(strings.TrimSpace(shown), 500)
			}
			r.mu.Unlock()
			return
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (r *runner) maskLocked(s string) string {
	for _, m := range r.masks {
		s = strings.ReplaceAll(s, m, masked)
	}
	return s
}

// setProgress writes .c4/progress. Within a run it never goes back.
func (r *runner) setProgress(f float64) {
	f = min(max(f, 0), 1)
	r.mu.Lock()
	if f <= r.written {
		r.mu.Unlock()
		return
	}
	r.written = f
	r.mu.Unlock()
	s := strconv.FormatFloat(f, 'f', 4, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if err := writeAtomic(filepath.Join(r.tree, ".c4", "progress"), []byte(s+"\n")); err != nil {
		fmt.Fprintln(r.e.errOut(), "c4-unit: progress:", err)
	}
}

// frameDone counts a finished frame toward progress and, with a path,
// queues its preview.
func (r *runner) frameDone(n int, path string) {
	r.mu.Lock()
	r.done[n] = true
	done := 0
	for f := range r.done {
		if !r.frames.known || r.frames.has(f) {
			done++
		}
	}
	r.mu.Unlock()
	if total := r.frames.count(); total > 0 {
		r.setProgress(float64(done) / float64(total))
	}
	if path != "" {
		r.queuePreview(n, path)
	}
}

// queuePreview queues a frame's preview, once per frame.
func (r *runner) queuePreview(n int, path string) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.tree, path)
	}
	r.mu.Lock()
	had := r.previewed[n]
	r.previewed[n] = true
	r.mu.Unlock()
	if had {
		return
	}
	select {
	case r.previewQ <- previewJob{path, n}:
	default:
		fmt.Fprintf(r.e.errOut(), "c4-unit: no preview of frame %d: the queue is full\n", n)
	}
}

// makePreviews makes each queued frame into preview/<frame>.png. After the
// first failure (no oiiotool in the image, say) it stops trying.
func (r *runner) makePreviews() {
	failed := false
	for j := range r.previewQ {
		if failed {
			continue
		}
		dst := filepath.Join(r.tree, "preview", strconv.Itoa(j.frame)+".png")
		if err := preview(r.e, j.src, dst, 960); err != nil {
			fmt.Fprintf(r.e.errOut(), "c4-unit: no previews: frame %d: %v\n", j.frame, err)
			failed = true
		}
	}
}

// watchTree finds frames the tool writes that match its previews pattern.
// With no progress rules, they are its progress.
func (r *runner) watchTree(stop <-chan struct{}) {
	seen := map[string]fileState{}
	r.scanTree(seen, false) // what the tree held before: not this run's
	for k := range seen {
		seen[k] = fileState{size: seen[k].size, mod: seen[k].mod, counted: true}
	}
	t := time.NewTicker(r.e.poll())
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.scanTree(seen, false)
		}
	}
}

type fileState struct {
	size    int64
	mod     time.Time
	stable  int
	counted bool
}

// scanTree looks for frames under the previews pattern's fixed prefix. A
// frame counts once its size and time have held for one scan (or at the
// end, final), and once only, unless it is written again. seen nil is the
// final scan: anything written since the run began counts.
func (r *runner) scanTree(seen map[string]fileState, final bool) {
	root := r.tree
	if pre := fixedPrefix(r.tool.Previews); pre != "" {
		root = filepath.Join(r.tree, pre)
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(r.tree, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".c4" || rel == "preview" {
				return filepath.SkipDir
			}
			return nil
		}
		m := r.previews.FindStringSubmatch(rel)
		if m == nil {
			return nil
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || r.frames.known && !r.frames.has(n) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if seen == nil {
			if final && !info.ModTime().Before(r.start.Add(-time.Second)) {
				r.countTreeFrame(n, p)
			}
			return nil
		}
		s, had := seen[rel]
		switch {
		case !had:
			seen[rel] = fileState{size: info.Size(), mod: info.ModTime()}
		case s.size != info.Size() || !s.mod.Equal(info.ModTime()):
			seen[rel] = fileState{size: info.Size(), mod: info.ModTime()}
		case !s.counted:
			s.counted = true
			seen[rel] = s
			r.countTreeFrame(n, p)
		}
		return nil
	})
}

// countTreeFrame takes a frame found in the tree: its preview, and, when
// the tool has no progress rules, its progress.
func (r *runner) countTreeFrame(n int, path string) {
	if len(r.progress) == 0 {
		r.frameDone(n, "")
	}
	r.queuePreview(n, path)
}

// fixedPrefix is the leading directories of a pattern that hold no
// wildcard or frame token (render/ in render/**/*.$F4.exr).
func fixedPrefix(p string) string {
	dirs := strings.Split(p, "/")
	var out []string
	for _, d := range dirs[:len(dirs)-1] {
		if strings.ContainsAny(d, "*?$<%") {
			break
		}
		out = append(out, d)
	}
	return filepath.FromSlash(strings.Join(out, "/"))
}

func (r *runner) tickEvery() time.Duration {
	if r.tool.Stall == nil || r.tool.Stall.QuietMinutes < 1 {
		return time.Hour
	}
	return min(max(time.Duration(r.tool.Stall.QuietMinutes)*r.e.minute()/20, 10*time.Millisecond), 5*time.Second)
}

func (e env) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e env) minute() time.Duration {
	if e.minuteLen > 0 {
		return e.minuteLen
	}
	return time.Minute
}

func (e env) grace() time.Duration {
	if e.stopGrace > 0 {
		return e.stopGrace
	}
	return stopGrace
}

func (e env) poll() time.Duration {
	if e.pollEvery > 0 {
		return e.pollEvery
	}
	return 2 * time.Second
}

func (e env) errOut() io.Writer {
	if e.stderr != nil {
		return e.stderr
	}
	return os.Stderr
}
