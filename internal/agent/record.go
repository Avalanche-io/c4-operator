package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

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
// does), under that job, using the sign-in of this machine's holder, on
// this runner (the holder's machine). The holder is the runner's, never the
// job's: one sign-in serves every job here, so a runner shared by two owners
// shows "for dana, using noor's sign-in". Until the runner's deployment is
// bound, no chain is accountable, and the gap says so.
func (r *Runner) chainOf(ctx context.Context, p *corev1.Pod) *schema.Chain {
	dep := r.Deployment()
	holder := r.holder()
	using := schema.Credential{SignIn: dep, Holder: holder}
	if holder == "" {
		using = schema.Credential{SignIn: dep, HolderSubject: "unknown"}
	}
	on := []schema.Deployment{{Deployment: dep, Owner: holder}}
	j, why, err := r.jobOf(ctx, p)
	if err != nil || why != "" {
		return &schema.Chain{Act: "build", By: schema.Actor{Deployment: dep, Grade: schema.GradeDirect},
			For:   schema.For{Subject: "pod " + p.Namespace + "/" + p.Name + ", of no job this runner can read", Grade: schema.GradeClaimed},
			Using: []schema.Credential{using}, On: on, Gap: "the pod is not a unit of a job this runner can read"}
	}
	owner, job := j.Owner, p.Namespace+"/"+j.Name
	c := &schema.Chain{
		Act:   "build",
		By:    schema.Actor{Unit: schema.UnitRef(p.Namespace, j.Name, j.UID, p.Labels[c4v1.LabelUnit]), Agent: memberOf(p), Grade: schema.GradeDirect},
		For:   schema.For{Party: owner, Grade: schema.GradeDirect},
		Under: []schema.Delegation{{Job: job, From: owner, Grant: j.Grant}},
		Using: []schema.Credential{using},
		On:    on,
	}
	switch {
	case owner == "":
		c.For = schema.For{Subject: "no owner recorded on job " + job, Grade: schema.GradeClaimed}
		c.Under = []schema.Delegation{{Job: job}}
		c.Gap = "job " + job + " has no owner"
	default:
		c.Gap = "deployment " + dep + " has no binding"
	}
	return c
}

// holder is whose sign-in this machine's members use: --holder, else the
// runner's one owner; "" when it has several and no --holder.
func (r *Runner) holder() string {
	if r.Holder != "" {
		return r.Holder
	}
	if len(r.Owners) == 1 {
		return r.Owners[0]
	}
	return ""
}

// recordEnd signs the record of a unit pod that is ending and notes it on
// the pod (output-provenance). A record that cannot be made leaves the pod
// without it, as a node daemon's capture without one does: the gap is
// visible, and logged.
func (r *Runner) recordEnd(ctx context.Context, p *corev1.Pod) {
	if r.Signer == nil {
		return
	}
	fresh, ok := r.freshPod(ctx, p) // the annotations the runner wrote since p was read
	if !ok {
		return
	}
	if fresh.Annotations[c4v1.AnnotationOutputProvenance] != "" {
		return
	}
	id, err := r.signRecord(ctx, fresh, r.now())
	if err != nil {
		r.logf("%s/%s: task record: %v", p.Namespace, p.Name, err)
		return
	}
	if id == "" {
		return
	}
	if err := r.annotate(ctx, fresh, map[string]string{c4v1.AnnotationOutputProvenance: id}); err != nil {
		r.logf("%s/%s: task record %s: %v", p.Namespace, p.Name, id, err)
		return
	}
	p.Annotations = fresh.Annotations
	r.logf("%s/%s: signed its task record %s", p.Namespace, p.Name, id)
}
