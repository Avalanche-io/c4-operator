package farmcli

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

//go:embed scenarios/*.yaml
var scenarioFiles embed.FS

// standIn is the script a scenario's units run instead of the real work.
//
//go:embed standin.sh
var standIn string

// StandInImage runs a scenario's stand-in units: busybox, for its sh,
// sleep, awk and date.
var StandInImage = "busybox:1.37"

// Scenario is a sample scenario: a moment on a farm that a guide walks
// through, loaded onto the local farm by `c4 farm samples load`. Its units
// are stand-ins that take the time the real work would and write
// placeholder files, so the queue, estimates, risks and the book are the
// farm's own.
type Scenario struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	// Projects are added to the local farm (Saltmarsh is always there).
	Projects []ScenarioProject `json:"projects,omitempty"`
	// Budgets are burst budgets per day, in dollars, for the scenario;
	// every other project keeps the local farm's.
	Budgets map[string]string `json:"budgets,omitempty"`
	// Defs holds YAML anchors the jobs share.
	Defs map[string]any `json:"defs,omitempty"`
	// Jobs are submitted in order; a job's After names earlier ones.
	Jobs []ScenarioJob `json:"jobs"`
}

// ScenarioProject is a project a scenario adds.
type ScenarioProject struct {
	Name              string `json:"name"`
	Title             string `json:"title"`
	BurstBudgetPerDay string `json:"burstBudgetPerDay,omitempty"`
	LateCostPerHour   string `json:"lateCostPerHour,omitempty"`
}

// ScenarioJob is one job of a scenario.
type ScenarioJob struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	// As is the local sign-in that submits it, and so owns it.
	As   string `json:"as"`
	Type string `json:"type"` // a prebuilt job type (c4 farm types)
	Shot string `json:"shot"`
	// Reads are the files of the tree the job mounts, as placeholders;
	// the first is its asset.
	Reads  []string `json:"reads"`
	After  []string `json:"after,omitempty"`
	Frames string   `json:"frames,omitempty"`
	Pools  []string `json:"pools,omitempty"`
	// Due is the deadline as an offset from loading (+1h30m); empty
	// leaves it to the farm's suggestion.
	Due string `json:"due,omitempty"`
	// FrameSeconds is one frame's time; Seconds a unit's, without frames.
	FrameSeconds float64 `json:"frameSeconds,omitempty"`
	Seconds      float64 `json:"seconds,omitempty"`
	// Steps, StepSeconds and CheckpointEvery shape a training run.
	Steps           int     `json:"steps,omitempty"`
	StepSeconds     float64 `json:"stepSeconds,omitempty"`
	CheckpointEvery int     `json:"checkpointEvery,omitempty"`
	// Fail makes every unit given to one node fail there.
	Fail    *ScenarioFail  `json:"fail,omitempty"`
	AIUsage map[string]any `json:"aiUsage,omitempty"`
}

// ScenarioFail is a node a job's units fail on.
type ScenarioFail struct {
	Node    string  `json:"node"`
	After   float64 `json:"after"` // seconds
	Message string  `json:"message"`
}

// Scenarios are the sample scenarios, by name.
func Scenarios() ([]Scenario, error) {
	names, err := scenarioFiles.ReadDir("scenarios")
	if err != nil {
		return nil, err
	}
	var out []Scenario
	for _, e := range names {
		data, err := scenarioFiles.ReadFile("scenarios/" + e.Name())
		if err != nil {
			return nil, err
		}
		var sc Scenario
		if err := yaml.UnmarshalStrict(data, &sc); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if err := sc.check(); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LookupScenario finds a scenario by name.
func LookupScenario(name string) (Scenario, error) {
	all, err := Scenarios()
	if err != nil {
		return Scenario{}, err
	}
	var names []string
	for _, sc := range all {
		if sc.Name == name {
			return sc, nil
		}
		names = append(names, sc.Name)
	}
	return Scenario{}, fmt.Errorf("no scenario %q; there are: %s", name, strings.Join(names, ", "))
}

func (sc Scenario) check() error {
	if sc.Name == "" || sc.Title == "" || len(sc.Jobs) == 0 {
		return fmt.Errorf("a scenario needs a name, a title and jobs")
	}
	seen := map[string]bool{}
	for _, j := range sc.Jobs {
		if seen[j.Project+"/"+j.Name] {
			return fmt.Errorf("%s/%s is listed twice", j.Project, j.Name)
		}
		for _, a := range j.After {
			if !seen[j.Project+"/"+a] {
				return fmt.Errorf("%s: after %s, which is not an earlier job of %s", j.Name, a, j.Project)
			}
		}
		seen[j.Project+"/"+j.Name] = true
		if _, err := j.spec(Clock{}); err != nil {
			return fmt.Errorf("%s: %w", j.Name, err)
		}
	}
	return nil
}

// standInOut is where each type's stand-in writes: the folder under the
// mount, the extension, and whether it writes a file per frame.
var standInOut = map[string]struct {
	dir, ext string
	perFrame bool
}{
	"render": {"render", "exr", true}, "comp": {"comp", "exr", true}, "infer": {"depth", "exr", true},
	"sim": {"cache", "vdb", true}, "transcode": {"review", "mov", false}, "usd": {"flat", "usda", false},
	"lora": {"out", "safetensors", false}, "ci": {"results", "xml", false},
}

// spec is the job's WorkJob spec: its type's, with the stand-in for the
// type's script.
func (j ScenarioJob) spec(clock Clock) (c4v1.WorkJobSpec, error) {
	t, err := LookupType(j.Type)
	if err != nil {
		return c4v1.WorkJobSpec{}, err
	}
	if len(j.Reads) == 0 || j.Project == "" || j.As == "" || j.Shot == "" {
		return c4v1.WorkJobSpec{}, fmt.Errorf("a job needs a project, as, a shot and the files it reads")
	}
	o := Options{Asset: j.Reads[0], Shot: j.Shot, Pools: j.Pools, Image: StandInImage}
	if t.Frames {
		o.Frames = j.Frames
	} else if j.Frames != "" {
		return c4v1.WorkJobSpec{}, fmt.Errorf("%s has no frames", t.Name)
	}
	spec, err := t.Spec(o)
	if err != nil {
		return c4v1.WorkJobSpec{}, err
	}
	out := standInOut[t.Name]
	name := j.Shot
	switch t.Name {
	case "render":
		name = strings.TrimSuffix(path.Base(j.Reads[0]), path.Ext(j.Reads[0]))
	case "transcode":
		name = j.Shot + "_review"
	}
	env := map[string]string{"S_MOUNT": t.Mount, "S_OUT": out.dir, "S_NAME": name, "S_EXT": out.ext}
	if out.perFrame {
		env["S_PER_FRAME"] = "1"
	}
	var unit float64
	switch {
	case t.Class == "ml":
		if j.Steps <= 0 || j.StepSeconds <= 0 || j.CheckpointEvery <= 0 {
			return c4v1.WorkJobSpec{}, fmt.Errorf("a training run needs steps, stepSeconds and checkpointEvery")
		}
		spec.Params["steps"] = strconv.Itoa(j.Steps)
		spec.Params["checkpointEvery"] = strconv.Itoa(j.CheckpointEvery)
		env["S_STEP_SECONDS"] = fmt.Sprint(j.StepSeconds)
		unit = float64(j.Steps) * j.StepSeconds
	case t.Frames:
		if j.FrameSeconds <= 0 {
			return c4v1.WorkJobSpec{}, fmt.Errorf("frameSeconds is needed")
		}
		chunk, err := strconv.Atoi(spec.Params["chunk"])
		if err != nil || chunk < 1 {
			chunk = 1
		}
		env["S_SECONDS"] = fmt.Sprint(j.FrameSeconds)
		unit = float64(chunk) * j.FrameSeconds
	default:
		if j.Seconds <= 0 {
			return c4v1.WorkJobSpec{}, fmt.Errorf("seconds is needed")
		}
		env["S_SECONDS"] = fmt.Sprint(j.Seconds)
		unit = j.Seconds
	}
	// the farm's first estimate, before it has measured a unit
	spec.Params["estimateSeconds"] = strconv.FormatFloat(unit, 'f', -1, 64)
	if f := j.Fail; f != nil {
		if f.Node == "" || f.After <= 0 || f.Message == "" {
			return c4v1.WorkJobSpec{}, fmt.Errorf("fail needs a node, after and a message")
		}
		env["S_FAIL_NODE"], env["S_FAIL_AFTER"], env["S_FAIL_MSG"] = f.Node, fmt.Sprint(f.After), f.Message
	}
	c := &spec.Template.Spec.Containers[0]
	c.Command, c.Args = []string{"sh", "-euc"}, []string{standIn}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: env[k]})
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: "NODE_NAME",
		ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}})
	if j.Due != "" {
		d, err := time.ParseDuration(strings.TrimPrefix(j.Due, "+"))
		if err != nil || !strings.HasPrefix(j.Due, "+") || d <= 0 {
			return c4v1.WorkJobSpec{}, fmt.Errorf("due %q: give an offset from loading, such as +1h30m", j.Due)
		}
		spec.Deadline = &metav1.Time{Time: clock.now().Add(d).Truncate(time.Minute).UTC()}
		spec.DeadlineMode = "Pinned"
	}
	return spec, nil
}

// what is the job in a few words: its kind, frames, upstream and pool.
func (j ScenarioJob) what() string {
	kind := map[string]string{"lora": "LoRA fine-tune", "infer": "inference", "ci": "tests"}[j.Type]
	if kind == "" {
		kind = j.Type
	}
	parts := []string{kind}
	if n := frameCount(j.Frames); j.Frames != "" && n > 0 && j.Type != "comp" && j.Type != "transcode" {
		parts = append(parts, plural(n, "frame", "frames"))
	}
	if len(j.After) > 0 {
		parts = append(parts, "after "+strings.Join(j.After, " and "))
	}
	if len(j.Pools) > 0 {
		parts = append(parts, "pool "+strings.Join(j.Pools, ", "))
	}
	return strings.Join(parts, " · ")
}

// LoadScenario submits a scenario's jobs, in order, each as its sign-in
// (client gives the HUD client for one), uploading each job's placeholder
// tree first. The local farm must already have the scenario's projects.
func LoadScenario(ctx context.Context, o Out, sc Scenario, client func(as string) (*Client, error)) error {
	width := 0
	for _, j := range sc.Jobs {
		width = max(width, len(j.Project)+1+len(j.Name))
	}
	for i, j := range sc.Jobs {
		c, err := client(j.As)
		if err != nil {
			return err
		}
		done, deadline, err := submitStandIn(ctx, c, o.Clock, sc.Name, j)
		if err != nil {
			return fmt.Errorf("%s/%s: %w", j.Project, j.Name, err)
		}
		due := "deadline suggested"
		if deadline != nil {
			due = "due " + o.Clock.At(*deadline)
		}
		head := "submitted"
		if i > 0 {
			head = ""
		}
		o.printf("%-9s %-*s   %s · %s\n", head, width, done.Namespace+"/"+done.Name, j.what(), due)
	}
	return nil
}

// submitStandIn uploads a job's placeholder tree and submits it; it
// returns the deadline it set, if any.
func submitStandIn(ctx context.Context, c *Client, clock Clock, scenario string, j ScenarioJob) (*api.SubmitResponse, *time.Time, error) {
	t, err := LookupType(j.Type)
	if err != nil {
		return nil, nil, err
	}
	spec, err := j.spec(clock)
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "c4-scenario-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	for _, p := range j.Reads {
		f := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			return nil, nil, err
		}
		body := fmt.Sprintf("placeholder for %s, shot %s (c4 farm samples load %s)\n", p, j.Shot, scenario)
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			return nil, nil, err
		}
	}
	v, err := view.ScanDir(dir)
	if err != nil {
		return nil, nil, err
	}
	need, err := missingIDs(ctx, c, j.Project, v.Objects())
	if err != nil {
		return nil, nil, err
	}
	if _, _, err := upload(ctx, c, j.Project, dir, v, need); err != nil {
		return nil, nil, err
	}
	if err := recordView(ctx, c, j.Project, v); err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, err
	}
	ai := j.AIUsage
	if ai == nil {
		ai = t.AIUsage
	}
	req := api.SubmitRequest{Project: j.Project, Name: j.Name, Type: t.Name, Stage: t.Stage, Shot: j.Shot, Spec: raw,
		Inputs: []api.SubmitInput{{View: v.ID.String(), MountPath: t.Mount}}, AIUsage: ai}
	for _, a := range j.After {
		// as c4 farm submit --after: one upstream job at /in, several at /in/<job>
		mp := "/in"
		if len(j.After) > 1 {
			mp = "/in/" + a
		}
		req.Inputs = append(req.Inputs, api.SubmitInput{FromJob: a, MountPath: mp, ReadOnly: true})
	}
	var done api.SubmitResponse
	if err := c.Do(ctx, "POST", "/jobs", req, &done); err != nil {
		return nil, nil, err
	}
	if spec.Deadline != nil {
		return &done, &spec.Deadline.Time, nil
	}
	return &done, nil, nil
}
