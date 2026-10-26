package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/node"
)

// A unit run on the host has no pod sandbox, so the runner gives it what
// the node daemon gives a pod: a unit directory with
//
//	out/        the unit's output root ($C4_OUTPUT): c4-unit writes
//	            .c4/safepoint there, and a generator .c4/expand.json
//	control/    $C4_CONTROL: the runner writes safepoint.decision here
//	podinfo     $C4_PODINFO: the pod's annotations, as the downward API
//	            writes them (key="value" lines)
//
// The member record names the directory (Member.UnitDir), so the Stop
// hook, which runs inside the session, finds it without relying on the
// session's environment.

// DocStore stores a small document in a project's store under its C4 ID.
// A tier uploader fits (tier.Uploader.PutBytes).
type DocStore interface {
	PutBytes(ctx context.Context, project string, id c4.ID, data []byte) error
}

// maxDoc bounds a control document, as the node daemon does.
const maxDoc = 16 << 20

// UnitDirs are the paths of a unit directory.
type UnitDirs struct{ Root, Out, Control, PodInfo string }

// UnitDirsAt names the unit directory under root.
func UnitDirsAt(root string) UnitDirs {
	return UnitDirs{Root: root, Out: filepath.Join(root, "out"), Control: filepath.Join(root, "control"), PodInfo: filepath.Join(root, "podinfo")}
}

func (r *Runner) unitRoot(p *corev1.Pod) string {
	base := r.UnitsDir
	if base == "" {
		base = filepath.Join(r.Harness.Store.Dir, "units")
	}
	return filepath.Join(base, string(p.UID))
}

// prepareUnit makes the pod's unit directory and records it on the member,
// with the tool a block's attempt declares.
func (r *Runner) prepareUnit(p *corev1.Pod, m *Member) (UnitDirs, error) {
	u := UnitDirsAt(r.unitRoot(p))
	for _, d := range []string{filepath.Join(u.Out, ".c4"), u.Control} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return u, err
		}
	}
	profile := ""
	if m.Block {
		profile = blockProfile(p)
	}
	if m.UnitDir != u.Root || m.Profile != profile {
		unlock, err := r.Harness.Store.Lock(m.Name)
		if err != nil {
			return u, err
		}
		defer unlock()
		cur, err := r.Harness.Store.Get(m.Name)
		if err != nil {
			return u, err
		}
		cur.UnitDir, cur.Profile = u.Root, profile
		if err := r.Harness.Store.Put(cur); err != nil {
			return u, err
		}
		m.UnitDir, m.Profile = u.Root, profile
	}
	return u, nil
}

// writePodInfo writes the pod's annotations as the downward API does.
func writePodInfo(path string, ann map[string]string) error {
	keys := make([]string, 0, len(ann))
	for k := range ann {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, strconv.Quote(ann[k]))
	}
	return writeAtomic(path, []byte(b.String()))
}

// hostSafepoint is a unit's .c4/safepoint, in the form c4-unit writes it
// and the node daemon reads it ("seq N", "at TIME", "checkpoint REF",
// "wait JSON").
type hostSafepoint struct {
	Seq        int             `json:"seq"`
	At         time.Time       `json:"at"`
	Checkpoint string          `json:"checkpoint,omitempty"`
	Wait       json.RawMessage `json:"wait,omitempty"`
}

func readSafepoint(out string) (hostSafepoint, bool) {
	b, err := os.ReadFile(filepath.Join(out, ".c4", "safepoint"))
	if err != nil || len(b) > 4096 {
		return hostSafepoint{}, false
	}
	var sp hostSafepoint
	for _, l := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		switch k {
		case "seq":
			sp.Seq, _ = strconv.Atoi(v)
		case "at":
			sp.At, _ = time.Parse(time.RFC3339Nano, v)
		case "checkpoint":
			sp.Checkpoint = v
		case "wait":
			if json.Valid([]byte(v)) {
				sp.Wait = json.RawMessage(v)
			}
		}
	}
	if sp.Seq <= 0 || sp.At.IsZero() {
		return hostSafepoint{}, false
	}
	return sp, true
}

// hostDecision is the queue's answer at a safe point, the
// c4.avalanche.io/safepoint-decision annotation and the file c4-unit reads.
type hostDecision struct {
	Seq int       `json:"seq"`
	Do  string    `json:"do"`
	Why string    `json:"why,omitempty"`
	At  time.Time `json:"at"`
}

// relay does for a host unit what the node daemon does for a pod: the
// pod's annotations go to the unit's podinfo file, a new safe point goes
// to the pod's c4.avalanche.io/safepoint annotation, and the queue's
// answer goes to the unit's control/safepoint.decision. It notes a stop
// decision, so the session ending after it is a yield, not the member's
// end.
func (r *Runner) relay(ctx context.Context, p *corev1.Pod, u UnitDirs) error {
	if err := writePodInfo(u.PodInfo, p.Annotations); err != nil {
		return err
	}
	st := r.relays[p.UID]
	if st == nil {
		st = &relayState{}
		r.relays[p.UID] = st
	}
	if err := r.relaySteer(ctx, p, u, st); err != nil {
		return err
	}
	if sp, ok := readSafepoint(u.Out); ok && sp.Seq > st.asked {
		note, _ := json.Marshal(sp)
		if err := r.annotate(ctx, p, map[string]string{c4v1.AnnotationSafepoint: string(note)}); err != nil {
			return err
		}
		st.asked = sp.Seq
	}
	if st.asked <= st.answered {
		return nil
	}
	var d hostDecision
	if json.Unmarshal([]byte(p.Annotations[c4v1.AnnotationSafepointDecision]), &d) != nil || d.Seq != st.asked {
		return nil
	}
	b, _ := json.Marshal(d)
	if err := writeAtomic(filepath.Join(u.Control, "safepoint.decision"), b); err != nil {
		return err
	}
	st.answered = d.Seq
	st.stopped = d.Do == "stop"
	if st.stopped {
		st.why = d.Why
	}
	return nil
}

// relaySteer does for a host unit what the node daemon does for a pod:
// the unit's unread messages go to control/steer, and how far it read
// (.c4/steer-seen) to the pod's c4.avalanche.io/steer-seen.
func (r *Runner) relaySteer(ctx context.Context, p *corev1.Pod, u UnitDirs, st *relayState) error {
	if s := p.Annotations[c4v1.AnnotationUnitSteer]; s != st.steer {
		msgs := "[]"
		if s != "" && json.Valid([]byte(s)) {
			msgs = s
		}
		if err := writeAtomic(filepath.Join(u.Control, "steer"), []byte(msgs)); err != nil {
			return err
		}
		st.steer = s
	}
	if n, ok := node.ReadSteerSeen(u.Out); ok && n > st.read {
		if err := r.annotate(ctx, p, map[string]string{c4v1.AnnotationSteerSeen: strconv.Itoa(n)}); err != nil {
			return err
		}
		st.read = n
	}
	for file, key := range node.UnitNotes {
		b, ok := node.ReadUnitNote(u.Out, file)
		if !ok || b == st.notes[file] {
			continue
		}
		if err := r.annotate(ctx, p, map[string]string{key: b}); err != nil {
			return err
		}
		if st.notes == nil {
			st.notes = map[string]string{}
		}
		st.notes[file] = b
	}
	return nil
}

type relayState struct {
	asked, answered int
	stopped         bool
	why             string
	// steer is the messages last written to control/steer, and read how
	// far the unit said it read them.
	steer string
	read  int
	// notes are the unit's .c4/asks and .c4/answers last relayed.
	notes map[string]string
}

// controlDocs are the documents a member leaves for the farm, as a pod
// leaves them in its .c4/ (node/control.go): a generator's expansion
// document, a review unit's verdict, a measure of the job's done, a model
// unit's token use, and a block's summary of its task. clean, when set, is
// what the farm keeps of it.
var controlDocs = []struct {
	file, what, annotation string
	clean                  func([]byte) []byte
}{
	{"expand.json", "expansion document", c4v1.AnnotationOutputExpand, nil},
	{"verdict.json", "verdict", c4v1.AnnotationOutputVerdict, nil},
	{"measure.json", "measure", c4v1.AnnotationOutputMeasure, nil},
	{"usage.json", "token use", c4v1.AnnotationOutputUsage, nil},
	{"done", "summary", c4v1.AnnotationOutputSummary, node.Summary},
}

// storeControlDocs stores the control documents a member wrote and names
// them on the pod (c4.avalanche.io/output-expand, output-verdict,
// output-measure, output-usage), as the
// node daemon does at capture. The member writes each to its unit's
// out/.c4/ or, when it cannot see $C4_OUTPUT, to .c4/ in its working
// directory. Without a store a document cannot reach the queue: the runner
// says so, and the queue acts on none.
func (r *Runner) storeControlDocs(ctx context.Context, p *corev1.Pod, m *Member, u UnitDirs) error {
	ann := map[string]string{}
	for _, cd := range controlDocs {
		dirs := []string{u.Out, m.Cwd}
		if m.Block {
			// block sessions share their folder: only the unit's own counts
			dirs = dirs[:1]
		}
		data, from, err := readControlDoc(cd.file, dirs...)
		if err != nil {
			return err
		}
		if from != "" && cd.clean != nil {
			if data = cd.clean(data); len(data) == 0 {
				continue
			}
		}
		if from == "" {
			continue
		}
		if r.Docs == nil {
			r.logf("%s wrote a %s (%s), but this runner has no store (--store); the queue will not see it", m.Name, cd.what, from)
			continue
		}
		project, err := r.projectOf(ctx, p.Namespace)
		if err != nil {
			return err
		}
		id := c4.Identify(bytes.NewReader(data))
		if err := r.Docs.PutBytes(ctx, project, id, data); err != nil {
			return fmt.Errorf("store %s: %w", from, err)
		}
		r.logf("%s: stored its %s %s (%s)", m.Name, cd.what, id, from)
		ann[cd.annotation] = id.String()
	}
	if len(ann) == 0 {
		return nil
	}
	return r.annotate(ctx, p, ann)
}

// readControlDoc reads .c4/<file> from the first of dirs that has it.
func readControlDoc(file string, dirs ...string) (data []byte, from string, err error) {
	for _, d := range dirs {
		path := filepath.Join(d, ".c4", file)
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		data, err = io.ReadAll(io.LimitReader(f, maxDoc+1))
		f.Close()
		if err != nil {
			return nil, "", err
		}
		if len(data) > maxDoc {
			return nil, "", fmt.Errorf("%s is over %d bytes", path, maxDoc)
		}
		return data, path, nil
	}
	return nil, "", nil
}

// projectOf is the project that owns namespace ns, as the queue reads it:
// DocsProject when the runner is told, else the Project listing ns.
func (r *Runner) projectOf(ctx context.Context, ns string) (string, error) {
	if r.DocsProject != "" {
		return r.DocsProject, nil
	}
	if r.readsHUD() {
		if p := r.nsProject[ns]; p != "" {
			return p, nil
		}
		return "namespace/" + ns, nil
	}
	var ps c4v1.ProjectList
	if err := r.Client.List(ctx, &ps); err != nil {
		return "", fmt.Errorf("the project of namespace %s: %w (set --store-project)", ns, err)
	}
	for _, p := range ps.Items {
		for _, x := range p.Spec.Namespaces {
			if x == ns {
				return p.Name, nil
			}
		}
	}
	return "namespace/" + ns, nil
}
