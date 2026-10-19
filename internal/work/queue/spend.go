package queue

import (
	"bytes"
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

// Spend, procedural and model (LLM-OR-PROCEDURE 6.2). Every unit is
// procedural or model (class.KindOf: the document's kind, else the
// class's; agent members are model units). Each ended attempt adds its
// slot time, priced at its pool's rate, to its unit; a model unit's
// attempt also adds the tokens it reported in .c4/usage.json (c4-unit
// usage; stored at capture as c4.avalanche.io/output-usage), per model,
// with a cost when whoever wrote it knows the price, and where the price
// came from:
//
//	{"models": [{"model": "...", "inputTokens": 120000, "outputTokens": 8000,
//	             "cacheReadTokens": 90000, "costUSD": 1.92}],
//	 "costSource": "metering-proxy"}
//
// (a single model's counts at the top level are read the same way).
// Status keeps the job's totals and model share of spend, tokens no price
// came with, and each model unit's use by model. A share rising on work
// that should be mechanical is what the HUD can flag.

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
	costSource string
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
	if id := p.Annotations[c4v1.AnnotationOutputUsage]; id != "" {
		b, err := q.expansionDoc(ctx, v.project, m, id)
		if err != nil {
			q.Log.Error(err, "usage document", "job", v.key, "pod", p.Name)
			return false
		}
		if err := json.NewDecoder(bytes.NewReader(b)).Decode(&doc); err != nil {
			q.Log.Info("usage document unreadable", "job", v.key, "pod", p.Name, "err", err.Error())
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
	s.add(doc.modelUse)
	for _, u := range doc.Models {
		s.add(u)
	}
	if doc.CostSource != "" {
		s.costSource = doc.CostSource
	}
	if m.spentPod == nil {
		m.spentPod = map[types.UID]bool{}
	}
	m.spentPod[p.UID] = true
	return true
}

// spendStatus is the job's spend and its model units' usage.
func spendStatus(v *view) (*c4v1.WorkSpend, map[string]c4v1.WorkUnitUsage) {
	m := v.mem
	if len(m.spend) == 0 && m.procBase == 0 {
		return nil, nil
	}
	isModel := func(i int) bool {
		return i < len(m.plan.Units) && v.driver != nil && class.KindOf(v.driver, m.plan.Units[i]) == class.KindModel
	}
	// The units of each kind that have run.
	mu, pu := 0, 0
	for i := range m.plan.Units {
		if !m.succeeded[i] && !m.failed[i] && m.attempts[i] == 0 && m.spend[i] == nil {
			continue
		}
		if isModel(i) {
			mu++
		} else {
			pu++
		}
	}
	proc := m.procBase
	var all tokens.Counts
	var raw, unpriced int64
	var weighted, unpricedW, tokenCost, estCost, modelSlot float64
	unknown := false
	usage := map[string]c4v1.WorkUnitUsage{}
	for idx, s := range m.spend {
		if !isModel(idx) {
			proc += s.slotCost
			continue
		}
		modelSlot += s.slotCost
		u := c4v1.WorkUnitUsage{SlotCost: money(s.slotCost), CostSource: s.costSource}
		var names []string
		for name := range s.models {
			names = append(names, name)
		}
		sort.Strings(names)
		cost, est, w, uw := 0.0, 0.0, 0.0, 0.0
		for _, name := range names {
			t := s.models[name]
			u.InputTokens += t.Input
			u.OutputTokens += t.Output
			u.CacheReadTokens += t.CacheRead
			u.CacheWriteTokens += t.CacheWrite
			u.UnpricedTokens += t.unpriced
			cost += t.cost
			est += t.estCost
			tw := tokens.Weighted(name, t.Counts)
			w += tw
			uw += t.unpricedW
			unknown = unknown || t.unknown
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
			all.Input += t.Input
			all.Output += t.Output
			all.CacheRead += t.CacheRead
			all.CacheWrite += t.CacheWrite
		}
		u.Cost = money(cost)
		if est > 0 {
			u.EstimatedCost = money(est)
		}
		u.WeightedTokens, u.UnpricedWeightedTokens = int64(math.Round(w)), int64(math.Round(uw))
		raw += u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
		unpriced += u.UnpricedTokens
		weighted += w
		unpricedW += uw
		tokenCost += cost
		estCost += est
		usage[strconv.Itoa(idx)] = u
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
		ModelShare: fmt.Sprintf("%.3f", share), ModelShareBasis: basis}
	if len(usage) == 0 {
		usage = nil
	}
	return out, usage
}

func money(x float64) string { return fmt.Sprintf("%.4f", x) }

// loadSpend restores what status recorded: each model unit's use, and
// the procedural units' slot cost as a total.
func (m *jobMem) loadSpend(st *c4v1.WorkJobStatus) {
	f := func(x string) float64 { v, _ := strconv.ParseFloat(x, 64); return v }
	if s := st.Spend; s != nil {
		m.procBase = f(s.ProceduralSlotCost)
	}
	for k, u := range st.ModelUsage {
		i, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		s := &unitSpend{slotCost: f(u.SlotCost), costSource: u.CostSource, models: map[string]*tokenUse{}}
		for _, mt := range u.Models {
			s.models[mt.Model] = &tokenUse{Counts: tokens.Counts{Input: mt.InputTokens, Output: mt.OutputTokens, CacheRead: mt.CacheReadTokens,
				CacheWrite: mt.CacheWriteTokens}, unpriced: mt.UnpricedTokens, unpricedW: float64(mt.UnpricedWeightedTokens),
				cost: f(mt.Cost), estCost: f(mt.EstimatedCost), unknown: mt.Unpriceable}
		}
		if m.spend == nil {
			m.spend = map[int]*unitSpend{}
		}
		m.spend[i] = s
	}
}

// Model work a procedure could do (LLM-OR-PROCEDURE 6.1: "an LLM unit
// should never be asked to produce a fact a procedural unit can
// compute"). A light report, not a decision: a model unit whose same
// computation (process and input views) ran at least minRepeats times and
// wrote the same bytes every time behaved as a pure function of its
// inputs, which a procedure could compute, cheaper and checkably. It is
// listed with the evidence; reworking the job is a person's call.
const minRepeats = 3

// candidatesKept bounds status.procedural.
const candidatesKept = 16

func (q *Queue) proceduralCandidates(ctx context.Context, v *view) []c4v1.WorkProceduralCandidate {
	b := q.books[v.project]
	m := v.mem
	if b == nil || b.Len() == 0 || v.driver == nil || m.plan == nil {
		return nil
	}
	var inID string
	resolved := false
	var out []c4v1.WorkProceduralCandidate
	for i, u := range m.plan.Units {
		if !m.succeeded[i] || class.KindOf(v.driver, u) != class.KindModel {
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
