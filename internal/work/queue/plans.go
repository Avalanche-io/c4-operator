package queue

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// A lead's plan (DESIGN 10.6, A lead's plan; LEAD.md stage 3). A lead
// edits its job's plan with c4_plan: one call's edits (add, split, merge,
// order, withdraw, release, edge) are one plan document, the expansion
// document a generator writes extended with a lead's edits
// (class/planedit.go). The HUD stores it, signs the request (action plan)
// and lists it in c4.avalanche.io/plan; the queue applies each request
// once, in token order, by the path an append takes: the document is
// checked as one against the table as it is now (names, edges, the
// lead-only rule, the unit bound, cycles, and that each unit it retires,
// orders or gives an edge has not started), and is applied whole or not
// at all. Its units go at the end of the table under the root, held until
// released; nothing is renumbered. Each call's outcome is signed
// (decision/v1, kind plan). Status keeps the latest calls and the store
// keeps every one: the log of every call applied is a document in the
// project's store, written before the edits are made, and status.plan.log
// names it, so a restart replays the calls among the job's expansions and
// appends in the order they were applied, and applies no key twice.
//
// A held unit is not runnable and is no work to ration; the units after
// it wait for it as for any unit. A withdrawn unit counts as done for the
// units after it, as a superseded one does, and never runs. Among the
// job's ready units the queue releases its lead's order first (the lower
// key), then the units without one, and the longer chain of work after a
// unit breaks a tie. A lead's job whose units left are all held, or wait
// on held ones, is idle: nothing to place until its lead releases them.
// Once it is closed they never run, and count as done.

// planRecent is how many calls status lists.
const planRecent = 16

// planWithdrawnShown is how many withdrawn units status describes.
const planWithdrawnShown = 256

// planLogKind marks the stored log of a job's plan calls.
const planLogKind = "c4.avalanche.io/plan-log"

// planRec is one call applied (c4v1.WorkPlanEdit).
type planRec struct {
	Key, Doc, Control, Record, Error string
	First, Units                     int
	// Expansions and Appends are how many of each the job had applied
	// before it.
	Expansions, Appends int
	At                  time.Time
}

// planLog is the stored log of a job's plan calls.
type planLog struct {
	Kind    string              `json:"kind"`
	Version int                 `json:"version"`
	Job     string              `json:"job"`
	Edits   []c4v1.WorkPlanEdit `json:"edits"`
}

// planned says whether a call with key was applied, made or refused.
func (m *jobMem) planned(key string) bool {
	for _, p := range m.plans {
		if p.Key == key {
			return true
		}
	}
	return false
}

// withdrawnUnit says whether unit i was withdrawn.
func (m *jobMem) withdrawnUnit(i int) bool {
	return m.plan != nil && i >= 0 && i < len(m.plan.Units) && m.plan.Units[i].Withdrawn != nil
}

// planRequests are a job's plan requests, in token order.
func planRequests(j *c4v1.WorkJob) []c4v1.WorkPlanRequest {
	raw := j.Annotations[c4v1.AnnotationPlan]
	if raw == "" {
		return nil
	}
	var reqs []c4v1.WorkPlanRequest
	if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
		return nil
	}
	sort.SliceStable(reqs, func(x, y int) bool { return tokenValue(reqs[x].Token) < tokenValue(reqs[y].Token) })
	return reqs
}

// applyPlans applies the job's plan requests after the last it carried
// out (ctl: the job with only the requests the queue trusts), in token
// order. A document or a log the store does not give back or keep yet
// stops the pass, so calls apply in order; it is tried next tick.
func (q *Queue) applyPlans(ctx context.Context, v *view, ctl *c4v1.WorkJob, now time.Time) {
	m := v.mem
	if m.planLoad != "" {
		return // the log of the calls before is not read yet (planJob)
	}
	last := tokenValue(m.planToken)
	for _, r := range planRequests(ctl) {
		if tokenValue(r.Token) <= last {
			continue
		}
		if r.Key != "" && !m.planned(r.Key) {
			var doc []byte
			if _, err := c4.Parse(r.Document); err == nil {
				b, err := q.expansionDoc(ctx, v.project, m, r.Document)
				if err != nil {
					q.Log.Error(err, "plan document", "job", v.key, "key", r.Key)
					return
				}
				doc = b
			}
			if !q.planWith(ctx, v, r, doc, now) {
				return
			}
		}
		m.planToken, last = r.Token, tokenValue(r.Token)
	}
}

// planWith makes request r's edits (doc, nil if its document is no C4
// ID), or refuses them all, and signs and logs the outcome. It reports
// false when the log could not be stored: then nothing was made, and the
// request is tried again.
func (q *Queue) planWith(ctx context.Context, v *view, r c4v1.WorkPlanRequest, doc []byte, now time.Time) bool {
	m := v.mem
	x := planRec{Key: r.Key, Doc: r.Document, Control: r.Record, First: len(m.plan.Units),
		Expansions: len(m.expansions), Appends: len(m.appends), At: now}
	var lead leadOnlyError
	var e *class.PlanEdits
	var err error
	switch phase := v.job.Status.Phase; {
	case doc == nil:
		err = fmt.Errorf("the document %q is not a C4 ID", r.Document)
	case leadSession(v.job) == "" || v.driver.Name() != "graph":
		err = fmt.Errorf("%s is not a lead's job: only a lead plans its own job (c4_plan)", v.job.Name)
	case m.lead.closed != nil:
		err = fmt.Errorf("%s is closed (%s): a closed lead's job takes no more edits; make a new job for them", v.job.Name, m.lead.closed.Why)
	case phase == "Succeeded" || phase == "Failed":
		err = fmt.Errorf("%s has finished (%s): a finished job takes no more edits", v.job.Name, strings.ToLower(phase))
	default:
		e, err = q.checkPlan(v, r.Key, doc)
	}
	if err != nil {
		e = nil
		x.Error = err.Error()
		if errors.As(err, &lead) {
			x.Error = fmt.Sprintf("%s (%s would run agents, and this job was submitted by a unit): %s",
				c4v1.LeadOnlyMessage, strings.Join(lead.units, ", "), c4v1.LeadOnlyFix)
		}
	} else {
		x.Units = len(e.Units)
	}
	x.Record = q.recordPlan(ctx, v, x, e, now, lead)
	// The log first: an edit the store does not keep is not made.
	plans := append(slices.Clone(m.plans), x)
	id, err := q.writePlanLog(ctx, v, plans)
	if err != nil {
		q.Log.Error(err, "plan log", "job", v.key, "key", x.Key)
		return false
	}
	if e != nil {
		m.plan.ApplyPlan(appendRoot, e)
		// a lead's job is not idle while its lead plans it (lead.go)
		m.lead.idleSince = time.Time{}
	}
	m.plans, m.planLog = plans, id
	q.Log.Info("planned", "job", v.key, "key", x.Key, "units", x.Units, "document", x.Doc, "error", x.Error)
	return true
}

// checkPlan is what plan document doc of the call with key would do to
// the job's table, or why it cannot: the expansion's checks, and the
// lead's edits against the units as they are now.
func (q *Queue) checkPlan(v *view, key string, doc []byte) (*class.PlanEdits, error) {
	m := v.mem
	e, err := class.ReadPlan(v.job, appendParent(m.plan), doc, m.plan, key, v.started)
	if err != nil {
		return nil, err
	}
	if err := q.leadExpands(v, e.Expansion); err != nil {
		return nil, err
	}
	if n := len(m.plan.Units) + len(e.Units); n > class.MaxUnits {
		return nil, fmt.Errorf("the plan would make %d units, over %d", n, class.MaxUnits)
	}
	c := m.plan.Clone()
	c.ApplyPlan(appendRoot, e)
	if cyc := c.Cycle(); cyc != nil {
		return nil, fmt.Errorf("the plan's edges would form a cycle: %v", cyc)
	}
	return e, nil
}

// started says why unit i of the job can no longer be changed by its
// lead's plan, or "" when it has not started (class.Started).
func (v *view) started(i int) string {
	m := v.mem
	_, running := v.running[i]
	switch {
	case m.superseded[i]:
		return "was superseded"
	case running || v.finishing[i] || m.attempts[i] > 0 && !m.succeeded[i] && !m.failed[i]:
		return "is running"
	case m.succeeded[i]:
		return "has succeeded"
	case m.accepted[i]:
		return "failed, and the failure was accepted"
	case m.openNeeds(i):
		return "stopped on a question"
	case m.failed[i]:
		return "has failed"
	case v.openWaits[i]:
		return "is waiting for its decision"
	}
	return ""
}

// writePlanLog stores the log of the job's calls, plans, and returns its
// C4 ID ("" with no store to keep it in).
func (q *Queue) writePlanLog(ctx context.Context, v *view, plans []planRec) (string, error) {
	if q.Stores == nil {
		return "", nil
	}
	st, err := q.Stores(ctx, v.project)
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", fmt.Errorf("project %s has no store", v.project)
	}
	b, err := json.Marshal(planLog{Kind: planLogKind, Version: 1, Job: v.key, Edits: planEdits(plans)})
	if err != nil {
		return "", err
	}
	id, err := st.PutBytes(ctx, b)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// loadPlanLog reads the stored log status named, once, before the job's
// table is replayed.
func (q *Queue) loadPlanLog(ctx context.Context, v *view) error {
	m := v.mem
	if m.planLoad == "" {
		return nil
	}
	b, err := q.expansionDoc(ctx, v.project, m, m.planLoad)
	if err != nil {
		return errLoading{err}
	}
	delete(m.docs, m.planLoad) // read once: the next is another document
	var l planLog
	if err := json.Unmarshal(b, &l); err != nil || l.Kind != planLogKind {
		return fmt.Errorf("the plan log %s is not a log of plan calls: %v", m.planLoad, err)
	}
	m.plans = nil
	for _, x := range l.Edits {
		m.plans = append(m.plans, planOf(x))
	}
	m.planLog, m.planLoad = m.planLoad, ""
	return nil
}

// recordPlan signs a call's outcome: its key and control record, its
// document, what it added, withdrew, released and ordered, or why it was
// refused (decision/v1, kind plan).
func (q *Queue) recordPlan(ctx context.Context, v *view, x planRec, e *class.PlanEdits, now time.Time, lead leadOnlyError) string {
	if q.Signer == nil {
		return ""
	}
	pred := map[string]any{"decision": "plan", "job": v.key, "key": x.Key, "parent": appendRoot,
		"first": x.First, "units": x.Units, "policy": q.PolicyName, "at": now.UTC().Format(time.RFC3339)}
	subjects := []provenance.Subject{jobSubject(v.job)}
	if cid, err := c4.Parse(x.Doc); err == nil {
		pred["document"] = x.Doc
		subjects = append(subjects, provenance.SubjectFor("document", cid))
	}
	if x.Control != "" {
		pred["control"] = x.Control
	}
	if x.Error != "" {
		pred["error"] = x.Error
	}
	if e != nil {
		var ws []int
		for i := range e.Withdraw {
			ws = append(ws, i)
		}
		sort.Ints(ws)
		if len(ws) > 0 {
			pred["withdrawn"] = order.EncodeSet(ws)
		}
		if len(e.Release) > 0 {
			pred["released"] = order.EncodeSet(slices.Sorted(slices.Values(e.Release)))
		}
		if e.Ordered {
			pred["order"] = e.Order
		}
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
	q.Log.Error(err, "plan record", "job", v.key, "key", x.Key)
	return ""
}

// replayPlan makes a recorded call's edits to plan p again, from its
// document.
func replayPlan(j *c4v1.WorkJob, p *class.Plan, x planRec, doc []byte) error {
	if len(p.Units) == 0 || x.First != len(p.Units) {
		return fmt.Errorf("plan call %s does not fit the plan (it began at %d, the plan has %d units)", x.Key, x.First, len(p.Units))
	}
	e, err := class.ReadPlan(j, appendParent(p), doc, p, x.Key, nil)
	if err != nil {
		return fmt.Errorf("replaying plan call %s: %w", x.Key, err)
	}
	if len(e.Units) != x.Units {
		return fmt.Errorf("replaying plan call %s: %d units, recorded %d", x.Key, len(e.Units), x.Units)
	}
	p.ApplyPlan(appendRoot, e)
	return nil
}

func planEdits(ps []planRec) []c4v1.WorkPlanEdit {
	out := make([]c4v1.WorkPlanEdit, 0, len(ps))
	for _, x := range ps {
		out = append(out, c4v1.WorkPlanEdit{Key: x.Key, Document: x.Doc, First: int32(x.First), Units: int32(x.Units),
			At: metav1.Time{Time: x.At.UTC().Truncate(time.Second)}, Expansions: int32(x.Expansions), Appends: int32(x.Appends),
			Control: x.Control, Record: x.Record, Error: x.Error})
	}
	return out
}

func planOf(x c4v1.WorkPlanEdit) planRec {
	return planRec{Key: x.Key, Doc: x.Document, First: int(x.First), Units: int(x.Units), At: x.At.Time,
		Expansions: int(x.Expansions), Appends: int(x.Appends), Control: x.Control, Record: x.Record, Error: x.Error}
}

// planStatus is status.plan: the latest calls, the log of all of them,
// the units withdrawn (the newest) and the order keys.
func planStatus(m *jobMem) *c4v1.WorkPlan {
	if len(m.plans) == 0 {
		return nil
	}
	st := &c4v1.WorkPlan{Edits: int32(len(m.plans)), Token: m.planToken, Log: m.planLog,
		Recent: planEdits(m.plans[max(0, len(m.plans)-planRecent):])}
	var keys []string
	shown := 0
	for i := len(m.plan.Units) - 1; i >= 0; i-- {
		w := m.plan.Units[i].Withdrawn
		if w == nil || shown >= planWithdrawnShown {
			continue
		}
		if st.Withdrawn == nil {
			st.Withdrawn = map[string]c4v1.WorkWithdrawnUnit{}
		}
		st.Withdrawn[strconv.Itoa(i)] = c4v1.WorkWithdrawnUnit{Why: w.Why, Replacements: order.EncodeSet(w.By), Edit: w.Edit}
		shown++
	}
	for i, u := range m.plan.Units {
		if u.OrderKey > 0 {
			keys = append(keys, strconv.Itoa(i)+":"+strconv.Itoa(u.OrderKey))
		}
	}
	st.Order = strings.Join(keys, ",")
	return st
}

func (m *jobMem) loadPlans(st *c4v1.WorkJobStatus) {
	p := st.Plan
	if p == nil {
		return
	}
	m.planToken, m.planLog = p.Token, p.Log
	if p.Log != "" {
		m.planLoad = p.Log
		return
	}
	// No store kept a log: status is all there is.
	for _, x := range p.Recent {
		m.plans = append(m.plans, planOf(x))
	}
}

// parkedUnits counts the units its lead holds and those that wait on
// them, directly or not: they run once it releases them.
func parkedUnits(plan *class.Plan, m *jobMem, held, blocked []int) int {
	if len(held) == 0 {
		return 0
	}
	memo := map[int]int8{} // 1 parked, 2 not
	var behind func(i int) bool
	behind = func(i int) bool {
		switch memo[i] {
		case 1:
			return true
		case 2:
			return false
		}
		memo[i] = 2
		if plan.Units[i].Held {
			memo[i] = 1
			return true
		}
		for _, a := range plan.Units[i].After {
			if a >= 0 && a < len(plan.Units) && m.waitsOn(i, a) && behind(a) {
				memo[i] = 1
				return true
			}
		}
		return false
	}
	n := len(held)
	for _, i := range blocked {
		if behind(i) {
			n++
		}
	}
	return n
}

// orderRunnable puts the job's ready units in its lead's order: the lower
// order key first, then the units without one, and among units of one key
// the one with the longer chain of remaining work after it (the critical
// path). A job none of whose ready units its lead ordered keeps its
// dispatch order.
func orderRunnable(plan *class.Plan, v *view, rem map[int]float64) {
	if !slices.ContainsFunc(v.runnable, func(i int) bool { return plan.Units[i].OrderKey > 0 }) {
		return
	}
	tail := tailPaths(plan, v.mem, rem)
	key := func(i int) int {
		if k := plan.Units[i].OrderKey; k > 0 {
			return k
		}
		return math.MaxInt
	}
	slices.SortStableFunc(v.runnable, func(a, b int) int {
		return cmp.Or(cmp.Compare(key(a), key(b)), cmp.Compare(tail[b], tail[a]))
	})
}

// tailPaths is, for each unit, its remaining work (rem) and the longest
// chain of remaining work of the units that wait on it, in seconds.
func tailPaths(plan *class.Plan, m *jobMem, rem map[int]float64) map[int]float64 {
	next := map[int][]int{}
	for k, u := range plan.Units {
		for _, a := range u.After {
			if a >= 0 && a < len(plan.Units) && m.waitsOn(k, a) {
				next[a] = append(next[a], k)
			}
		}
	}
	memo := map[int]float64{}
	seen := map[int]bool{}
	var tail func(i int) float64
	tail = func(i int) float64 {
		if d, ok := memo[i]; ok {
			return d
		}
		if seen[i] {
			return 0
		}
		seen[i] = true
		longest := 0.0
		for _, k := range next[i] {
			longest = math.Max(longest, tail(k))
		}
		memo[i] = rem[i] + longest
		return memo[i]
	}
	for i := range plan.Units {
		tail(i)
	}
	return memo
}
