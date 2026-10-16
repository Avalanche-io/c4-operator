package class

import (
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/estimate"
	"github.com/Avalanche-io/c4-operator/internal/work/order"
)

func init() {
	register(render{})
	register(ci{})
	register(ml{})
}

// render: one unit per chunk of frames of one pass, in binary order by
// default, due at the job's deadline or a default turnaround.
//
//	params: frames "1001-1240" (required), chunk "1", order "binary",
//	        pass "beauty", turnaround "24h", estimateSeconds
type render struct{}

func (render) Name() string      { return "render" }
func (render) Pools() []string   { return []string{"cpu"} }
func (render) Preemptible() bool { return true }
func (render) Checkpoints() bool { return false }
func (render) Lens() string      { return "render" }

func (render) Plan(job *c4v1.WorkJob) (*Plan, error) {
	frames, err := order.ParseFrames(param(job, "frames", ""))
	if err != nil {
		return nil, fmt.Errorf("param frames: %w", err)
	}
	chunk, err := intParam(job, "chunk", 1, 1, 1000)
	if err != nil {
		return nil, err
	}
	kind, err := orderKind(param(job, "order", "binary"))
	if err != nil {
		return nil, err
	}
	prior, err := priorParam(job)
	if err != nil {
		return nil, err
	}
	pass := param(job, "pass", "beauty")
	chunks := order.Chunk(frames, chunk)
	p := &Plan{OrderName: string(kind), Estimate: estimate.DefaultConfig(), Prior: prior,
		Axis: Axis{Kind: "frame", Start: frames[0], Step: 1}}
	if len(frames) > 1 {
		p.Axis.Step = frames[1] - frames[0]
	}
	for i, c := range chunks {
		label := strconv.Itoa(c[0])
		if len(c) > 1 {
			label = fmt.Sprintf("%d-%d", c[0], c[len(c)-1])
		}
		p.Units = append(p.Units, Unit{
			Index: i, Label: label, Key: "render/" + pass, Pos: c[0] - frames[0], Slots: 1,
			Env: []corev1.EnvVar{env("C4_FRAMES", label), env("C4_FRAME_START", strconv.Itoa(c[0])),
				env("C4_FRAME_END", strconv.Itoa(c[len(c)-1])), env("C4_PASS", pass)},
		})
	}
	p.Order, err = order.Units(kind, len(p.Units), nil)
	if err != nil {
		return nil, err
	}
	if kind == order.Binary {
		// The coarse levels, up to every 8th unit, are information: they
		// reveal what the rest of the shot costs.
		for _, i := range p.Order[:order.CoarsePrefix(len(p.Units), 8)] {
			p.Units[i].Info = true
		}
	}
	return p, nil
}

func (render) Objective(job *c4v1.WorkJob, ready time.Time) Objective {
	if job.Spec.Deadline != nil {
		return Objective{Type: "deadline", Deadline: job.Spec.Deadline.Time}
	}
	t, err := durationParam(job, "turnaround", 24*time.Hour)
	if err != nil {
		t = 24 * time.Hour
	}
	return Objective{Type: "deadline", Deadline: job.CreationTimestamp.Add(t)}
}

// ci: one unit per test shard, due a latency target after the job became
// ready (DESIGN.md section 10.7: a latency SLO becomes a deadline).
//
//	params: shards "16", target "//...", latencyTarget "10m", estimateSeconds
type ci struct{}

func (ci) Name() string      { return "ci" }
func (ci) Pools() []string   { return []string{"cpu"} }
func (ci) Preemptible() bool { return false }
func (ci) Checkpoints() bool { return false }
func (ci) Lens() string      { return "ci" }

func (ci) Plan(job *c4v1.WorkJob) (*Plan, error) {
	n, err := intParam(job, "shards", 8, 1, 10000)
	if err != nil {
		return nil, err
	}
	prior, err := priorParam(job)
	if err != nil {
		return nil, err
	}
	target := param(job, "target", "//...")
	cfg := estimate.DefaultConfig()
	cfg.NoLocality = true
	cfg.Default = estimate.Prior{Mean: 120, SD: 60}
	p := &Plan{OrderName: string(order.Sequential), Estimate: cfg, Prior: prior, Axis: Axis{Kind: "shard"}}
	for i := 0; i < n; i++ {
		p.Units = append(p.Units, Unit{Index: i, Label: fmt.Sprintf("shard %d", i), Key: "ci/" + target, Pos: i, Slots: 1,
			Env: []corev1.EnvVar{env("C4_SHARD", strconv.Itoa(i)), env("C4_SHARDS", strconv.Itoa(n)), env("C4_TARGET", target)}})
	}
	p.Order = order.SequentialOrder(n)
	return p, nil
}

func (ci) Objective(job *c4v1.WorkJob, ready time.Time) Objective {
	if job.Spec.Deadline != nil {
		return Objective{Type: "deadline", Deadline: job.Spec.Deadline.Time}
	}
	t, err := durationParam(job, "latencyTarget", 10*time.Minute)
	if err != nil {
		t = 10 * time.Minute
	}
	return Objective{Type: "latency", Deadline: ready.Add(t), LatencyTarget: t}
}

// ml: one training run, a gang of ranks that checkpoints and can yield at a
// checkpoint boundary. Placement of real multi-node gangs is delegated to
// Kueue and JobSet (DESIGN.md section 10.15); this driver runs the gang as
// one pod holding all of its slots.
//
//	params: ranks "2", steps "600", checkpointEvery "50", deadline via spec
//	        or turnaround "36h", estimateSeconds
type ml struct{}

func (ml) Name() string      { return "ml" }
func (ml) Pools() []string   { return []string{"gpu"} }
func (ml) Preemptible() bool { return true }
func (ml) Checkpoints() bool { return true }
func (ml) Lens() string      { return "ml" }

func (ml) Plan(job *c4v1.WorkJob) (*Plan, error) {
	ranks, err := intParam(job, "ranks", 1, 1, 64)
	if err != nil {
		return nil, err
	}
	steps, err := intParam(job, "steps", 600, 1, 1<<30)
	if err != nil {
		return nil, err
	}
	every, err := intParam(job, "checkpointEvery", 50, 1, 1<<30)
	if err != nil {
		return nil, err
	}
	prior, err := priorParam(job)
	if err != nil {
		return nil, err
	}
	p := &Plan{OrderName: "gang", Estimate: estimate.DefaultConfig(), Prior: prior, Axis: Axis{Kind: "step"}}
	p.Units = []Unit{{Index: 0, Label: "run", Key: "ml/run", Pos: 0, Slots: ranks,
		Env: []corev1.EnvVar{env("C4_RANKS", strconv.Itoa(ranks)), env("C4_STEPS", strconv.Itoa(steps)),
			env("C4_CHECKPOINT_EVERY", strconv.Itoa(every))}}}
	p.Order = []int{0}
	return p, nil
}

func (ml) Objective(job *c4v1.WorkJob, ready time.Time) Objective {
	if job.Spec.Deadline != nil {
		return Objective{Type: "deadline", Deadline: job.Spec.Deadline.Time}
	}
	t, err := durationParam(job, "turnaround", 36*time.Hour)
	if err != nil {
		t = 36 * time.Hour
	}
	return Objective{Type: "deadline", Deadline: job.CreationTimestamp.Add(t)}
}

func orderKind(s string) (order.Kind, error) {
	switch s {
	case "binary", "Binary":
		return order.Binary, nil
	case "sequential", "Sequential":
		return order.Sequential, nil
	case "reverse", "Reverse":
		return order.Reverse, nil
	}
	return "", fmt.Errorf("param order=%q: want binary, sequential or reverse", s)
}
