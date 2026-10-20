package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// A Submission is a c4: submit/1 document: what to do for one piece of
// production. Jobs keep the order they were written in.
type Submission struct {
	C4      string `json:"c4"`
	Project string `json:"project,omitempty"`
	Shot    string `json:"shot,omitempty"`
	Name    string `json:"name,omitempty"`
	Due     string `json:"due,omitempty"`
	Tree    string `json:"tree,omitempty"`
	// Root is the folder on the submitter's machine that is the top of
	// Tree, for mapping the paths its scenes hold.
	Root    string            `json:"root,omitempty"`
	Read    map[string]string `json:"read,omitempty"`
	Tracker string            `json:"tracker,omitempty"`
	For     string            `json:"for,omitempty"`
	Note    string            `json:"note,omitempty"`
	Policy  *Policy           `json:"policy,omitempty"`
	Jobs    Jobs              `json:"jobs"`
}

// Jobs are a submission's jobs in the order written; each carries its key.
type Jobs []Job

// Job returns the job with the key, or nil.
func (js Jobs) Job(key string) *Job {
	for i := range js {
		if js[i].Key == key {
			return &js[i]
		}
	}
	return nil
}

// A Job is one tool over its units, or a plan whose blocks run a tool.
type Job struct {
	// Key is the job's key in the file: its stage, the middle of its name.
	Key         string         `json:"-"`
	Tool        string         `json:"tool,omitempty"`
	Frames      Frames         `json:"frames,omitempty"`
	With        map[string]any `json:"with,omitempty"`
	After       After          `json:"after,omitempty"`
	Due         string         `json:"due,omitempty"`
	Size        string         `json:"size,omitempty"`
	Needs       *Resources     `json:"needs,omitempty"`
	Pools       []string       `json:"pools,omitempty"`
	Preview     string         `json:"preview,omitempty"`
	Check       *Check         `json:"check,omitempty"`
	Approve     *Approve       `json:"approve,omitempty"`
	Wait        *Wait          `json:"wait,omitempty"`
	Answers     *Answers       `json:"answers,omitempty"`
	Uses        map[string]Use `json:"uses,omitempty"`
	Plan        string         `json:"plan,omitempty"`
	Each        *Each          `json:"each,omitempty"`
	Over        Axes           `json:"over,omitempty"`
	Combine     string         `json:"combine,omitempty"`
	Measure     *Measure       `json:"measure,omitempty"`
	Retries     *int           `json:"retries,omitempty"`
	MaxParallel *int           `json:"maxParallel,omitempty"`
	Chunk       *Chunk         `json:"chunk,omitempty"`
	Order       string         `json:"order,omitempty"`
	Follows     string         `json:"follows,omitempty"`
	Policy      *Policy        `json:"policy,omitempty"`
}

// Policy names a policy set and narrows its values for a submission or a
// job. It never defines policy; the farm does.
type Policy struct {
	Set    string         `json:"set,omitempty"`
	Narrow map[string]any `json:"narrow,omitempty"`
}

// Frames is a frame list in OpenJD's range grammar ("1001-1240:2"); a
// single frame written as a number reads as its decimal string.
type Frames string

// UnmarshalJSON takes a string or a whole number.
func (f *Frames) UnmarshalJSON(b []byte) error {
	var n json.Number
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return err
	}
	switch v := v.(type) {
	case string:
		*f = Frames(v)
	case json.Number:
		n = v
		*f = Frames(n.String())
	default:
		return fmt.Errorf("frames is a frame list or a frame")
	}
	return nil
}

// After is the jobs a job follows, each with how: "" (the default: frame
// by frame when both have frames, else when it is done), "frames", "all"
// or "previous". A key, a list of keys and a map all read as this.
type After []AfterJob

// AfterJob is one job followed, and how.
type AfterJob struct {
	Job string
	How string
}

// UnmarshalJSON takes a key, a list of keys, or a map of key to how. A
// map keeps the order written.
func (a *After) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) > 0 && b[0] == '"':
		var k string
		if err := json.Unmarshal(b, &k); err != nil {
			return err
		}
		*a = After{{Job: k}}
	case len(b) > 0 && b[0] == '[':
		var ks []string
		if err := json.Unmarshal(b, &ks); err != nil {
			return err
		}
		*a = nil
		for _, k := range ks {
			*a = append(*a, AfterJob{Job: k})
		}
	default:
		keys, raws, err := orderedObject(b)
		if err != nil {
			return err
		}
		*a = nil
		for i, k := range keys {
			var how string
			if err := json.Unmarshal(raws[i], &how); err != nil {
				return err
			}
			*a = append(*a, AfterJob{Job: k, How: how})
		}
	}
	return nil
}

// Chunk is frames per unit: N, or All for one unit over the whole range.
type Chunk struct {
	N   int
	All bool
}

// UnmarshalJSON takes a whole number or "all".
func (c *Chunk) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == `"all"` {
		*c = Chunk{All: true}
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("chunk is a number of frames or all")
	}
	*c = Chunk{N: n}
	return nil
}

// MarshalJSON writes the number, or "all".
func (c Chunk) MarshalJSON() ([]byte, error) {
	if c.All {
		return []byte(`"all"`), nil
	}
	return json.Marshal(c.N)
}

// Resources are what a unit needs, or a submission's overrides of them.
// Quantities keep their units as written ("64G").
type Resources struct {
	CPU       float64 `json:"cpu,omitempty"`
	Memory    string  `json:"memory,omitempty"`
	GPU       *int    `json:"gpu,omitempty"`
	GPUMemory string  `json:"gpuMemory,omitempty"`
	Scratch   string  `json:"scratch,omitempty"`
}

// Check is a check tool over a job's units.
type Check struct {
	Tool   string         `json:"tool"`
	With   map[string]any `json:"with,omitempty"`
	Every  int            `json:"every,omitempty"`
	Rework *int           `json:"rework,omitempty"`
	Then   string         `json:"then,omitempty"`
}

// Approve is a person's decision, declared on purpose: After "preview", or
// at the end of the job when After is empty.
type Approve struct {
	By      string   `json:"by"`
	After   string   `json:"after,omitempty"`
	Ask     string   `json:"ask,omitempty"`
	Expect  Duration `json:"expect,omitempty"`
	Choices []string `json:"choices,omitempty"`
}

// Wait is a wait on a party or person (For), or a clock (Until).
type Wait struct {
	For     string   `json:"for,omitempty"`
	Until   string   `json:"until,omitempty"`
	Ask     string   `json:"ask,omitempty"`
	Expect  Duration `json:"expect,omitempty"`
	Choices []string `json:"choices,omitempty"`
	Reply   bool     `json:"reply,omitempty"`
}

// Answers names the agent that answers a job's units' questions: "lead",
// "tool:<name>" or "unit:<name>".
type Answers struct {
	By   string         `json:"by"`
	With map[string]any `json:"with,omitempty"`
}

// Use is the settings for a tool a generator's children run.
type Use struct {
	With  map[string]any `json:"with,omitempty"`
	Size  string         `json:"size,omitempty"`
	Needs *Resources     `json:"needs,omitempty"`
	Pools []string       `json:"pools,omitempty"`
}

// Each is the tool a plan's planned blocks run.
type Each struct {
	Tool string         `json:"tool"`
	With map[string]any `json:"with,omitempty"`
}

// Measure is a job's measure of done.
type Measure struct {
	Name     string  `json:"name"`
	Of       float64 `json:"of"`
	Required bool    `json:"required,omitempty"`
}

// Axes are the values to make units over, by parameter name, in the order
// written.
type Axes []Axis

// Axis is one parameter's values.
type Axis struct {
	Name   string
	Values []any
}

// UnmarshalJSON keeps the order the names were written in.
func (a *Axes) UnmarshalJSON(b []byte) error {
	keys, raws, err := orderedObject(b)
	if err != nil {
		return err
	}
	*a = nil
	for i, k := range keys {
		var vs []any
		if err := json.Unmarshal(raws[i], &vs); err != nil {
			return err
		}
		*a = append(*a, Axis{Name: k, Values: vs})
	}
	return nil
}

// Duration is a time as written: "2d", "90m", or seconds as a number
// (which reads as "<n>s").
type Duration string

// UnmarshalJSON takes a string or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*d = Duration(s)
		return nil
	}
	if _, err := strconv.ParseFloat(string(b), 64); err != nil {
		return fmt.Errorf("a duration is a string or seconds")
	}
	*d = Duration(string(b) + "s")
	return nil
}

// UnmarshalJSON reads the jobs in the order written, each with its key.
func (js *Jobs) UnmarshalJSON(b []byte) error {
	keys, raws, err := orderedObject(b)
	if err != nil {
		return err
	}
	*js = make(Jobs, len(keys))
	for i, k := range keys {
		if err := json.Unmarshal(raws[i], &(*js)[i]); err != nil {
			return fmt.Errorf("jobs.%s: %w", k, err)
		}
		(*js)[i].Key = k
	}
	return nil
}

// A Tool is a c4: tool/1 document: how one kind of work runs.
type Tool struct {
	C4         string         `json:"c4"`
	Name       string         `json:"name"`
	Title      string         `json:"title,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Owner      string         `json:"owner,omitempty"`
	Docs       string         `json:"docs,omitempty"`
	Kind       string         `json:"kind"`
	Runs       Runs           `json:"runs"`
	Params     []Param        `json:"params,omitempty"`
	Frames     *FrameDefaults `json:"frames,omitempty"`
	Writes     []string       `json:"writes,omitempty"`
	Previews   string         `json:"previews,omitempty"`
	Needs      Needs          `json:"needs,omitempty"`
	Progress   []ProgressRule `json:"progress,omitempty"`
	Errors     Errors         `json:"errors,omitempty"`
	Stall      *Stall         `json:"stall,omitempty"`
	Retries    *int           `json:"retries,omitempty"`
	RetryOn    *RetryOn       `json:"retryOn,omitempty"`
	Reuse      string         `json:"reuse,omitempty"`
	Emits      []string       `json:"emits,omitempty"`
	Judges     string         `json:"judges,omitempty"`
	Rework     *Rework        `json:"rework,omitempty"`
	Gate       string         `json:"gate,omitempty"`
	AIUsage    map[string]any `json:"aiUsage,omitempty"`
	Test       *Test          `json:"test,omitempty"`
	Kubernetes *Kubernetes    `json:"kubernetes,omitempty"`
}

// Param returns the parameter with the name, or nil.
func (t *Tool) Param(name string) *Param {
	for i := range t.Params {
		if t.Params[i].Name == name {
			return &t.Params[i]
		}
	}
	return nil
}

// Runs is how one unit runs.
type Runs struct {
	Image    string            `json:"image,omitempty"`
	Exe      string            `json:"exe,omitempty"`
	Host     *Host             `json:"host,omitempty"`
	Command  []string          `json:"command"`
	Env      map[string]string `json:"env,omitempty"`
	Workdir  string            `json:"workdir,omitempty"`
	Sidecars []Sidecar         `json:"sidecars,omitempty"`
}

// Host is a tool's host runner: the capabilities a host must advertise,
// and the program's path per operating system (linux, windows, macos).
type Host struct {
	Has []string          `json:"has"`
	Exe map[string]string `json:"exe,omitempty"`
}

// Sidecar is a server that runs beside the unit, at $C4_TOOL_<NAME>.
type Sidecar struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Args  []string          `json:"args,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	Port  int               `json:"port"`
}

// Param is an OpenJD job parameter definition, plus Show, From and
// AffectsOutput.
type Param struct {
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	Description   string         `json:"description,omitempty"`
	Default       any            `json:"default,omitempty"`
	AllowedValues []any          `json:"allowedValues,omitempty"`
	MinValue      *float64       `json:"minValue,omitempty"`
	MaxValue      *float64       `json:"maxValue,omitempty"`
	MinLength     *int           `json:"minLength,omitempty"`
	MaxLength     *int           `json:"maxLength,omitempty"`
	ObjectType    string         `json:"objectType,omitempty"`
	DataFlow      string         `json:"dataFlow,omitempty"`
	UserInterface *UserInterface `json:"userInterface,omitempty"`
	Show          string         `json:"show,omitempty"`
	From          string         `json:"from,omitempty"`
	AffectsOutput *bool          `json:"affectsOutput,omitempty"`
}

// Required says whether the parameter must be given: it has no default.
func (p *Param) Required() bool { return p.Default == nil }

// UserInterface is OpenJD's hints for drawing a parameter.
type UserInterface struct {
	Control           string       `json:"control,omitempty"`
	Label             string       `json:"label,omitempty"`
	GroupLabel        string       `json:"groupLabel,omitempty"`
	SingleStepDelta   *float64     `json:"singleStepDelta,omitempty"`
	Decimals          *int         `json:"decimals,omitempty"`
	FileFilters       []FileFilter `json:"fileFilters,omitempty"`
	FileFilterDefault *FileFilter  `json:"fileFilterDefault,omitempty"`
}

// FileFilter is one entry of a file chooser's filter list.
type FileFilter struct {
	Label    string   `json:"label"`
	Patterns []string `json:"patterns"`
}

// FrameDefaults are a frames tool's chunk and order.
type FrameDefaults struct {
	Chunk *Chunk `json:"chunk,omitempty"`
	Order string `json:"order,omitempty"`
}

// Needs is what a tool's unit needs.
type Needs struct {
	Resources
	Farm     string               `json:"farm,omitempty"`
	Licences map[string]int       `json:"licences,omitempty"`
	Network  []string             `json:"network,omitempty"`
	Sizes    map[string]Resources `json:"sizes,omitempty"`
}

// ProgressRule maps a log line to progress: Percent or Frame is the
// capture group that holds it.
type ProgressRule struct {
	Match   string `json:"match"`
	Percent int    `json:"percent,omitempty"`
	Frame   int    `json:"frame,omitempty"`
}

// Errors classify a failure by its log lines.
type Errors struct {
	Licence   []string `json:"licence,omitempty"`
	Permanent []string `json:"permanent,omitempty"`
	Question  []string `json:"question,omitempty"`
	Retry     []string `json:"retry,omitempty"`
	Ignore    []string `json:"ignore,omitempty"`
}

// Stall ends a unit whose log has been quiet this long.
type Stall struct {
	QuietMinutes int `json:"quietMinutes"`
}

// RetryOn lists exit codes that are retried.
type RetryOn struct {
	ExitCodes []int `json:"exitCodes,omitempty"`
}

// Rework bounds a check's send-backs.
type Rework struct {
	MaxPasses   *int   `json:"maxPasses,omitempty"`
	OnExhausted string `json:"onExhausted,omitempty"`
}

// Test is a tool's own test.
type Test struct {
	Tree   string         `json:"tree"`
	Frames Frames         `json:"frames,omitempty"`
	With   map[string]any `json:"with,omitempty"`
	Expect []string       `json:"expect,omitempty"`
}

// Kubernetes is a tool's typed overlay of pod and container fields.
type Kubernetes struct {
	Pod       map[string]any `json:"pod,omitempty"`
	Container map[string]any `json:"container,omitempty"`
}

// A Lock is a published tool's tool.lock (c4: lock/1): each image the tool
// names (runs.image and every sidecar's), as written, pinned by digest.
type Lock struct {
	C4     string            `json:"c4"`
	Images map[string]string `json:"images"`
}

// orderedObject reads a JSON object's keys in order, with each value raw.
func orderedObject(b []byte) ([]string, []json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	t, err := d.Token()
	if err != nil {
		return nil, nil, err
	}
	if t != json.Delim('{') {
		return nil, nil, fmt.Errorf("want an object")
	}
	var keys []string
	var raws []json.RawMessage
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, nil, err
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, nil, err
		}
		keys = append(keys, t.(string))
		raws = append(raws, raw)
	}
	return keys, raws, nil
}

// ImageRef is the image a tool's runs.image (or a sidecar's image) names.
// c4/<name>[:<tag>] is one of the farm's own images,
// ghcr.io/avalanche-io/c4-<name>, at the tag written, else release. Any
// other name is returned as written. Publishing resolves the result to a
// digest for tool.lock.
// ImageRegistry is where the farm's own images (c4/<name>) live.
const ImageRegistry = "ghcr.io/avalanche-io/"

func ImageRef(written, release string) string {
	rest, ok := strings.CutPrefix(written, "c4/")
	if !ok || rest == "" {
		return written
	}
	name, tag, tagged := strings.Cut(rest, ":")
	if !tagged {
		tag = release
	}
	return ImageRegistry + "c4-" + name + ":" + tag
}
