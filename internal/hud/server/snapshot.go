package server

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// snapshot is GET /snapshot for one viewer.
func (s *Server) snapshot(m *model, v *api.Viewer) *api.Snapshot {
	snap := &api.Snapshot{Now: m.at, Viewer: v, Pools: []api.Pool{}, Classes: []api.Class{}, Projects: []api.Project{},
		Jobs: []api.Job{}, Nodes: []api.Node{}, Capacity: []api.Capacity{}}
	visible := map[string]bool{}
	for _, p := range v.Projects {
		visible[p] = true
	}

	// Pools and farm totals (farm-wide for every viewer).
	burstCost := map[string]float64{}
	for _, p := range m.pools {
		ap := api.Pool{Name: p.Name, Attrs: map[string]any{}, Slots: int(p.Status.Slots), Used: int(p.Status.Used),
			CostPerSlotHour: num(p.Spec.CostPerSlotHour)}
		for k, val := range p.Spec.Attributes {
			ap.Attrs[k] = val
		}
		if b := p.Spec.Burst; b != nil {
			per := int(max(1, p.Spec.SlotsPerNode))
			ap.Burst = &api.PoolBurst{MaxSlots: int(b.MaxNodes) * per, ProvisioningSeconds: float64(b.ProvisioningSeconds),
				CostPerSlotHour: num(b.CostPerSlotHour), Requested: int(p.Status.Requested), Provisioned: int(p.Status.Slots)}
			burstCost[p.Name] = num(b.CostPerSlotHour)
			snap.Farm.BurstSlots += int(p.Status.Slots)
			snap.Farm.BurstCostPerHour += float64(p.Status.Slots) * num(b.CostPerSlotHour)
		}
		snap.Farm.SlotsUsed += int(p.Status.Used)
		snap.Farm.SlotsTotal += int(p.Status.Slots)
		snap.Pools = append(snap.Pools, ap)
	}
	for _, j := range m.jobs {
		snap.Farm.QueuedUnits += int(j.job.Status.Units.Queued + j.job.Status.Units.Backoff)
	}
	snap.Farm.UnitsPerHour, snap.Farm.FailuresLastHour = m.unitsHour, m.failsHour
	mat, fetched := m.metrics.bytes("")
	snap.Farm.BytesMaterialized, snap.Farm.BytesFetchedStore = mat, fetched
	snap.Farm.BytesAvoided = math.Max(0, mat-fetched)

	// Classes.
	names := class.Names()
	sort.Strings(names)
	for _, n := range names {
		d, _ := class.Lookup(n)
		ac := api.Class{Name: n, Driver: n, Lens: d.Lens(), SLO: map[string]any{"type": "deadline"}}
		if n == "ci" {
			ac.SLO = map[string]any{"type": "latency", "p95Seconds": 600}
		}
		snap.Classes = append(snap.Classes, ac)
	}

	// Slots in use per project and pool.
	usedBy := map[[2]string]int{}
	for _, p := range m.pods {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		proj := m.nsProject[p.Namespace]
		usedBy[[2]string{proj, p.Labels[c4v1.LabelPool]}] += podSlots(&p)
		if !visible[proj] {
			snap.Farm.OtherProjectsSlots += podSlots(&p)
		}
	}

	// Projects the viewer sees.
	for _, p := range m.projects {
		if !visible[p.Name] {
			continue
		}
		ap := api.Project{Name: p.Name, Quotas: []map[string]any{}, WorkerHours: []map[string]any{}}
		if w := p.Spec.Work; w != nil {
			for _, q := range w.Quotas {
				bl := any(nil)
				if q.BorrowingLimit != nil {
					bl = *q.BorrowingLimit
				}
				ap.Quotas = append(ap.Quotas, map[string]any{"pool": q.Pool, "nominal": q.Nominal, "borrowingLimit": bl,
					"used": usedBy[[2]string{p.Name, q.Pool}]})
			}
			ap.BurstBudgetPerDay = num(w.BurstBudgetPerDay)
		}
		if s.Queue != nil {
			ap.BurstSpendToday = math.Round(s.Queue.Spent(p.Name)*100) / 100
		}
		pm, pf := m.metrics.bytes(p.Name)
		ap.BytesAvoided = math.Max(0, pm-pf)
		for k, h := range m.usage.container {
			if k[0] == p.Name {
				ap.WorkerHours = append(ap.WorkerHours, map[string]any{"pool": k[1], "class": poolClass(m, k[1]), "hours": round2(h)})
			}
		}
		sort.Slice(ap.WorkerHours, func(a, b int) bool { return ap.WorkerHours[a]["pool"].(string) < ap.WorkerHours[b]["pool"].(string) })
		ap.GPUHours = round2(m.usage.gpu[p.Name])
		snap.Projects = append(snap.Projects, ap)
	}

	// Jobs in dispatch order, ranked within the viewer's scope.
	rank := 0
	for _, j := range m.jobs {
		if !visible[j.project] {
			continue
		}
		aj := s.jobSummary(m, j)
		if aj.Rank > 0 {
			rank++
			aj.Rank = rank
		}
		snap.Jobs = append(snap.Jobs, aj)
		addGates(snap, j)
	}
	sortGates(snap)

	// Nodes of the pools: shared infrastructure, listed for everyone.
	for _, n := range m.nodes {
		pool := nodePool(m, &n)
		if pool == nil {
			continue
		}
		snap.Nodes = append(snap.Nodes, s.nodeSummary(m, &n, pool, visible))
	}

	snap.Tiers = m.metrics.tiers()

	for _, cr := range m.crs {
		ac := api.Capacity{Pool: cr.Spec.Pool, Slots: int(cr.Spec.Slots), Reason: cr.Spec.Reason, Jobs: []string{},
			EstCost: num(cr.Spec.EstimatedCost), State: cr.Status.State, PrewarmViews: cr.Spec.PrewarmViews}
		if ac.PrewarmViews == nil {
			ac.PrewarmViews = []string{}
		}
		if cr.Spec.Until != nil {
			ac.Until = cr.Spec.Until.Time.UTC()
		}
		if ac.State == "" {
			ac.State = "Provisioning"
			if !cr.Spec.Approved {
				ac.State = "PendingApproval"
			}
		}
		anyVisible := len(cr.Spec.Jobs) == 0
		for _, jk := range cr.Spec.Jobs {
			if r := m.byKey[jk]; r != nil && visible[r.project] {
				ac.Jobs = append(ac.Jobs, jk)
				anyVisible = true
			}
		}
		if anyVisible && (cr.Spec.Slots > 0 || ac.State == "PendingApproval") {
			snap.Capacity = append(snap.Capacity, ac)
		}
	}

	snap.Sites = s.Sites

	snap.Usage = api.Usage{ClusterHours: round2(m.usage.clusterHours), ContainerHours: []api.ContainerHours{},
		GPUHours: []api.GPUHours{}, ProductionWindows: []api.Window{}}
	for k, h := range m.usage.container {
		if visible[k[0]] {
			snap.Usage.ContainerHours = append(snap.Usage.ContainerHours, api.ContainerHours{Project: k[0], Pool: k[1], Hours: round2(h)})
		}
	}
	sort.Slice(snap.Usage.ContainerHours, func(a, b int) bool {
		x, y := snap.Usage.ContainerHours[a], snap.Usage.ContainerHours[b]
		return x.Project+x.Pool < y.Project+y.Pool
	})
	for p, h := range m.usage.gpu {
		if visible[p] {
			snap.Usage.GPUHours = append(snap.Usage.GPUHours, api.GPUHours{Project: p, Hours: round2(h)})
		}
	}
	sort.Slice(snap.Usage.GPUHours, func(a, b int) bool { return snap.Usage.GPUHours[a].Project < snap.Usage.GPUHours[b].Project })
	snap.Incidents = incidents(m, v, snap.Jobs)
	return snap
}

// jobSummary maps a WorkJob to the HUD's JobSummary.
func (s *Server) jobSummary(m *model, r *jobRec) api.Job {
	j := &r.job
	st := &j.Status
	aj := api.Job{Namespace: j.Namespace, Name: j.Name, Project: r.project, Class: j.Spec.Class, Driver: j.Spec.Class,
		Owner: j.Annotations["c4.avalanche.io/owner"], Phase: st.Phase, Order: st.Order, Rank: int(st.Rank),
		Pools: j.Spec.Pools, DependsOn: []string{}, CreatedAt: j.CreationTimestamp.Time.UTC(),
		Nudge:     api.Nudge{Hold: j.Spec.Nudge.Hold || j.Spec.Nudge.HoldStop, Boost: num(j.Spec.Nudge.Boost)},
		Blacklist: st.Blacklist, Inputs: []api.Input{}}
	aj.Shot = j.Labels[LabelShot]
	aj.Work = r.work
	if !r.hasWork {
		aj.Work = jobWork(m, r)
	}
	if aj.Phase == "" {
		aj.Phase = "Queued"
	}
	if aj.Nudge.Boost == 0 {
		aj.Nudge.Boost = 1
	}
	if len(aj.Pools) == 0 && r.driver != nil {
		aj.Pools = r.driver.Pools()
	}
	for _, d := range j.Spec.DependsOn {
		aj.DependsOn = append(aj.DependsOn, j.Namespace+"/"+d)
	}
	u := st.Units
	aj.Units = api.Counts{Total: int(u.Total), Queued: int(u.Queued), Blocked: int(u.Blocked), Running: int(u.Running),
		Retrying: int(u.Retrying), Succeeded: int(u.Succeeded), Failed: int(u.Failed), Held: int(u.Held),
		Preflight: int(u.Preflight), Backoff: int(u.Backoff), Egress: int(u.Egress), Accepted: int(u.Accepted), Waits: int(u.Waits),
		Unevaluated: int(u.Planned), Planned: int(u.Planned), Flagged: int(u.Flagged), Locked: int(u.Locked), Reused: int(u.Reused), Superseded: int(u.Superseded)}
	us := st.UnitStates
	aj.UnitStates = api.States{Queued: us["queued"], Blocked: us["blocked"], Running: us["running"], Retrying: us["retrying"],
		Succeeded: us["succeeded"], Failed: us["failed"], Preflight: us["preflight"], Backoff: us["backoff"],
		Egress: us["egress"], Accepted: us["accepted"], Waits: us["waits"],
		Unevaluated: us["planned"], Planned: us["planned"], Flagged: us["flagged"], Locked: us["locked"], Superseded: us["superseded"]}
	if sp := st.Spend; sp != nil {
		f := func(x string) float64 { v, _ := strconv.ParseFloat(x, 64); return v }
		aj.Spend = &api.Spend{ModelUnits: int(sp.ModelUnits), ProceduralUnits: int(sp.ProceduralUnits), ModelTokens: sp.ModelTokens, UnpricedTokens: sp.UnpricedTokens,
			InputTokens: sp.InputTokens, OutputTokens: sp.OutputTokens, CacheReadTokens: sp.CacheReadTokens, CacheWriteTokens: sp.CacheWriteTokens,
			WeightedTokens: sp.WeightedTokens, UnpricedWeightedTokens: sp.UnpricedWeightedTokens, EstimatedTokenCost: f(sp.EstimatedTokenCost),
			ModelShareBasis: sp.ModelShareBasis,
			TokenCost:       f(sp.TokenCost), ModelSlotCost: f(sp.ModelSlotCost), ProceduralSlotCost: f(sp.ProceduralSlotCost), ModelShare: f(sp.ModelShare)}
		if sp.ModelWorkShare != "" {
			ms := f(sp.ModelWorkShare)
			aj.ModelShare = &ms
		}
	}
	if ms := st.Measure; ms != nil {
		aj.Measure = &api.Measure{Name: ms.Name, Value: ms.Value, Of: ms.Of, Done: ms.Done, Detail: ms.Detail,
			Required: j.Spec.Measure != nil && j.Spec.Measure.Required}
		if ms.At != nil {
			aj.Measure.At = ms.At.Time.UTC()
		}
	}
	if c := meta.FindStatusCondition(st.Conditions, c4v1.ConditionNeedsPerson); c != nil && c.Status == metav1.ConditionTrue {
		aj.NeedsPerson = &api.NeedsPerson{Reason: strings.ToLower(c.Reason), Message: c.Message, Since: c.LastTransitionTime.Time.UTC()}
	}
	for k, n := range st.Needs {
		i, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		an := api.Needs{Unit: i, Text: n.Text, Why: n.Why, At: n.At.Time.UTC(), Record: n.Record, Supplied: n.Supplied, By: n.By}
		if r.plan != nil && i < len(r.plan.Units) {
			an.Name = r.plan.Units[i].Name
		}
		aj.Needs = append(aj.Needs, an)
	}
	sort.Slice(aj.Needs, func(a, b int) bool { return aj.Needs[a].Unit < aj.Needs[b].Unit })
	for _, a := range st.Asks {
		aa := api.Ask{ID: a.ID, From: int(a.From), To: int(a.To), Text: a.Text, At: a.At.Time.UTC(), Answer: a.Answer, Cannot: a.Cannot, Wait: a.Wait}
		if a.Answered != nil {
			t := a.Answered.Time.UTC()
			aa.Answered = &t
		}
		aj.Asks = append(aj.Asks, aa)
	}
	for _, m := range st.Steers {
		as := api.Steer{Unit: int(m.Unit), Name: m.Name, Seq: int(m.Seq), Actor: m.Actor, At: m.At.Time.UTC(), Text: m.Text,
			Record: m.Record, Seen: m.Seen, Ask: m.Ask, Answer: m.Answer}
		if m.Delivered != nil {
			t := m.Delivered.Time.UTC()
			as.Delivered = &t
		}
		aj.Steers = append(aj.Steers, as)
	}
	for _, r := range st.Rounds {
		ar := api.Rounds{Unit: int(r.Unit), Name: r.Name, Round: int(r.Round), Max: int(r.Max), Until: r.Until,
			StoppedBy: r.StoppedBy, ExpectSeconds: r.ExpectSeconds}
		for _, g := range r.Generators {
			ar.Generators = append(ar.Generators, int(g))
		}
		ar.ExpectMore, _ = strconv.ParseFloat(r.ExpectMore, 64)
		aj.Rounds = append(aj.Rounds, ar)
	}
	for _, w := range st.Waits {
		aw := api.Wait{Unit: int(w.Unit), Name: w.Name, WaitsFor: w.Server, Ask: w.Ask, Choices: w.Choices, Escalated: w.Escalated,
			ExpectSeconds: w.ExpectSeconds, Blocking: w.Blocking, Do: w.Do, Note: w.Note, Actor: w.Actor, Record: w.Record,
			Reply: w.Reply, View: w.View, Block: w.Block}
		// a person's open wait that holds up work needs them; an escalated
		// unit always does (it is flagged)
		aw.NeedsYou = w.Do == "" && strings.HasPrefix(w.Server, "person:") && (w.Blocking || w.Escalated)
		if w.Since != nil {
			t := w.Since.Time.UTC()
			aw.Since = &t
		}
		if w.At != nil {
			t := w.At.Time.UTC()
			aw.At = &t
		}
		aj.Waits = append(aj.Waits, aw)
	}
	for _, a := range st.Acks {
		aj.Acks = append(aj.Acks, api.Ack{Kind: a.Kind, Units: a.Units, Actor: a.Actor, Assignee: a.Assignee, Note: a.Note,
			At: a.At.Time.UTC(), Record: a.Record})
	}
	for _, c := range st.Causes {
		aj.Causes = append(aj.Causes, api.Cause{Kind: c.Kind, Key: c.Key, Title: c.Title, Units: c.Units, Count: int(c.Count),
			Sample: int(c.Sample), First: c.First.Time.UTC(), Last: c.Last.Time.UTC(), Nodes: c.Nodes, Did: c.Did, To: c.To})
	}
	if r.plan != nil && aj.Units.Total == 0 {
		// Not yet seen by the queue: every unit is queued.
		aj.Units.Total, aj.Units.Queued = len(r.plan.Units), len(r.plan.Units)
		aj.UnitStates.Queued = "0-" + strconv.Itoa(len(r.plan.Units)-1)
	}
	aj.Objective = api.Objective{Type: "deadline"}
	if r.driver != nil {
		o := r.driver.Objective(j, j.CreationTimestamp.Time)
		aj.Objective.Type = o.Type
		if o.LatencyTarget > 0 {
			aj.Objective.LatencyTargetSeconds = o.LatencyTarget.Seconds()
		}
	}
	if st.Deadline != nil {
		d := st.Deadline.Time.UTC()
		aj.Objective.Deadline = &d
	}
	aj.Objective.Source, aj.Objective.Milestone = st.DeadlineSource, st.Milestone
	aj.Objective.Mode = deadlineMode(j)
	aj.Objective.Suggestion = s.suggestion(st.Suggestion)
	if e := st.Estimate; e != nil {
		aj.Estimate = api.Estimate{RemainingSeconds: float64(e.RemainingSeconds), P90RemainingSeconds: float64(e.P90RemainingSeconds),
			RequiredSlots: round1(num(e.RequiredSlots)), AttainableSlots: round1(num(e.AttainableSlots)),
			RunningSlots: int(e.RunningSlots), SlackSeconds: float64(e.SlackSeconds), Feasible: e.Feasible, Message: e.Message}
		if e.ETA50 != nil {
			aj.Estimate.EtaP50 = e.ETA50.Time.UTC()
		}
		if e.ETA90 != nil {
			aj.Estimate.EtaP90 = e.ETA90.Time.UTC()
		}
		if e.ETA50 == nil {
			aj.Estimate.EtaP50, aj.Estimate.EtaP90 = m.at, m.at
			aj.Estimate.NoETA = noETA(m, j, st.Deadline != nil, int(e.RunningSlots))
		}
		aj.Estimate.EtaP80, aj.Estimate.EtaP95 = s.timePtr(e.ETA80), s.timePtr(e.ETA95)
		aj.Estimate.Calibrated = e.Calibrated
		aj.Estimate.Risk = e.Risk
		for _, h := range e.HeavyRegions {
			aj.HeavyRegions = append(aj.HeavyRegions, api.Heavy{From: int(h.From), To: int(h.To), Factor: num(h.Factor)})
		}
	} else {
		aj.Estimate.Feasible = true
		aj.Estimate.EtaP50, aj.Estimate.EtaP90 = m.at, m.at
		aj.Estimate.NoETA = "no estimate yet"
	}
	if st.Finished != nil {
		// A finished job's "ETA" is when it finished.
		f := st.Finished.Time.UTC()
		aj.Estimate.EtaP50, aj.Estimate.EtaP90 = f, f
		aj.Estimate.EtaP80, aj.Estimate.EtaP95 = &f, &f
		aj.Estimate.RemainingSeconds, aj.Estimate.P90RemainingSeconds = 0, 0
		if st.Deadline != nil {
			aj.Estimate.SlackSeconds = st.Deadline.Time.Sub(f).Seconds()
			aj.Estimate.Feasible = !f.After(st.Deadline.Time)
		}
	}
	aj.Estimate.Risk = shownRisk(m.at, st, aj.Estimate.Risk)
	if st.Phase == "Held" && aj.Estimate.Message == "" {
		aj.Estimate.Message = j.Namespace + "/" + j.Name + " is held: no new units are released."
	}
	for _, cr := range m.crs {
		if cr.Spec.Approved || !strings.Contains(cr.Name, "-approve-") {
			continue
		}
		for _, jk := range cr.Spec.Jobs {
			if jk == r.key {
				aj.Estimate.Burst = &api.BurstOffer{Pool: cr.Spec.Pool, Slots: int(cr.Spec.Slots), Cost: num(cr.Spec.EstimatedCost), NeedsApproval: true}
			}
		}
	}
	for _, in := range j.Spec.Inputs {
		ai := api.Input{Materialization: in.Materialization}
		if in.FromJob != "" {
			ai.FromJob = j.Namespace + "/" + in.FromJob
			if up := m.byKey[ai.FromJob]; up != nil && up.job.Status.Output != nil && up.job.Status.Phase == "Succeeded" {
				ai.View = up.job.Status.Output.View
			}
		}
		aj.Inputs = append(aj.Inputs, ai)
	}
	if o := st.Output; o != nil && o.View != "" {
		aj.Output = &api.Output{View: o.View, Changesets: int(o.Changesets)}
	}
	return aj
}

// nodeSummary describes one node.
func (s *Server) nodeSummary(m *model, n *corev1.Node, pool *c4v1.Pool, visible map[string]bool) api.Node {
	an := api.Node{Name: n.Name, Pool: pool.Name, Locality: n.Labels[c4v1.LabelLocality], Zone: n.Labels[corev1.LabelTopologyZone],
		Region: n.Labels[corev1.LabelTopologyRegion], Slots: int(max(1, pool.Spec.SlotsPerNode))}
	if v, ok := n.Labels[c4v1.LabelSlotLimit]; ok {
		if lim, err := strconv.Atoi(v); err == nil && lim >= 0 && lim < an.Slots {
			an.Capped, an.ShapeSlots, an.Slots = true, an.Slots, lim
			an.CapacityReason = n.Annotations[c4v1.AnnotationCapacityReason]
		}
	}
	for _, p := range m.pods {
		if p.Spec.NodeName != n.Name || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		sl := podSlots(&p)
		an.Used += sl
		if !visible[m.nsProject[p.Namespace]] {
			continue
		}
		u, _ := strconv.Atoi(p.Labels[c4v1.LabelUnit])
		an.Running = append(an.Running, api.Running{Job: p.Namespace + "/" + p.Labels[c4v1.LabelJob], Unit: u, Slots: sl})
	}
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	unprov := false
	for _, t := range n.Spec.Taints {
		if t.Key == c4v1.TaintUnprovisioned {
			unprov = true
		}
	}
	switch {
	case unprov:
		an.State, an.Slots = "unprovisioned", 0
	case !ready:
		an.State = "notReady"
	case n.Spec.Unschedulable:
		an.State = "disabled"
	case an.Used > 0:
		an.State = "busy"
	default:
		an.State = "idle"
	}
	if ss := m.metrics.nodes[n.Name]; ss != nil {
		an.CacheBytes = ss.sum("c4_node_cache_bytes")
		hit := ss.sum("c4_node_object_requests_total", "result", "hit")
		all := ss.sum("c4_node_object_requests_total")
		if all > 0 {
			an.CacheHitRate = round3(hit / all)
		}
	}
	return an
}

func nodePool(m *model, n *corev1.Node) *c4v1.Pool {
	for i := range m.pools {
		p := &m.pools[i]
		match := len(p.Spec.NodeSelector) > 0
		for k, v := range p.Spec.NodeSelector {
			if n.Labels[k] != v {
				match = false
			}
		}
		if match {
			return p
		}
	}
	return nil
}

func poolClass(m *model, name string) string {
	for _, p := range m.pools {
		if p.Name == name {
			if c := p.Spec.Attributes["class"]; c != "" {
				return c
			}
		}
	}
	return "cpu"
}

// bytes is (materialized, fetched from the store) for a project, or for
// the farm when project is empty. Counters run since each process
// started.
func (fm farmMetrics) bytes(project string) (float64, float64) {
	var mat, fetched float64
	for _, ss := range fm.nodes {
		if project == "" {
			mat += ss.sum("c4_node_materialized_bytes_total")
		} else {
			mat += ss.sum("c4_node_materialized_bytes_total", "project", project)
		}
		fetched += ss.sum("c4_tier_upstream_bytes_total", "tier", "node", "upstream", "store")
	}
	for _, ss := range fm.regional {
		fetched += ss.sum("c4_tier_upstream_bytes_total", "tier", "regional", "upstream", "store")
	}
	if project != "" {
		// Store bytes are not labeled by project; attribute them in
		// proportion to materialized bytes.
		farmMat, farmFetched := fm.bytes("")
		if farmMat > 0 {
			fetched = farmFetched * mat / farmMat
		} else {
			fetched = 0
		}
	}
	return mat, fetched
}

// tiers summarizes the cache tiers, in fetch order.
func (fm farmMetrics) tiers() api.Tiers {
	t := api.Tiers{Tiers: []api.Tier{}, FanIn: map[string]int{}}
	var node, regional series
	for _, ss := range fm.nodes {
		node = append(node, ss...)
	}
	for _, ss := range fm.regional {
		regional = append(regional, ss...)
	}
	objHit := node.sum("c4_node_object_requests_total", "result", "hit")
	objAll := node.sum("c4_node_object_requests_total")
	t.Tiers = append(t.Tiers, api.Tier{Name: "node", HitRate: ratio(objHit, objAll), RequestsIn: int(objAll),
		UpstreamRequests: int(node.sum("c4_tier_upstream_requests_total", "tier", "node")),
		InflightFills:    int(node.sum("c4_tier_fills_inflight", "tier", "node"))})
	peerAll := node.sum("c4_tier_requests_total", "tier", "node")
	peerHit := node.sum("c4_tier_requests_total", "tier", "node", "result", "hit") + node.sum("c4_tier_requests_total", "tier", "node", "result", "join")
	t.Tiers = append(t.Tiers, api.Tier{Name: "peer", HitRate: ratio(peerHit, peerAll), RequestsIn: int(peerAll),
		UpstreamRequests: int(node.sum("c4_tier_upstream_requests_total", "tier", "node", "upstream", "regional")),
		BytesServed:      node.sum("c4_tier_served_bytes_total", "tier", "node"),
		ShedRate:         ratio(node.sum("c4_tier_requests_total", "tier", "node", "result", "busy"), peerAll)})
	regAll := regional.sum("c4_tier_requests_total", "tier", "regional")
	regHit := regional.sum("c4_tier_requests_total", "tier", "regional", "result", "hit") + regional.sum("c4_tier_requests_total", "tier", "regional", "result", "join")
	storeReq := regional.sum("c4_tier_upstream_requests_total", "tier", "regional", "upstream", "store") +
		node.sum("c4_tier_upstream_requests_total", "tier", "node", "upstream", "store")
	t.Tiers = append(t.Tiers, api.Tier{Name: "regional", HitRate: ratio(regHit, regAll), RequestsIn: int(regAll),
		UpstreamRequests: int(storeReq), BytesServed: regional.sum("c4_tier_served_bytes_total", "tier", "regional"),
		ShedRate:      ratio(regional.sum("c4_tier_requests_total", "tier", "regional", "result", "busy"), regAll),
		InflightFills: int(regional.sum("c4_tier_fills_inflight", "tier", "regional"))})
	t.Tiers = append(t.Tiers, api.Tier{Name: "store", HitRate: 1, RequestsIn: int(storeReq),
		BytesServed: regional.sum("c4_tier_upstream_bytes_total", "tier", "regional", "upstream", "store") +
			node.sum("c4_tier_upstream_bytes_total", "tier", "node", "upstream", "store")})
	t.FanIn = map[string]int{"requesters": int(objAll), "nodeRequests": int(objAll - objHit), "peerRequests": int(peerAll),
		"regionalRequests": int(regAll), "storeRequests": int(storeReq)}
	t.PublishP50, t.PublishP95 = round3(quantile(fm.pub, 0.5)), round3(quantile(fm.pub, 0.95))
	t.CaptureP50 = round3(quantile(fm.capture, 0.5))
	return t
}

func podSlots(p *corev1.Pod) int {
	if len(p.Spec.Containers) > 0 {
		if q, ok := p.Spec.Containers[0].Resources.Requests["c4.avalanche.io/slots"]; ok {
			return int(q.Value())
		}
	}
	return 1
}

func num(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func ratio(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return round3(a / b)
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
func round2(x float64) float64 { return math.Round(x*100) / 100 }
func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

// deadlineMode is who owns the job's deadline: the spec's deadlineMode,
// else Pinned for a job with a deadline of its own, else Suggested when
// the farm suggested one.
func deadlineMode(j *c4v1.WorkJob) string {
	switch {
	case j.Spec.DeadlineMode != "":
		return j.Spec.DeadlineMode
	case j.Spec.Deadline != nil:
		return "Pinned"
	case j.Status.DeadlineSource == "suggested" || j.Status.Suggestion != nil:
		return "Suggested"
	}
	return ""
}

// suggestion is a job's suggested deadline as the API reports it.
func (s *Server) suggestion(sg *c4v1.WorkSuggestion) *api.Suggestion {
	if sg == nil || sg.Deadline.IsZero() {
		return nil
	}
	q, _ := strconv.ParseFloat(sg.Quantile, 64)
	out := &api.Suggestion{Deadline: sg.Deadline.Time.UTC(), State: sg.State, Policy: sg.Policy, Quantile: q,
		Revisions: int(sg.Revisions), MaxRevisions: int(sg.MaxRevisions), NextSession: s.timePtr(sg.NextSession),
		History: []api.SuggestionRevision{}, AtRisk: sg.AtRisk}
	for _, h := range sg.History {
		out.History = append(out.History, api.SuggestionRevision{At: h.At.Time.UTC(), Deadline: h.Deadline.Time.UTC(),
			UnitsMeasured: int(h.UnitsMeasured), Record: h.Record, Reason: h.Reason})
	}
	return out
}

// timePtr is t in the farm's time zone, nil when t is.
func (s *Server) timePtr(t *metav1.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

// shownRisk is Estimate.Risk: the queue's, made current by the clock (a
// deadline that passed while the job runs is missed now, not at the next
// status write), and none once the job has finished.
func shownRisk(now time.Time, st *c4v1.WorkJobStatus, queued string) string {
	switch {
	case st.Finished != nil || st.Deadline == nil:
		return c4v1.RiskNone
	case now.After(st.Deadline.Time):
		return c4v1.RiskMissed
	case queued == "":
		return c4v1.RiskNone
	}
	return queued
}

// noETA says, in plain words, why the scheduler projects no ETA for a job.
func noETA(m *model, j *c4v1.WorkJob, hasDeadline bool, running int) string {
	if j.Spec.Nudge.Hold || j.Spec.Nudge.HoldStop {
		return "held"
	}
	if name := j.Labels["c4.avalanche.io/agent-bundle"]; name != "" {
		for i := range m.bundles {
			b := &m.bundles[i]
			if b.Namespace != j.Namespace || b.Name != name {
				continue
			}
			switch b.Status.Phase {
			case "Suspended":
				return "suspended: it resumes from its checkpoints when capacity returns"
			case "Held", "Holding":
				return "held by a person"
			}
		}
	}
	if running == 0 {
		return "waiting for capacity"
	}
	if !hasDeadline {
		return "no deadline, so none projected"
	}
	return "no estimate yet"
}

// addGates adds a job's units at gates to the farm's gates.
func addGates(snap *api.Snapshot, j *jobRec) {
	for _, e := range j.job.Status.Gates {
		var g *api.Gate
		for i := range snap.Gates {
			if snap.Gates[i].Namespace == j.job.Namespace && snap.Gates[i].Name == e.Gate {
				g = &snap.Gates[i]
			}
		}
		if g == nil {
			snap.Gates = append(snap.Gates, api.Gate{Namespace: j.job.Namespace, Name: e.Gate, Waiting: []api.GateUnit{}})
			g = &snap.Gates[len(snap.Gates)-1]
		}
		u := api.GateUnit{Job: j.key, Unit: int(e.Unit), Name: e.Name}
		if e.Since != nil {
			t := e.Since.Time.UTC()
			u.Since = &t
		}
		if e.ExpectStart != nil {
			t := e.ExpectStart.Time.UTC()
			u.ExpectStart = &t
		}
		if e.Position == 0 {
			g.Holder = &u
			continue
		}
		g.Waiting = append(g.Waiting, u)
	}
}

// sortGates orders gates by name and each line first come, first served.
func sortGates(snap *api.Snapshot) {
	sort.Slice(snap.Gates, func(a, b int) bool {
		x, y := snap.Gates[a], snap.Gates[b]
		if x.Namespace != y.Namespace {
			return x.Namespace < y.Namespace
		}
		return x.Name < y.Name
	})
	for _, g := range snap.Gates {
		sort.SliceStable(g.Waiting, func(a, b int) bool {
			x, y := g.Waiting[a], g.Waiting[b]
			if x.Since == nil || y.Since == nil {
				return y.Since == nil && x.Since != nil
			}
			return x.Since.Before(*y.Since)
		})
	}
}
