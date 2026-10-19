package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// The job's measure of done (PRIMITIVES Q12; LLM-OR-PROCEDURE 7.2 item 3).
// A job declares what it counts (spec.measure: "rules sited", of 379), and
// a unit, usually a procedural one re-run after each merge, reports a
// reading in .c4/measure.json (c4-unit measure):
//
//	{"value": 361, "of": 379, "name": "rules sited", "detail": "18 pending"}
//
// The node daemon stores it at capture (c4.avalanche.io/output-measure).
// The latest reading by finish time is the job's measure, shown as its
// progress. A required measure is a completion predicate: the job succeeds
// only when the value reaches the target; with its units finished short of
// it, the job fails and needs a person (NeedsPerson Measure).

type measureDoc struct {
	Name   string `json:"name"`
	Value  int64  `json:"value"`
	Of     int64  `json:"of"`
	Detail string `json:"detail"`
}

// measureOf reads unit pod p's measure reading, if it wrote one. It
// reports false when the document could not be read yet.
func (q *Queue) measureOf(ctx context.Context, v *view, p *corev1.Pod) (string, []byte, bool) {
	id := p.Annotations[c4v1.AnnotationOutputMeasure]
	if id == "" {
		return "", nil, true
	}
	doc, err := q.expansionDoc(ctx, v.project, v.mem, id)
	if err != nil {
		q.Log.Error(err, "measure document", "job", v.key, "pod", p.Name)
		return "", nil, false
	}
	return id, doc, true
}

// noteMeasure keeps unit idx's reading if it is the latest.
func (q *Queue) noteMeasure(v *view, idx int, id string, doc []byte, at time.Time) {
	var d measureDoc
	if err := json.NewDecoder(bytes.NewReader(doc)).Decode(&d); err != nil {
		q.Log.Info("measure document unreadable", "job", v.key, "unit", idx, "err", err.Error())
		return
	}
	m := v.mem
	if m.measure != nil && m.measure.At != nil && m.measure.At.Time.After(at) {
		return
	}
	m.measure = &c4v1.WorkMeasure{Name: d.Name, Value: d.Value, Of: d.Of, At: &metav1.Time{Time: at.Truncate(time.Second)},
		Unit: int32(idx), Document: id, Detail: d.Detail}
}

// measureStatus is the job's measure as status shows it: the target from
// the reading, else the spec; done when the value reaches it.
func measureStatus(v *view) *c4v1.WorkMeasure {
	m := v.mem.measure
	spec := v.job.Spec.Measure
	if m == nil {
		if spec == nil {
			return nil
		}
		out := &c4v1.WorkMeasure{Name: spec.Name, Unit: -1}
		if spec.Of != nil {
			out.Of = *spec.Of
		}
		return out
	}
	out := *m
	if spec != nil {
		if out.Name == "" {
			out.Name = spec.Name
		}
		if out.Of == 0 && spec.Of != nil {
			out.Of = *spec.Of
		}
	}
	out.Done = out.Of > 0 && out.Value >= out.Of
	return &out
}

// measureShort reports whether the job's required measure is not met.
func measureShort(v *view, ms *c4v1.WorkMeasure) bool {
	return v.job.Spec.Measure != nil && v.job.Spec.Measure.Required && (ms == nil || !ms.Done)
}

// measureMessage says how a finished job falls short of its measure.
func measureMessage(ms *c4v1.WorkMeasure) string {
	if ms == nil || ms.Unit < 0 {
		return "its units finished, and its measure was never reported"
	}
	return fmt.Sprintf("its units finished at %d of %d %s", ms.Value, ms.Of, ms.Name)
}
