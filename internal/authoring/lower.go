package authoring

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/schema"
	order_ "github.com/Avalanche-io/c4-operator/work/order"
)

// ToolsMount is where a unit's tool tree mounts, read-only ($C4_TOOLS).
const ToolsMount = "/etc/c4/tools"

// ReadRoot is where a submission's read trees mount, each at its name.
const ReadRoot = "/c4/read"

// RunnerCommand is every tool unit's command: the tool runner, which
// starts the tool's own command (TOOL.md section 4).
var RunnerCommand = []string{"c4-unit", "run"}

// classParams are the class parameters a tool's parameter of the same
// name is also given as: render's pass, ci's shards and target, ml's
// steps, checkpointEvery and ranks.
var classParams = map[string][]string{
	"render": {"pass"},
	"ci":     {"shards", "target"},
	"ml":     {"steps", "checkpointEvery", "ranks"},
}

// toolJob is one job of a tool, as a submission or a tool's test asks.
type toolJob struct {
	tool    *Tool
	ref     ToolRef
	project string
	ns      string
	root    string
	shot    string
	name    string // the job's name, or "" for the HUD's <shot>-<stage>-vNNN
	stage   string
	frames  Frames
	with    map[string]any
	size    string
	needs   *Resources
	pools   []string
	retries *int
	maxPar  int
	chunk   *schema.Chunk
	order   string
	inputs  []api.SubmitInput
}

// lowerTool lowers one job of a frames or once tool to a POST /jobs
// request (TOOL.md section 9): class render, the runner as the command in
// the tool's pinned image, the tool's tree read-only at ToolsMount, its
// parameters resolved into C4_PARAMS, and its needs as requests. Paths in
// errors are relative to the job (with.stage, frames).
func lowerTool(j toolJob) (*api.SubmitRequest, []Error) {
	t := j.tool
	var errs []Error
	errs = append(errs, refusals(t)...)
	params, perrs := resolveParams(t, j.with, j.root)
	errs = append(errs, perrs...)
	if len(errs) > 0 {
		return nil, errs
	}
	frames := string(j.frames)
	switch t.Kind {
	case "once":
		if frames != "" && frames != "1" {
			return nil, []Error{errf("frames", "remove frames: a once tool runs one unit over the whole job", "%s is a once tool, which has no frames", t.Name)}
		}
		frames = "1"
	case "frames":
		if frames == "" {
			return nil, []Error{errf("frames", "add frames: 1001-1240", "%s runs over frames: say which", t.Name)}
		}
	}
	cp := map[string]string{"frames": frames}
	chunk, order := j.chunk, j.order
	if f := t.Frames; f != nil {
		if chunk == nil {
			chunk = f.Chunk
		}
		if order == "" {
			order = f.Order
		}
	}
	if chunk != nil && t.Kind == "frames" {
		n := chunk.N
		if chunk.All {
			// one unit over the whole range
			fs, err := order_.ParseFrames(frames)
			if err != nil {
				return nil, []Error{errf("frames", "write frames as 1001-1240", "%q is not a frame list: %v", frames, err)}
			}
			n = len(fs)
		}
		if n > 0 {
			cp["chunk"] = strconv.Itoa(n)
		}
	}
	if order != "" && t.Kind == "frames" {
		cp["order"] = order
	}
	for _, name := range classParams["render"] {
		if v, ok := params[name]; ok {
			cp[name] = fmt.Sprint(v)
		}
	}
	pj, err := json.Marshal(params)
	if err != nil {
		return nil, []Error{errf("with", "", "the parameters cannot be written: %v", err)}
	}
	image := j.ref.Image
	if image == "" {
		image = t.Runs.Image
	}
	env := []corev1.EnvVar{{Name: "C4_TOOLS", Value: ToolsMount}, {Name: "C4_PARAMS", Value: string(pj)}}
	if j.shot != "" {
		env = append(env, corev1.EnvVar{Name: "C4_SHOT", Value: j.shot})
	}
	req, rerrs := requests(t, j.size, j.needs)
	if len(rerrs) > 0 {
		return nil, rerrs
	}
	c := corev1.Container{Name: "unit", Image: image, Command: append([]string(nil), RunnerCommand...), Env: env,
		Resources: corev1.ResourceRequirements{Requests: req}}
	spec := c4v1.WorkJobSpec{Class: "render", Params: cp, Pools: j.pools, MaxParallel: int32(j.maxPar),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{c}}}}
	retries := j.retries
	if retries == nil {
		retries = t.Retries
	}
	if retries != nil {
		r := int32(*retries)
		spec.Retries = &r
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, []Error{errf("", "", "the job cannot be written: %v", err)}
	}
	inputs := []api.SubmitInput{{View: j.ref.View, MountPath: ToolsMount, ReadOnly: true}}
	inputs = append(inputs, j.inputs...)
	out := &api.SubmitRequest{Project: j.project, Namespace: j.ns, Name: j.name, Type: t.Name, Stage: j.stage, Shot: j.shot,
		Spec: raw, Inputs: inputs}
	if len(t.AIUsage) > 0 {
		out.AIUsage = fillAIUsage(t.AIUsage, params)
	}
	return out, nil
}

// refusals are the fields of a tool that slice 1 cannot lower yet, each
// with the reason, rather than run something other than what the tool
// says.
func refusals(t *Tool) []Error {
	var errs []Error
	if t.Kind != "frames" && t.Kind != "once" {
		errs = append(errs, errf("tool", "use a frames or once tool for now", "%s is a %s tool; the farm runs frames and once tools so far", t.Name, t.Kind))
	}
	if t.Runs.Image == "" {
		errs = append(errs, errf("tool", "give the tool runs.image as well as runs.host", "%s runs only on host runners, which run tools from slice 3", t.Name))
	}
	if len(t.Runs.Sidecars) > 0 {
		errs = append(errs, errf("tool", "remove runs.sidecars for now", "%s runs sidecars, which need the job's Tool, from slice 3", t.Name))
	}
	if t.Kubernetes != nil {
		errs = append(errs, errf("tool", "remove kubernetes for now", "%s overlays the pod, which needs a farm admin's approval, from slice 3", t.Name))
	}
	if t.Gate != "" {
		errs = append(errs, errf("tool", "remove gate for now", "%s passes a gate, which tools reach in slice 3", t.Name))
	}
	if n := t.Needs; true {
		if len(n.Network) > 0 {
			errs = append(errs, errf("tool", "remove needs.network for now", "%s needs the network, which a tool's units reach from slice 3: until then they are sealed", t.Name))
		}
		if len(n.Licences) > 0 {
			errs = append(errs, errf("tool", "remove needs.licences for now", "%s needs licences, which the farm counts from slice 2", t.Name))
		}
		if n.Farm != "" {
			errs = append(errs, errf("tool", "remove needs.farm for now", "%s needs the farm (%s), which a tool's units reach from slice 3", t.Name, n.Farm))
		}
	}
	return errs
}

// requests are the container's resource requests: the tool's needs, a
// named size of it, then the job's own (slice 1 sets no limits).
func requests(t *Tool, size string, jn *Resources) (corev1.ResourceList, []Error) {
	n := t.Needs.Resources
	if size != "" {
		s, ok := t.Needs.Sizes[size]
		if !ok {
			fix := t.Name + " has no sizes"
			if names := sortedKeys(t.Needs.Sizes); len(names) > 0 {
				fix = "its sizes are " + strings.Join(names, ", ")
			}
			return nil, []Error{errf("size", fix, "%s has no size %q", t.Name, size)}
		}
		over(&n, &s)
	}
	if jn != nil {
		over(&n, jn)
	}
	out := corev1.ResourceList{}
	if n.CPU > 0 {
		out[corev1.ResourceCPU] = *resource.NewMilliQuantity(int64(n.CPU*1000), resource.DecimalSI)
	}
	for _, q := range []struct {
		name corev1.ResourceName
		v    string
	}{{corev1.ResourceMemory, n.Memory}, {corev1.ResourceEphemeralStorage, n.Scratch}} {
		if q.v == "" {
			continue
		}
		r, err := resource.ParseQuantity(kubeSize(q.v))
		if err != nil {
			return nil, []Error{errf("needs", "write a size with its unit: 64G", "%q is not a size", q.v)}
		}
		out[q.name] = r
	}
	if n.GPU != nil && *n.GPU > 0 {
		out["nvidia.com/gpu"] = *resource.NewQuantity(int64(*n.GPU), resource.DecimalSI)
	}
	return out, nil
}

func over(n *Resources, o *Resources) {
	if o.CPU > 0 {
		n.CPU = o.CPU
	}
	if o.Memory != "" {
		n.Memory = o.Memory
	}
	if o.GPU != nil {
		n.GPU = o.GPU
	}
	if o.GPUMemory != "" {
		n.GPUMemory = o.GPUMemory
	}
	if o.Scratch != "" {
		n.Scratch = o.Scratch
	}
}

// kubeSize is a size as Kubernetes writes it: 64G stays 64G, 64Gi too.
func kubeSize(s string) string { return s }

// resolveParams is a tool's parameters as C4_PARAMS has them: the
// submission's values, else the defaults, checked against each
// parameter's type and range, with a PATH relative to the job's tree made
// absolute under root. A parameter with neither is required; an optional
// one with no value is left out.
func resolveParams(t *Tool, with map[string]any, root string) (map[string]any, []Error) {
	var errs []Error
	out := map[string]any{}
	known := map[string]*Param{}
	var names []string
	for i := range t.Params {
		known[t.Params[i].Name] = &t.Params[i]
		names = append(names, t.Params[i].Name)
	}
	for _, k := range sortedKeys(with) {
		if known[k] == nil {
			fix := t.Name + " has no parameters"
			if len(names) > 0 {
				fix = "its parameters are " + strings.Join(names, ", ")
			}
			if c, ok := closest(k, names); ok {
				fix = fmt.Sprintf("did you mean %q?", c)
			}
			errs = append(errs, errf("with."+k, fix, "%s has no parameter %q", t.Name, k))
		}
	}
	var missing []*Param
	for _, p := range t.Params {
		v, given := with[p.Name]
		if !given {
			if p.Default == nil {
				if p.From == "" {
					missing = append(missing, known[p.Name])
				}
				continue
			}
			v = p.Default
		}
		v = plain(v)
		val, e := paramValue(p, v, root)
		if e != nil {
			e.Path = "with." + p.Name
			errs = append(errs, *e)
			continue
		}
		out[p.Name] = val
	}
	if len(missing) > 0 {
		var what, adds []string
		for _, p := range missing {
			w := p.Name
			label := ""
			if p.UserInterface != nil {
				label = p.UserInterface.Label
			}
			if label == "" {
				label = p.Description
			}
			if label != "" {
				w += " (" + label + ")"
			}
			what = append(what, w)
			adds = append(adds, p.Name+": "+placeholder(p))
		}
		errs = append(errs, errf("with", "add with: {"+strings.Join(adds, ", ")+"}", "%s needs %s", t.Name, strings.Join(what, ", ")))
	}
	return out, errs
}

func placeholder(p *Param) string {
	switch p.Type {
	case "PATH":
		if p.ObjectType == "DIRECTORY" {
			return "<a folder in the tree>"
		}
		if ext := extOf(p); ext != "" {
			return "<a " + ext + " file in the tree>"
		}
		return "<a file in the tree>"
	case "INT", "FLOAT":
		return "<a number>"
	case "BOOL":
		return "<true or false>"
	}
	return "<" + strings.ToLower(p.Type) + ">"
}

// extOf is the file kind a PATH parameter's file filter names, as .usd.
func extOf(p *Param) string {
	ui := p.UserInterface
	if ui == nil {
		return ""
	}
	f := ui.FileFilterDefault
	if f == nil && len(ui.FileFilters) > 0 {
		f = &ui.FileFilters[0]
	}
	if f == nil || len(f.Patterns) == 0 {
		return ""
	}
	s := strings.TrimPrefix(f.Patterns[0], "*")
	if i := strings.IndexAny(s, "*?[{"); i >= 0 {
		s = s[:i]
	}
	return s
}

// paramValue checks v against p and makes it what C4_PARAMS holds.
func paramValue(p Param, v any, root string) (any, *Error) {
	bad := func(fix, format string, args ...any) (any, *Error) {
		e := errf("", fix, format, args...)
		return nil, &e
	}
	if len(p.AllowedValues) > 0 && !strings.HasPrefix(p.Type, "LIST[") && !allowed(v, p.AllowedValues) {
		vals := make([]string, len(p.AllowedValues))
		for i, a := range p.AllowedValues {
			vals[i] = fmt.Sprint(plain(a))
		}
		fix := "choose one of them"
		if c, ok := closestValue(fmt.Sprint(v), vals); ok {
			fix = fmt.Sprintf("did you mean %q?", c)
		}
		return bad(fix, "%s is not one of %s", show(v), strings.Join(vals, ", "))
	}
	switch p.Type {
	case "STRING", "RANGE_EXPR":
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		if p.MinLength != nil && len(s) < *p.MinLength || p.MaxLength != nil && len(s) > *p.MaxLength {
			return bad("write it at most and at least the lengths the tool allows", "%q is not the length %s takes", s, p.Name)
		}
		return s, nil
	case "PATH":
		s, ok := v.(string)
		if !ok {
			return bad("write a path in the job's tree", "%s is a path, not %v", p.Name, v)
		}
		return absPath(s, root), nil
	case "INT":
		n, ok := toFloat(v)
		if !ok || n != float64(int64(n)) {
			return bad("write a whole number", "%s is a whole number, not %v", p.Name, v)
		}
		if e := inRange(p, n); e != nil {
			return nil, e
		}
		return int64(n), nil
	case "FLOAT":
		n, ok := toFloat(v)
		if !ok {
			return bad("write a number", "%s is a number, not %v", p.Name, v)
		}
		if e := inRange(p, n); e != nil {
			return nil, e
		}
		return n, nil
	case "BOOL":
		b, ok := v.(bool)
		if !ok {
			return bad("write true or false", "%s is true or false, not %v", p.Name, v)
		}
		return b, nil
	}
	l, ok := v.([]any)
	if !ok {
		return bad("write a list", "%s is a list, not %v", p.Name, v)
	}
	if p.Type == "LIST[PATH]" {
		out := make([]any, len(l))
		for i, e := range l {
			s, ok := e.(string)
			if !ok {
				return bad("write paths in the job's tree", "%s is a list of paths", p.Name)
			}
			out[i] = absPath(s, root)
		}
		return out, nil
	}
	return l, nil
}

func inRange(p Param, n float64) *Error {
	if p.MinValue != nil && n < *p.MinValue || p.MaxValue != nil && n > *p.MaxValue {
		lo, hi := "", ""
		if p.MinValue != nil {
			lo = strconv.FormatFloat(*p.MinValue, 'f', -1, 64)
		}
		if p.MaxValue != nil {
			hi = strconv.FormatFloat(*p.MaxValue, 'f', -1, 64)
		}
		e := errf("", fmt.Sprintf("choose a value from %s to %s", orDash(lo), orDash(hi)), "%s is out of %s's range", strconv.FormatFloat(n, 'f', -1, 64), p.Name)
		return &e
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// absPath is a path written relative to the job's tree, made absolute
// where the unit mounts it; an absolute one is left as written.
func absPath(s, root string) string {
	if path.IsAbs(s) || root == "" {
		return s
	}
	return path.Join(root, s)
}

// closestValue suggests an allowed value: the nearest by edit distance,
// else one the value is a part of when both are compared without case and
// punctuation (karma-xpu for BRAY_HdKarmaXPU).
func closestValue(v string, vals []string) (string, bool) {
	if c, ok := closest(v, vals); ok {
		return c, true
	}
	norm := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				return r
			}
			if r >= 'A' && r <= 'Z' {
				return r + 'a' - 'A'
			}
			return -1
		}, s)
	}
	nv := norm(v)
	best := ""
	for _, c := range vals {
		nc := norm(c)
		if nv != "" && strings.HasSuffix(nc, nv) && (best == "" || len(c) < len(best)) {
			best = c
		}
	}
	return best, best != ""
}

// fillAIUsage is a tool's aiUsage with its {{Param.<name>}} format
// strings filled from the job's parameters.
func fillAIUsage(u map[string]any, params map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range u {
		v = plain(v)
		if s, ok := v.(string); ok {
			v = formatRe.ReplaceAllStringFunc(s, func(m string) string {
				name := formatRe.FindStringSubmatch(m)[1]
				if p, ok := strings.CutPrefix(name, "Param."); ok {
					if x, ok := params[p]; ok {
						return fmt.Sprint(x)
					}
				}
				return m
			})
		}
		out[k] = v
	}
	return out
}

// LowerTest is a tool's test (its test: tree, frames and with) as one job
// named <tool>-test-<version>, for publishing to run before the tool is
// offered (TOOL.md section 2.10): testTree, the C4 ID of the view of the
// test's folder in the published tree, mounts writable at the project's
// root, and the tool's own tree read-only at ToolsMount, as for any job
// of the tool.
func LowerTest(t *Tool, ref ToolRef, testTree, project, namespace, root string) (*api.SubmitRequest, error) {
	if t.Test == nil {
		return nil, fmt.Errorf("%s has no test", t.Name)
	}
	if root == "" {
		root = c4v1.DefaultProjectRoot
	}
	frames := t.Test.Frames
	if t.Kind == "frames" && frames == "" {
		frames = "1"
	}
	req, errs := lowerTool(toolJob{tool: t, ref: ref, project: project, ns: namespace, root: root,
		name: fmt.Sprintf("%s-test-%s", t.Name, ref.Version), stage: "test", frames: frames, with: t.Test.With,
		inputs: []api.SubmitInput{{View: testTree, MountPath: root}}})
	if len(errs) > 0 {
		return nil, Errors(prefix("test", errs))
	}
	return req, nil
}

// prefix puts errors' paths under p.
func prefix(p string, errs []Error) []Error {
	out := make([]Error, len(errs))
	for i, e := range errs {
		e.Path = join(p, e.Path)
		out[i] = e
	}
	return out
}
