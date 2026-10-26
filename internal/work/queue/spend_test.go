package queue

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/tokens"
)

// usageJob is a job of n units that have run, every tenth procedural and
// the rest model units, each with one attempt's spend: a quarter dollar
// of priced tokens and a hundred more unpriced, an eighth of a dollar of
// slot time over half an hour, and the reasoning a worker observed.
func usageJob(t *testing.T, n int) *view {
	t.Helper()
	d, err := class.Lookup("graph")
	if err != nil {
		t.Fatal(err)
	}
	m := newMem()
	m.plan = &class.Plan{}
	m.spend = map[int]*unitSpend{}
	for i := range n {
		does := class.DoesModel
		if i%10 == 0 {
			does = class.DoesProcedure
		}
		m.plan.Units = append(m.plan.Units, class.Unit{Name: "u" + strconv.Itoa(i), Reasoning: class.Reasoning{Does: does}})
		m.succeeded[i] = true
		s := &unitSpend{slotCost: 0.125, slotHours: 0.5}
		if does == class.DoesModel {
			cost := 0.25
			s.add(modelUse{Model: "claude-opus-5-5", InputTokens: 12000, OutputTokens: 800, CacheReadTokens: 90000, CostUSD: &cost})
			s.add(modelUse{Model: "claude-haiku-4-5", InputTokens: 100})
			s.costSource = "metering-proxy"
			s.reasoning = &c4v1.WorkReasoning{Does: "model", Model: "claude-opus-5-5", Effort: "high", Tools: "edit",
				ToolNames: []string{"Read", "Edit", "Write", "Bash", "Grep", "Glob"}, MCPServers: []string{"c4"}}
		}
		m.spend[i] = s
	}
	return &view{key: "show-a/lead", mem: m, driver: d}
}

// wantSpend is usageJob(n)'s spend, counted by hand.
func wantSpend(t *testing.T, n int) (models, procs int) {
	t.Helper()
	for i := range n {
		if i%10 == 0 {
			procs++
		} else {
			models++
		}
	}
	return models, procs
}

func checkSpend(t *testing.T, when string, sp *c4v1.WorkSpend, n int) {
	t.Helper()
	mu, pu := wantSpend(t, n)
	est, _ := tokens.Estimate("claude-haiku-4-5", tokens.Counts{Input: 100})
	f := float64(mu)
	want := c4v1.WorkSpend{ModelUnits: int32(mu), ProceduralUnits: int32(pu), ModelTokens: int64(mu) * 102900, UnpricedTokens: int64(mu) * 100,
		InputTokens: int64(mu) * 12100, OutputTokens: int64(mu) * 800, CacheReadTokens: int64(mu) * 90000,
		TokenCost: money(f * 0.25), EstimatedTokenCost: money(f * est), ModelSlotCost: money(f * 0.125), ProceduralSlotCost: money(float64(pu) * 0.125),
		ModelShareBasis: "estimated", ModelSlotHours: hours(f * 0.5), ProceduralSlotHours: hours(float64(pu) * 0.5)}
	got := *sp
	got.WeightedTokens, got.UnpricedWeightedTokens, got.ModelShare, got.ModelWorkShare = 0, 0, "", ""
	if got != want {
		t.Fatalf("%s: spend\n%+v, want\n%+v", when, got, want)
	}
}

// A lead's job grows by appends, to thousands of model units. Status
// lists the newest model units' usage, in a bounded size, and folds the
// older ones into one sum: the job's totals are what every unit spent.
func TestModelUsageStaysSmall(t *testing.T) {
	const n = 3000
	v := usageJob(t, n)
	sp, usage, older := spendStatus(v)
	checkSpend(t, "folded", sp, n)
	b, err := json.Marshal(c4v1.WorkJobStatus{ModelUsage: usage, ModelUsageOlder: older})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > usageBudget+4<<10 {
		t.Fatalf("%d model units take %d KiB of status", n, len(b)>>10)
	}
	if len(usage) == 0 || len(usage) > maxUsage {
		t.Fatalf("%d units listed, want 1 to %d", len(usage), maxUsage)
	}
	if u, ok := usage["2999"]; !ok || u.Reasoning == nil || u.Cost != "0.2500" {
		t.Fatalf("the newest unit lost its usage: %+v", u)
	}
	if _, ok := usage["1"]; ok {
		t.Fatal("the oldest model unit is still listed")
	}
	if older == nil {
		t.Fatal("no fold of the older units")
	}
	folded := 0
	for i := range n {
		_, listed := usage[strconv.Itoa(i)]
		switch {
		case i%10 == 0:
			if listed {
				t.Fatalf("procedural unit %d is listed", i)
			}
		case i < int(older.Below):
			folded++
			if listed {
				t.Fatalf("unit %d is below the fold, %d, and listed", i, older.Below)
			}
		case !listed:
			t.Fatalf("unit %d is above the fold, %d, and not listed", i, older.Below)
		}
	}
	if int(older.Units) != folded || folded+len(usage) != int(sp.ModelUnits) {
		t.Fatalf("%d units folded, %d listed, of %d model units; the fold says %d", folded, len(usage), sp.ModelUnits, older.Units)
	}
	// The listed and the folded add up to the job's totals.
	in, cost := older.InputTokens, 0.0
	for _, u := range usage {
		in += u.InputTokens
		c, _ := strconv.ParseFloat(u.Cost, 64)
		cost += c
	}
	c, _ := strconv.ParseFloat(older.Cost, 64)
	if in != sp.InputTokens || money(cost+c) != sp.TokenCost {
		t.Fatalf("listed and folded: %d tokens and $%s, the job %d and $%s", in, money(cost+c), sp.InputTokens, sp.TokenCost)
	}
	if len(older.Models) != 2 || older.CostSource != "" || older.Reasoning != nil {
		t.Fatalf("fold %+v", older.WorkUnitUsage)
	}
	// Status says the same the next time it is written.
	sp2, usage2, older2 := spendStatus(v)
	if !reflect.DeepEqual(sp, sp2) || !reflect.DeepEqual(usage, usage2) || !reflect.DeepEqual(older, older2) {
		t.Fatal("a second write changed the status")
	}
	// Thin entries (one model, nothing observed) are held by the count:
	// the 500 newest model units are listed.
	v = usageJob(t, n)
	for _, s := range v.mem.spend {
		delete(s.models, "claude-haiku-4-5")
		s.reasoning, s.costSource = nil, ""
	}
	_, usage, older = spendStatus(v)
	if len(usage) != maxUsage || older == nil || older.Below != 2445 {
		t.Fatalf("thin entries: %d listed, the fold %+v", len(usage), older)
	}
}

// The queue restarts in the middle of a big job: it reloads the listed
// units and the fold from status and counts nothing twice, and what
// later attempts spend, a folded unit's too, is added once.
func TestModelUsageFoldSurvivesReload(t *testing.T) {
	const n = 3000
	v := usageJob(t, n)
	var st c4v1.WorkJobStatus
	st.Spend, st.ModelUsage, st.ModelUsageOlder = spendStatus(v)
	if st.ModelUsageOlder == nil {
		t.Fatal("no fold of the older units")
	}
	// Through JSON, as the API server keeps it.
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	reload := func(b []byte) *view {
		t.Helper()
		var st c4v1.WorkJobStatus
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		r := usageJob(t, n)
		r.mem.spend = nil
		r.mem.loadSpend(&st)
		return r
	}
	r := reload(b)
	sp, usage, older := spendStatus(r)
	checkSpend(t, "after a restart", sp, n)
	if !reflect.DeepEqual(usage, st.ModelUsage) || !reflect.DeepEqual(older, st.ModelUsageOlder) {
		t.Fatalf("after a restart the fold is %+v, was %+v", older, st.ModelUsageOlder)
	}
	// A folded unit runs again: its attempt's hour goes to the fold, and
	// it is not listed.
	q := &Queue{}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lead-u1", UID: types.UID("pod-u1")}}
	if !q.noteSpend(context.Background(), r, 1, pod, nil, 1) {
		t.Fatal("noteSpend")
	}
	sp, usage, older = spendStatus(r)
	if _, ok := usage["1"]; ok {
		t.Fatal("a folded unit's later attempt listed it again")
	}
	mu, _ := wantSpend(t, n)
	if sp.ModelSlotHours != hours(float64(mu)*0.5+1) || older.SlotHours != hours(float64(older.Units)*0.5+1) || older.Units != st.ModelUsageOlder.Units {
		t.Fatalf("a folded unit's hour: job %s, fold %s over %d units", sp.ModelSlotHours, older.SlotHours, older.Units)
	}
	// And a second restart, after it, still counts it once.
	st.Spend, st.ModelUsage, st.ModelUsageOlder = sp, usage, older
	if b, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	r2 := reload(b)
	sp2, usage2, older2 := spendStatus(r2)
	if !reflect.DeepEqual(sp, sp2) || !reflect.DeepEqual(usage, usage2) || !reflect.DeepEqual(older, older2) {
		t.Fatalf("a second restart changed the spend: %+v, was %+v", sp2, sp)
	}
}
