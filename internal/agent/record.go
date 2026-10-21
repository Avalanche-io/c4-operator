package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// hostBuilder names the host runner as the builder of its task records.
const hostBuilder = "https://cccc.io/c4-operator/host-runner"

// signRecord makes the task record of a unit pod this runner ran: what the
// pod was, what it resumed from, what it left (its output view and its
// session's checkpoint), signed as this machine's node, as the node daemon
// signs for units in pods. The record is stored in the project's store and
// returned by its ID; "" when the unit left nothing to record.
func (r *Runner) signRecord(ctx context.Context, p *corev1.Pod, finished time.Time) (string, error) {
	var subjects []provenance.Subject
	var out provenance.Output
	if v := p.Annotations[c4v1.AnnotationOutputTree]; v != "" {
		if id, err := c4.Parse(v); err == nil {
			subjects = append(subjects, provenance.SubjectFor("view", id))
			out.View = v
		}
	}
	if cp := p.Annotations[c4v1.AnnotationOutputCheckpoint]; cp != "" {
		if _, head, ok := strings.Cut(cp, ":"); ok {
			if id, err := c4.Parse(head); err == nil {
				subjects = append(subjects, provenance.SubjectFor("checkpoint", id))
			}
		}
	}
	if len(subjects) == 0 {
		return "", nil
	}
	if r.Docs == nil {
		return "", errors.New("no store for task records (--store)")
	}
	project, err := r.projectOf(ctx, p.Namespace)
	if err != nil {
		return "", err
	}
	pred := provenance.TaskPredicate{
		Builder: provenance.Builder{ID: hostBuilder, Version: r.Version},
		Invocation: provenance.Invocation{Project: project, Namespace: p.Namespace, Pod: p.Name, PodUID: string(p.UID),
			Job: p.Labels[c4v1.LabelJob], Unit: p.Labels[c4v1.LabelUnit], Attempt: p.Annotations[c4v1.AnnotationAttempt]},
		Inputs:  []provenance.Input{},
		Output:  out,
		Runtime: provenance.Runtime{Node: r.Node, Images: []provenance.Image{}, FinishedOn: finished.UTC()},
	}
	if v := podEnv(p, "C4_RESUME_VIEW"); v != "" {
		pred.Inputs = append(pred.Inputs, provenance.Input{View: v})
	}
	if p.Status.StartTime != nil {
		pred.Runtime.StartedOn = p.Status.StartTime.UTC()
	}
	if ai := p.Annotations[c4v1.AnnotationAIUsage]; ai != "" && json.Valid([]byte(ai)) {
		pred.AI = json.RawMessage(ai)
	}
	pred.Chain = r.chainOf(ctx, p)
	env, err := provenance.SignStatement(ctx, r.Signer, provenance.PredicateTask, subjects, pred, r.now())
	if err != nil {
		return "", err
	}
	id := c4.Identify(bytes.NewReader(env))
	if err := r.Docs.PutBytes(ctx, project, id, env); err != nil {
		return "", err
	}
	return id.String(), nil
}

// Deployment is this runner's name as a deployment (chain/1's
// <farm>/runner/<node>): the person's own machine, running their members
// under their own sign-in.
func (r *Runner) Deployment() string {
	farm := r.Farm
	if farm == "" {
		farm = "farm"
	}
	return farm + "/runner/" + r.Node
}

// chainOf says whom a host-run unit accrues to: run by the unit (and the
// member it is), for its job's owner (read from the job, as the owner check
// does), under that job's ownership, using its holder's own sign-in on this
// machine, on this runner. Until deployments are bound (slice 2), no chain
// is accountable, and the gap says so.
func (r *Runner) chainOf(ctx context.Context, p *corev1.Pod) *schema.Chain {
	var j c4v1.WorkJob
	ref := metav1.GetControllerOf(p)
	if ref == nil || ref.Kind != "WorkJob" || r.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: ref.Name}, &j) != nil || j.UID != ref.UID {
		return &schema.Chain{Act: "build", By: schema.Actor{Unit: p.Namespace + "/" + p.Name, Grade: schema.GradeDirect},
			For: schema.For{Grade: schema.GradeClaimed}, Gap: "the pod is not a unit of a job this runner can read"}
	}
	owner := j.Annotations[c4v1.AnnotationOwner]
	holder := owner
	if !slices.Contains(r.Owners, holder) && len(r.Owners) > 0 {
		holder = r.Owners[0]
	}
	dep := r.Deployment()
	uid := string(j.UID)
	if len(uid) > 6 {
		uid = uid[:6]
	}
	c := &schema.Chain{
		Act:   "build",
		By:    schema.Actor{Unit: fmt.Sprintf("%s/%s@%s/%s", j.Namespace, j.Name, uid, p.Labels[c4v1.LabelUnit]), Agent: memberOf(p), Grade: schema.GradeDirect},
		For:   schema.For{Party: owner, Grade: schema.GradeDirect},
		Under: []schema.Delegation{{Job: j.Namespace + "/" + j.Name, Kind: "ownership", From: owner}},
		Using: []schema.Credential{{SignIn: dep, Holder: holder}},
		On:    []schema.Deployment{{Deployment: dep, Owner: holder}},
	}
	switch {
	case owner == "":
		c.For = schema.For{Grade: schema.GradeClaimed}
		c.Gap = "job " + j.Namespace + "/" + j.Name + " has no owner"
	default:
		c.Gap = "deployment " + dep + " has no binding"
	}
	return c
}

// recordEnd signs the record of a unit pod that is ending and notes it on
// the pod (output-provenance). A record that cannot be made leaves the pod
// without it, as a node daemon's capture without one does: the gap is
// visible, and logged.
func (r *Runner) recordEnd(ctx context.Context, p *corev1.Pod) {
	if r.Signer == nil {
		return
	}
	var fresh corev1.Pod // the annotations the runner wrote since p was read
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, &fresh); err != nil || fresh.UID != p.UID {
		return
	}
	if fresh.Annotations[c4v1.AnnotationOutputProvenance] != "" {
		return
	}
	id, err := r.signRecord(ctx, &fresh, r.now())
	if err != nil {
		r.logf("%s/%s: task record: %v", p.Namespace, p.Name, err)
		return
	}
	if id == "" {
		return
	}
	if err := r.annotate(ctx, &fresh, map[string]string{c4v1.AnnotationOutputProvenance: id}); err != nil {
		r.logf("%s/%s: task record %s: %v", p.Namespace, p.Name, id, err)
		return
	}
	p.Annotations = fresh.Annotations
	r.logf("%s/%s: signed its task record %s", p.Namespace, p.Name, id)
}
