package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

// The types below are the HUD API contract (internal/hud/API.md).

type viewer struct {
	User     string   `json:"user"`
	Projects []string `json:"projects"`
	All      bool     `json:"allProjects"`
}

func (v *viewer) sees(p string) bool { return contains(v.Projects, p) }

var viewers = map[string]*viewer{
	"wrangler": {User: "dana", Projects: []string{"harbor", "kestrel"}, All: true},
	"artist":   {User: "maya", Projects: []string{"harbor"}, All: false},
}

type apiSnapshot struct {
	Now      time.Time    `json:"now"`
	Viewer   *viewer      `json:"viewer"`
	Farm     apiFarm      `json:"farm"`
	Pools    []apiPool    `json:"pools"`
	Classes  []apiClass   `json:"classes"`
	Projects []apiProject `json:"projects"`
	Jobs     []apiJob     `json:"jobs"`
	Nodes    []apiNode    `json:"nodes"`
	Tiers    apiTiers     `json:"tiers"`
	Capacity []apiCap     `json:"capacity"`
}

type apiFarm struct {
	SlotsUsed          int     `json:"slotsUsed"`
	SlotsTotal         int     `json:"slotsTotal"`
	QueuedUnits        int     `json:"queuedUnits"`
	UnitsPerHour       int     `json:"unitsPerHour"`
	FailuresLastHour   int     `json:"failuresLastHour"`
	BurstSlots         int     `json:"burstSlots"`
	BurstCostPerHour   float64 `json:"burstCostPerHour"`
	BytesMaterialized  float64 `json:"bytesMaterialized"`
	BytesFetchedStore  float64 `json:"bytesFetchedFromStore"`
	BytesAvoided       float64 `json:"bytesAvoided"`
	OtherProjectsSlots int     `json:"otherProjectsSlots"`
}

type apiPool struct {
	Name            string         `json:"name"`
	Attrs           map[string]any `json:"attrs"`
	Slots           int            `json:"slots"`
	Used            int            `json:"used"`
	CostPerSlotHour float64        `json:"costPerSlotHour"`
	Burst           *apiPoolBurst  `json:"burst,omitempty"`
}

type apiPoolBurst struct {
	MaxSlots            int     `json:"maxSlots"`
	ProvisioningSeconds float64 `json:"provisioningSeconds"`
	CostPerSlotHour     float64 `json:"costPerSlotHour"`
	Requested           int     `json:"requested"`
	Provisioned         int     `json:"provisioned"`
}

type apiClass struct {
	Name   string         `json:"name"`
	Driver string         `json:"driver"`
	SLO    map[string]any `json:"slo"`
	Lens   string         `json:"lens"`
}

type apiProject struct {
	Name              string           `json:"name"`
	Quotas            []map[string]any `json:"quotas"`
	BurstSpendToday   float64          `json:"burstSpendToday"`
	BurstBudgetPerDay float64          `json:"burstBudgetPerDay"`
	BytesAvoided      float64          `json:"bytesAvoided"`
	WorkerHours       []map[string]any `json:"workerHours"`
	GPUHours          float64          `json:"gpuHours"`
}

type apiCounts struct {
	Total       int `json:"total"`
	Unevaluated int `json:"unevaluated"`
	Queued      int `json:"queued"`
	Blocked     int `json:"blocked"`
	Running     int `json:"running"`
	Retrying    int `json:"retrying"`
	Succeeded   int `json:"succeeded"`
	Failed      int `json:"failed"`
	Held        int `json:"held"`
}

type apiStates struct {
	Unevaluated string `json:"unevaluated"`
	Queued      string `json:"queued"`
	Blocked     string `json:"blocked"`
	Running     string `json:"running"`
	Retrying    string `json:"retrying"`
	Succeeded   string `json:"succeeded"`
	Failed      string `json:"failed"`
}

type apiObjective struct {
	Type                 string     `json:"type"`
	Deadline             *time.Time `json:"deadline,omitempty"`
	LatencyTargetSeconds float64    `json:"latencyTargetSeconds,omitempty"`
	From                 string     `json:"from,omitempty"`
}

type apiEstimate struct {
	RemainingSeconds    float64     `json:"remainingSeconds"`
	P90RemainingSeconds float64     `json:"p90RemainingSeconds"`
	EtaP50              time.Time   `json:"etaP50"`
	EtaP90              time.Time   `json:"etaP90"`
	RequiredSlots       float64     `json:"requiredSlots"`
	AttainableSlots     float64     `json:"attainableSlots"`
	RunningSlots        int         `json:"runningSlots"`
	SlackSeconds        float64     `json:"slackSeconds"`
	Feasible            bool        `json:"feasible"`
	Message             string      `json:"message,omitempty"`
	Burst               *BurstOffer `json:"burst,omitempty"`
}

type apiInput struct {
	Materialization string `json:"materialization,omitempty"`
	View            string `json:"view,omitempty"`
	FromJob         string `json:"fromJob,omitempty"`
}

type apiJob struct {
	Namespace    string       `json:"namespace"`
	Name         string       `json:"name"`
	Project      string       `json:"project"`
	Class        string       `json:"class"`
	Driver       string       `json:"driver"`
	Owner        string       `json:"owner,omitempty"`
	Phase        string       `json:"phase"`
	Order        string       `json:"order"`
	Units        apiCounts    `json:"units"`
	UnitStates   apiStates    `json:"unitStates"`
	Objective    apiObjective `json:"objective"`
	Estimate     apiEstimate  `json:"estimate"`
	Rank         int          `json:"rank"`
	Pools        []string     `json:"pools"`
	Inputs       []apiInput   `json:"inputs"`
	Output       *apiOutput   `json:"output,omitempty"`
	DependsOn    []string     `json:"dependsOn"`
	CreatedAt    time.Time    `json:"createdAt"`
	Nudge        apiNudge     `json:"nudge"`
	HeavyRegions []apiHeavy   `json:"heavyRegions,omitempty"`
	Blacklist    []string     `json:"blacklist,omitempty"`
}

type apiOutput struct {
	View       string `json:"view"`
	Changesets int    `json:"changesets"`
}

type apiNudge struct {
	Hold  bool    `json:"hold"`
	Boost float64 `json:"boost"`
}

type apiHeavy struct {
	From   int     `json:"from"`
	To     int     `json:"to"`
	Factor float64 `json:"factor"`
	Label  string  `json:"label,omitempty"`
}

type apiNode struct {
	Name             string       `json:"name"`
	Pool             string       `json:"pool"`
	Locality         string       `json:"locality"`
	State            string       `json:"state"`
	Slots            int          `json:"slots"`
	Used             int          `json:"used"`
	FailuresLastHour int          `json:"failuresLastHour"`
	Suspect          bool         `json:"suspect"`
	CacheBytes       float64      `json:"cacheBytes"`
	CacheHitRate     float64      `json:"cacheHitRate"`
	Running          []apiRunning `json:"running,omitempty"`
}

type apiRunning struct {
	Job  string `json:"job"`
	Unit int    `json:"unit"`
}

type apiTiers struct {
	Tiers      []apiTier      `json:"tiers"`
	FanIn      map[string]int `json:"fanIn"`
	PublishP50 float64        `json:"publishP50"`
	PublishP95 float64        `json:"publishP95"`
	CaptureP50 float64        `json:"captureP50"`
}

type apiTier struct {
	Name             string  `json:"name"`
	HitRate          float64 `json:"hitRate"`
	RequestsIn       int     `json:"requestsIn"`
	UpstreamRequests int     `json:"upstreamRequests"`
	BytesServed      float64 `json:"bytesServed"`
	ShedRate         float64 `json:"shedRate"`
	InflightFills    int     `json:"inflightFills"`
}

type apiCap struct {
	Pool         string    `json:"pool"`
	Slots        int       `json:"slots"`
	Until        time.Time `json:"until"`
	Reason       string    `json:"reason"`
	Jobs         []string  `json:"jobs"`
	EstCost      float64   `json:"estCost"`
	State        string    `json:"state"`
	PrewarmViews []string  `json:"prewarmViews"`
}

func (s *Sim) ts(sec float64) time.Time { return s.at(math.Round(sec)) }

func (s *Sim) visible(v *viewer, j *Job) bool {
	return v.sees(j.Project) && s.t >= j.Created
}

func (s *Sim) snapshot(v *viewer) *apiSnapshot {
	out := &apiSnapshot{Now: s.ts(s.t), Viewer: v}
	f := &out.Farm
	for _, n := range s.nodes {
		if n.ready(s.t) || (n.Provisioned && n.Disabled) {
			f.SlotsTotal += n.Slots
		}
		for _, r := range n.running {
			f.SlotsUsed += r.u.Slots
			if !v.sees(r.j.Project) {
				f.OtherProjectsSlots += r.u.Slots
			}
		}
	}
	for _, j := range s.jobs {
		if s.t < j.Created {
			continue
		}
		for _, u := range j.Units {
			if u != nil && u.Node == "" && (u.State == stQueued || u.State == stRetrying || u.State == stUnevaluated) {
				f.QueuedUnits++
			}
		}
	}
	window := math.Min(3600, math.Max(s.t, 600))
	c := 0
	for _, d := range s.done {
		if s.t-d <= window {
			c++
		}
	}
	f.UnitsPerHour = int(float64(c) * 3600 / window)
	for _, fr := range s.failures {
		if s.t-fr.at <= 3600 {
			f.FailuresLastHour++
		}
	}
	for _, p := range s.pools {
		if p.Burst != nil {
			f.BurstSlots += p.Burst.Provisioned
			f.BurstCostPerHour += float64(p.Burst.Provisioned) * p.Burst.Cost
		}
	}
	for _, p := range s.projects {
		f.BytesMaterialized += p.BytesMat
		f.BytesFetchedStore += p.BytesStore
	}
	f.BytesAvoided = f.BytesMaterialized - f.BytesFetchedStore

	for _, p := range s.pools {
		ap := apiPool{Name: p.Name, CostPerSlotHour: p.Cost, Attrs: map[string]any{"class": p.Class, "capacityType": p.CapType}}
		if p.GPUModel != "" {
			ap.Attrs["gpuModel"], ap.Attrs["vram"] = p.GPUModel, p.VRAM
		}
		for _, n := range s.nodes {
			if n.Pool != p.Name {
				continue
			}
			if n.ready(s.t) || (n.Provisioned && n.Disabled) {
				ap.Slots += n.Slots
			}
			ap.Used += n.used()
		}
		if p.Burst != nil {
			ap.Burst = &apiPoolBurst{MaxSlots: p.Burst.MaxSlots, ProvisioningSeconds: p.Burst.ProvSeconds, CostPerSlotHour: p.Burst.Cost,
				Requested: p.Burst.Requested, Provisioned: p.Burst.Provisioned}
		}
		out.Pools = append(out.Pools, ap)
	}
	for _, c := range s.classes {
		slo := map[string]any{"type": c.SLOType}
		if c.P50 > 0 {
			slo["p50Seconds"], slo["p95Seconds"] = c.P50, c.P95
		}
		if c.Turn > 0 {
			slo["turnaroundSeconds"] = c.Turn
		}
		out.Classes = append(out.Classes, apiClass{Name: c.Name, Driver: c.Driver, SLO: slo, Lens: c.Lens})
	}
	for _, p := range s.projects {
		if !v.sees(p.Name) {
			continue
		}
		ap := apiProject{Name: p.Name, BurstSpendToday: round2(p.BurstSpent), BurstBudgetPerDay: p.BurstBudget,
			BytesAvoided: p.BytesMat - p.BytesStore, GPUHours: round1(p.GPUHours)}
		for _, pool := range s.pools {
			q := p.Quotas[pool.Name]
			if q[0] == 0 && q[1] == 0 {
				continue
			}
			ap.Quotas = append(ap.Quotas, map[string]any{"pool": pool.Name, "nominal": q[0], "borrowingLimit": q[1], "used": s.usedIn(p.Name, pool.Name)})
		}
		keys := make([][2]string, 0, len(p.WorkerHours))
		for k := range p.WorkerHours {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool { return keys[a][0]+keys[a][1] < keys[b][0]+keys[b][1] })
		for _, k := range keys {
			ap.WorkerHours = append(ap.WorkerHours, map[string]any{"pool": k[0], "class": k[1], "hours": round1(p.WorkerHours[k])})
		}
		out.Projects = append(out.Projects, ap)
	}
	for i, j := range s.ranked() {
		if !s.visible(v, j) {
			continue
		}
		aj := s.jobSummary(j)
		aj.Rank = i + 1
		out.Jobs = append(out.Jobs, aj)
	}
	// Ranks are positions in the farm-wide dispatch order; renumber within
	// the viewer's scope so a scoped viewer sees 1..n.
	for i := range out.Jobs {
		out.Jobs[i].Rank = i + 1
	}
	for _, n := range s.nodes {
		out.Nodes = append(out.Nodes, s.nodeSummary(n, v))
	}
	out.Tiers = s.tiers()
	for _, c := range s.capReqs {
		if !v.sees(c.Project) {
			continue
		}
		out.Capacity = append(out.Capacity, apiCap{Pool: c.Pool, Slots: c.Slots, Until: s.ts(c.Until), Reason: c.Reason,
			Jobs: c.Jobs, EstCost: c.EstCost, State: c.State, PrewarmViews: c.Prewarm})
	}
	if out.Capacity == nil {
		out.Capacity = []apiCap{}
	}
	if out.Jobs == nil {
		out.Jobs = []apiJob{}
	}
	return out
}

func (s *Sim) nodeSummary(n *Node, v *viewer) apiNode {
	an := apiNode{Name: n.Name, Pool: n.Pool, Locality: n.Locality, Slots: n.Slots, Used: n.used(),
		CacheBytes: math.Round(n.CacheBytes), CacheHitRate: round2(n.CacheHit*1000) / 1000}
	an.CacheHitRate = math.Round(n.CacheHit*1000) / 1000
	switch {
	case !n.Provisioned:
		an.State = "unprovisioned"
	case n.Disabled:
		an.State = "disabled"
	case s.t < n.WarmUntil:
		an.State = "warming"
	case an.Used > 0:
		an.State = "busy"
	default:
		an.State = "idle"
	}
	projects := map[string]bool{}
	for _, f := range s.failures {
		if f.node == n.Name && s.t-f.at <= 3600 {
			an.FailuresLastHour++
			projects[f.job] = true
		}
	}
	an.Suspect = an.FailuresLastHour >= 3 && len(projects) >= 2
	for _, r := range n.running {
		if v.sees(r.j.Project) {
			an.Running = append(an.Running, apiRunning{Job: r.j.key(), Unit: r.u.I})
		}
	}
	return an
}

func (s *Sim) phase(j *Job) string {
	switch {
	case j.Done:
		return "Succeeded"
	case j.FailedJob:
		return "Failed"
	case j.Hold:
		return "Held"
	case !j.ready:
		return "Blocked"
	case j.ML != nil && !j.ML.Running:
		return "Yielded"
	}
	if s.running(j) > 0 {
		return "Running"
	}
	return "Queued"
}

func (s *Sim) unitState(j *Job, u *Unit) string {
	if u.State == stQueued && j.Hold {
		return "held"
	}
	return u.State
}

func (s *Sim) jobSummary(j *Job) apiJob {
	aj := apiJob{
		Namespace: j.NS, Name: j.Name, Project: j.Project, Class: j.Class, Driver: j.Driver, Owner: j.Owner,
		Phase: s.phase(j), Order: j.Order, Pools: j.Pools, DependsOn: j.DependsOn, CreatedAt: s.ts(j.Created),
		Nudge: apiNudge{Hold: j.Hold, Boost: j.Boost}, Blacklist: j.Blacklist,
	}
	if aj.DependsOn == nil {
		aj.DependsOn = []string{}
	}
	sets := map[string][]int{}
	for _, u := range j.Units {
		if u == nil {
			continue
		}
		aj.Units.Total++
		st := u.State
		switch st {
		case stUnevaluated:
			aj.Units.Unevaluated++
		case stQueued:
			if j.Hold {
				aj.Units.Held++
			} else {
				aj.Units.Queued++
			}
		case stBlocked:
			aj.Units.Blocked++
		case stRunning:
			aj.Units.Running++
		case stRetrying:
			aj.Units.Retrying++
		case stSucceeded:
			aj.Units.Succeeded++
		case stFailed:
			aj.Units.Failed++
		}
		sets[st] = append(sets[st], u.I)
	}
	aj.UnitStates = apiStates{Unevaluated: rle(sets[stUnevaluated]), Queued: rle(sets[stQueued]), Blocked: rle(sets[stBlocked]),
		Running: rle(sets[stRunning]), Retrying: rle(sets[stRetrying]), Succeeded: rle(sets[stSucceeded]), Failed: rle(sets[stFailed])}
	aj.Objective = s.objective(j)
	e := j.est
	aj.Estimate = apiEstimate{
		RemainingSeconds: math.Round(math.Max(0, e.EtaP50-s.t)), P90RemainingSeconds: math.Round(math.Max(0, e.EtaP90-s.t)),
		EtaP50: s.ts(e.EtaP50), EtaP90: s.ts(e.EtaP90), RequiredSlots: round1(e.Required), AttainableSlots: round1(e.Attainable),
		RunningSlots: s.running(j), SlackSeconds: math.Round(e.Slack), Feasible: e.Feasible, Message: e.Message, Burst: e.Burst,
	}
	for _, in := range j.Inputs {
		ai := apiInput{Materialization: in.Materialization, View: in.View, FromJob: in.FromJob}
		if in.FromJob != "" && ai.View == "" {
			if up := s.jobBy[in.FromJob]; up != nil && up.Done {
				ai.View = up.OutputView
			}
		}
		aj.Inputs = append(aj.Inputs, ai)
	}
	if aj.Inputs == nil {
		aj.Inputs = []apiInput{}
	}
	if j.Changesets > 0 {
		aj.Output = &apiOutput{View: j.OutputView, Changesets: j.Changesets}
	}
	for _, h := range e.HeavyFlagged {
		aj.HeavyRegions = append(aj.HeavyRegions, apiHeavy{From: j.AxisStart + h.From*j.AxisStep, To: j.AxisStart + h.To*j.AxisStep, Factor: h.Factor, Label: h.Label})
	}
	return aj
}

func (s *Sim) objective(j *Job) apiObjective {
	o := apiObjective{Type: "deadline"}
	if j.LatencyTarget > 0 {
		o.Type = "latency"
		o.LatencyTargetSeconds = j.LatencyTarget
	}
	if j.hasEff {
		d := s.ts(j.eff)
		o.Deadline = &d
	}
	if j.hasEff && (!j.HasDeadline || j.eff < j.Deadline-1) {
		// Inherited from the dependent whose deadline binds.
		best, bestV := "", math.Inf(1)
		for _, d := range s.jobs {
			if !contains(d.DependsOn, j.key()) || !d.hasEff {
				continue
			}
			if v := d.eff - d.duration90(); v < bestV {
				best, bestV = d.key(), v
			}
		}
		o.From = best
	}
	return o
}

// jobDetail is GET /api/v1/jobs/{ns}/{name}.
func (s *Sim) jobDetail(j *Job, v *viewer) map[string]any {
	sum := s.jobSummary(j)
	out := map[string]any{}
	// Flatten the summary into the detail.
	out["namespace"], out["name"], out["project"], out["class"], out["driver"] = sum.Namespace, sum.Name, sum.Project, sum.Class, sum.Driver
	if sum.Owner != "" {
		out["owner"] = sum.Owner
	}
	out["phase"], out["order"], out["units"], out["unitStates"] = sum.Phase, sum.Order, sum.Units, sum.UnitStates
	out["objective"], out["estimate"], out["pools"], out["inputs"] = sum.Objective, sum.Estimate, sum.Pools, sum.Inputs
	if sum.Output != nil {
		out["output"] = sum.Output
	}
	out["dependsOn"], out["createdAt"], out["nudge"] = sum.DependsOn, sum.CreatedAt, sum.Nudge
	if sum.HeavyRegions != nil {
		out["heavyRegions"] = sum.HeavyRegions
	}
	if sum.Blacklist != nil {
		out["blacklist"] = sum.Blacklist
	}
	for i, r := range s.ranked() {
		if r == j {
			out["rank"] = i + 1
		}
	}

	axis := map[string]any{"kind": j.AxisKind}
	if j.AxisKind == "frame" {
		axis["start"], axis["step"] = j.AxisStart, j.AxisStep
	}
	if j.AxisLabels != nil {
		axis["labels"] = j.AxisLabels
	}
	out["axis"] = axis
	byGroup := map[string][]int{}
	var risk []int
	total := 0
	for _, u := range j.Units {
		if u == nil {
			continue
		}
		total++
		byGroup[u.Group] = append(byGroup[u.Group], u.I)
		if u.Risk {
			risk = append(risk, u.I)
		}
	}
	var groups []map[string]any
	for _, g := range j.Groups {
		gm := map[string]any{"key": g.Key, "label": g.Label, "units": rle(byGroup[g.Key])}
		if g.Generator {
			gm["generator"] = true
			gm["spans"] = g.Spans
		}
		groups = append(groups, gm)
	}
	out["groups"] = groups
	out["riskUnits"] = rle(risk)
	if total <= 5000 {
		var units []map[string]any
		for _, u := range j.Units {
			if u == nil {
				continue
			}
			um := map[string]any{"i": u.I, "label": u.Label, "state": s.unitState(j, u), "attempts": len(u.Attempts), "group": u.Group, "pos": u.Pos}
			if u.Node != "" {
				um["node"] = u.Node
			}
			if u.Risk {
				um["risk"] = true
			}
			units = append(units, um)
		}
		out["units"] = units
	}
	progress := map[string]any{}
	for _, u := range j.Units {
		if u == nil || u.Node == "" {
			continue
		}
		el := s.t - u.StartAt
		var fr float64
		measured := j.Measured
		switch {
		case j.ML != nil:
			fr = j.ML.step(s.t) / float64(j.ML.TotalSteps)
		case measured:
			fr = el / u.Cost
		default:
			fr = el / math.Max(u.Est, 1)
		}
		progress[strconv.Itoa(u.I)] = map[string]any{"fraction": round2(math.Min(0.99, math.Max(0, fr))), "measured": measured}
	}
	out["progress"] = progress
	if j.AxisKind == "frame" && len(j.Passes) > 0 {
		out["costByFrame"] = s.costByFrame(j)
	}
	if len(j.Expansions) > 0 {
		var ex []map[string]any
		for _, e := range j.Expansions {
			ex = append(ex, map[string]any{"generator": e.Gen, "children": rle(append([]int(nil), e.Children...)), "at": s.ts(e.At)})
		}
		out["expansions"] = ex
	}
	if total <= 5000 {
		var edges []map[string]int
		for _, u := range j.Units {
			if u == nil {
				continue
			}
			for _, d := range u.Deps {
				edges = append(edges, map[string]int{"from": d, "to": u.I})
			}
		}
		if edges != nil {
			out["edges"] = edges
		}
	}
	switch j.Driver {
	case "ci":
		cs := s.ciSummary(v)
		out["classSummary"] = cs
	case "ml":
		m := j.ML
		var cks []map[string]any
		for _, c := range m.Checkpoints {
			cks = append(cks, map[string]any{"view": c.View, "step": c.Step, "at": s.ts(c.At), "bytesNew": math.Round(c.BytesNew), "bytesDeduped": math.Round(c.BytesDedup)})
		}
		var ys []map[string]any
		for _, y := range m.Yields {
			ym := map[string]any{"at": s.ts(y.At), "reason": y.Reason}
			if y.Resumed {
				ym["resumedAt"] = s.ts(y.ResumedAt)
			}
			ys = append(ys, ym)
		}
		if ys == nil {
			ys = []map[string]any{}
		}
		out["classSummary"] = map[string]any{"wrapped": m.Wrapped, "ranks": m.Ranks, "checkpoints": cks, "yields": ys,
			"usefulGpuHours": round2(m.Useful), "lostGpuHours": round2(m.Lost), "datasetView": m.DatasetView, "codeView": m.CodeView,
			"step": int(m.step(s.t)), "totalSteps": m.TotalSteps}
	default:
		out["classSummary"] = map[string]any{"passes": j.Passes, "fps": j.Fps}
	}
	return out
}

func (s *Sim) costByFrame(j *Job) []map[string]any {
	g := j.Passes[len(j.Passes)-1]
	var us []*Unit
	for _, u := range j.Units {
		if u != nil && u.Group == g {
			us = append(us, u)
		}
	}
	sort.Slice(us, func(a, b int) bool { return us[a].Pos < us[b].Pos })
	// Frames whose generator has not expanded yet have no unit in the pass;
	// they get the pass median with a wide band.
	n := 0
	for _, u := range j.Units {
		if u != nil && u.Pos+1 > n {
			n = u.Pos + 1
		}
	}
	if len(us) < n {
		var ds []float64
		for _, u := range us {
			ds = append(ds, u.Est)
		}
		med := median(ds)
		if med == 0 {
			med = j.prior[g]
		}
		have := map[int]bool{}
		for _, u := range us {
			have[u.Pos] = true
		}
		for p := 0; p < n; p++ {
			if !have[p] {
				us = append(us, &Unit{Pos: p, Est: med, EstErr: med * 0.5, State: stUnevaluated})
			}
		}
		sort.Slice(us, func(a, b int) bool { return us[a].Pos < us[b].Pos })
	}
	out := make([]map[string]any, 0, len(us))
	for _, u := range us {
		m := map[string]any{"frame": j.AxisStart + u.Pos*j.AxisStep}
		if u.State == stSucceeded {
			m["seconds"] = math.Round(u.ranTime)
		} else {
			m["estimate"] = math.Round(u.Est)
			m["low"] = math.Round(math.Max(0, u.Est-u.EstErr))
			m["high"] = math.Round(u.Est + u.EstErr)
		}
		out = append(out, m)
	}
	return out
}

// unitDetail is GET /api/v1/jobs/{ns}/{name}/units/{i}.
func (s *Sim) unitDetail(j *Job, u *Unit) map[string]any {
	var atts []map[string]any
	for _, a := range u.Attempts {
		am := map[string]any{"n": a.N, "node": a.Node, "start": s.ts(a.Start)}
		if a.Ended {
			am["end"] = s.ts(a.End)
			am["exitCode"] = a.Exit
			if a.Reason != "" {
				am["reason"] = a.Reason
			}
		}
		atts = append(atts, am)
	}
	if atts == nil {
		atts = []map[string]any{}
	}
	out := map[string]any{"i": u.I, "label": u.Label, "state": s.unitState(j, u), "attempts": atts,
		"estimateSeconds": math.Round(u.Est)}
	if u.State == stSucceeded {
		out["durationSeconds"] = math.Round(u.ranTime)
		out["outputChangeset"] = u.Change
	} else if u.Node != "" {
		out["durationSeconds"] = math.Round(s.t - u.StartAt)
	}
	if u.Gen && u.Expanded {
		out["expandedInto"] = rle(append([]int(nil), u.Children...))
	}
	out["logTail"] = s.logTail(j, u)
	out["events"] = s.events(j, u)
	return out
}

func (s *Sim) events(j *Job, u *Unit) []map[string]any {
	var ev []map[string]any
	add := func(typ, reason, msg string, at float64) {
		ev = append(ev, map[string]any{"type": typ, "reason": reason, "message": msg, "at": s.ts(at)})
	}
	for _, a := range u.Attempts {
		add("Normal", "Released", fmt.Sprintf("Released to pool %s (attempt %d)", s.nodePool(a.Node), a.N), a.Start-1)
		add("Normal", "Scheduled", fmt.Sprintf("Assigned to %s", a.Node), a.Start)
		add("Normal", "Mounted", "c4 volume ready: view from node cache, 0 objects fetched from store", a.Start+u.Checkout)
		if !a.Ended {
			continue
		}
		switch {
		case a.Exit != 0:
			add("Warning", "Failed", fmt.Sprintf("exit %d: %s", a.Exit, a.Reason), a.End)
		case a.Reason != "":
			add("Normal", "Stopped", a.Reason, a.End)
		default:
			add("Normal", "Captured", "Upper captured as changeset and folded into the job's output view", a.End)
		}
	}
	if u.Gen && u.Expanded {
		add("Normal", "Expanded", fmt.Sprintf("Expansion document added %d units", len(u.Children)), u.Attempts[len(u.Attempts)-1].End)
	}
	if ev == nil {
		ev = []map[string]any{}
	}
	return ev
}

func (s *Sim) nodePool(name string) string {
	if n := s.nodeBy(name); n != nil {
		return n.Pool
	}
	return "?"
}

func (s *Sim) logTail(j *Job, u *Unit) []string {
	if len(u.Attempts) == 0 {
		return []string{}
	}
	a := u.Attempts[len(u.Attempts)-1]
	stamp := func(off float64) string { return s.ts(a.Start + off).Format("15:04:05") }
	var lines []string
	switch j.Driver {
	case "render", "sim":
		lines = []string{
			stamp(1) + " c4 view mounted at /show (lower: 3 layers, upper: empty)",
			stamp(3) + " loading scene " + j.Name + ".usd",
			stamp(9) + " textures: 1,284 files resolved from view, 0 fetched",
		}
		if u.Gen {
			lines = append(lines, stamp(20)+" evaluating RIB: emitting shadow, reflection, beauty", stamp(30)+" wrote .c4/expand.json (3 tasks, 2 edges)")
			break
		}
		el := s.t - a.Start
		if a.Ended {
			el = a.End - a.Start
		}
		for k := 1; k <= 3; k++ {
			p := int(math.Min(99, el/math.Max(u.Cost, 1)*100*float64(k)/3))
			lines = append(lines, fmt.Sprintf("%s render: %d%% complete, %d of 960 buckets", stamp(el*float64(k)/3), p, p*960/100))
		}
	case "ci":
		lines = []string{
			stamp(1) + " checkout: sparse view " + shortID(fakeID("view", j.Name)) + " (" + u.CacheOut + ")",
			stamp(2) + " running " + u.Label,
		}
		if a.Ended && a.Exit == 0 {
			lines = append(lines, stamp(a.End-a.Start)+" PASSED")
		}
	case "ml":
		lines = []string{
			stamp(1) + " dataset view mounted read-only at /data",
			stamp(90) + " restored checkpoint step " + strconv.Itoa(j.ML.LastCkpt),
			fmt.Sprintf("%s step %d/%d loss 0.0%d", s.ts(s.t).Format("15:04:05"), int(j.ML.step(s.t)), j.ML.TotalSteps, 412-int(j.ML.step(s.t))/40),
		}
	default:
		lines = []string{stamp(1) + " encoding " + u.Label}
	}
	if a.Ended && a.Exit != 0 {
		lines = append(lines, s.ts(a.End).Format("15:04:05")+" ERROR "+a.Reason)
	}
	return lines
}

func shortID(id string) string {
	if len(id) < 12 {
		return id
	}
	return id[:10] + "…"
}

// graph is GET /api/v1/graph.
func (s *Sim) graph(v *viewer) map[string]any {
	var nodes []map[string]any
	var edges []map[string]any
	vis := map[string]bool{}
	for _, j := range s.jobs {
		if s.visible(v, j) {
			vis[j.key()] = true
		}
	}
	// Critical path: from each sink, walk upstream through the input that
	// finishes last.
	crit := map[string]bool{}
	hasDependent := map[string]bool{}
	for _, j := range s.jobs {
		for _, d := range j.DependsOn {
			hasDependent[d] = true
		}
	}
	for _, j := range s.jobs {
		if hasDependent[j.key()] || len(j.DependsOn) == 0 || !vis[j.key()] {
			continue
		}
		cur := j
		for cur != nil {
			crit[cur.key()] = true
			var next *Job
			for _, d := range cur.DependsOn {
				u := s.jobBy[d]
				if u == nil || u.Done {
					continue
				}
				if next == nil || u.est.EtaP90 > next.est.EtaP90 {
					next = u
				}
			}
			cur = next
		}
	}
	for _, j := range s.ranked() {
		if !vis[j.key()] {
			continue
		}
		n := map[string]any{"id": j.key(), "class": j.Class, "driver": j.Driver, "phase": s.phase(j), "critical": crit[j.key()]}
		if !j.Done {
			n["etaP90"] = s.ts(j.est.EtaP90)
		}
		if j.hasEff {
			n["deadline"] = s.ts(j.eff)
			n["slackSeconds"] = math.Round(j.est.Slack)
		}
		nodes = append(nodes, n)
		for _, d := range j.DependsOn {
			if !vis[d] {
				continue
			}
			e := map[string]any{"from": d, "to": j.key()}
			if u := s.jobBy[d]; u != nil && u.Changesets > 0 {
				e["view"] = u.OutputView
			}
			edges = append(edges, e)
		}
	}
	if nodes == nil {
		nodes = []map[string]any{}
	}
	if edges == nil {
		edges = []map[string]any{}
	}
	return map[string]any{"nodes": nodes, "edges": edges}
}

// tiers is synthetic but tracks farm activity: more running units mean more
// requests into node daemons, and each tier forwards only its misses.
func (s *Sim) tiers() apiTiers {
	running := 0
	for _, n := range s.nodes {
		running += len(n.running)
	}
	w := 0.5 + 0.5*math.Sin(s.t/900)
	nodeIn := running*1900 + 3000
	hit := []float64{0.962 + 0.01*w, 0.884 + 0.02*w, 0.9935, 1}
	peerIn := int(float64(nodeIn) * (1 - hit[0]))
	regIn := int(float64(peerIn) * (1 - hit[1]))
	storeIn := int(math.Ceil(float64(regIn) * (1 - hit[2])))
	t := apiTiers{
		Tiers: []apiTier{
			{Name: "node", HitRate: round3(hit[0]), RequestsIn: nodeIn, UpstreamRequests: peerIn, BytesServed: float64(nodeIn) * 4.1e6, ShedRate: 0, InflightFills: 3 + running/9},
			{Name: "peer", HitRate: round3(hit[1]), RequestsIn: peerIn, UpstreamRequests: regIn, BytesServed: float64(peerIn) * 3.6e6, ShedRate: round3(0.002 * w), InflightFills: 2 + running/20},
			{Name: "regional", HitRate: round3(hit[2]), RequestsIn: regIn, UpstreamRequests: storeIn, BytesServed: float64(regIn) * 3.2e6, ShedRate: 0, InflightFills: 1 + running/40},
			{Name: "store", HitRate: 1, RequestsIn: storeIn, UpstreamRequests: 0, BytesServed: float64(storeIn) * 2.9e6, ShedRate: 0, InflightFills: 0},
		},
		FanIn:      map[string]int{"requesters": running, "nodeRequests": nodeIn, "peerRequests": peerIn, "regionalRequests": regIn, "storeRequests": storeIn},
		PublishP50: round2(0.82 + 0.1*w), PublishP95: round2(2.7 + 0.5*w), CaptureP50: round2(1.4 + 0.2*w),
	}
	return t
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
