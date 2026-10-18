package queue

import (
	"context"
	"math"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/process"
)

// Derivations (work/process; proposal eta-deadlines section 13). Every
// unit that succeeds cleanly is a derivation: its process (images,
// command, parameters, platform) and the views it mounted, giving the
// view it wrote, with what it cost. The queue keeps one book per project
// and uses it for the estimate of a unit that ran before with the same
// process and inputs, and to say what recomputing an output would cost.
//
// Experimental: the annotation, the status field and the record format
// will change with C4's derivation-record design.

// AnnotationDerivationClass on a WorkJob declares its process's class:
// exact (a fresh run writes the same bytes), validated, or volatile (the
// default). Experimental.
const AnnotationDerivationClass = "c4.avalanche.io/derivation-class"

// Node labels the platform of GPU work is read from (NVIDIA GPU feature
// discovery). Work on a node with a GPU is compared only with work on the
// same GPU model, driver and compute stack.
var platformLabels = []string{
	"nvidia.com/gpu.product", "nvidia.com/cuda.driver.major", "nvidia.com/cuda.driver.minor",
	"nvidia.com/cuda.driver.rev", "nvidia.com/cuda.runtime.major", "nvidia.com/cuda.runtime.minor",
}

// platformOf is a node's GPU platform, "" for a node without a GPU.
func platformOf(n *corev1.Node) string {
	if n == nil {
		return ""
	}
	var parts []string
	for _, l := range platformLabels {
		if v := n.Labels[l]; v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "/")
	}
	if q, ok := n.Status.Capacity["nvidia.com/gpu"]; ok && !q.IsZero() {
		// A GPU without discovery labels: only this node is comparable.
		return "gpu-node/" + n.Name
	}
	return ""
}

// containersOf is the process view of a pod spec: each container's image
// digest (from the status when given, else a digest the spec pins),
// command, arguments, environment and working directory. It also says
// whether the spec keeps the unit to its closure, and if not, why.
func containersOf(spec corev1.PodSpec, statuses []corev1.ContainerStatus, digestOf map[string]string) ([]process.Container, bool, string) {
	sealed, why := true, ""
	unseal := func(s string) {
		if sealed {
			sealed, why = false, s
		}
	}
	if spec.HostNetwork {
		unseal("the pod uses the host network")
	}
	for _, v := range spec.Volumes {
		if v.HostPath != nil {
			unseal("the pod mounts a host path")
		}
	}
	status := map[string]string{}
	for _, s := range statuses {
		if _, d, ok := strings.Cut(s.ImageID, "@"); ok {
			status[s.Name] = d
		}
	}
	out := make([]process.Container, 0, len(spec.Containers))
	for _, c := range spec.Containers {
		pc := process.Container{Command: c.Command, Args: c.Args, WorkingDir: c.WorkingDir, Env: map[string]string{}}
		_, pinned, isPinned := strings.Cut(c.Image, "@")
		switch {
		case status[c.Name] != "":
			pc.Digest = status[c.Name]
		case isPinned:
			pc.Digest = pinned
		default:
			pc.Digest = digestOf[c.Image]
		}
		if !isPinned {
			unseal("an image is not pinned by digest")
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil {
				// A secret's or a field's value is not in the record.
				pc.Env[e.Name] = "valueFrom"
				unseal("the environment reads a secret, a config map or a field")
				continue
			}
			pc.Env[e.Name] = e.Value
		}
		if len(c.EnvFrom) > 0 {
			unseal("the environment reads a secret or a config map")
		}
		out = append(out, pc)
	}
	return out, sealed, why
}

// unitContainers is what a unit's pod will run, before it runs: the
// template with the environment podFor gives every unit (less the
// variables that name the run, which the process ID leaves out anyway).
func unitContainers(j *c4v1.WorkJob, u class.Unit, digestOf map[string]string) []process.Container {
	spec := j.Spec.Template.Spec.DeepCopy()
	extra := append([]corev1.EnvVar{{Name: "C4_UNIT_LABEL", Value: u.Label}}, u.Env...)
	for i := range spec.Containers {
		spec.Containers[i].Env = append(spec.Containers[i].Env, extra...)
	}
	cs, _, _ := containersOf(*spec, nil, digestOf)
	return cs
}

// egressDenied reports whether a NetworkPolicy in the pod's namespace
// selects it and allows no egress.
func (q *Queue) egressDenied(ctx context.Context, ns string, podLabels map[string]string) bool {
	var l networkingv1.NetworkPolicyList
	if err := q.Client.List(ctx, &l, client.InNamespace(ns)); err != nil {
		return false
	}
	for _, np := range l.Items {
		sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
		if err != nil || !sel.Matches(labels.Set(podLabels)) || len(np.Spec.Egress) > 0 {
			continue
		}
		for _, t := range np.Spec.PolicyTypes {
			if t == networkingv1.PolicyTypeEgress {
				return true
			}
		}
	}
	return false
}

// inputViews resolves the views a job mounts: each input's mount path
// and the view its Materialization resolved to. It reports false while
// any input is unresolved.
func (q *Queue) inputViews(ctx context.Context, j *c4v1.WorkJob) ([]process.Input, bool) {
	var out []process.Input
	for _, in := range j.Spec.Inputs {
		name := in.Materialization
		if in.FromJob != "" {
			name = fromJobName(j.Name, in.FromJob)
		}
		var m c4v1.Materialization
		if err := q.Client.Get(ctx, types.NamespacedName{Namespace: j.Namespace, Name: name}, &m); err != nil || m.Status.ViewID == "" {
			return nil, false
		}
		out = append(out, process.Input{Mount: in.MountPath, View: m.Status.ViewID})
	}
	return out, true
}

// book is a project's record of derivations.
func (q *Queue) book(project string) *process.Book {
	if q.books == nil {
		q.books = map[string]*process.Book{}
	}
	b := q.books[project]
	if b == nil {
		b = &process.Book{}
		q.books[project] = b
	}
	return b
}

// recordDerivation adds a unit that succeeded to its project's book. A
// unit that resumed from a checkpoint is not a clean run of its
// derivation and is left out, as is one whose output was not captured or
// whose process or inputs cannot be identified.
func (q *Queue) recordDerivation(ctx context.Context, v *view, idx int, p *corev1.Pod, ms int64) {
	out := p.Annotations[c4v1.AnnotationOutputTree]
	if out == "" || ms <= 0 || v.mem.plan == nil || idx >= len(v.mem.plan.Units) {
		return
	}
	for _, c := range p.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "C4_RESUME_VIEW" {
				return
			}
		}
	}
	for _, s := range p.Status.ContainerStatuses {
		if _, d, ok := strings.Cut(s.ImageID, "@"); ok {
			if q.digests == nil {
				q.digests = map[string]string{}
			}
			for _, c := range p.Spec.Containers {
				if c.Name == s.Name {
					q.digests[c.Image] = d
				}
			}
		}
	}
	cs, sealed, why := containersOf(p.Spec, p.Status.ContainerStatuses, q.digests)
	var node corev1.Node
	var np *corev1.Node
	if p.Spec.NodeName != "" && q.Client.Get(ctx, types.NamespacedName{Name: p.Spec.NodeName}, &node) == nil {
		np = &node
	}
	platform := platformOf(np)
	u := v.mem.plan.Units[idx]
	proc, ok := process.ProcessID(cs, platform)
	if !ok {
		return
	}
	program, _ := process.ProgramID(cs, platform, perUnit(u))
	if q.lastPlatform == nil {
		q.lastPlatform = map[string]string{}
	}
	q.lastPlatform[v.key] = platform
	inputs, ok := q.inputViews(ctx, v.job)
	if !ok {
		return
	}
	in, ok := process.InputsID(inputs)
	if !ok {
		return
	}
	if sealed && !q.egressDenied(ctx, p.Namespace, p.Labels) {
		sealed, why = false, "no network policy denies the pod egress"
	}
	declared := process.ParseClass(v.job.Annotations[AnnotationDerivationClass])
	slots := max(1, u.Slots)
	q.book(v.project).Record(process.Run{Key: process.Key{Process: proc, Inputs: in}, Program: program, Inputs: inputs, Declared: declared,
		Sealed: sealed, Output: out, Work: float64(ms) / 1000 * float64(slots), Duration: float64(ms) / 1000, At: finishOf(p, time.Now())})
	m := v.mem
	m.proc, m.unsealedWhy = program, why
}

// perUnit are the environment variables that tell one unit of a job from
// another: the class driver's per-unit variables and the unit's label.
func perUnit(u class.Unit) map[string]bool {
	out := map[string]bool{"C4_UNIT_LABEL": true}
	for _, e := range u.Env {
		out[e.Name] = true
	}
	return out
}

// useDerivations gives a job's estimator, for each unit whose exact
// derivation ran before in its project, what it took then. It overrides
// the shot history's profile at those positions.
func (q *Queue) useDerivations(ctx context.Context, v *view, byKey map[string]map[int]float64) {
	b := q.books[v.project]
	if b == nil || b.Len() == 0 {
		return
	}
	plan := v.mem.plan
	var inID string
	resolved := false
	for _, u := range plan.Units {
		cs := unitContainers(v.job, u, q.digests)
		proc, ok := process.ProcessID(cs, q.lastPlatform[v.key])
		if !ok {
			continue
		}
		program, _ := process.ProgramID(cs, q.lastPlatform[v.key], perUnit(u))
		if !b.Has(program) {
			continue
		}
		if !resolved {
			resolved = true
			inputs, ok := q.inputViews(ctx, v.job)
			if !ok {
				return
			}
			if inID, ok = process.InputsID(inputs); !ok {
				return
			}
		}
		cost, ok := b.Recompute(process.Key{Process: proc, Inputs: inID}, program)
		if !ok || cost.Basis != "exact" || cost.Work.Mean <= 0 {
			continue
		}
		if byKey[u.Key] == nil {
			byKey[u.Key] = map[int]float64{}
		}
		byKey[u.Key][u.Pos] = cost.Work.Mean / float64(max(1, u.Slots))
	}
}

// derivationStatus is a job's record as its status reports it.
func (q *Queue) derivationStatus(v *view) *c4v1.WorkDerivation {
	m := v.mem
	if m.proc == "" {
		return nil
	}
	b := q.books[v.project]
	if b == nil {
		return nil
	}
	c, verified := b.Class(m.proc)
	matched, mismatched := b.Evidence(m.proc)
	st := &c4v1.WorkDerivation{Program: m.proc, Declared: string(process.ParseClass(v.job.Annotations[AnnotationDerivationClass])),
		Class: string(c), Verified: verified, Matched: int32(matched), Mismatched: int32(mismatched), Why: b.Why(m.proc)}
	if st.Why == "" && m.unsealedWhy != "" && st.Declared == string(process.Exact) {
		st.Why = "declared exact, but " + m.unsealedWhy
	}
	return st
}

// RecomputeCost is what recomputing an output would cost in its project,
// from the record: for the scheduler's choice between moving an output,
// recomputing it, and reusing an identical one (proposals/THREADS.md,
// "Recompute is a transfer channel"). It reports false when the output's
// derivation is not recorded. Reuse is withheld for the share of
// opportunities sampled for verification; u is the caller's uniform draw.
func (q *Queue) RecomputeCost(project, output string, u float64) (process.Key, []process.Input, process.Cost, bool) {
	q.mu.Lock()
	b := q.books[project]
	q.mu.Unlock()
	if b == nil {
		return process.Key{}, nil, process.Cost{}, false
	}
	k, program, inputs, ok := b.ProducedBy(output)
	if !ok {
		return process.Key{}, nil, process.Cost{}, false
	}
	c, ok := b.Recompute(k, program)
	if !ok {
		return k, inputs, c, false
	}
	if c.Reuse != "" && b.NeedsVerification(program, u) {
		c.Reuse = ""
	}
	if math.IsNaN(c.W90) {
		c.W90 = c.Work.Mean
	}
	return k, inputs, c, true
}
