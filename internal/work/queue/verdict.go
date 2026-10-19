package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// Verdicts (PRIMITIVES Q4, Q5). A review unit judges the units it runs
// after, and writes its judgment as .c4/verdict.json; the node daemon
// stores it at capture (c4.avalanche.io/output-verdict). When the review
// unit succeeds the queue carries the verdict out, once for that pod, and
// signs it (decision/v1, kind verdict). The document, version 1:
//
//	{ "version": 1, "verdict": "mixed",
//	  "items": [
//	    {"units": ["1043", "1101-1102"], "do": "rework", "why": "NaN in beauty",
//	     "with": {"avoidNodes": ["node-17"], "memory": "96Gi", "pool": "", "image": "",
//	              "env": {}, "inputs": [{"view": "c4...", "mountPath": "/note"}], "fresh": false}},
//	    {"units": ["1088"], "do": "escalate", "to": "lighting-lead", "why": "flicker"},
//	    {"units": ["1010"], "do": "accept", "note": "firefly, paint in comp"},
//	    {"do": "expand", "expand": { an expansion document (DESIGN 10.6) }}],
//	  "pass": "rest", "maxPasses": 2, "onExhausted": "escalate:lead" }
//
// A unit is named by its name, its label, a range of labels ("1101-1102",
// every unit between the two in the table) or its index ("#12"), and only
// units the review runs after may be judged; "pass": "rest" passes the
// others it runs after.
//
//   - rework runs the units again, with what "with" changes: nodes to
//     avoid, the only pool to use, the first container's memory or image,
//     env, views to mount (a counterexample), and, unless fresh, from the
//     unit's last output view as a yield resumes. The review unit then
//     runs again after them: a loop with a bound. A unit reworked
//     maxPasses times (2) gets onExhausted instead: escalate (to a named
//     person), accept, or fail.
//   - escalate flags the units for a person (NeedsPerson Review). They
//     are failed units to the queue, so requeue, take and accept work on
//     them as on any failure; the review that flagged them does not wait
//     on them when it runs again.
//   - accept lets the units stand, with the note recorded.
//   - expand adds units under the review unit, as a generator's
//     expansion does.
//
// A verdict that names a unit the review does not run after, names a
// unit twice, or carries a bad override or expansion is refused whole:
// nothing is done, and the review unit fails.

const (
	defaultMaxPasses = 2
	maxVerdictsKept  = 32
)

type verdictDoc struct {
	Version     int           `json:"version"`
	Verdict     string        `json:"verdict"`
	Items       []verdictItem `json:"items"`
	Pass        string        `json:"pass"`
	MaxPasses   *int          `json:"maxPasses"`
	OnExhausted string        `json:"onExhausted"`
}

type verdictItem struct {
	Units  []string         `json:"units"`
	Do     string           `json:"do"`
	Why    string           `json:"why"`
	To     string           `json:"to"`
	Note   string           `json:"note"`
	With   *c4v1.WorkRework `json:"with"`
	Expand json.RawMessage  `json:"expand"`
}

// action is one judged unit's outcome.
type action struct {
	unit      int
	do        string // pass, rework, escalate, accept, fail
	why, to   string
	note      string
	with      *c4v1.WorkRework
	exhausted bool
}

// verdictDone reports whether the verdict of review pod uid was carried
// out.
func (m *jobMem) verdictDone(uid string) bool {
	for _, x := range m.verdicts {
		if x.pod == uid {
			return true
		}
	}
	return false
}

type verdictRec struct {
	c4v1.WorkVerdict
	pod string
}

// flagged reports whether a review escalated unit i to a person, who has
// not acted yet.
func (m *jobMem) flagged(i int) bool {
	r := m.reviews[i]
	return r != nil && r.Do == "escalate" && m.failed[i] && !m.accepted[i]
}

// waitsOn reports whether unit i still waits on unit a: a has not
// succeeded (nor been accepted), and it is not a unit i itself flagged.
func (m *jobMem) waitsOn(i, a int) bool {
	if m.done(a) {
		return false
	}
	if r := m.reviews[a]; r != nil && int(r.By) == i && m.flagged(a) {
		return false
	}
	return true
}

// verdictOf reads review pod p's verdict, if it wrote one. It reports
// false when the document could not be read yet.
func (q *Queue) verdictOf(ctx context.Context, v *view, p *corev1.Pod) (id string, doc []byte, ok bool) {
	id = p.Annotations[c4v1.AnnotationOutputVerdict]
	if id == "" || v.mem.verdictDone(string(p.UID)) {
		return "", nil, true
	}
	doc, err := q.expansionDoc(ctx, v.project, v.mem, id)
	if err != nil {
		q.Log.Error(err, "verdict document", "job", v.key, "pod", p.Name)
		return "", nil, false
	}
	return id, doc, true
}

// carryOut carries out review unit idx's verdict doc (stored as id), from
// pod p. ok is false when it must be tried again next tick (the store is
// unreachable), and nothing was done. refused: the verdict was refused,
// nothing was done, and the caller fails the review unit. rerun: units
// were reworked, so the review runs again after them.
func (q *Queue) carryOut(ctx context.Context, v *view, idx int, p *corev1.Pod, id string, doc []byte, now time.Time) (ok, refused, rerun bool) {
	m := v.mem
	rec := verdictRec{WorkVerdict: c4v1.WorkVerdict{Unit: int32(idx), Attempt: int32(attemptOf(p)), Document: id, Pod: string(p.UID),
		At: metav1.Time{Time: now.Truncate(time.Second)}}, pod: string(p.UID)}
	acts, expand, d, err := q.judge(ctx, v, idx, doc)
	if err != nil {
		rec.Error = err.Error()
	}
	rec.Verdict = d.Verdict
	var expandID string
	if err == nil && expand != nil {
		if expandID, err = q.storeDoc(ctx, v, expand); err != nil {
			// The store is unreachable: nothing is done, and the review
			// is tried again next tick.
			q.Log.Error(err, "store a verdict's expansion", "job", v.key, "unit", idx)
			return false, false, false
		}
	}
	if rec.Error == "" {
		reworked := false
		for _, a := range acts {
			q.apply(v, idx, a)
			switch a.do {
			case "pass":
				rec.Passed++
			case "rework":
				rec.Reworked++
				reworked = true
			case "escalate":
				rec.Escalated++
			case "accept":
				rec.Accepted++
			}
		}
		if expand != nil {
			x := q.expandWith(ctx, v, idx, expandID, expand, now)
			rec.Expanded = int32(x.Units)
		}
		rerun = reworked
	}
	rec.Record = q.recordVerdict(ctx, v, rec, acts, p, now)
	m.verdicts = append(m.verdicts, rec)
	q.Log.Info("verdict", "job", v.key, "unit", idx, "verdict", rec.Verdict, "passed", rec.Passed, "reworked", rec.Reworked,
		"escalated", rec.Escalated, "accepted", rec.Accepted, "expanded", rec.Expanded, "error", rec.Error)
	return true, rec.Error != "", rerun
}

// judge reads a verdict into one action per judged unit, and the
// expansion it carries, if any. It changes nothing.
func (q *Queue) judge(ctx context.Context, v *view, idx int, doc []byte) ([]action, []byte, verdictDoc, error) {
	m := v.mem
	var d verdictDoc
	dec := json.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&d); err != nil {
		return nil, nil, d, fmt.Errorf("verdict document: %w", err)
	}
	if d.Version != 1 {
		return nil, nil, d, fmt.Errorf("verdict document version %d, want 1", d.Version)
	}
	judged := m.plan.Units[idx].After
	if len(judged) == 0 {
		return nil, nil, d, fmt.Errorf("unit %d wrote a verdict, and runs after no unit it could judge", idx)
	}
	maxPasses := defaultMaxPasses
	if d.MaxPasses != nil {
		if *d.MaxPasses < 0 {
			return nil, nil, d, fmt.Errorf("maxPasses %d", *d.MaxPasses)
		}
		maxPasses = *d.MaxPasses
	}
	exhaustDo, exhaustTo, _ := strings.Cut(d.OnExhausted, ":")
	switch exhaustDo {
	case "":
		exhaustDo = "escalate"
	case "escalate", "accept", "fail":
	default:
		return nil, nil, d, fmt.Errorf("onExhausted %q: want escalate[:who], accept or fail", d.OnExhausted)
	}
	var acts []action
	seen := map[int]bool{}
	var expand []byte
	for _, it := range d.Items {
		switch it.Do {
		case "expand":
			if len(it.Expand) == 0 || expand != nil {
				return nil, nil, d, fmt.Errorf("a verdict carries one expansion document")
			}
			if _, err := checkExpansion(v, idx, it.Expand); err != nil {
				return nil, nil, d, fmt.Errorf("the verdict's expansion: %w", err)
			}
			expand = it.Expand
			continue
		case "pass", "rework", "escalate", "accept":
		default:
			return nil, nil, d, fmt.Errorf("do %q: want pass, rework, escalate, accept or expand", it.Do)
		}
		if it.With != nil {
			if err := checkRework(v, it.With); err != nil {
				return nil, nil, d, err
			}
		}
		for _, ref := range it.Units {
			units, err := resolveUnits(m.plan, judged, ref)
			if err != nil {
				return nil, nil, d, err
			}
			for _, u := range units {
				if seen[u] {
					return nil, nil, d, fmt.Errorf("the verdict judges unit %d twice", u)
				}
				seen[u] = true
				a := action{unit: u, do: it.Do, why: it.Why, to: it.To, note: it.Note, with: it.With}
				if a.do == "rework" {
					if r := m.reviews[u]; r != nil && int(r.Passes) >= maxPasses {
						a.do, a.to, a.exhausted = exhaustDo, exhaustTo, true
						a.why = fmt.Sprintf("reworked %d times, the most this review allows: %s", r.Passes, it.Why)
						if a.do == "accept" {
							a.note = a.why
						}
					}
				}
				acts = append(acts, a)
			}
		}
	}
	if d.Pass == "" || d.Pass == "rest" {
		for _, u := range judged {
			if !seen[u] {
				acts = append(acts, action{unit: u, do: "pass"})
			}
		}
	} else if d.Pass != "none" {
		return nil, nil, d, fmt.Errorf("pass %q: want rest or none", d.Pass)
	}
	sort.SliceStable(acts, func(a, b int) bool { return acts[a].unit < acts[b].unit })
	return acts, expand, d, nil
}

// resolveUnits finds the judged units ref names: a name, a label, a range
// of labels, or #index.
func resolveUnits(plan *class.Plan, judged []int, ref string) ([]int, error) {
	in := func(i int) bool { return slices.Contains(judged, i) }
	find := func(s string) (int, bool) {
		if n, ok := strings.CutPrefix(s, "#"); ok {
			i, err := strconv.Atoi(n)
			return i, err == nil && i >= 0 && i < len(plan.Units)
		}
		for _, i := range judged {
			if plan.Units[i].Name == s {
				return i, true
			}
		}
		for _, i := range judged {
			if plan.Units[i].Label == s {
				return i, true
			}
		}
		return 0, false
	}
	if i, ok := find(ref); ok {
		if !in(i) {
			return nil, fmt.Errorf("the verdict judges %s, which the review does not run after", ref)
		}
		return []int{i}, nil
	}
	if a, b, ok := strings.Cut(ref, "-"); ok {
		i, ok1 := find(a)
		j, ok2 := find(b)
		if ok1 && ok2 {
			var out []int
			for _, u := range judged {
				if u >= min(i, j) && u <= max(i, j) {
					out = append(out, u)
				}
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("the verdict names %s, which is no unit the review runs after", ref)
}

func checkRework(v *view, w *c4v1.WorkRework) error {
	if w.Memory != "" {
		if _, err := resource.ParseQuantity(w.Memory); err != nil {
			return fmt.Errorf("rework memory %q: %w", w.Memory, err)
		}
	}
	if w.Pool != "" && !slices.Contains(v.pools, w.Pool) {
		return fmt.Errorf("rework pool %s is not one the job may use %v", w.Pool, v.pools)
	}
	for _, in := range w.Inputs {
		if _, err := c4.Parse(in.View); err != nil || in.MountPath == "" {
			return fmt.Errorf("rework input %q at %q: want a C4 ID and a mountPath", in.View, in.MountPath)
		}
	}
	return nil
}

// apply carries out one action of review idx's verdict.
func (q *Queue) apply(v *view, idx int, a action) {
	m := v.mem
	if m.reviews == nil {
		m.reviews = map[int]*c4v1.WorkReview{}
	}
	prev := m.reviews[a.unit]
	r := &c4v1.WorkReview{Do: a.do, Why: a.why, Note: a.note, To: a.to, By: int32(idx)}
	if prev != nil {
		r.Passes = prev.Passes
	}
	switch a.do {
	case "pass":
		if prev != nil && prev.Do == "rework" {
			// A rework that now passes: its record stays, as passed.
			r.Do, r.With = "pass", nil
			m.reviews[a.unit] = r
		}
		return
	case "rework":
		r.Passes++
		r.With = a.with
		i := a.unit
		delete(m.succeeded, i)
		delete(m.millis, i)
		delete(m.ended, i)
		m.forgetAcks(i)
		m.attempts[i] = 0
		_, resumer := v.driver.(class.Resumer)
		if view := m.unitOut[i]; view != "" && !resumer && (a.with == nil || !a.with.Fresh) && needsCapture(v.job) {
			// The next attempt starts from what the last one wrote.
			m.resume[i] = view
		}
	case "escalate", "fail":
		// Held for a person: a failed unit to the queue (requeue, take
		// and accept work as on any failure), flagged when escalated.
		delete(m.succeeded, a.unit)
		m.failed[a.unit] = true
		if a.do == "fail" {
			r.Do = "fail"
		}
	case "accept":
	}
	m.reviews[a.unit] = r
}

// storeDoc stores a document in the job's project store and caches it.
func (q *Queue) storeDoc(ctx context.Context, v *view, doc []byte) (string, error) {
	if q.Stores == nil {
		return "", fmt.Errorf("no store")
	}
	st, err := q.Stores(ctx, v.project)
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", fmt.Errorf("project %s has no store", v.project)
	}
	cid, err := st.PutBytes(ctx, doc)
	if err != nil {
		return "", err
	}
	if v.mem.docs == nil {
		v.mem.docs = map[string][]byte{}
	}
	v.mem.docs[cid.String()] = doc
	return cid.String(), nil
}

// recordVerdict signs a verdict as carried out: the review unit and its
// record, the document, and each judged unit's output and outcome.
func (q *Queue) recordVerdict(ctx context.Context, v *view, rec verdictRec, acts []action, p *corev1.Pod, now time.Time) string {
	if q.Signer == nil {
		return ""
	}
	var outcomes []map[string]any
	for _, a := range acts {
		o := map[string]any{"unit": a.unit, "do": a.do}
		if out := v.mem.unitOut[a.unit]; out != "" {
			o["output"] = out
		}
		for k, s := range map[string]string{"why": a.why, "to": a.to, "note": a.note} {
			if s != "" {
				o[k] = s
			}
		}
		if a.with != nil {
			o["with"] = a.with
		}
		if a.exhausted {
			o["exhausted"] = true
		}
		outcomes = append(outcomes, o)
	}
	pred := map[string]any{"decision": "verdict", "job": v.key, "unit": rec.Unit, "attempt": rec.Attempt,
		"document": rec.Document, "verdict": rec.Verdict, "outcomes": outcomes,
		"policy": q.PolicyName, "at": now.UTC().Format(time.RFC3339)}
	if r := p.Annotations[c4v1.AnnotationOutputProvenance]; r != "" {
		pred["review"] = r
	}
	if rec.Error != "" {
		pred["error"] = rec.Error
	}
	subjects := []provenance.Subject{jobSubject(v.job)}
	if cid, err := c4.Parse(rec.Document); err == nil {
		subjects = append(subjects, provenance.SubjectFor("verdict", cid))
	}
	st, err := provenance.SignStatement(ctx, q.Signer, provenance.PredicateDecision, subjects, pred, now)
	if err == nil {
		var id string
		if id, err = q.storeRecord(ctx, v, st); err == nil {
			return id
		}
	}
	q.Log.Error(err, "verdict record", "job", v.key, "unit", rec.Unit)
	return ""
}

// rework is unit idx's standing rework override, if any.
func (m *jobMem) rework(idx int) *c4v1.WorkRework {
	if r := m.reviews[idx]; r != nil && r.Do == "rework" {
		return r.With
	}
	return nil
}

// reworkInputName is the Materialization a reworked unit mounts input k
// through.
func reworkInputName(job string, idx, k int) string {
	return fmt.Sprintf("%s-rework-u%d-%d", job, idx, k)
}

// ensureReworkInputs makes the Materializations a reworked unit's added
// inputs mount, and reports whether they are all Ready.
func (q *Queue) ensureReworkInputs(ctx context.Context, v *view, idx int, w *c4v1.WorkRework) (bool, error) {
	ready := true
	for k, in := range w.Inputs {
		ok, err := q.ensureMaterialization(ctx, v.job, reworkInputName(v.job.Name, idx, k), in.View, "")
		if err != nil {
			return false, err
		}
		ready = ready && ok
	}
	return ready, nil
}

// applyRework changes a reworked unit's pod as its verdict said.
func applyRework(pod *corev1.Pod, job string, idx int, w *c4v1.WorkRework) {
	if len(w.AvoidNodes) > 0 {
		pod.Spec.Affinity = notOnNodes(pod.Spec.Affinity, w.AvoidNodes)
	}
	if len(pod.Spec.Containers) > 0 {
		c := &pod.Spec.Containers[0]
		if w.Image != "" {
			c.Image = w.Image
		}
		if w.Memory != "" {
			if qty, err := resource.ParseQuantity(w.Memory); err == nil {
				if c.Resources.Requests == nil {
					c.Resources.Requests = corev1.ResourceList{}
				}
				if c.Resources.Limits == nil {
					c.Resources.Limits = corev1.ResourceList{}
				}
				c.Resources.Requests[corev1.ResourceMemory] = qty
				c.Resources.Limits[corev1.ResourceMemory] = qty
			}
		}
	}
	keys := make([]string, 0, len(w.Env))
	for k := range w.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for k := range w.Inputs {
		name, ro := fmt.Sprintf("c4-rework-input-%d", k), true
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			CSI: &corev1.CSIVolumeSource{Driver: c4v1.DriverName, ReadOnly: &ro,
				VolumeAttributes: map[string]string{c4v1.VolumeAttrMaterialization: reworkInputName(job, idx, k)}}}})
	}
	for c := range pod.Spec.Containers {
		for _, k := range keys {
			pod.Spec.Containers[c].Env = append(pod.Spec.Containers[c].Env, corev1.EnvVar{Name: k, Value: w.Env[k]})
		}
		for k, in := range w.Inputs {
			pod.Spec.Containers[c].VolumeMounts = append(pod.Spec.Containers[c].VolumeMounts,
				corev1.VolumeMount{Name: fmt.Sprintf("c4-rework-input-%d", k), MountPath: in.MountPath, ReadOnly: true})
		}
	}
}

func verdictStatus(m *jobMem) []c4v1.WorkVerdict {
	var out []c4v1.WorkVerdict
	for _, x := range m.verdicts[max(0, len(m.verdicts)-maxVerdictsKept):] {
		out = append(out, x.WorkVerdict)
	}
	return out
}

func reviewStatus(m *jobMem) map[string]c4v1.WorkReview {
	if len(m.reviews) == 0 {
		return nil
	}
	out := map[string]c4v1.WorkReview{}
	for i, r := range m.reviews {
		out[strconv.Itoa(i)] = *r
	}
	return out
}

// Expected rework (PRIMITIVES Q5: the ETA prices rework before it
// happens). Each review key learns its rework rate from the verdicts its
// units wrote in this job, like a generator key's fan-out: the units
// reworked over the units judged, shrunk toward a prior (the review unit's
// reworkRate param from its document, else the job's, else none) with a
// pseudo-count of reworkShrink. A review that has not run yet adds, for
// each unit it will judge, the further passes it may send back: r, r²...
// up to maxPasses less the unit's passes so far, each as tentative work
// (estimate.Unit.Exists); and its own rerun, with the chance that it
// reworks anything. The same chains lengthen the critical path. A key
// with no verdicts and no prior adds nothing.
const reworkShrink = 2.0

// expectedRework is the tentative work a job's pending reviews may send
// back, and how much each review lengthens the chain through it.
func expectedRework(v *view, e *estimate.Estimator) ([]estimate.Unit, map[int]float64) {
	m, plan := v.mem, v.mem.plan
	seen := map[string][2]float64{} // reworked, judged
	for _, x := range m.verdicts {
		if x.Error != "" || int(x.Unit) >= len(plan.Units) {
			continue
		}
		k := plan.Units[x.Unit].Key
		r := seen[k]
		r[0] += float64(x.Reworked)
		r[1] += float64(x.Passed + x.Reworked + x.Escalated + x.Accepted)
		seen[k] = r
	}
	var extra []estimate.Unit
	path := map[int]float64{}
	for i, u := range plan.Units {
		if m.succeeded[i] || m.failed[i] || len(u.After) == 0 || isWait(plan, i) {
			continue
		}
		obs, learned := seen[u.Key]
		prior, err := strconv.ParseFloat(u.Params["reworkRate"], 64)
		if err != nil {
			prior, err = strconv.ParseFloat(v.job.Spec.Params["reworkRate"], 64)
		}
		hasPrior := err == nil && prior > 0
		if !learned && !(hasPrior && u.Params["review"] == "true") {
			continue
		}
		if !hasPrior {
			prior = 0
		}
		r := min((obs[0]+reworkShrink*prior)/(obs[1]+reworkShrink), 0.95)
		if r <= 0 {
			continue
		}
		pNone, longest := 1.0, 0.0
		for _, a := range u.After {
			if a < 0 || a >= len(plan.Units) || m.failed[a] {
				continue
			}
			ju := plan.Units[a]
			passes := 0
			if rv := m.reviews[a]; rv != nil {
				passes = int(rv.Passes)
			}
			if passes >= defaultMaxPasses {
				continue
			}
			p, chain := 1.0, 0.0
			for k := passes; k < defaultMaxPasses; k++ {
				p *= r
				extra = append(extra, estimate.Unit{Key: ju.Key, Pos: ju.Pos, Slots: ju.Slots, Exists: p})
				chain += p * e.Predict(ju.Key, ju.Pos).Mean
			}
			longest = math.Max(longest, chain)
			pNone *= 1 - r
		}
		if pAny := 1 - pNone; pAny > 0 {
			extra = append(extra, estimate.Unit{Key: u.Key, Pos: u.Pos, Slots: u.Slots, Exists: pAny})
			path[i] = longest + pAny*e.Predict(u.Key, u.Pos).Mean
		}
	}
	return extra, path
}
