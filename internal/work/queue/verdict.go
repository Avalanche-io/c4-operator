package queue

import (
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

// Verdicts (DESIGN.md 10.6). A review unit judges the units it runs
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
//	    {"units": ["a3"], "do": "pass", "score": 0.82},
//	    {"units": ["a4"], "do": "supersede", "why": "a3 is better"},
//	    {"do": "expand", "expand": { an expansion document (DESIGN 10.6) }}],
//	  "pass": "rest", "maxPasses": 2, "onExhausted": "escalate:lead",
//	  "select": {"top": 1} }        // or {"threshold": 0.8}, {"ucb": 1.4, "top": 1}
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
//   - score is a number per judged unit, higher better unless the review
//     unit's params say scoreOrder: asc. A later verdict may score a unit
//     again (a backup in a tree search): the last score stands, and every
//     one is in the signed record.
//   - select chooses among the passing units with a score (this verdict's,
//     else the one standing): the top k, those at or past a threshold, or
//     the top k (1) by UCB, score plus c times sqrt(ln N / n), n being how
//     many verdicts scored the unit and N their sum over the candidates.
//     The chosen pass; the rest are superseded.
//   - supersede ends units because others were chosen: judged units, or
//     the review's unfinished siblings (alternatives still running stop,
//     with no retry spent and nothing failed). A superseded unit is done,
//     not accepted: nothing waits on it, and its changesets leave the job's
//     output, which is refolded without them.
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
	Select      *selectRule   `json:"select"`
}

// selectRule chooses among scored units: Top, Threshold, or UCB with Top.
type selectRule struct {
	Top       *int     `json:"top"`
	Threshold *float64 `json:"threshold"`
	UCB       *float64 `json:"ucb"`
}

// String names the rule, for the record and the HUD.
func (r *selectRule) String() string {
	switch {
	case r == nil:
		return ""
	case r.UCB != nil:
		return fmt.Sprintf("ucb %s top %d", strconv.FormatFloat(*r.UCB, 'f', -1, 64), r.k())
	case r.Threshold != nil:
		return "threshold " + strconv.FormatFloat(*r.Threshold, 'f', -1, 64)
	}
	return fmt.Sprintf("top %d", r.k())
}

func (r *selectRule) k() int {
	if r.Top == nil {
		return 1
	}
	return *r.Top
}

func (r *selectRule) check() error {
	switch {
	case r.UCB == nil && r.Threshold == nil && r.Top == nil:
		return fmt.Errorf("select: want top, threshold or ucb")
	case r.Threshold != nil && (r.Top != nil || r.UCB != nil):
		return fmt.Errorf("select: a threshold stands alone")
	case r.Top != nil && *r.Top < 1:
		return fmt.Errorf("select: top %d, want 1 or more", *r.Top)
	case r.UCB != nil && (*r.UCB < 0 || math.IsNaN(*r.UCB)):
		return fmt.Errorf("select: ucb %v, want 0 or more", *r.UCB)
	}
	return nil
}

type verdictItem struct {
	Units  []string         `json:"units"`
	Do     string           `json:"do"`
	Why    string           `json:"why"`
	To     string           `json:"to"`
	Note   string           `json:"note"`
	With   *c4v1.WorkRework `json:"with"`
	Expand json.RawMessage  `json:"expand"`
	Score  *float64         `json:"score"`
}

// action is one judged unit's outcome.
type action struct {
	unit      int
	do        string // pass, rework, escalate, accept, fail
	why, to   string
	note      string
	with      *c4v1.WorkRework
	exhausted bool
	score     *float64
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
	rec.Verdict, rec.Select = d.Verdict, d.Select.String()
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
			case "supersede":
				rec.Superseded++
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
	if rec.Error == "" && idx < len(m.plan.Units) {
		q.noteVerdict(v.project, v.job.Spec.Class, m.plan.Units[idx].Key, int(rec.Reworked),
			int(rec.Passed+rec.Reworked+rec.Escalated+rec.Accepted+rec.Superseded))
	}
	q.Log.Info("verdict", "job", v.key, "unit", idx, "verdict", rec.Verdict, "passed", rec.Passed, "reworked", rec.Reworked,
		"escalated", rec.Escalated, "accepted", rec.Accepted, "expanded", rec.Expanded, "error", rec.Error)
	return true, rec.Error != "", rerun
}

// judge reads a verdict into one action per judged unit, and the
// expansion it carries, if any. It changes nothing.
func (q *Queue) judge(ctx context.Context, v *view, idx int, doc []byte) ([]action, []byte, verdictDoc, error) {
	m := v.mem
	var d verdictDoc
	if err := class.Strict(doc, &d); err != nil {
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
			if _, err := q.checkExpansion(v, idx, it.Expand); err != nil {
				return nil, nil, d, fmt.Errorf("the verdict's expansion: %w", err)
			}
			expand = it.Expand
			continue
		case "pass", "rework", "escalate", "accept", "supersede":
		default:
			return nil, nil, d, fmt.Errorf("do %q: want pass, rework, escalate, accept, supersede or expand", it.Do)
		}
		if it.Score != nil && (math.IsNaN(*it.Score) || math.IsInf(*it.Score, 0)) {
			return nil, nil, d, fmt.Errorf("score %v", *it.Score)
		}
		among := judged
		if it.Do == "supersede" {
			among = supersedable(m, idx)
		}
		if it.With != nil {
			if err := checkRework(v, it.With); err != nil {
				return nil, nil, d, err
			}
		}
		for _, ref := range it.Units {
			units, err := resolveUnits(m.plan, among, ref, roundPrefix(m.plan, idx))
			if err != nil {
				return nil, nil, d, err
			}
			for _, u := range units {
				if seen[u] {
					return nil, nil, d, fmt.Errorf("the verdict judges unit %d twice", u)
				}
				seen[u] = true
				a := action{unit: u, do: it.Do, why: it.Why, to: it.To, note: it.Note, with: it.With, score: it.Score}
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
	if d.Select != nil {
		if err := d.Select.check(); err != nil {
			return nil, nil, d, err
		}
		selectAmong(m, acts, d.Select, m.plan.Units[idx].Params["scoreOrder"] == "asc")
	}
	sort.SliceStable(acts, func(a, b int) bool { return acts[a].unit < acts[b].unit })
	return acts, expand, d, nil
}

// supersedable are the units review idx may supersede: those it judges,
// and its unfinished siblings (alternatives still running).
func supersedable(m *jobMem, idx int) []int {
	plan := m.plan
	out := slices.Clone(plan.Units[idx].After)
	r := plan.Units[idx]
	for i, u := range plan.Units {
		if i == idx || u.Depth != r.Depth || u.Parent != r.Parent || slices.Contains(out, i) || m.done(i) || m.failed[i] {
			continue
		}
		if slices.Contains(u.After, idx) {
			continue // it runs after the review
		}
		out = append(out, i)
	}
	return out
}

// selectAmong applies rule to the passing units with a score: the chosen
// pass, the rest become supersede.
func selectAmong(m *jobMem, acts []action, rule *selectRule, asc bool) {
	type cand struct {
		at    int
		score float64
		n     float64
	}
	var cs []cand
	total := 0.0
	for k, a := range acts {
		if a.do != "pass" {
			continue
		}
		s, n, ok := scoreOf(m, a)
		if !ok {
			continue
		}
		if asc {
			s = -s
		}
		cs = append(cs, cand{at: k, score: s, n: n})
		total += n
	}
	value := func(c cand) float64 {
		if rule.UCB == nil {
			return c.score
		}
		return c.score + *rule.UCB*math.Sqrt(math.Log(max(total, 1))/c.n)
	}
	sort.SliceStable(cs, func(a, b int) bool { return value(cs[a]) > value(cs[b]) })
	for rank, c := range cs {
		keep := rank < rule.k()
		if rule.Threshold != nil {
			t := *rule.Threshold
			if asc {
				t = -t
			}
			keep = c.score >= t
		}
		if !keep {
			acts[c.at].do, acts[c.at].why = "supersede", "not selected: "+rule.String()
		}
	}
}

// scoreOf is a passing unit's score for selection (this verdict's, else
// the one standing) and how many verdicts scored it, this one included.
func scoreOf(m *jobMem, a action) (float64, float64, bool) {
	prev := m.reviews[a.unit]
	n := 0.0
	if prev != nil {
		n = float64(prev.Scored)
	}
	if a.score != nil {
		return *a.score, n + 1, true
	}
	if prev != nil && prev.Score != "" {
		if s, err := strconv.ParseFloat(prev.Score, 64); err == nil {
			return s, max(n, 1), true
		}
	}
	return 0, 0, false
}

// resolveUnits finds the judged units ref names: a name, a label, a range
// of labels, or #index. A review in a round (prefix r<k>/) may name its
// round's units as their document did.
func resolveUnits(plan *class.Plan, judged []int, ref, prefix string) ([]int, error) {
	in := func(i int) bool { return slices.Contains(judged, i) }
	find := func(s string) (int, bool) {
		if n, ok := strings.CutPrefix(s, "#"); ok {
			i, err := strconv.Atoi(n)
			return i, err == nil && i >= 0 && i < len(plan.Units)
		}
		for _, i := range judged {
			if n := plan.Units[i].Name; n == s || prefix != "" && n == prefix+s {
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
	switch a.do {
	case "rework", "escalate", "fail":
		did := map[string]string{"rework": "reworked", "escalate": "escalated", "fail": "failed"}[a.do]
		why := causeText(a.why)
		title := "review: " + a.do
		if why != "" {
			title += ": " + why
		}
		c := m.noteCause("verdict", "verdict:"+a.do+":"+why, title, a.unit, "", q.Now(), did)
		if a.do == "escalate" && a.to != "" {
			c.To = a.to
		}
	}
	if m.reviews == nil {
		m.reviews = map[int]*c4v1.WorkReview{}
	}
	prev := m.reviews[a.unit]
	r := &c4v1.WorkReview{Do: a.do, Why: a.why, Note: a.note, To: a.to, By: int32(idx)}
	if prev != nil {
		r.Passes, r.Score, r.Scored = prev.Passes, prev.Score, prev.Scored
	}
	if a.score != nil {
		r.Score, r.Scored = strconv.FormatFloat(*a.score, 'f', -1, 64), r.Scored+1
	}
	switch a.do {
	case "pass":
		if prev != nil && prev.Do == "rework" || a.score != nil {
			// A rework that now passes: its record stays, as passed. A
			// scored unit keeps its score.
			r.Do, r.With = "pass", nil
			m.reviews[a.unit] = r
		}
		return
	case "supersede":
		// Done, not accepted; its pod, if any, is stopped, and its
		// changesets leave the output (observe, refoldOutput).
		m.superseded[a.unit] = true
		delete(m.succeeded, a.unit)
		delete(m.failed, a.unit)
		m.refold = true
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
		if a.score != nil {
			o["score"] = *a.score
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
		"document": rec.Document, "verdict": rec.Verdict, "select": rec.Select, "outcomes": outcomes,
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

// Expected rework (DESIGN.md 10.6, Expected rework is priced from the
// start: the ETA prices rework before it happens). A review is a unit its
// document marks (params.review), or one whose key has written verdicts in
// the project (rework.go: history across jobs, by class and key) or in this
// job. Its rework rate is the units its key reworked over the units it
// judged, shrunk toward a prior (the review unit's reworkRate param from
// its document, else the job's, else reworkDefault) with a pseudo-count of
// reworkShrink, so p50 and p90 carry rework from the start. A review that
// has not run yet adds, for each unit it will judge, the further passes it
// may send back: r, r²... up to maxPasses less the unit's passes so far
// (once it has judged, only for the units it sent back), each as tentative
// work (estimate.Unit.Exists) at what a rerun of that key costs
// (reworkFrac: a rework resumes from the unit's last output, so it is
// priced as a resumed unit); and its own rerun, with the chance that it
// reworks anything. The same chains lengthen the critical path.
const reworkShrink = 2.0

// expectedRework is the tentative work a job's pending reviews may send
// back, and how much each review lengthens the chain through it.
func (q *Queue) expectedRework(v *view, e *estimate.Estimator) ([]estimate.Unit, map[int]float64) {
	m, plan := v.mem, v.mem.plan
	class := v.job.Spec.Class
	seen := map[string][2]float64{} // reworked, judged
	ran := map[int]bool{}           // reviews that have judged
	for _, x := range m.verdicts {
		if x.Error != "" || int(x.Unit) >= len(plan.Units) {
			continue
		}
		ran[int(x.Unit)] = true
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
		// The project's history includes this job's verdicts once they
		// are carried out; the job's own count stands in when the history
		// was lost (a restart before it was saved).
		if h := q.reworkOf(v.project, class, u.Key); h.Judged >= obs[1] {
			obs, learned = [2]float64{h.Reworked, h.Judged}, learned || h.Judged > 0
		}
		if !learned && u.Params["review"] != "true" {
			continue
		}
		prior, err := strconv.ParseFloat(u.Params["reworkRate"], 64)
		if err != nil {
			prior, err = strconv.ParseFloat(v.job.Spec.Params["reworkRate"], 64)
		}
		if err != nil || prior < 0 {
			prior = reworkDefault
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
			rv := m.reviews[a]
			if rv != nil {
				passes = int(rv.Passes)
			}
			if ran[i] && (rv == nil || rv.By != int32(i) || rv.Do != "rework") {
				// A review running again judges again what it sent back;
				// what it passed or accepted stands.
				continue
			}
			if passes >= defaultMaxPasses {
				continue
			}
			p, chain := 1.0, 0.0
			mean, frac := e.Predict(ju.Key, ju.Pos).Mean, q.reworkFrac(v.project, class, ju.Key)
			for k := passes; k < defaultMaxPasses; k++ {
				p *= r
				extra = append(extra, estimate.Unit{Key: ju.Key, Pos: ju.Pos, Slots: ju.Slots, Exists: p, Done: max(0, 1-frac) * mean})
				chain += p * frac * mean
			}
			longest = math.Max(longest, chain)
			pNone *= 1 - r
		}
		if pAny := 1 - pNone; pAny > 0 {
			mean, frac := e.Predict(u.Key, u.Pos).Mean, q.reworkFrac(v.project, class, u.Key)
			extra = append(extra, estimate.Unit{Key: u.Key, Pos: u.Pos, Slots: u.Slots, Exists: pAny, Done: max(0, 1-frac) * mean})
			path[i] = longest + pAny*frac*mean
		}
	}
	return extra, path
}
