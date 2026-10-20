package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/process"
)

// Recompute (experimental) is the staging actuator for a recompute
// (sched.Source Via "recompute"): it rebuilds an input at another site by
// running there the unit that produced it, instead of moving the input.
//
// The recompute is a WorkJob in the producing job's namespace, made from
// that job's spec: its class, parameters and pod template, so the class
// plans the same unit with the same environment, and the process is the
// same one (the job's name and the unit's index are run variables, left
// out of the process key). AnnotationRecomputeUnit keeps the plan to
// that one unit. Its inputs are the views the producing unit read,
// pinned by view ID in Materializations the recompute owns, mounted
// where they were; its pools are the producing job's pools at the site;
// its deadline is when the stage needs the output. An exact process
// writes the same bytes, so the site then holds the input the consuming
// job lacked; a run that writes something else is recorded as a
// mismatch, which demotes the process to validated, and the consumer's
// input is moved or staged again as the site still lacks it.
//
// The unit is taken from the producing job's plan as the queue holds it,
// expansions included, so a unit an expansion added reruns too; the
// recompute carries its definition (AnnotationRecomputeUnit). A unit that
// read other units' outputs has them in the record as inputs (their
// views, at the mounts it read them at), pinned like the job's inputs.

// AnnotationRecomputeUnit on a WorkJob makes it a recompute of one unit:
// the queue plans that unit alone. Its value is the unit's definition
// (JSON: name, label, key, position, slots, reasoning, environment, work).
// Recompute sets it; a person does not.
const AnnotationRecomputeUnit = "c4.avalanche.io/recompute-unit"

// AnnotationRecomputeOf names the job and unit a recompute reruns
// (namespace/name#index), for people and the HUD.
const AnnotationRecomputeOf = "c4.avalanche.io/recompute-of"

// LabelRecompute marks recompute WorkJobs.
const LabelRecompute = "c4.avalanche.io/recompute"

// errNotRerunnable is a recompute the record cannot carry out.
var errNotRerunnable = errors.New("recompute")

// Recompute starts a recompute of key's computation at site, rebuilding
// view, due by.
// It is idempotent: a recompute of the same computation at the same site
// that exists already is left as it is.
func (q *Queue) Recompute(ctx context.Context, project string, key process.Key, inputs []process.Input, site, view string, by time.Time) error {
	b := q.bookOf(project)
	if b == nil {
		return fmt.Errorf("%w: project %s has no derivation record", errNotRerunnable, project)
	}
	pr, ok := b.ProducerOf(key)
	if !ok {
		return fmt.Errorf("%w: no unit that ran %s is recorded", errNotRerunnable, key.Process)
	}
	ns, name, ok := strings.Cut(pr.Job, "/")
	if !ok {
		return fmt.Errorf("%w: producer %q", errNotRerunnable, pr.Job)
	}
	var src c4v1.WorkJob
	if err := q.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &src); err != nil {
		return fmt.Errorf("%w: the producing job %s: %v", errNotRerunnable, pr.Job, err)
	}
	d, err := class.Lookup(src.Spec.Class)
	if err != nil {
		return err
	}
	u, err := q.producingUnit(&src, d, pr.Unit)
	if err != nil {
		return err
	}
	pools := src.Spec.Pools
	if len(pools) == 0 {
		pools = d.Pools()
	}
	pools = q.poolsAt(pools, site)
	if len(pools) == 0 {
		return fmt.Errorf("%w: no pool of %s is at %s", errNotRerunnable, pr.Job, site)
	}

	j := recomputeJob(&src, pr.Unit, u, key, inputs, site, view, pools, by)
	switch err := q.Client.Create(ctx, j); {
	case apierrors.IsAlreadyExists(err):
		return nil
	case err != nil:
		return err
	}
	for i, in := range inputs {
		if _, err := q.ensureMaterialization(ctx, j, j.Spec.Inputs[i].Materialization, in.View, ""); err != nil {
			return err
		}
	}
	q.Log.Info("recompute", "job", j.Name, "of", j.Annotations[AnnotationRecomputeOf], "site", site, "pools", pools, "by", by)
	return nil
}

// producingUnit is unit idx of src: from the plan the queue holds for
// it, expansions included, else from its class's plan.
func (q *Queue) producingUnit(src *c4v1.WorkJob, d class.Driver, idx int) (class.Unit, error) {
	if m := q.jobs[src.UID]; m != nil && m.plan != nil && idx >= 0 && idx < len(m.plan.Units) {
		return m.plan.Units[idx], nil
	}
	plan, err := d.Plan(src)
	if err != nil {
		return class.Unit{}, err
	}
	if idx < 0 || idx >= len(plan.Units) {
		return class.Unit{}, fmt.Errorf("%w: unit %d of %s/%s is not in its plan yet", errNotRerunnable, idx, src.Namespace, src.Name)
	}
	return plan.Units[idx], nil
}

// recomputeSpec is the unit a recompute runs (AnnotationRecomputeUnit).
type recomputeSpec struct {
	Name      string          `json:"name,omitempty"`
	Label     string          `json:"label"`
	Key       string          `json:"key,omitempty"`
	Pos       int             `json:"pos,omitempty"`
	Slots     int             `json:"slots,omitempty"`
	Reasoning class.Reasoning `json:"reasoning,omitempty"`
	Env       []corev1.EnvVar `json:"env,omitempty"`
	Work      float64         `json:"work,omitempty"`
	WorkSD    float64         `json:"workSD,omitempty"`
}

// recomputeJob is the WorkJob that reruns src's unit idx, u, at site.
func recomputeJob(src *c4v1.WorkJob, idx int, u class.Unit, key process.Key, inputs []process.Input, site, view string, pools []string, by time.Time) *c4v1.WorkJob {
	h := sha256.Sum256([]byte(key.Process + "\x00" + key.Inputs + "\x00" + site))
	name := "rc-" + hex.EncodeToString(h[:])[:16]
	// An input the job does not list is another unit's output, read-only.
	readOnly := map[string]bool{}
	for _, in := range inputs {
		readOnly[in.Mount] = true
	}
	for _, in := range src.Spec.Inputs {
		readOnly[in.MountPath] = in.ReadOnly
	}
	spec, _ := json.Marshal(recomputeSpec{Name: u.Name, Label: u.Label, Key: u.Key, Pos: u.Pos, Slots: u.Slots, Reasoning: u.Reasoning,
		Env: u.Env, Work: u.Work, WorkSD: u.WorkSD})
	deadline := metav1.NewTime(by)
	j := &c4v1.WorkJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: src.Namespace, Name: name,
			Labels: map[string]string{LabelRecompute: "true"},
			Annotations: map[string]string{
				AnnotationRecomputeUnit: string(spec),
				AnnotationRecomputeOf:   fmt.Sprintf("%s/%s#%d", src.Namespace, src.Name, idx),
				AnnotationRecomputeView: view,
				AnnotationRecomputeSite: site,
			}},
		Spec: c4v1.WorkJobSpec{Class: src.Spec.Class, Params: src.Spec.Params, Deadline: &deadline,
			DeadlineMode: "Pinned", Pools: pools, Retries: src.Spec.Retries, NodeErrorLimit: src.Spec.NodeErrorLimit,
			Template: *src.Spec.Template.DeepCopy()},
	}
	for _, a := range []string{AnnotationDerivationClass, c4v1.AnnotationAcceptValidated} {
		if v, ok := src.Annotations[a]; ok {
			j.Annotations[a] = v
		}
	}
	for i, in := range inputs {
		j.Spec.Inputs = append(j.Spec.Inputs, c4v1.WorkInput{Materialization: fmt.Sprintf("%s-in%d", name, i),
			MountPath: in.Mount, ReadOnly: readOnly[in.Mount]})
	}
	return j
}

// poolsAt is those of pools at site, as the last tick placed them.
func (q *Queue) poolsAt(pools []string, site string) []string {
	q.derivMu.Lock()
	defer q.derivMu.Unlock()
	var out []string
	for _, p := range pools {
		if s := q.sites[p]; s != "" && s == site {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// recomputeUnit keeps a recompute's plan to the unit it reruns, as the
// only unit: no edges, no expansion.
func recomputeUnit(j *c4v1.WorkJob, p *class.Plan) (*class.Plan, error) {
	s, ok := j.Annotations[AnnotationRecomputeUnit]
	if !ok {
		return p, nil
	}
	var rs recomputeSpec
	if err := json.Unmarshal([]byte(s), &rs); err != nil || rs.Label == "" {
		return nil, fmt.Errorf("%s: not a unit: %q", AnnotationRecomputeUnit, s)
	}
	u := class.Unit{Name: rs.Name, Label: rs.Label, Key: rs.Key, Pos: rs.Pos, Slots: max(1, rs.Slots), Reasoning: rs.Reasoning,
		Env: rs.Env, Work: rs.Work, WorkSD: rs.WorkSD, Exists: 1}
	u.Index, u.After, u.Inputs, u.Generator, u.Parent, u.Depth = 0, nil, nil, false, 0, 0
	p.Units, p.Order = []class.Unit{u}, []int{0}
	return p, nil
}
