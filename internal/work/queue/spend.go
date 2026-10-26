package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/process"
	"github.com/Avalanche-io/c4-operator/work/tokens"
)

// Spend, procedural and model (DESIGN.md 10.6, Procedural and model units).
// Every unit is a procedure's or a model's (class.DoesOf: the document's
// reasoning.does, else the class's; agent members are model units). Each
// ended attempt adds its slot time, priced at its pool's rate, to its unit;
// a model unit's attempt also adds the tokens it reported in .c4/usage.json
// (c4-unit usage; stored at capture as c4.avalanche.io/output-usage), per
// model, with a cost when whoever wrote it knows the price, and where the
// price came from:
//
//	{"models": [{"model": "...", "inputTokens": 120000, "outputTokens": 8000,
//	             "cacheReadTokens": 90000, "costUSD": 1.92}],
//	 "costSource": "metering-proxy"}
//
// (a single model's counts at the top level are read the same way).
// Status keeps the job's totals and model share of spend, tokens no price
// came with, and each of its newest model units' use by model, the older
// ones' summed (foldUsage). A share rising on work that should be
// mechanical is what the HUD can flag.

type modelUse struct {
	Model            string   `json:"model"`
	InputTokens      int64    `json:"inputTokens"`
	OutputTokens     int64    `json:"outputTokens"`
	CacheReadTokens  int64    `json:"cacheReadTokens"`
	CacheWriteTokens int64    `json:"cacheWriteTokens"`
	CostUSD          *float64 `json:"costUSD,omitempty"`
}

type usageDoc struct {
	modelUse
	Models     []modelUse `json:"models"`
	CostSource string     `json:"costSource"`
	// Reasoning is what the attempt reasoned with, as observed.
	Reasoning *c4v1.WorkReasoning `json:"reasoning"`
}

func (m modelUse) counts() tokens.Counts {
	return tokens.Counts{Input: m.InputTokens, Output: m.OutputTokens, CacheRead: m.CacheReadTokens, CacheWrite: m.CacheWriteTokens}
}

// tokenUse is one model's tokens in a unit by class; what the priced ones
// cost; and the unpriced ones raw, weighted (work/tokens), and at the
// model's published rates when the model is known (unknown says one is
// not).
type tokenUse struct {
	tokens.Counts
	cost               float64
	unpriced           int64
	unpricedW, estCost float64
	unknown            bool
}

// unitSpend is one unit's spend over its attempts.
type unitSpend struct {
	models     map[string]*tokenUse
	slotCost   float64
	slotHours  float64
	costSource string
	reasoning  *c4v1.WorkReasoning // the latest attempt's, as observed
	usageErr   string              // why an attempt's usage document was refused
}

func (s *unitSpend) add(u modelUse) {
	c := u.counts()
	if c.Raw() == 0 && u.CostUSD == nil {
		return
	}
	if s.models == nil {
		s.models = map[string]*tokenUse{}
	}
	t := s.models[u.Model]
	if t == nil {
		t = &tokenUse{}
		s.models[u.Model] = t
	}
	t.Input += c.Input
	t.Output += c.Output
	t.CacheRead += c.CacheRead
	t.CacheWrite += c.CacheWrite
	if u.CostUSD != nil {
		t.cost += *u.CostUSD
		return
	}
	t.unpriced += c.Raw()
	t.unpricedW += tokens.Weighted(u.Model, c)
	if est, ok := tokens.Estimate(u.Model, c); ok {
		t.estCost += est
	} else {
		t.unknown = true
	}
}

// noteSpend adds pod p's attempt, which ran for hours, to unit idx's
// spend, once per pod. It reports false when the pod's usage could not be
// read yet.
func (q *Queue) noteSpend(ctx context.Context, v *view, idx int, p *corev1.Pod, pools map[string]*pool, hours float64) bool {
	m := v.mem
	if m.spentPod[p.UID] {
		return true
	}
	var doc usageDoc
	usageErr := ""
	if id := p.Annotations[c4v1.AnnotationOutputUsage]; id != "" {
		b, err := q.expansionDoc(ctx, v.project, m, id)
		if err != nil {
			q.Log.Error(err, "usage document", "job", v.key, "pod", p.Name)
			return false
		}
		if err := class.Strict(b, &doc); err != nil {
			// Its tokens are not counted: status says so, not just a log.
			q.Log.Error(err, "usage document refused", "job", v.key, "pod", p.Name)
			doc, usageErr = usageDoc{}, err.Error()
		}
	}
	if m.spend == nil {
		m.spend = map[int]*unitSpend{}
	}
	s := m.spend[idx]
	if s == nil {
		s = &unitSpend{}
		m.spend[idx] = s
	}
	if pl := pools[p.Labels[c4v1.LabelPool]]; pl != nil {
		rate := pl.cost
		if pl.spec.Spec.Burst != nil {
			rate = pl.burstCost
		}
		slots := 1
		if idx < len(m.plan.Units) {
			slots = max(1, m.plan.Units[idx].Slots)
		}
		s.slotCost += hours * float64(slots) * rate
	}
	slots := 1
	if idx < len(m.plan.Units) {
		slots = max(1, m.plan.Units[idx].Slots)
	}
	s.slotHours += hours * float64(slots)
	s.add(doc.modelUse)
	for _, u := range doc.Models {
		s.add(u)
	}
	if doc.CostSource != "" {
		s.costSource = doc.CostSource
	}
	if doc.Reasoning != nil {
		s.reasoning = doc.Reasoning
	}
	if usageErr != "" {
		s.usageErr = usageErr
	}
	if m.spentPod == nil {
		m.spentPod = map[types.UID]bool{}
	}
	m.spentPod[p.UID] = true
	return true
}

// A job's status keeps each model unit's usage for at most maxUsage
// units and usageBudget bytes of JSON in all, the newest units (the
// highest indices: a lead's latest workers) first, and folds every older
// unit into one sum (status.modelUsageOlder), so a job of thousands of
// model units stays a fraction of what etcd holds and its totals never
// change by a unit being folded. An entry is about 300 bytes with one
// model and no observed reasoning, more with several models or a long
// tool list: 500 thin entries are 150 KiB, and the budget holds a job of
// fat ones.
const (
	maxUsage    = 500
	usageBudget = 256 << 10
)

// doesModel is whether unit i does model work, as its spend counts it.
func doesModel(v *view, i int) bool {
	m := v.mem
	return i < len(m.plan.Units) && v.driver != nil && class.DoesOf(v.driver, m.plan.Units[i]) == class.DoesModel
}

// unitUse is a spend's status entry and what it adds to the job's
// totals.
type unitUse struct {
	u                c4v1.WorkUnitUsage
	cost, est, w, uw float64
	unknown          bool
}

func (s *unitSpend) use() unitUse {
	x := unitUse{u: c4v1.WorkUnitUsage{SlotCost: money(s.slotCost), SlotHours: hours(s.slotHours), CostSource: s.costSource, Reasoning: s.reasoning, UsageError: s.usageErr}}
	u := &x.u
	var names []string
	for name := range s.models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := s.models[name]
		u.InputTokens += t.Input
		u.OutputTokens += t.Output
		u.CacheReadTokens += t.CacheRead
		u.CacheWriteTokens += t.CacheWrite
		u.UnpricedTokens += t.unpriced
		x.cost += t.cost
		x.est += t.estCost
		tw := tokens.Weighted(name, t.Counts)
		x.w += tw
		x.uw += t.unpricedW
		x.unknown = x.unknown || t.unknown
		mt := c4v1.WorkModelTokens{Model: name, InputTokens: t.Input, OutputTokens: t.Output, CacheReadTokens: t.CacheRead,
			CacheWriteTokens: t.CacheWrite, WeightedTokens: int64(math.Round(tw)), UnpricedTokens: t.unpriced,
			UnpricedWeightedTokens: int64(math.Round(t.unpricedW)), Unpriceable: t.unknown}
		if t.cost > 0 || t.unpriced == 0 {
			mt.Cost = money(t.cost)
		}
		if t.estCost > 0 {
			mt.EstimatedCost = money(t.estCost)
		}
		u.Models = append(u.Models, mt)
	}
	u.Cost = money(x.cost)
	if x.est > 0 {
		u.EstimatedCost = money(x.est)
	}
	u.WeightedTokens, u.UnpricedWeightedTokens = int64(math.Round(x.w)), int64(math.Round(x.uw))
	return x
}

// fold adds o's spend to s, model by model.
func (s *unitSpend) fold(o *unitSpend) {
	s.slotCost += o.slotCost
	s.slotHours += o.slotHours
	for name, t := range o.models {
		if s.models == nil {
			s.models = map[string]*tokenUse{}
		}
		f := s.models[name]
		if f == nil {
			f = &tokenUse{}
			s.models[name] = f
		}
		f.Input += t.Input
		f.Output += t.Output
		f.CacheRead += t.CacheRead
		f.CacheWrite += t.CacheWrite
		f.cost += t.cost
		f.unpriced += t.unpriced
		f.unpricedW += t.unpricedW
		f.estCost += t.estCost
		f.unknown = f.unknown || t.unknown
	}
	if o.usageErr != "" {
		s.usageErr = o.usageErr
	}
}

// foldUsage folds model units out of m.spend into m.older: every one
// below m.olderBelow, and, newest first, every one past the count or the
// budget, which moves olderBelow up past them. A unit is folded for good:
// a later attempt of a folded unit (a requeue, or a unit that ran long,
// such as a lead) is folded in as it is written, so what memory holds is
// what status says, and a reload counts nothing twice. It returns the
// entries of the units kept.
func foldUsage(v *view) map[int]unitUse {
	m := v.mem
	var idx []int
	for i := range m.spend {
		if doesModel(v, i) {
			idx = append(idx, i)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(idx)))
	kept := make(map[int]unitUse, min(len(idx), maxUsage))
	left := usageBudget
	for n, i := range idx {
		if i >= m.olderBelow && n < maxUsage {
			x := m.spend[i].use()
			b, _ := json.Marshal(x.u)
			size := len(b) + len(strconv.Itoa(i)) + 4 // its key, quoted, a colon and a comma
			if size <= left {
				left -= size
				kept[i] = x
				continue
			}
		}
		// This unit and every older one are folded.
		if m.older == nil {
			m.older = &unitSpend{}
		}
		for _, j := range idx[n:] {
			m.older.fold(m.spend[j])
			delete(m.spend, j)
		}
		m.olderBelow = max(m.olderBelow, i+1)
		break
	}
	return kept
}

// spendStatus is the job's spend, its newest model units' usage, and the
// fold of the older ones.
func spendStatus(v *view) (*c4v1.WorkSpend, map[string]c4v1.WorkUnitUsage, *c4v1.WorkUsageFold) {
	m := v.mem
	if len(m.spend) == 0 && m.older == nil && m.procBase == 0 && m.procHours == 0 {
		return nil, nil, nil
	}
	kept := foldUsage(v)
	// The units of each kind that have run, and the model units of them
	// folded.
	mu, pu, folded := 0, 0, 0
	for i := range m.plan.Units {
		if !m.succeeded[i] && !m.failed[i] && m.attempts[i] == 0 && m.spend[i] == nil {
			continue
		}
		switch {
		case !doesModel(v, i):
			pu++
		case i < m.olderBelow:
			mu++
			folded++
		default:
			mu++
		}
	}
	proc, procHours, modelHours := m.procBase, m.procHours, 0.0
	var all tokens.Counts
	var raw, unpriced int64
	var weighted, unpricedW, tokenCost, estCost, modelSlot float64
	unknown := false
	add := func(s *unitSpend, x unitUse) {
		u := x.u
		modelSlot += s.slotCost
		modelHours += s.slotHours
		all.Input += u.InputTokens
		all.Output += u.OutputTokens
		all.CacheRead += u.CacheReadTokens
		all.CacheWrite += u.CacheWriteTokens
		raw += u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
		unpriced += u.UnpricedTokens
		weighted += x.w
		unpricedW += x.uw
		tokenCost += x.cost
		estCost += x.est
		unknown = unknown || x.unknown
	}
	usage := map[string]c4v1.WorkUnitUsage{}
	for idx, s := range m.spend {
		if !doesModel(v, idx) {
			proc += s.slotCost
			procHours += s.slotHours
			continue
		}
		x := kept[idx]
		add(s, x)
		usage[strconv.Itoa(idx)] = x.u
	}
	var older *c4v1.WorkUsageFold
	if m.older != nil {
		x := m.older.use()
		add(m.older, x)
		x.u.CostSource, x.u.Reasoning = "", nil
		older = &c4v1.WorkUsageFold{Below: int32(m.olderBelow), Units: int32(folded), WorkUnitUsage: x.u}
	}
	// The model share counts reported costs, and for tokens no price came
	// with, their estimate at the model's published rates; a model it
	// cannot price leaves the share partial.
	basis := "priced"
	switch {
	case unknown:
		basis = "partial"
	case unpriced > 0:
		basis = "estimated"
	}
	total := tokenCost + estCost + modelSlot + proc
	share := 0.0
	if total > 0 {
		share = (tokenCost + estCost + modelSlot) / total
	}
	out := &c4v1.WorkSpend{ModelUnits: int32(mu), ProceduralUnits: int32(pu), ModelTokens: raw, UnpricedTokens: unpriced,
		InputTokens: all.Input, OutputTokens: all.Output, CacheReadTokens: all.CacheRead, CacheWriteTokens: all.CacheWrite,
		WeightedTokens: int64(math.Round(weighted)), UnpricedWeightedTokens: int64(math.Round(unpricedW)),
		TokenCost: money(tokenCost), EstimatedTokenCost: money(estCost), ModelSlotCost: money(modelSlot), ProceduralSlotCost: money(proc),
		ModelShare: fmt.Sprintf("%.3f", share), ModelShareBasis: basis,
		ModelSlotHours: hours(modelHours), ProceduralSlotHours: hours(procHours)}
	if t := modelHours + procHours; t > 0 {
		out.ModelWorkShare = fmt.Sprintf("%.3f", modelHours/t)
	}
	if len(usage) == 0 {
		usage = nil
	}
	return out, usage, older
}

func money(x float64) string { return fmt.Sprintf("%.4f", x) }

func hours(x float64) string { return fmt.Sprintf("%.4f", x) }

// loadSpend restores what status recorded: each listed model unit's use,
// the fold of the older ones, and the procedural units' slot cost as a
// total.
func (m *jobMem) loadSpend(st *c4v1.WorkJobStatus) {
	f := func(x string) float64 { v, _ := strconv.ParseFloat(x, 64); return v }
	if s := st.Spend; s != nil {
		m.procBase, m.procHours = f(s.ProceduralSlotCost), f(s.ProceduralSlotHours)
	}
	spendOf := func(u c4v1.WorkUnitUsage) *unitSpend {
		s := &unitSpend{slotCost: f(u.SlotCost), slotHours: f(u.SlotHours), costSource: u.CostSource, reasoning: u.Reasoning, usageErr: u.UsageError, models: map[string]*tokenUse{}}
		for _, mt := range u.Models {
			s.models[mt.Model] = &tokenUse{Counts: tokens.Counts{Input: mt.InputTokens, Output: mt.OutputTokens, CacheRead: mt.CacheReadTokens,
				CacheWrite: mt.CacheWriteTokens}, unpriced: mt.UnpricedTokens, unpricedW: float64(mt.UnpricedWeightedTokens),
				cost: f(mt.Cost), estCost: f(mt.EstimatedCost), unknown: mt.Unpriceable}
		}
		return s
	}
	for k, u := range st.ModelUsage {
		i, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		if m.spend == nil {
			m.spend = map[int]*unitSpend{}
		}
		m.spend[i] = spendOf(u)
	}
	if o := st.ModelUsageOlder; o != nil {
		m.older, m.olderBelow = spendOf(o.WorkUnitUsage), int(o.Below)
	}
}

// Model work a procedure could do (DESIGN.md 10.6): a model unit should
// never be asked to produce a fact a procedural unit can compute. A light
// report, not a decision: a model unit whose same computation (process and
// input views) ran at least minRepeats times and
// wrote the same bytes every time behaved as a pure function of its
// inputs, which a procedure could compute, cheaper and checkably. It is
// listed with the evidence; reworking the job is a person's call.
const minRepeats = 3

// candidatesKept bounds status.procedural.
const candidatesKept = 16

func (q *Queue) proceduralCandidates(ctx context.Context, v *view) []c4v1.WorkProceduralCandidate {
	b := q.bookOf(v.project)
	m := v.mem
	if b == nil || b.Len() == 0 || v.driver == nil || m.plan == nil {
		return nil
	}
	var inID string
	resolved := false
	var out []c4v1.WorkProceduralCandidate
	for i, u := range m.plan.Units {
		if !m.succeeded[i] || class.DoesOf(v.driver, u) != class.DoesModel {
			continue
		}
		proc, ok := process.ProcessID(unitContainers(v.job, u, q.digests), q.lastPlatform[v.key])
		if !ok {
			continue
		}
		if !resolved {
			resolved = true
			inputs, ok := q.inputViews(ctx, v.job)
			if !ok {
				return nil
			}
			if inID, ok = process.InputsID(inputs); !ok {
				return nil
			}
		}
		runs, outputs := b.Repeats(process.Key{Process: proc, Inputs: inID})
		if runs < minRepeats || outputs != 1 {
			continue
		}
		out = append(out, c4v1.WorkProceduralCandidate{Unit: int32(i), Name: unitName(m.plan, i), Runs: int32(runs),
			Why: fmt.Sprintf("run %d times on the same inputs, it wrote the same bytes every time: a procedure may do this work", runs)})
		if len(out) == candidatesKept {
			break
		}
	}
	return out
}
