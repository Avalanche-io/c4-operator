package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Safe-point decisions. A unit at a safe point (c4-unit safepoint) writes
// .c4/safepoint and waits for the farm's answer in its control directory.
// The daemon relays both ways: each new safe point becomes the pod's
// c4.avalanche.io/safepoint annotation, which the queue answers with
// c4.avalanche.io/safepoint-decision; the daemon writes that answer into the
// pod's c4-control emptyDir, through kubelet's pods directory, where the
// unit reads it as $C4_CONTROL/safepoint.decision. Nothing is written into
// a mounted overlay.
//
// Messages to a running unit go the same way (steering): the queue puts
// the unit's unread messages on its pod (c4.avalanche.io/unit-steer), the
// daemon writes them to the control directory as steer, and relays how far
// the unit read (.c4/steer-seen) back as c4.avalanche.io/steer-seen.

// ControlVolume is the emptyDir every unit pod mounts at ControlMount.
const (
	ControlVolume = "c4-control"
	ControlMount  = "/etc/c4/control"
)

// SafepointNote is the c4.avalanche.io/safepoint annotation's value.
type SafepointNote struct {
	Seq        int             `json:"seq"`
	At         time.Time       `json:"at"`
	Checkpoint string          `json:"checkpoint,omitempty"`
	Wait       json.RawMessage `json:"wait,omitempty"`
}

// SafepointDecision is the c4.avalanche.io/safepoint-decision annotation's
// value, and the decision file the unit reads.
type SafepointDecision struct {
	Seq int       `json:"seq"`
	Do  string    `json:"do"` // stop or continue
	Why string    `json:"why,omitempty"`
	At  time.Time `json:"at"`
}

type spRelay struct {
	asked, answered int
	seen            bool
	// steer is the messages last written to the control directory, read
	// how far the unit said it read, and steerAt when the pod was last
	// read for them.
	steer   string
	read    int
	steerAt time.Time
	// notes are the unit's .c4/asks and .c4/answers last relayed.
	notes map[string]string
}

// steerEvery is how often the daemon reads a pod for new messages.
const steerEvery = 2 * time.Second

func (d *Daemon) podsDir() string {
	if d.Cfg.PodsDir != "" {
		return d.Cfg.PodsDir
	}
	return "/var/lib/kubelet/pods"
}

// RunSafepoints relays safe points and decisions every interval until ctx
// ends. It is quicker than the progress scrape: a unit is waiting.
func (d *Daemon) RunSafepoints(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 500 * time.Millisecond
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	relays := map[string]*spRelay{}
	for {
		d.safepointsOnce(ctx, relays)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (d *Daemon) safepointsOnce(ctx context.Context, relays map[string]*spRelay) {
	if d.Kube == nil {
		return
	}
	for _, r := range relays {
		r.seen = false
	}
	for _, v := range d.Volumes() {
		if !v.Capture || v.ReadOnly || v.Pod == "" || v.PodUID == "" {
			continue
		}
		r := relays[v.PodUID]
		if r == nil {
			r = &spRelay{}
			relays[v.PodUID] = r
		}
		r.seen = true
		upper, _ := d.volumeDirs(v.Partition, v.ID)
		if sp, ok := ReadSafepoint(upper); ok && sp.Seq > r.asked {
			note := SafepointNote{Seq: sp.Seq, At: sp.At, Checkpoint: sp.Checkpoint, Wait: sp.Wait}
			if d.setPodNote(ctx, v.Namespace, v.Pod, v.PodUID, c4v1.AnnotationSafepoint, note) {
				r.asked = sp.Seq
			}
		}
		if time.Since(r.steerAt) >= steerEvery {
			r.steerAt = time.Now()
			d.relaySteer(ctx, v.Namespace, v.Pod, v.PodUID, upper, r)
		}
		if r.asked <= r.answered {
			continue
		}
		var pod corev1.Pod
		if err := d.Kube.Get(ctx, types.NamespacedName{Namespace: v.Namespace, Name: v.Pod}, &pod); err != nil || string(pod.UID) != v.PodUID {
			continue
		}
		var dec SafepointDecision
		if json.Unmarshal([]byte(pod.Annotations[c4v1.AnnotationSafepointDecision]), &dec) != nil || dec.Seq != r.asked {
			continue
		}
		dir := filepath.Join(d.podsDir(), v.PodUID, "volumes", "kubernetes.io~empty-dir", ControlVolume)
		if err := writeFileAtomic(filepath.Join(dir, "safepoint.decision"), dec); err != nil {
			d.Log.V(1).Info("safepoint decision", "pod", v.Namespace+"/"+v.Pod, "err", err.Error())
			continue
		}
		r.answered = dec.Seq
	}
	for uid, r := range relays {
		if !r.seen {
			delete(relays, uid)
		}
	}
}

// relaySteer writes a pod's unread messages to its control directory, and
// relays how far its unit read them.
func (d *Daemon) relaySteer(ctx context.Context, ns, name, uid, upper string, r *spRelay) {
	var pod corev1.Pod
	if err := d.Kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod); err != nil || string(pod.UID) != uid {
		return
	}
	if s := pod.Annotations[c4v1.AnnotationUnitSteer]; s != r.steer {
		msgs := json.RawMessage("[]")
		if s != "" && json.Valid([]byte(s)) {
			msgs = json.RawMessage(s)
		}
		dir := filepath.Join(d.podsDir(), uid, "volumes", "kubernetes.io~empty-dir", ControlVolume)
		if err := writeFileAtomic(filepath.Join(dir, "steer"), msgs); err != nil {
			d.Log.V(1).Info("steer", "pod", ns+"/"+name, "err", err.Error())
			return
		}
		r.steer = s
	}
	if n, ok := ReadSteerSeen(upper); ok && n > r.read && d.setPodNote(ctx, ns, name, uid, c4v1.AnnotationSteerSeen, n) {
		r.read = n
	}
	for file, key := range UnitNotes {
		b, ok := ReadUnitNote(upper, file)
		if !ok || b == r.notes[file] {
			continue
		}
		if d.setPodNote(ctx, ns, name, uid, key, json.RawMessage(b)) {
			if r.notes == nil {
				r.notes = map[string]string{}
			}
			r.notes[file] = b
		}
	}
}

// UnitNotes are the lists a unit writes in its .c4/ for the farm, and the
// pod annotation each is relayed as.
var UnitNotes = map[string]string{"asks": c4v1.AnnotationAsks, "answers": c4v1.AnnotationAnswers}

// ReadUnitNote reads one of a unit's lists (.c4/asks, .c4/answers): a
// JSON array of at most 64 KiB.
func ReadUnitNote(out, file string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(out, ".c4", file))
	if err != nil || len(b) > 64<<10 || !json.Valid(b) || len(b) == 0 || b[0] != '[' {
		return "", false
	}
	return string(b), true
}

// ReadSteerSeen reads how far a unit read its messages (.c4/steer-seen,
// written by c4-unit steer).
func ReadSteerSeen(out string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(out, ".c4", "steer-seen"))
	if err != nil || len(b) > 32 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return n, err == nil && n > 0
}

// setPodNote sets one annotation on a pod, if it is still the pod with uid.
func (d *Daemon) setPodNote(ctx context.Context, ns, name, uid, key string, value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var pod corev1.Pod
	if err := d.Kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod); err != nil || string(pod.UID) != uid {
		return false
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{key: string(data)}}})
	return d.Kube.Patch(ctx, &pod, client.RawPatch(types.MergePatchType, patch)) == nil
}

func writeFileAtomic(p string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Dir(p)); err != nil {
		return err // the pod has no control volume (an older pod)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
