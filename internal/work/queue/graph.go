package queue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// The growing unit graph (DESIGN 10.6). A job's unit table is its class's
// plan with every expansion applied, in order: when a generator unit
// succeeds, the expansion document the node daemon stored at capture
// (c4.avalanche.io/output-expand) adds its children at the end of the
// table, and units that waited on the generator now wait on them too.
// Each expansion is signed and listed in status (expansions), so a
// restart replays the same documents from the store to the same table.
//
// A unit runs once every unit in its After has succeeded (or was
// accepted); until then it is blocked. A generator that has not run is
// planned. A unit that waits, directly or not, on a failed unit is stuck:
// it is blocked until a person requeues or accepts the failure.

// expansionRec is one generator's evaluation (c4v1.WorkExpansion).
type expansionRec struct {
	Unit, First, Units int
	Doc, Record, Error string
	At                 time.Time
}

func (m *jobMem) expanded(idx int) bool {
	for _, x := range m.expansions {
		if x.Unit == idx && x.Error == "" {
			return true
		}
	}
	return false
}

// errLoading says the plan waits on expansion documents the store did not
// give back yet; the job keeps its last status and is tried next tick.
type errLoading struct{ err error }

func (e errLoading) Error() string { return "loading expansion documents: " + e.err.Error() }

// planJob builds the job's unit table: the class's plan, then each
// recorded expansion replayed from its document.
func (q *Queue) planJob(ctx context.Context, v *view) (*class.Plan, error) {
	m, j := v.mem, v.job
	if f := j.Spec.Forecast; f != nil {
		// A forecast is sized, never run: slot-hours, or the class's
		// units (forecast.go). No review: it is never done.
		if f.SlotHours != "" {
			return forecastPlan(j)
		}
		p, err := v.driver.Plan(j)
		if err != nil {
			return nil, err
		}
		forecastUnits(p, f)
		return p, nil
	}
	p, err := v.driver.Plan(j)
	var nt *class.NeedsTable
	if errors.As(err, &nt) {
		// The job's units were stored with it at submit.
		doc, derr := q.expansionDoc(ctx, v.project, m, nt.ID)
		if derr != nil {
			return nil, errLoading{derr}
		}
		p, err = class.TablePlan(j, doc)
	}
	if err == nil {
		p, err = recomputeUnit(j, p)
	}
	if err == nil {
		p, err = class.WithReview(j, p)
	}
	if err != nil {
		return nil, err
	}
	// Expansions and appends replay in the order they were applied: by
	// where each began in the table. Of two that began at the same place,
	// one added nothing, and it came first. A lead's plan calls replay
	// after the expansions and appends each was applied after (plans.go).
	if err := q.loadPlanLog(ctx, v); err != nil {
		return nil, err
	}
	xs, as := m.expansions, m.appends
	appendNext := func() bool {
		return len(as) > 0 && (len(xs) == 0 || as[0].First < xs[0].First || as[0].First == xs[0].First && xs[0].Units > 0)
	}
	replayAppend1 := func() error {
		a := as[0]
		as = as[1:]
		if a.Error != "" {
			return nil
		}
		doc, err := q.expansionDoc(ctx, v.project, m, a.Doc)
		if err != nil {
			return errLoading{err}
		}
		return replayAppend(v.driver, j, p, a, doc)
	}
	replayExpansion1 := func() error {
		x := xs[0]
		xs = xs[1:]
		if x.Error != "" || x.Doc == "" {
			return nil
		}
		doc, err := q.expansionDoc(ctx, v.project, m, x.Doc)
		if err != nil {
			return errLoading{err}
		}
		if x.Unit < 0 || x.Unit >= len(p.Units) || x.First != len(p.Units) {
			return fmt.Errorf("expansion of unit %d does not fit the plan (it began at %d, the plan has %d units)", x.Unit, x.First, len(p.Units))
		}
		ex, err := class.Expand(v.driver, j, p.Units[x.Unit], doc, p.Names(), x.First)
		if err != nil {
			return fmt.Errorf("replaying the expansion of unit %d: %w", x.Unit, err)
		}
		if len(ex.Units) != x.Units {
			return fmt.Errorf("replaying the expansion of unit %d: %d units, recorded %d", x.Unit, len(ex.Units), x.Units)
		}
		p.Apply(x.Unit, ex)
		return nil
	}
	// replay replays expansions and appends until wantX and wantA of each
	// are, in their order.
	replay := func(wantX, wantA int) error {
		wantX, wantA = min(wantX, len(m.expansions)), min(wantA, len(m.appends))
		for {
			needX, needA := len(m.expansions)-len(xs) < wantX, len(m.appends)-len(as) < wantA
			var err error
			switch {
			case needA && (!needX || appendNext()):
				err = replayAppend1()
			case needX:
				err = replayExpansion1()
			default:
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	for _, x := range m.plans {
		if err := replay(x.Expansions, x.Appends); err != nil {
			return nil, err
		}
		if x.Error != "" {
			continue
		}
		doc, err := q.expansionDoc(ctx, v.project, m, x.Doc)
		if err != nil {
			return nil, errLoading{err}
		}
		if err := replayPlan(j, p, x, doc); err != nil {
			return nil, err
		}
	}
	if err := replay(len(m.expansions), len(m.appends)); err != nil {
		return nil, err
	}
	return p, nil
}

// expansionDoc returns a stored expansion document, cached by ID.
func (q *Queue) expansionDoc(ctx context.Context, project string, m *jobMem, id string) ([]byte, error) {
	if b, ok := m.docs[id]; ok {
		return b, nil
	}
	cid, err := c4.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("expansion document %q: %w", id, err)
	}
	if q.Stores == nil {
		return nil, fmt.Errorf("no store to read expansion document %s from", id)
	}
	st, err := q.Stores(ctx, project)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("project %s has no store", project)
	}
	r, err := st.Open(cid)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, class.MaxExpandDoc+1))
	if err != nil {
		return nil, err
	}
	if got := c4.Identify(bytes.NewReader(b)); got != cid {
		return nil, fmt.Errorf("expansion document %s read back as %s", id, got)
	}
	if m.docs == nil {
		m.docs = map[string][]byte{}
	}
	m.docs[id] = b
	return b, nil
}

// expand evaluates generator idx, whose pod p succeeded. It reports false
// when the document could not be read yet (the pod is kept and tried next
// tick), and refused when the document was refused: the generator then
// fails, and nothing is added.
func (q *Queue) expand(ctx context.Context, v *view, idx int, p *corev1.Pod, now time.Time) (ok, refused bool) {
	m := v.mem
	x := expansionRec{Unit: idx, First: len(m.plan.Units), At: now}
	if id := p.Annotations[c4v1.AnnotationOutputExpand]; id != "" {
		doc, err := q.expansionDoc(ctx, v.project, m, id)
		if err != nil {
			q.Log.Error(err, "expansion document", "job", v.key, "unit", idx)
			return false, false
		}
		return true, q.expandWith(ctx, v, idx, id, doc, now).Error != ""
	}
	x.Record = q.recordExpansion(ctx, v, x, now, leadOnlyError{})
	m.expansions = append(m.expansions, x)
	return true, false
}

// checkExpansion is what document doc would add under unit gen, or why it
// cannot.
func (q *Queue) checkExpansion(v *view, gen int, doc []byte) (*class.Expansion, error) {
	return q.checkUnits(v, gen, v.mem.plan.Units[gen], doc, false)
}

// checkUnits is what document doc would add under parent (unit gen of the
// plan, as it is evaluated), or why it cannot. Appended units (appends.go)
// hold up nothing that waited on their parent: they have no dependents.
func (q *Queue) checkUnits(v *view, gen int, parent class.Unit, doc []byte, appended bool) (*class.Expansion, error) {
	m := v.mem
	first := len(m.plan.Units)
	ex, err := class.Expand(v.driver, v.job, parent, doc, m.plan.Names(), first)
	if err == nil && appended {
		ex.Dependents = nil
	}
	if err == nil {
		err = q.leadExpands(v, ex)
	}
	if err == nil && first+len(ex.Units) > class.MaxUnits {
		err = fmt.Errorf("the expansion would make %d units, over %d", first+len(ex.Units), class.MaxUnits)
	}
	if err == nil && len(ex.Edges) > 0 {
		// Edges between units already in the job can close a cycle the
		// document alone cannot show.
		c := m.plan.Clone()
		c.Apply(gen, ex)
		if cyc := c.Cycle(); cyc != nil {
			err = fmt.Errorf("the expansion's edges would form a cycle: %v", cyc)
		}
	}
	return ex, err
}

// expandWith adds document doc (stored as id) under unit gen, signs and
// lists the expansion, and returns it; its Error is set if the document
// was refused, and then nothing was added.
func (q *Queue) expandWith(ctx context.Context, v *view, gen int, id string, doc []byte, now time.Time) expansionRec {
	m := v.mem
	x := expansionRec{Unit: gen, First: len(m.plan.Units), At: now, Doc: id}
	var lead leadOnlyError
	if ex, err := q.checkExpansion(v, gen, doc); err != nil {
		x.Error = err.Error()
		errors.As(err, &lead)
	} else {
		m.plan.Apply(gen, ex)
		x.Units = len(ex.Units)
	}
	x.Record = q.recordExpansion(ctx, v, x, now, lead)
	m.expansions = append(m.expansions, x)
	q.Log.Info("expanded", "job", v.key, "unit", gen, "units", x.Units, "document", x.Doc, "error", x.Error)
	return x
}

// recordExpansion signs an expansion: the generator, its document and
// what it added (decision/v1, kind expand). A refusal of a unit's agent
// work (lead) also names the agent units, the fix and the policy value.
func (q *Queue) recordExpansion(ctx context.Context, v *view, x expansionRec, now time.Time, lead leadOnlyError) string {
	if q.Signer == nil {
		return ""
	}
	gen := v.mem.plan.Units[x.Unit]
	pred := map[string]any{"decision": "expand", "job": v.key, "unit": x.Unit, "generator": gen.Name,
		"first": x.First, "units": x.Units, "policy": q.PolicyName, "at": now.UTC().Format(time.RFC3339)}
	subjects := []provenance.Subject{jobSubject(v.job)}
	if cid, err := c4.Parse(x.Doc); err == nil {
		pred["document"] = x.Doc
		subjects = append(subjects, provenance.SubjectFor("document", cid))
	}
	if x.Error != "" {
		pred["error"] = x.Error
	}
	if len(lead.units) > 0 {
		pred["agentUnits"], pred["fix"] = lead.units, c4v1.LeadOnlyFix
		pred["policy"], pred["scheduler"] = policy.Record(lead.use), q.PolicyName
		pred["submittedByUnit"] = v.job.Annotations[c4v1.AnnotationSubmittedByUnit]
	}
	rec, err := provenance.SignStatement(ctx, q.Signer, provenance.PredicateDecision, subjects, pred, now)
	if err == nil {
		var id string
		if id, err = q.storeRecord(ctx, v, rec); err == nil {
			return id
		}
	}
	q.Log.Error(err, "expansion record", "job", v.key, "unit", x.Unit)
	return ""
}

func expansionStatus(xs []expansionRec) []c4v1.WorkExpansion {
	var out []c4v1.WorkExpansion
	for _, x := range xs {
		out = append(out, c4v1.WorkExpansion{Unit: int32(x.Unit), Document: x.Doc, First: int32(x.First), Units: int32(x.Units),
			At: metav1.Time{Time: x.At.Truncate(time.Second)}, Record: x.Record, Error: x.Error})
	}
	return out
}

// done reports whether unit i no longer holds anything up: it succeeded,
// or failed and a person accepted it, or a review superseded it, or its
// lead withdrew it (plans.go).
func (m *jobMem) done(i int) bool {
	return m.succeeded[i] || m.accepted[i] || m.superseded[i] || m.withdrawnUnit(i)
}

// unitWaits sorts the job's unfinished units that are not running into
// runnable ones, in dispatch order, and blocked ones (waiting on a unit of
// their own job); stuck are the blocked ones that wait on a failed unit,
// directly or not.
func unitWaits(plan *class.Plan, m *jobMem, v *view) (runnable, blocked []int, stuck map[int]bool) {
	stuck = map[int]bool{}
	// A unit is stuck if any unit it waits on failed or is stuck. Edges
	// may point either way in index order, so this is a memoized walk.
	memo := map[int]int8{} // 1 stuck, 2 not
	var isStuck func(i int) bool
	isStuck = func(i int) bool {
		switch memo[i] {
		case 1:
			return true
		case 2:
			return false
		}
		memo[i] = 2 // the graph is acyclic; this only stops a bad table looping
		for _, a := range plan.Units[i].After {
			if a < 0 || a >= len(plan.Units) || !m.waitsOn(i, a) {
				continue
			}
			if m.failed[a] || isStuck(a) {
				memo[i] = 1
				return true
			}
		}
		return false
	}
	for _, idx := range plan.Order {
		if m.succeeded[idx] || m.failed[idx] || m.superseded[idx] || v.finishing[idx] {
			continue
		}
		if u := plan.Units[idx]; u.Withdrawn != nil || u.Held {
			continue // retired, or not released yet: neither runnable nor waiting (plans.go)
		}
		if _, ok := v.running[idx]; ok {
			continue
		}
		ready := true
		for _, a := range plan.Units[idx].After {
			if a >= 0 && a < len(plan.Units) && m.waitsOn(idx, a) {
				ready = false
				break
			}
		}
		if ready {
			runnable = append(runnable, idx)
			continue
		}
		blocked = append(blocked, idx)
		if isStuck(idx) {
			stuck[idx] = true
		}
	}
	return runnable, blocked, stuck
}

// criticalPath is the longest chain of remaining work through the unit
// graph, in seconds: each unfinished unit's predicted remaining duration
// (rem), plus, for a generator that has not run, its children's expected
// longest, and the longest chain of unfinished units it waits on.
func criticalPath(plan *class.Plan, m *jobMem, rem map[int]float64, genLongest func(key string) float64) float64 {
	memo := map[int]float64{}
	seen := map[int]bool{}
	var path func(i int) float64
	path = func(i int) float64 {
		if d, ok := memo[i]; ok {
			return d
		}
		if seen[i] {
			return 0
		}
		seen[i] = true
		u := plan.Units[i]
		d := rem[i]
		if u.Generator && !m.expanded(i) {
			d += genLongest(u.Key)
		}
		longest := 0.0
		for _, a := range u.After {
			if a >= 0 && a < len(plan.Units) && m.waitsOn(i, a) {
				longest = math.Max(longest, path(a))
			}
		}
		memo[i] = d + longest
		return memo[i]
	}
	t := 0.0
	for i := range rem {
		t = math.Max(t, path(i))
	}
	return t
}

// unitPriors sets the estimator's priors from units that carry their own
// (an expansion document's work): a key's first such unit sets the key's
// per-unit prior, a generator's its fan-out prior.
func unitPriors(e *estimate.Estimator, plan *class.Plan, hasPrior map[string]bool) {
	for _, u := range plan.Units {
		if u.Work <= 0 || hasPrior[u.Key] {
			continue
		}
		hasPrior[u.Key] = true
		sd := u.WorkSD
		if sd <= 0 {
			sd = u.Work / 2
		}
		if u.Generator {
			e.SetGenPrior(u.Key, estimate.GenPrior{Children: estimate.Prior{Mean: 1, SD: 1},
				Work: estimate.Prior{Mean: u.Work, SD: sd}, Longest: estimate.Prior{Mean: u.Work, SD: sd}})
			continue
		}
		slots := float64(max(1, u.Slots))
		e.SetPrior(u.Key, estimate.Prior{Mean: u.Work / slots, SD: sd / slots})
	}
}

// noteExpansions tells the estimator what the job's generators have done:
// how many children each added, and, once they have all finished, their
// total work and longest duration.
func noteExpansions(e *estimate.Estimator, plan *class.Plan, m *jobMem) {
	for _, x := range m.expansions {
		if x.Error != "" || x.Unit >= len(plan.Units) {
			continue
		}
		key := plan.Units[x.Unit].Key
		e.Expanded(key, x.Units)
		work, longest, all := 0.0, 0.0, true
		for i := x.First; i < x.First+x.Units && i < len(plan.Units); i++ {
			if r := plan.Units[i].Rounds; r != nil && r.Round > 1 {
				continue // the next round's generator: a round of its own
			}
			ms, ok := m.millis[i]
			if !m.succeeded[i] || !ok {
				all = false
				break
			}
			s := float64(ms) / 1000
			work += s * float64(max(1, plan.Units[i].Slots))
			longest = math.Max(longest, s)
		}
		if all && x.Units > 0 {
			e.ExpansionDone(key, work, longest)
		}
	}
}

// unitInputName is the Materialization through which a unit reads
// another unit's output.
func unitInputName(job string, from int) string { return fmt.Sprintf("%s-unit-u%d", job, from) }

// readers are the units some unit of the plan reads (fromUnit).
func readers(plan *class.Plan) map[int]bool {
	out := map[int]bool{}
	for _, u := range plan.Units {
		for _, in := range u.Inputs {
			out[in.From] = true
		}
	}
	return out
}

// ensureUnitInputs makes the Materializations a unit's fromUnit inputs
// mount, and reports whether they are all Ready.
func (q *Queue) ensureUnitInputs(ctx context.Context, v *view, idx int) (bool, error) {
	ready := true
	for _, in := range v.mem.plan.Units[idx].Inputs {
		view := v.mem.unitOut[in.From]
		if view == "" {
			return false, fmt.Errorf("unit %d reads unit %d, whose output view is not known", idx, in.From)
		}
		ok, err := q.ensureMaterialization(ctx, v.job, unitInputName(v.job.Name, in.From), view, "")
		if err != nil {
			return false, err
		}
		ready = ready && ok
	}
	return ready, nil
}

// PlanOf returns a copy of a job's unit table as the queue has it,
// expansions included, and the generation it was planned at; nil before
// the queue has planned the job.
func (q *Queue) PlanOf(key string) (*class.Plan, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, m := range q.jobs {
		if m.key == key && m.plan != nil {
			return m.plan.Clone(), m.planGen
		}
	}
	return nil, 0
}
