// Package class holds the work classes (DESIGN.md section 10.4): what
// differs between render, CI and ML work. The queue imports drivers only
// through Lookup, and nothing in the queue mentions frames, shards or
// ranks.
package class

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// Unit is one unit of a job's plan.
type Unit struct {
	// Index is stable for the life of the job.
	Index int
	// Label is what a person reads: "1001", "1001-1004", "shard 3".
	Label string
	// Key and Pos place the unit for the estimator: units of one key are
	// comparable, and Pos is the unit's place on the job's axis.
	Key string
	Pos int
	// Slots the unit occupies while running (a gang's size).
	Slots int
	// Info marks units that reveal what the rest of the job costs (the
	// coarse levels of binary order).
	Info bool
	// Env is added to every container of the unit's pod.
	Env []corev1.EnvVar

	// The unit graph (DESIGN 10.6). Name names the unit within the job,
	// for edges and inputs; the class may leave it empty. After lists the
	// units that must succeed first, by index.
	Name  string
	After []int
	// Generator: when the unit succeeds, the expansion document it wrote
	// adds units under it. Until it has run it is planned.
	Generator bool
	// Exists is the probability the unit runs (0 means 1). It weights the
	// unit's expected work; the unit still runs when its turn comes.
	Exists float64
	// Parent is the generator whose expansion added the unit, and Depth
	// how deeply nested it is; at Depth 0 the class planned it and Parent
	// means nothing.
	Parent, Depth int
	// Inputs mount other units' captured output views; each is also in
	// After.
	Inputs []UnitInput
	// Work and WorkSD are a prior for the unit's work in slot-seconds (0:
	// none), from the document that added it.
	Work, WorkSD float64
	// Group (the HUD row), Holds (paths the unit writes, for claims) and
	// Params (facts from its plan) are carried from the document.
	Group  string
	Holds  []string
	Params map[string]string
	// Claims are what the unit writes, with a mode (claims.go): its
	// holds as exclusive claims, and the document's claims.
	Claims []Claim
	// Reasoning is what the unit reasons with, as its document or plan
	// declares it: a model or a procedure, which model, at what effort,
	// with which tool profile. An empty Does takes the class's (Doer),
	// else procedure (DoesOf).
	Reasoning Reasoning
	// Gate names the gate the unit passes (a unit of kind gate): a
	// resource of one slot shared by the jobs of a namespace, taken first
	// come, first served (the queue's gates.go).
	Gate string
	// Wait makes the unit a wait (PRIMITIVES Q6): it holds no slot and
	// creates no pod; it completes on a signed decision, or at its time.
	Wait *Wait
}

// Wait is what a wait unit waits for.
type Wait struct {
	// Server is person:<id>, party:<name> or clock:<RFC 3339 time>.
	Server string
	// Ask is the question, in plain words.
	Ask string
	// Expect is the expected wait in seconds, before the job has measured
	// the server's own decisions (0: the queue's default).
	Expect float64
	// Choices, when set, are the decisions that complete it, besides fail.
	Choices []string
	// Reply: the decision carries content, the unit's output view.
	Reply bool
	// At is a clock wait's time.
	At time.Time
}

// Axis describes the job's unit axis for the HUD.
type Axis struct {
	Kind  string // frame, shard, step
	Start int
	Step  int
}

// Plan is a job's units in dispatch order.
type Plan struct {
	Units []Unit
	// Order is the dispatch order, a permutation of unit indexes.
	Order     []int
	OrderName string
	Axis      Axis
	// Estimator settings and a prior for the job's key.
	Estimate estimate.Config
	Prior    *estimate.Prior
}

// Objective is a job's effective objective at a time.
type Objective struct {
	// Deadline is the effective deadline; zero means none.
	Deadline time.Time
	// Rate is a throughput objective in slots, used when Deadline is zero.
	Rate float64
	// Type is deadline, latency or throughput.
	Type string
	// LatencyTarget is set for latency objectives.
	LatencyTarget time.Duration
}

// Driver is a work class.
type Driver interface {
	Name() string
	// Plan lays out the job's units. It depends only on the spec.
	Plan(job *c4v1.WorkJob) (*Plan, error)
	// Objective returns the job's effective objective; ready is when the
	// job's dependencies were met.
	Objective(job *c4v1.WorkJob, ready time.Time) Objective
	// Pools are the default pools for jobs that name none.
	Pools() []string
	// Preemptible says whether the class's units may be stopped to make
	// room, and Checkpoints whether they can yield at a checkpoint.
	Preemptible() bool
	Checkpoints() bool
	// Lens names the HUD lens.
	Lens() string
}

// Resumer is implemented by classes whose units resume from a checkpoint
// their runner reports (AnnotationOutputCheckpoint), not from a captured
// view: an agent member resumes its own session. The queue then mounts no
// resume Materialization, and adds ResumeEnv to the next attempt.
type Resumer interface {
	ResumeEnv(point string) []corev1.EnvVar
}

// Checkpointer is implemented by classes whose units checkpoint: how long
// one needs, from SIGTERM, to reach its next checkpoint and save. The
// queue sets the units' grace period from it (when the job has no learned
// cadence of safe points).
type Checkpointer interface {
	CheckpointTime(job *c4v1.WorkJob) time.Duration
}

// What does a unit's work.
const (
	DoesProcedure = "procedure"
	DoesModel     = "model"
)

// Doer is implemented by classes whose units are model units by default:
// an agent member's turns are a model's.
type Doer interface {
	UnitDoes() string
}

// Reasoning is what a unit reasons with (LLM-OR-PROCEDURE 6.2): Does is
// model or procedure; Model the model, Effort its effort level (low,
// medium, high, xhigh, max), Tools the tool profile's name. All optional.
type Reasoning struct {
	Does   string `json:"does,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Tools  string `json:"tools,omitempty"`
}

// Efforts are the effort levels a reasoning may declare.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// Check refuses a does or effort outside their values.
func (r Reasoning) Check() error {
	if err := checkDoes(r.Does); err != nil {
		return err
	}
	if r.Effort != "" && !slices.Contains(Efforts, r.Effort) {
		return fmt.Errorf("effort %q, want one of %s", r.Effort, strings.Join(Efforts, ", "))
	}
	return nil
}

// ReasoningOf is unit u's reasoning in class d, with the class's does
// when the unit declares none.
func ReasoningOf(d Driver, u Unit) Reasoning {
	r := u.Reasoning
	r.Does = DoesOf(d, u)
	return r
}

// DoesOf is what does unit u's work in class d.
func DoesOf(d Driver, u Unit) string {
	if u.Reasoning.Does != "" {
		return u.Reasoning.Does
	}
	if k, ok := d.(Doer); ok {
		return k.UnitDoes()
	}
	return DoesProcedure
}

func checkDoes(does string) error {
	switch does {
	case "", DoesModel, DoesProcedure:
		return nil
	}
	return fmt.Errorf("does %q, want model or procedure", does)
}

// Mover is implemented by classes whose units the queue may move at a
// safe point without being asked to stop: the unit stops there and its
// next attempt runs in a cheaper pool the job may use. A class opts in
// when resuming elsewhere costs little next to what the move saves.
type Mover interface {
	MovesAtSafepoints() bool
}

var drivers = map[string]Driver{}

func register(d Driver) { drivers[d.Name()] = d }

// Lookup returns the driver of a class.
func Lookup(name string) (Driver, error) {
	d, ok := drivers[name]
	if !ok {
		return nil, fmt.Errorf("unknown work class %q", name)
	}
	return d, nil
}

// Names lists the registered classes.
func Names() []string {
	var out []string
	for n := range drivers {
		out = append(out, n)
	}
	return out
}

// param reads a string parameter with a default.
func param(job *c4v1.WorkJob, name, def string) string {
	if v, ok := job.Spec.Params[name]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func intParam(job *c4v1.WorkJob, name string, def, lo, hi int) (int, error) {
	v := param(job, name, strconv.Itoa(def))
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("param %s=%q: want an integer from %d to %d", name, v, lo, hi)
	}
	return n, nil
}

func durationParam(job *c4v1.WorkJob, name string, def time.Duration) (time.Duration, error) {
	v := param(job, name, def.String())
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("param %s=%q: want a positive duration", name, v)
	}
	return d, nil
}

// priorParam reads estimateSeconds, a person's guess at one unit's cost,
// used until the job has measured units of its own.
func priorParam(job *c4v1.WorkJob) (*estimate.Prior, error) {
	v := param(job, "estimateSeconds", "")
	if v == "" {
		return nil, nil
	}
	s, err := strconv.ParseFloat(v, 64)
	if err != nil || s <= 0 {
		return nil, fmt.Errorf("param estimateSeconds=%q: want a positive number", v)
	}
	return &estimate.Prior{Mean: s, SD: s / 2}, nil
}

func env(name, value string) corev1.EnvVar { return corev1.EnvVar{Name: name, Value: value} }
