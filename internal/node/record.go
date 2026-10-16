package node

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/capture"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// maxSubjects bounds the files a record lists by name; past it the
// changeset subject still covers every file.
const maxSubjects = 1000

// signRecord makes the task record of a capture: what the pod was, what it
// mounted, what it wrote, signs it as this node, and stores it in the
// project's store like any object. The pod cannot reach the node's key,
// so it cannot shape its own record, and no other node can sign for this
// one.
func (d *Daemon) signRecord(ctx context.Context, pc *projectCtx, vol *Volume, res *capture.Result, finished time.Time) (c4.ID, error) {
	var pod corev1.Pod
	podErr := d.Kube.Get(ctx, types.NamespacedName{Namespace: vol.Namespace, Name: vol.Pod}, &pod)
	st := &provenance.Statement{
		Subject: []provenance.Subject{
			provenance.SubjectFor("changeset", res.Changeset),
			provenance.SubjectFor("view", res.Output),
		},
		Predicate: provenance.TaskPredicate{
			Builder: provenance.Builder{ID: "https://cccc.io/c4-operator/node", Version: d.Cfg.Version},
			Invocation: provenance.Invocation{Project: vol.Project, Namespace: vol.Namespace, Pod: vol.Pod, PodUID: vol.PodUID,
				Class: vol.Class},
			Inputs: []provenance.Input{{Materialization: vol.Materialization, View: vol.View}},
			Output: provenance.Output{View: res.Output.String(), Changeset: res.Changeset.String(),
				Added: res.Added, Modified: res.Modified, Deleted: res.Deleted, Bytes: res.Bytes},
			Runtime: provenance.Runtime{Node: d.Cfg.NodeName, Images: []provenance.Image{},
				StartedOn: vol.Published.UTC(), FinishedOn: finished.UTC()},
		},
	}
	for i, f := range res.Files {
		if i == maxSubjects {
			break
		}
		st.Subject = append(st.Subject, provenance.SubjectFor(f.Path, f.ID))
		st.Predicate.Output.FilesListed++
	}
	if podErr == nil && string(pod.UID) == vol.PodUID {
		inv := &st.Predicate.Invocation
		inv.Job = pod.Labels[c4v1.LabelJob]
		inv.Unit = pod.Labels[c4v1.LabelUnit]
		inv.Attempt = pod.Annotations[c4v1.AnnotationAttempt]
		if ai := pod.Annotations[c4v1.AnnotationAIUsage]; ai != "" && json.Valid([]byte(ai)) {
			st.Predicate.AI = json.RawMessage(ai)
		}
		for _, c := range pod.Status.ContainerStatuses {
			img := provenance.Image{Container: c.Name, Image: c.Image}
			// The kubelet reports the digest it ran as image ID, for example
			// docker.io/library/alpine@sha256:...
			if _, digest, ok := strings.Cut(c.ImageID, "@"); ok {
				img.Digest = digest
			}
			st.Predicate.Runtime.Images = append(st.Predicate.Runtime.Images, img)
		}
	}
	env, err := provenance.SignStatement(ctx, d.Cfg.Signer, provenance.PredicateTask, st.Subject, st.Predicate, time.Now())
	if err != nil {
		return c4.ID{}, err
	}
	id := c4.Identify(bytes.NewReader(env))
	if err := pc.cache.Put(id, env); err != nil {
		return c4.ID{}, err
	}
	if err := d.sink(pc).PutBytes(ctx, vol.Project, id, env); err != nil {
		return c4.ID{}, err
	}
	return id, nil
}
