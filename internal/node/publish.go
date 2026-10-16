package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/capture"
	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/metrics"
	"github.com/Avalanche-io/c4-operator/internal/overlay"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Volume is what the daemon remembers about a published volume, between
// NodePublishVolume and NodeUnpublishVolume (which carries only the
// volume ID and target path).
type Volume struct {
	ID              string    `json:"id"`
	Target          string    `json:"target"`
	Project         string    `json:"project"`
	Partition       string    `json:"partition"`
	Namespace       string    `json:"namespace"`
	Pod             string    `json:"pod"`
	PodUID          string    `json:"podUID"`
	Materialization string    `json:"materialization"`
	View            string    `json:"view"`
	Layers          []string  `json:"layers"`
	Capture         bool      `json:"capture"`
	ReadOnly        bool      `json:"readOnly"`
	Class           string    `json:"class,omitempty"`
	Published       time.Time `json:"published"`
}

// PublishRequest is a NodePublishVolume call.
type PublishRequest struct {
	VolumeID string
	Target   string
	ReadOnly bool
	Context  map[string]string
	Secrets  map[string]string
}

func (d *Daemon) volumePath(id string) string {
	return filepath.Join(d.volDir(), safeName(id)+".json")
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

func (d *Daemon) loadVolume(id string) (*Volume, error) {
	data, err := os.ReadFile(d.volumePath(id))
	if err != nil {
		return nil, err
	}
	var v Volume
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (d *Daemon) saveVolume(v *Volume) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := d.volumePath(v.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.volumePath(v.ID))
}

// Volumes lists the published volumes.
func (d *Daemon) Volumes() []*Volume {
	matches, _ := filepath.Glob(filepath.Join(d.volDir(), "*.json"))
	var out []*Volume
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var v Volume
		if json.Unmarshal(data, &v) == nil {
			out = append(out, &v)
		}
	}
	return out
}

func (d *Daemon) volumeDirs(part, id string) (upper, work string) {
	base := filepath.Join(d.Cfg.Root, "p", part, "vols", safeName(id))
	return filepath.Join(base, "upper"), filepath.Join(base, "work")
}

// Publish authenticates the pod, makes its view's layers present, and
// mounts the view at the target path.
func (d *Daemon) Publish(ctx context.Context, req PublishRequest) (err error) {
	start := time.Now()
	outcome, project, class := "failed", "", req.Context[c4v1.VolumeAttrClass]
	defer func() {
		metrics.NodePublish.WithLabelValues(project, class, outcome).Observe(time.Since(start).Seconds())
	}()

	if v, err := d.loadVolume(req.VolumeID); err == nil {
		if mounted, _ := overlay.IsMounted(v.Target); mounted && v.Target == req.Target {
			outcome, project = "warm", v.Project
			return nil
		}
	}

	ns := req.Context["csi.storage.k8s.io/pod.namespace"]
	podName := req.Context["csi.storage.k8s.io/pod.name"]
	podUID := req.Context["csi.storage.k8s.io/pod.uid"]
	if d.Reviewer != nil {
		id, err := d.Reviewer.VerifyPod(ctx, req.Context, req.Secrets)
		if err != nil {
			return fmt.Errorf("pod identity: %w", err)
		}
		ns, podName, podUID = id.Namespace, id.PodName, id.PodUID
	}
	if req.Context["csi.storage.k8s.io/ephemeral"] != "true" {
		return errors.New("only inline ephemeral volumes are supported")
	}
	name := req.Context[c4v1.VolumeAttrMaterialization]
	if name == "" {
		return errors.New("volume attribute materialization is required")
	}
	p, err := d.Projects.ForNamespace(ctx, ns)
	if err != nil {
		return err
	}
	project = p.Name
	mat, err := d.materialization(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("materialization %s/%s: %w", ns, name, err)
	}
	if mat.Status.Phase != c4v1.PhaseReady || mat.Status.ObservedGeneration != mat.Generation || mat.Status.ViewID == "" {
		return fmt.Errorf("materialization %s/%s is not ready (phase %q)", ns, name, mat.Status.Phase)
	}
	if mat.Status.Project != "" && mat.Status.Project != p.Name {
		return fmt.Errorf("materialization %s/%s was resolved for project %s, not %s", ns, name, mat.Status.Project, p.Name)
	}
	// A Project change (a revoked grant, new shards) re-resolves its
	// Materializations; until that lands, the old resolution is not used.
	if mat.Status.ProjectGeneration != p.Generation {
		return fmt.Errorf("materialization %s/%s was resolved under project %s generation %d, now %d; re-resolving",
			ns, name, p.Name, mat.Status.ProjectGeneration, p.Generation)
	}
	target, err := c4.Parse(mat.Status.ViewID)
	if err != nil {
		return err
	}
	pc, err := d.projectCtx(ctx, p)
	if err != nil {
		return err
	}

	st, how, err := d.ensureStack(ctx, pc, target, mat)
	if err != nil {
		return err
	}
	vol := &Volume{
		ID: req.VolumeID, Target: req.Target, Project: p.Name, Partition: pc.part,
		Namespace: ns, Pod: podName, PodUID: podUID, Materialization: name, View: target.String(),
		Layers: st.Layers, ReadOnly: req.ReadOnly, Class: class, Published: time.Now(),
		Capture: !req.ReadOnly && req.Context[c4v1.VolumeAttrCapture] != "false",
	}
	var upper, work string
	if !req.ReadOnly {
		upper, work = d.volumeDirs(pc.part, req.VolumeID)
		for _, dir := range []string{upper, work} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		// The merged root takes the upper's mode: writable by any
		// container user, like an emptyDir.
		if err := os.Chmod(upper, layer.DirMode); err != nil {
			return err
		}
	}
	// Record the volume before mounting, so an unpublish after a crash
	// between the two still finds and cleans it.
	if err := d.saveVolume(vol); err != nil {
		return err
	}
	if err := overlay.Mount(req.Target, pc.layers.Dirs(st), upper, work); err != nil {
		return err
	}
	pc.layers.Touch(st)
	d.rememberView(pc, mat, target)
	metrics.NodeMounts.WithLabelValues(p.Name).Inc()
	metrics.NodeMaterialized.WithLabelValues(p.Name).Add(float64(mat.Status.Bytes))
	outcome = how
	d.Log.Info("published", "pod", ns+"/"+podName, "materialization", name, "view", target.String(),
		"layers", len(st.Layers), "outcome", how, "seconds", time.Since(start).Seconds())
	return nil
}

// ensureStack makes the layers of a view present, once per (partition,
// view) no matter how many publishes ask: the work runs detached from the
// caller, so kubelet's retry after a timeout joins it.
func (d *Daemon) ensureStack(ctx context.Context, pc *projectCtx, target c4.ID, mat *c4v1.Materialization) (*layer.Stack, string, error) {
	if st, ok := pc.layers.Stack(target); ok {
		return st, "warm", nil
	}
	type result struct {
		st  *layer.Stack
		how string
	}
	ch := d.stacks.DoChan(pc.part+"/"+target.String(), func() (any, error) {
		bctx, cancel := context.WithTimeout(context.Background(), d.Cfg.PublishTimeout)
		defer cancel()
		st, how, err := d.buildStack(bctx, pc, target, d.baseCandidates(pc, mat, target))
		return result{st, how}, err
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, "", r.Err
		}
		res := r.Val.(result)
		return res.st, res.how, nil
	case <-ctx.Done():
		return nil, "", fmt.Errorf("view %s is still being prepared; kubelet will retry: %w", target, ctx.Err())
	}
}

// baseCandidates lists views this node may already hold that are likely
// close to target: the Materialization's changeset chain, newest first,
// and the view this node last built for the same Materialization.
func (d *Daemon) baseCandidates(pc *projectCtx, mat *c4v1.Materialization, target c4.ID) []c4.ID {
	var out []c4.ID
	seen := map[c4.ID]bool{target: true}
	add := func(s string) {
		id, err := c4.Parse(s)
		if err != nil || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for i := len(mat.Status.Layers) - 1; i >= 0; i-- {
		add(mat.Status.Layers[i])
	}
	if b, err := os.ReadFile(d.lastViewPath(pc, mat)); err == nil {
		add(strings.TrimSpace(string(b)))
	}
	return out
}

func (d *Daemon) lastViewPath(pc *projectCtx, mat *c4v1.Materialization) string {
	return filepath.Join(pc.cache.Dir(), "stacks", "mat-"+safeName(mat.Namespace+"."+mat.Name))
}

func (d *Daemon) rememberView(pc *projectCtx, mat *c4v1.Materialization, v c4.ID) {
	os.WriteFile(d.lastViewPath(pc, mat), []byte(v.String()), 0o600)
}

// buildStack stacks one delta on a stack this node holds when the change
// is small, and otherwise builds the view as one full layer.
func (d *Daemon) buildStack(ctx context.Context, pc *projectCtx, target c4.ID, bases []c4.ID) (*layer.Stack, string, error) {
	rs := d.records(pc)
	for _, b := range bases {
		base, ok := pc.layers.Stack(b)
		if !ok || len(base.Layers) >= layer.MaxDepth {
			continue
		}
		changes, stats, err := view.Diff(ctx, b, target, rs)
		if err != nil {
			return nil, "", err
		}
		if stats.Adds+stats.Removes+stats.Opaques > max(64, base.Files/2) {
			continue
		}
		var objs []view.Object
		files := base.Files
		for _, c := range changes {
			switch {
			case c.Op == view.OpRemove:
				files--
			case c.Entry != nil && !c.Entry.IsDir() && !view.IsLink(c.Entry):
				objs = append(objs, view.Object{ID: c.Entry.C4ID, Size: c.Entry.Size, Exec: view.IsExec(c.Entry)})
				files++
			}
		}
		if err := d.ensureObjects(ctx, pc, objs); err != nil {
			return nil, "", err
		}
		meta, err := pc.layers.BuildDelta(b, target, changes)
		if err != nil {
			return nil, "", err
		}
		metrics.NodeLayerBuilds.WithLabelValues(pc.p.Name, "delta").Inc()
		metrics.NodeLayerFiles.WithLabelValues("delta").Add(float64(meta.Entries))
		st := &layer.Stack{View: target.String(), Layers: append([]string{meta.Key}, base.Layers...), Base: base.Base, Files: files, Built: time.Now()}
		return st, "delta", pc.layers.SaveStack(st)
	}

	v, err := view.Load(ctx, target, rs, 32)
	if err != nil {
		return nil, "", err
	}
	if err := d.ensureObjects(ctx, pc, v.Objects()); err != nil {
		return nil, "", err
	}
	meta, err := pc.layers.BuildFull(v)
	if err != nil {
		return nil, "", err
	}
	metrics.NodeLayerBuilds.WithLabelValues(pc.p.Name, "full").Inc()
	metrics.NodeLayerFiles.WithLabelValues("full").Add(float64(meta.Entries))
	st := &layer.Stack{View: target.String(), Layers: []string{meta.Key}, Base: target.String(), Files: len(v.Files), Built: time.Now()}
	return st, "cold", pc.layers.SaveStack(st)
}

// Unpublish unmounts a volume, captures what its pod wrote, records the
// result on the pod, and removes the volume's private directories. The
// volume is not released until its outputs are stored.
func (d *Daemon) Unpublish(ctx context.Context, volumeID, target string) error {
	vol, err := d.loadVolume(volumeID)
	if errors.Is(err, os.ErrNotExist) {
		if err := overlay.Unmount(target); err != nil {
			return err
		}
		os.Remove(target)
		return nil
	}
	if err != nil {
		return err
	}
	if err := overlay.Unmount(vol.Target); err != nil {
		return err
	}
	metrics.NodeMounts.WithLabelValues(vol.Project).Dec()
	if vol.Capture {
		if err := d.capture(ctx, vol); err != nil {
			return err
		}
	}
	upper, _ := d.volumeDirs(vol.Partition, vol.ID)
	os.RemoveAll(filepath.Dir(upper))
	os.Remove(d.volumePath(vol.ID))
	os.Remove(vol.Target)
	return nil
}

// capture runs once per volume, detached from kubelet's call.
func (d *Daemon) capture(ctx context.Context, vol *Volume) error {
	ch := d.captures.DoChan(vol.ID, func() (any, error) {
		bctx, cancel := context.WithTimeout(context.Background(), d.Cfg.PublishTimeout)
		defer cancel()
		return nil, d.runCapture(bctx, vol)
	})
	select {
	case r := <-ch:
		return r.Err
	case <-ctx.Done():
		return fmt.Errorf("capture of %s is still running; kubelet will retry: %w", vol.ID, ctx.Err())
	}
}

func (d *Daemon) runCapture(ctx context.Context, vol *Volume) error {
	start := time.Now()
	p, err := d.Projects.Get(ctx, vol.Project)
	if err != nil {
		return err
	}
	pc, err := d.projectCtx(ctx, p)
	if err != nil {
		return err
	}
	parent, err := c4.Parse(vol.View)
	if err != nil {
		return err
	}
	upper, _ := d.volumeDirs(vol.Partition, vol.ID)
	res, err := capture.Run(ctx, capture.Input{
		Project: p.Name, Parent: parent, Upper: upper,
		Cache: pc.cache, Records: d.records(pc), Sink: d.sink(pc), Workers: 8,
	})
	if err != nil {
		metrics.NodeCapture.WithLabelValues(p.Name, "error").Inc()
		return fmt.Errorf("capture %s/%s: %w", vol.Namespace, vol.Pod, err)
	}
	metrics.NodeCaptureSec.WithLabelValues(p.Name).Observe(time.Since(start).Seconds())
	if res.Changeset.IsNil() {
		metrics.NodeCapture.WithLabelValues(p.Name, "empty").Inc()
	} else {
		metrics.NodeCapture.WithLabelValues(p.Name, "changeset").Inc()
		metrics.NodeCaptureObj.WithLabelValues(p.Name, "added").Add(float64(res.Added))
		metrics.NodeCaptureObj.WithLabelValues(p.Name, "modified").Add(float64(res.Modified))
		metrics.NodeCaptureObj.WithLabelValues(p.Name, "deleted").Add(float64(res.Deleted))
		metrics.NodeCaptureB.WithLabelValues(p.Name).Add(float64(res.Bytes))
	}
	metrics.NodeCaptureObj.WithLabelValues(p.Name, "reused").Add(float64(res.Reused))
	_, changeset, _ := res.AnnotationValues()
	d.Log.Info("captured", "pod", vol.Namespace+"/"+vol.Pod, "output", res.Output.String(),
		"changeset", changeset, "added", res.Added, "modified", res.Modified, "deleted", res.Deleted,
		"uploaded", res.Uploaded, "reused", res.Reused, "seconds", time.Since(start).Seconds())
	record := ""
	if d.Cfg.Signer != nil && !res.Changeset.IsNil() {
		id, err := d.signRecord(ctx, pc, vol, res, start)
		if err != nil {
			// A capture without its record is still a capture; the gap is
			// visible (no output-provenance annotation) and counted.
			metrics.NodeRecords.WithLabelValues(p.Name, "error").Inc()
			d.Log.Error(err, "task record", "pod", vol.Namespace+"/"+vol.Pod)
		} else {
			metrics.NodeRecords.WithLabelValues(p.Name, "signed").Inc()
			record = id.String()
		}
	}
	return d.annotate(ctx, vol, res, record)
}

// annotate records a capture on its pod. The node daemon's admission
// policy lets it change only c4.avalanche.io/output-* annotations, and
// only on pods bound to its own node.
func (d *Daemon) annotate(ctx context.Context, vol *Volume, res *capture.Result, record string) error {
	var pod corev1.Pod
	err := d.Kube.Get(ctx, types.NamespacedName{Namespace: vol.Namespace, Name: vol.Pod}, &pod)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(pod.UID) != vol.PodUID {
		return nil
	}
	tree, changeset, changes := res.AnnotationValues()
	ann := map[string]string{
		c4v1.AnnotationOutputTree:      tree,
		c4v1.AnnotationOutputChangeset: changeset,
		c4v1.AnnotationOutputChanges:   changes,
	}
	if record != "" {
		ann[c4v1.AnnotationOutputProvenance] = record
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": ann}}
	data, _ := json.Marshal(patch)
	err = d.Kube.Patch(ctx, &pod, client.RawPatch(types.MergePatchType, data))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// captureSink is capture.Sink.
type captureSink = capture.Sink

// directSink writes a Direct project's captures to its store. The node
// computed every ID; in Direct mode it is trusted to.
type directSink struct{ st *storeurl.Sharded }

func (s directSink) PutFile(ctx context.Context, _ string, id c4.ID, path string) error {
	if _, err := s.st.Head(ctx, id); err == nil {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return s.st.Put(ctx, id, f, info.Size())
}

func (s directSink) PutBytes(ctx context.Context, _ string, id c4.ID, data []byte) error {
	if _, err := s.st.Head(ctx, id); err == nil {
		return nil
	}
	return s.st.Put(ctx, id, strings.NewReader(string(data)), int64(len(data)))
}
