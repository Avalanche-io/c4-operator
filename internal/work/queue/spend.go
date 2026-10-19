package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/process"
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
	Model           string   `json:"model"`
	InputTokens     int64    `json:"inputTokens"`
	OutputTokens    int64    `json:"outputTokens"`
	CacheReadTokens int64    `json:"cacheReadTokens"`
	CostUSD         *float64 `json:"costUSD,omitempty"`
}

type usageDoc struct {
	modelUse
	Models     []modelUse `json:"models"`
	CostSource string     `json:"costSource"`
}

func (m modelUse) tokens() int64 { return m.InputTokens + m.OutputTokens + m.CacheReadTokens }

// tokenUse is one model's tokens in a unit, and what the priced ones cost.
type tokenUse struct {
	in, out, cache, unpriced int64
	cost                     float64
}

// unitSpend is one unit's spend over its attempts.
type unitSpend struct {
	models     map[string]*tokenUse
	slotCost   float64
	costSource string
}

func (s *unitSpend) add(u modelUse) {
	if u.tokens() == 0 && u.CostUSD == nil {
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
	t.in += u.InputTokens
	t.out += u.OutputTokens
	t.cache += u.CacheReadTokens
	if u.CostUSD != nil {
		t.cost += *u.CostUSD
	} else {
		t.unpriced += u.tokens()
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
	var tokens, unpriced int64
	var tokenCost, modelSlot float64
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
		cost := 0.0
		for _, name := range names {
			t := s.models[name]
			u.InputTokens += t.in
			u.OutputTokens += t.out
			u.CacheReadTokens += t.cache
			u.UnpricedTokens += t.unpriced
			cost += t.cost
			mt := c4v1.WorkModelTokens{Model: name, InputTokens: t.in, OutputTokens: t.out, CacheReadTokens: t.cache, UnpricedTokens: t.unpriced}
			if t.cost > 0 || t.unpriced == 0 {
				mt.Cost = money(t.cost)
			}
			u.Models = append(u.Models, mt)
		}
		u.Cost = money(cost)
		tokens += u.InputTokens + u.OutputTokens + u.CacheReadTokens
		unpriced += u.UnpricedTokens
		tokenCost += cost
		usage[strconv.Itoa(idx)] = u
	}
	total := tokenCost + modelSlot + proc
	share := 0.0
	if total > 0 {
		share = (tokenCost + modelSlot) / total
	}
	out := &c4v1.WorkSpend{ModelUnits: int32(mu), ProceduralUnits: int32(pu), ModelTokens: tokens, UnpricedTokens: unpriced,
		TokenCost: money(tokenCost), ModelSlotCost: money(modelSlot), ProceduralSlotCost: money(proc), ModelShare: fmt.Sprintf("%.3f", share)}
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
			s.models[mt.Model] = &tokenUse{in: mt.InputTokens, out: mt.OutputTokens, cache: mt.CacheReadTokens,
				unpriced: mt.UnpricedTokens, cost: f(mt.Cost)}
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
