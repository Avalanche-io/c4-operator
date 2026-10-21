package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Avalanche-io/c4-operator/schema"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// ReadTool reads dir/tool.yaml as the farm reads it (c4: tool/1, strictly,
// through the schema package), so `c4 farm tool test` refuses exactly
// what publishing would, each problem with its field and its fix.
func ReadTool(dir string) (*schema.Tool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "tool.yaml"))
	if err != nil {
		return nil, fmt.Errorf("%s has no tool.yaml (c4 farm tool new makes one)", dir)
	}
	t, errs := schema.ParseTool(data)
	if len(errs) > 0 {
		return t, &ToolError{Errors: errs}
	}
	if t.Test != nil {
		var probs []schema.Error
		for k := range t.Test.With {
			if t.Param(k) == nil {
				probs = append(probs, schema.Error{Path: "test.with." + k, Message: fmt.Sprintf("the tool has no parameter %q", k),
					Fix: "name one of the tool's params, or add it to params"})
			}
		}
		if t.Kind == "frames" && t.Test.Frames == "" {
			probs = append(probs, schema.Error{Path: "test.frames", Message: "a frames tool's test names its frames",
				Fix: "add frames: 1001-1002 (or the frames the test tree holds)"})
		}
		if len(probs) > 0 {
			return t, &ToolError{Errors: probs}
		}
	}
	return t, nil
}

// ToolError lists what is wrong with a tool.yaml.
type ToolError struct{ Errors []schema.Error }

func (e *ToolError) Error() string {
	var b strings.Builder
	b.WriteString("tool.yaml:")
	for _, x := range e.Errors {
		fmt.Fprintf(&b, "\n  %s: %s", x.Path, x.Message)
		if x.Fix != "" {
			fmt.Fprintf(&b, "\n    %s", x.Fix)
		}
	}
	return b.String()
}

// ResolveImage turns a tool's image into one Docker can pull, by the
// farm's rule: c4/runtime-oiio is ghcr.io/avalanche-io/c4-runtime-oiio at
// this release.
func ResolveImage(ref string) string { return schema.ImageRef(ref, ImageTag) }

// DevBuild says this c4-farm is a development build, whose release images
// are not published: tool test runs c4/<name> as this machine's
// c4-<name>:dev, the image `c4 farm image load` gives a local farm.
var DevBuild bool

// devImage is the local image a development build runs for ref, when this
// machine's Docker has it.
func devImage(ctx context.Context, docker, ref string) (string, bool) {
	name, ok := strings.CutPrefix(ref, "c4/")
	if !DevBuild || !ok || strings.ContainsAny(name, ":@/") {
		return "", false
	}
	local := "c4-" + name + ":dev"
	if exec.CommandContext(ctx, docker, "image", "inspect", local).Run() != nil {
		return "", false
	}
	return local, true
}

// paramValues are a unit's parameters: the tool's defaults, then the
// test's, then the person's; a PATH becomes absolute under the tree.
func paramValues(t *schema.Tool, tree string, with ...map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, p := range t.Params {
		if p.Default != nil {
			out[p.Name] = p.Default
		}
	}
	for _, w := range with {
		for k, v := range w {
			if t.Param(k) == nil {
				return nil, fmt.Errorf("%s has no parameter %q", t.Name, k)
			}
			out[k] = v
		}
	}
	for k, v := range out {
		if t.Param(k).Type == "PATH" {
			if s, ok := v.(string); ok && s != "" && !filepath.IsAbs(s) {
				out[k] = tree + "/" + strings.TrimPrefix(s, "./")
			}
		}
	}
	return out, nil
}

// paramEnv is C4_PARAMS, as the farm's lowering sets it; c4-unit run
// gives the script each as C4_PARAM_<NAME> (schema.ParamEnv).
func paramEnv(params map[string]any) ([]string, error) {
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return []string{"C4_PARAMS=" + string(b)}, nil
}

// testUnit is one unit of a tool's test: its frames, as a range.
type testUnit struct {
	label      string
	start, end int
}

// testUnits splits the test's frames into the tool's chunks (chunk: all is
// one unit over them); a tool without frames runs one unit, which the farm
// gives the single frame 1.
func testUnits(t *schema.Tool, frames string) ([]testUnit, error) {
	if t.Kind != "frames" {
		return []testUnit{{label: "0", start: 1, end: 1}}, nil
	}
	fs, err := order.ParseFrames(frames)
	if err != nil {
		return nil, fmt.Errorf("frames %q: %w", frames, err)
	}
	chunk := 1
	if t.Frames != nil && t.Frames.Chunk != nil {
		switch {
		case t.Frames.Chunk.All:
			chunk = len(fs)
		case t.Frames.Chunk.N > 0:
			chunk = t.Frames.Chunk.N
		}
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
	if a.Image == "" {
		if local, ok := devImage(ctx, docker, t.Runs.Image); ok {
			image = local
			o.printf("image       %s, this machine's (a development build)\n", local)
		}
	}
	if a.Log != "" {
		return replayLog(ctx, o, docker, image, dir, a.Log)
	}
	frames := string(t.Test.Frames)
	if a.Frames != "" {
		frames = a.Frames
	}
	units, err := testUnits(t, string(frames))
	if err != nil {
		return err
	}
	with := map[string]any{}
	for k, v := range a.With {
		with[k] = v
	}
	params, err := paramValues(t, "/show", t.Test.With, with)
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
			"C4_JOB=test", "C4_SHOT=test", "C4_UNIT=" + u.label, "C4_UNIT_NAME=" + u.label, "C4_UNIT_LABEL=" + u.label, "C4_ATTEMPT=1"}, penv...)
		frames := fmt.Sprintf("%d-%d", u.start, u.end)
		if u.start == u.end {
			frames = fmt.Sprint(u.start)
		}
		env = append(env, "C4_FRAMES="+frames, fmt.Sprintf("C4_FRAME_START=%d", u.start), fmt.Sprintf("C4_FRAME_END=%d", u.end))
		args := append(unitDockerArgs(dir, tree, ctl, env), image, "c4-unit", "run")
		cmd := exec.CommandContext(ctx, docker, args...)
		var said bytes.Buffer
		cmd.Stdout, cmd.Stderr = io.MultiWriter(indent(o.W), &said), io.MultiWriter(indent(o.W), &said)
		if err := cmd.Run(); err != nil {
			if strings.Contains(said.String(), `no command "run"`) {
				return fmt.Errorf("unit %s failed: %s carries a c4-unit older than the tool runner (c4-unit run); rebuild the image, or give one with --image", u.label, image)
			}
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

func unitFrames(t *schema.Tool, units []testUnit) string {
	if t.Kind != "frames" {
		return "the whole job"
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
