package farmcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4-operator/work/order"
)

// ToolFile is what the CLI reads of a tool.yaml (c4: tool/1, the
// authoring proposal's TOOL.md). The farm reads the whole file strictly
// against the tool schema when it is published; the CLI reads what it
// needs to test a tool and to fill in a submission.
type ToolFile struct {
	C4      string      `json:"c4"`
	Name    string      `json:"name"`
	Title   string      `json:"title"`
	Summary string      `json:"summary"`
	Kind    string      `json:"kind"`
	Runs    ToolRuns    `json:"runs"`
	Frames  *ToolFrames `json:"frames,omitempty"`
	Params  []ToolParam `json:"params,omitempty"`
	Test    *ToolTest   `json:"test,omitempty"`
}

// ToolRuns is where a tool runs. Slice 1 runs container tools only.
type ToolRuns struct {
	Image string          `json:"image,omitempty"`
	Host  json.RawMessage `json:"host,omitempty"`
}

// ToolFrames are a frames tool's defaults.
type ToolFrames struct {
	Chunk int    `json:"chunk,omitempty"`
	Order string `json:"order,omitempty"`
}

// ToolParam is one of a tool's parameters (an OpenJD job parameter
// definition), as far as the CLI needs it.
type ToolParam struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Default    any    `json:"default,omitempty"`
	ObjectType string `json:"objectType,omitempty"`
	DataFlow   string `json:"dataFlow,omitempty"`
}

// ToolTest is a tool's test: a tree, frames, parameters and the files it
// must leave.
type ToolTest struct {
	Tree   string         `json:"tree"`
	Frames string         `json:"frames,omitempty"`
	With   map[string]any `json:"with,omitempty"`
	Expect []string       `json:"expect,omitempty"`
}

var (
	toolNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	toolKinds  = map[string]bool{"frames": true, "once": true, "shards": true, "generator": true, "check": true, "agent": true, "train": true}
)

// ReadTool reads and checks dir/tool.yaml: the fields the CLI relies on.
// Every problem is reported, each with its field.
func ReadTool(dir string) (*ToolFile, error) {
	data, err := os.ReadFile(filepath.Join(dir, "tool.yaml"))
	if err != nil {
		return nil, fmt.Errorf("%s has no tool.yaml (c4 farm tool new makes one)", dir)
	}
	var t ToolFile
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("tool.yaml: %w", err)
	}
	var probs []string
	bad := func(field, format string, a ...any) { probs = append(probs, field+": "+fmt.Sprintf(format, a...)) }
	if t.C4 != "tool/1" {
		bad("c4", "is %q; a tool says c4: tool/1", t.C4)
	}
	if !toolNameRE.MatchString(t.Name) {
		bad("name", "%q: lowercase letters, digits and -, starting with a letter", t.Name)
	}
	if !toolKinds[t.Kind] {
		bad("kind", "%q: one of frames, once, shards, generator, check, agent, train", t.Kind)
	}
	if t.Runs.Image == "" && len(t.Runs.Host) == 0 {
		bad("runs", "needs an image (or a host)")
	}
	seen := map[string]bool{}
	for i, p := range t.Params {
		if p.Name == "" || seen[p.Name] {
			bad(fmt.Sprintf("params[%d].name", i), "%q is empty or used twice", p.Name)
		}
		seen[p.Name] = true
	}
	if t.Test != nil {
		for k := range t.Test.With {
			if !seen[k] {
				bad("test.with."+k, "the tool has no parameter %q", k)
			}
		}
		if t.Kind == "frames" && t.Test.Frames == "" {
			bad("test.frames", "a frames tool's test names its frames, for example 1001-1002")
		}
	}
	if len(probs) > 0 {
		return &t, &ToolError{Problems: probs}
	}
	return &t, nil
}

// ToolError lists what is wrong with a tool.yaml.
type ToolError struct{ Problems []string }

func (e *ToolError) Error() string {
	return "tool.yaml:\n  " + strings.Join(e.Problems, "\n  ")
}

// ResolveImage turns a tool's image into one Docker can pull: the farm's
// short names (c4/runtime-oiio, c4/job-hydra) are its own images,
// ghcr.io/avalanche-io/c4-runtime-oiio at this release.
func ResolveImage(ref string) string {
	if rest, ok := strings.CutPrefix(ref, "c4/"); ok {
		return ImageRepo + "/c4-" + rest + ":" + ImageTag
	}
	return ref
}

// paramValues are a unit's parameters: the tool's defaults, then the
// test's, then the person's; a PATH becomes absolute under the tree.
func (t *ToolFile) paramValues(tree string, with ...map[string]any) (map[string]any, error) {
	out := map[string]any{}
	types := map[string]ToolParam{}
	for _, p := range t.Params {
		types[p.Name] = p
		if p.Default != nil {
			out[p.Name] = p.Default
		}
	}
	for _, w := range with {
		for k, v := range w {
			if _, ok := types[k]; !ok {
				return nil, fmt.Errorf("%s has no parameter %q", t.Name, k)
			}
			out[k] = v
		}
	}
	for k, v := range out {
		if types[k].Type == "PATH" {
			if s, ok := v.(string); ok && s != "" && !filepath.IsAbs(s) {
				out[k] = tree + "/" + strings.TrimPrefix(s, "./")
			}
		}
	}
	return out, nil
}

// paramEnv is C4_PARAMS and C4_PARAM_<NAME>, as the farm sets them.
func paramEnv(params map[string]any) ([]string, error) {
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	env := []string{"C4_PARAMS=" + string(b)}
	names := make([]string, 0, len(params))
	for k := range params {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v := params[k]
		s, ok := v.(string)
		if !ok {
			vb, _ := json.Marshal(v)
			s = string(vb)
		}
		env = append(env, "C4_PARAM_"+strings.ToUpper(strings.ReplaceAll(k, "-", "_"))+"="+s)
	}
	return env, nil
}

// testUnit is one unit of a tool's test: its frames, as a range.
type testUnit struct {
	label      string
	start, end int
}

// testUnits splits the test's frames into the tool's chunks; a tool
// without frames runs one unit.
func (t *ToolFile) testUnits(frames string) ([]testUnit, error) {
	if t.Kind != "frames" {
		// one unit, as the farm runs it: the single frame "1"
		return []testUnit{{label: "0", start: 1, end: 1}}, nil
	}
	fs, err := order.ParseFrames(frames)
	if err != nil {
		return nil, fmt.Errorf("frames %q: %w", frames, err)
	}
	chunk := 1
	if t.Frames != nil && t.Frames.Chunk > 0 {
		chunk = t.Frames.Chunk
	}
	var out []testUnit
	for i := 0; i < len(fs); {
		j := i
		for j+1 < len(fs) && j+1-i < chunk && fs[j+1] == fs[j]+1 {
			j++
		}
		out = append(out, testUnit{label: strconv.Itoa(len(out)), start: fs[i], end: fs[j]})
		i = j + 1
	}
	return out, nil
}

// TestArgs are `c4 farm tool test`'s.
type TestArgs struct {
	Dir    string
	Frames string            // instead of the test's
	With   map[string]string // over the test's parameters
	Image  string            // instead of the tool's (a local build)
	Keep   bool              // keep the scratch tree
	Log    string            // replay a captured log through the runner instead of running
	// Docker is the docker command (default "docker").
	Docker string
}

// TestTool checks a tool's file, then runs its test's units on this
// machine in Docker as the farm would: the tool's tree read-only at
// /etc/c4/tools, a scratch copy of the test tree writable at /show,
// started through the tool runner (c4-unit run) under the restrictions a
// unit has on the farm; then it checks the files the test expects.
func TestTool(ctx context.Context, o Out, a TestArgs) error {
	dir, err := filepath.Abs(a.Dir)
	if err != nil {
		return err
	}
	t, err := ReadTool(dir)
	if err != nil {
		return err
	}
	if t.Runs.Image == "" {
		return errors.New("runs: this tool runs on hosts only; test it there with c4 farm tool test --host (not in this version)")
	}
	image := ResolveImage(t.Runs.Image)
	if a.Image != "" {
		image = a.Image
	}
	o.printf("tool.yaml   ok · %s · %s · image %s\n", t.Kind, plural(len(t.Params), "parameter", "parameters"), t.Runs.Image)
	if t.Test == nil && a.Log == "" {
		o.printf("test        none: add a test: block (TOOL.md section 2.10) so publish can check the tool on the farm\n")
		return nil
	}
	docker := a.Docker
	if docker == "" {
		docker = "docker"
	}
	if err := exec.CommandContext(ctx, docker, "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		return errors.New("Docker is not running: tool test runs the tool in Docker, as the farm does")
	}
	if a.Log != "" {
		return replayLog(ctx, o, docker, image, dir, a.Log)
	}
	frames := t.Test.Frames
	if a.Frames != "" {
		frames = a.Frames
	}
	units, err := t.testUnits(frames)
	if err != nil {
		return err
	}
	with := map[string]any{}
	for k, v := range a.With {
		with[k] = v
	}
	params, err := t.paramValues("/show", t.Test.With, with)
	if err != nil {
		return err
	}
	penv, err := paramEnv(params)
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "c4-tool-test-")
	if err != nil {
		return err
	}
	if a.Keep {
		defer o.printf("kept        %s\n", scratch)
	} else {
		defer os.RemoveAll(scratch)
	}
	tree, ctl := filepath.Join(scratch, "show"), filepath.Join(scratch, "control")
	if err := copyTree(filepath.Join(dir, t.Test.Tree), tree); err != nil {
		return fmt.Errorf("test.tree: %w", err)
	}
	// the container user (65532) writes the scratch tree
	if err := chmodTree(tree); err != nil {
		return err
	}
	if err := os.MkdirAll(ctl, 0o777); err != nil {
		return err
	}
	os.Chmod(ctl, 0o777)
	o.printf("test        %s in %s, on this machine\n", unitFrames(t, units), plural(len(units), "unit", "units"))
	for _, u := range units {
		env := append([]string{"C4_TOOLS=/etc/c4/tools", "C4_TREE=/show", "C4_OUTPUT=/show", "C4_CONTROL=/etc/c4/control",
			"C4_JOB=test", "C4_UNIT=" + u.label, "C4_UNIT_LABEL=" + u.label, "C4_ATTEMPT=1"}, penv...)
		frames := fmt.Sprintf("%d-%d", u.start, u.end)
		if u.start == u.end {
			frames = fmt.Sprint(u.start)
		}
		env = append(env, "C4_FRAMES="+frames, fmt.Sprintf("C4_FRAME_START=%d", u.start), fmt.Sprintf("C4_FRAME_END=%d", u.end))
		args := append(unitDockerArgs(dir, tree, ctl, env), image, "c4-unit", "run")
		cmd := exec.CommandContext(ctx, docker, args...)
		cmd.Stdout, cmd.Stderr = indent(o.W), indent(o.W)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("unit %s failed: %w%s", u.label, err, outcome(tree))
		}
	}
	missing := 0
	for _, e := range t.Test.Expect {
		if _, err := os.Stat(filepath.Join(tree, filepath.FromSlash(e))); err != nil {
			o.printf("            missing  %s\n", e)
			missing++
		}
	}
	if len(t.Test.Expect) > 0 {
		o.printf("            expect: %d of %d files\n", len(t.Test.Expect)-missing, len(t.Test.Expect))
	}
	if missing > 0 {
		return fmt.Errorf("the test left %d of the files it expects missing", missing)
	}
	o.printf("ok\n")
	return nil
}

// unitDockerArgs run one unit as the farm runs it: as 65532, every
// capability dropped, no privilege escalation, a read-only root, the
// tool's tree read-only and the job's tree writable.
func unitDockerArgs(tool, tree, ctl string, env []string) []string {
	args := []string{"run", "--rm", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--tmpfs", "/tmp", "--network", "none",
		"-v", tool + ":/etc/c4/tools:ro", "-v", tree + ":/show", "-v", ctl + ":/etc/c4/control", "-w", "/show"}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	return args
}

// replayLog runs a captured log through the tool's patterns with the
// runner, printing the progress and errors it would have reported.
func replayLog(ctx context.Context, o Out, docker, image, dir, log string) error {
	abs, err := filepath.Abs(log)
	if err != nil {
		return err
	}
	o.printf("replay      %s through the tool's patterns\n", filepath.Base(log))
	args := []string{"run", "--rm", "--user", "65532:65532", "--cap-drop", "ALL", "--network", "none",
		"-v", dir + ":/etc/c4/tools:ro", "-v", abs + ":/etc/c4/replay.log:ro", "-e", "C4_TOOLS=/etc/c4/tools",
		image, "c4-unit", "run", "--replay", "/etc/c4/replay.log"}
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Stdout, cmd.Stderr = indent(o.W), indent(o.W)
	return cmd.Run()
}

func unitFrames(t *ToolFile, units []testUnit) string {
	if t.Kind != "frames" {
		return "1 run"
	}
	n := 0
	for _, u := range units {
		n += u.end - u.start + 1
	}
	return plural(n, "frame", "frames")
}

// outcome is the runner's .c4/outcome, for an error message.
func outcome(tree string) string {
	b, err := os.ReadFile(filepath.Join(tree, ".c4", "outcome"))
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return ""
	}
	return ": " + strings.TrimSpace(string(b))
}

// indent writes a unit's output indented under the test's lines.
func indent(w io.Writer) io.Writer { return &indenter{w: w, start: true} }

type indenter struct {
	w     io.Writer
	start bool
}

func (in *indenter) Write(p []byte) (int, error) {
	for _, b := range p {
		if in.start {
			if _, err := io.WriteString(in.w, "            "); err != nil {
				return 0, err
			}
		}
		if _, err := in.w.Write([]byte{b}); err != nil {
			return 0, err
		}
		in.start = b == '\n'
	}
	return len(p), nil
}

// copyTree copies a folder, keeping links as links.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(to, 0o755)
		case info.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, to)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, info.Mode().Perm())
	})
}

// chmodTree makes a scratch tree writable by any user, so the unit's
// user (65532) can write it.
func chmodTree(dir string) error {
	return filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return err
		}
		mode := os.FileMode(0o666)
		if info.IsDir() || info.Mode()&0o111 != 0 {
			mode = 0o777
		}
		return os.Chmod(p, mode)
	})
}
