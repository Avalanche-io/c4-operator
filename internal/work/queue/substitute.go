package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/process"
)

// Input substitution (experimental, staging). A job that accepts
// validated recomputes (c4v1.AnnotationAcceptValidated) may, at a site,
// read the output of a recompute the queue ran there in place of the
// input it names: a validated recompute writes other bytes than the
// recorded view, so the site holds that output and not the input. Nothing
// in the job's spec, its Materializations or other jobs changes, and
// elsewhere it reads its inputs as named. A unit placed in a pool at the
// site mounts the substitute through a Materialization the job owns; its
// pod says what it read in place of what (AnnotationInputSubstitutes), so
// the derivation record takes the inputs the unit actually read, and
// status lists the substitutions it used.
//
// The substitutes are the recompute jobs that succeeded: each names the
// view it rebuilt and its site (AnnotationRecomputeView, ...Site), and the
// queue notes the view its one unit wrote (AnnotationRecomputeOutput),
// the substitute. An exact recompute that wrote the recorded
// view needs none; one that wrote something else falsified the process's
// declaration (the record demotes it to validated) and substitutes like a
// validated one.

// AnnotationRecomputeView on a recompute WorkJob is the view it rebuilds,
// and AnnotationRecomputeSite the site it rebuilds it at.
// AnnotationRecomputeOutput is the view its unit wrote, which the queue
// sets when the unit succeeds.
const (
	AnnotationRecomputeView   = "c4.avalanche.io/recompute-view"
	AnnotationRecomputeSite   = "c4.avalanche.io/recompute-site"
	AnnotationRecomputeOutput = "c4.avalanche.io/recompute-output"
)

// AnnotationInputSubstitutes on a unit pod maps each input mount it read
// a substitute at to the view it read (JSON).
const AnnotationInputSubstitutes = "c4.avalanche.io/input-substitutes"

// substitute is a recompute's output standing in for a view at a site.
type substitute struct {
	view, sub, site, recompute string
}

// noteSubstitutes learns the substitutes from the recompute jobs that
// succeeded with an output other than the view they rebuilt, by project,
// site and view.
func (q *Queue) noteSubstitutes(jobs []c4v1.WorkJob, nsProject map[string]string) {
	subs := map[string]map[string]map[string]substitute{}
	for i := range jobs {
		j := &jobs[i]
		view, site := j.Annotations[AnnotationRecomputeView], j.Annotations[AnnotationRecomputeSite]
		out := j.Annotations[AnnotationRecomputeOutput]
		if view == "" || site == "" || j.Status.Phase != "Succeeded" || out == "" || out == view {
			continue
		}
		project := nsProject[j.Namespace]
		if project == "" {
			project = "namespace/" + j.Namespace
		}
		if subs[project] == nil {
			subs[project] = map[string]map[string]substitute{}
		}
		if subs[project][site] == nil {
			subs[project][site] = map[string]substitute{}
		}
		subs[project][site][view] = substitute{view: view, sub: out, site: site, recompute: j.Name}
	}
	q.derivMu.Lock()
	q.subs = subs
	q.derivMu.Unlock()
}

// noteRecomputeOutput notes on a recompute job the view its unit wrote.
func (q *Queue) noteRecomputeOutput(ctx context.Context, v *view, out string) {
	j := v.job
	if j.Annotations[AnnotationRecomputeView] == "" || j.Annotations[AnnotationRecomputeOutput] == out {
		return
	}
	patch := client.MergeFrom(j.DeepCopy())
	j.Annotations[AnnotationRecomputeOutput] = out
	if err := q.Client.Patch(ctx, j, patch); err != nil {
		q.Log.Error(err, "recompute output", "job", v.key)
	}
}

// substitutesAt are the substitutes job v may read at site, by input
// index: none unless it accepts validated recomputes.
func (q *Queue) substitutesAt(v *view, inputs []process.Input, site string) map[int]substitute {
	if v.job.Annotations[c4v1.AnnotationAcceptValidated] != "true" || site == "" {
		return nil
	}
	q.derivMu.Lock()
	at := q.subs[v.project][site]
	q.derivMu.Unlock()
	var out map[int]substitute
	for i, in := range inputs {
		if s, ok := at[in.View]; ok {
			if out == nil {
				out = map[int]substitute{}
			}
			out[i] = s
		}
	}
	return out
}

// substitute makes the job's Materializations of its substitutes at site
// and keeps those that are ready in v.subs; it returns the inputs as the
// job reads them there.
func (q *Queue) substitute(ctx context.Context, v *view, inputs []process.Input, site string) []process.Input {
	subs := q.substitutesAt(v, inputs, site)
	if len(subs) == 0 {
		return inputs
	}
	out := append([]process.Input(nil), inputs...)
	for i, s := range subs {
		ready, err := q.ensureMaterialization(ctx, v.job, subName(v.job.Name, i, site), s.sub, "")
		if err != nil {
			q.Log.Error(err, "substitute input", "job", v.key, "site", site, "view", s.sub)
			continue
		}
		if !ready {
			continue
		}
		if v.subs == nil {
			v.subs = map[string]map[int]substitute{}
		}
		if v.subs[site] == nil {
			v.subs[site] = map[int]substitute{}
		}
		v.subs[site][i] = s
		out[i].View = s.sub
	}
	return out
}

// subName is the Materialization of input i's substitute at site.
func subName(job string, i int, site string) string {
	h := sha256.Sum256([]byte(site))
	return fmt.Sprintf("%s-sub%d-%s", job, i, hex.EncodeToString(h[:])[:8])
}

// substitutePod mounts, in pod p for pool site, each input substitute in
// place of the input, notes it on the pod, and remembers it for status.
func substitutePod(v *view, pod *corev1.Pod, site string) {
	subs := v.subs[site]
	if len(subs) == 0 {
		return
	}
	read := map[string]string{}
	for i, s := range subs {
		name := fmt.Sprintf("c4-input-%d", i)
		for k := range pod.Spec.Volumes {
			vol := &pod.Spec.Volumes[k]
			if vol.Name != name || vol.CSI == nil {
				continue
			}
			if vol.CSI.VolumeAttributes[c4v1.VolumeAttrMaterialization] == resumeName(v.job.Name, unitOfPod(pod)) {
				continue // a resumed attempt reads its own last view
			}
			vol.CSI.VolumeAttributes[c4v1.VolumeAttrMaterialization] = subName(v.job.Name, i, site)
			mount := v.job.Spec.Inputs[i].MountPath
			read[mount] = s.sub
			if v.mem.substituted == nil {
				v.mem.substituted = map[string]c4v1.WorkInputSubstitute{}
			}
			v.mem.substituted[mount+"@"+site] = c4v1.WorkInputSubstitute{Mount: mount, View: s.view, Substitute: s.sub, Site: site, Recompute: s.recompute}
		}
	}
	if len(read) == 0 {
		return
	}
	b, _ := json.Marshal(read)
	pod.Annotations[AnnotationInputSubstitutes] = string(b)
}

func unitOfPod(p *corev1.Pod) int {
	var i int
	fmt.Sscan(p.Labels[c4v1.LabelUnit], &i)
	return i
}

// readInputs are the inputs pod p read: the job's, with what it read in
// their place.
func readInputs(p *corev1.Pod, inputs []process.Input) []process.Input {
	var read map[string]string
	if s := p.Annotations[AnnotationInputSubstitutes]; s == "" || json.Unmarshal([]byte(s), &read) != nil {
		return inputs
	}
	out := append([]process.Input(nil), inputs...)
	for i := range out {
		if s, ok := read[out[i].Mount]; ok {
			out[i].View = s
		}
	}
	return out
}

// substituteStatus lists the substitutions the job's units read.
func substituteStatus(m *jobMem) []c4v1.WorkInputSubstitute {
	var out []c4v1.WorkInputSubstitute
	for _, s := range m.substituted {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Site != out[j].Site {
			return out[i].Site < out[j].Site
		}
		return out[i].Mount < out[j].Mount
	})
	return out
}

func (m *jobMem) loadSubstitutes(st *c4v1.WorkJobStatus) {
	for _, s := range st.InputSubstitutes {
		if m.substituted == nil {
			m.substituted = map[string]c4v1.WorkInputSubstitute{}
		}
		m.substituted[s.Mount+"@"+s.Site] = s
	}
}
