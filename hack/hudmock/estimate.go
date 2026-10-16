package main

import (
	"container/heap"
	"fmt"
	"math"
	"sort"
)

type pt struct {
	pos int
	d   float64
}

// unitEstimates sets Est and EstErr on every unfinished unit of j. Along a
// frame axis an unrendered unit is interpolated between the nearest measured
// units on each side; elsewhere it takes the group median or the prior.
func (s *Sim) unitEstimates(j *Job) {
	byGroup := map[string][]*Unit{}
	for _, u := range j.Units {
		if u == nil {
			continue
		}
		byGroup[u.Group] = append(byGroup[u.Group], u)
	}
	axis := j.AxisKind == "frame"
	for g, us := range byGroup {
		var pts []pt
		for _, u := range us {
			if u.State == stSucceeded {
				pts = append(pts, pt{u.Pos, u.ranTime})
			}
		}
		sort.Slice(pts, func(a, b int) bool { return pts[a].pos < pts[b].pos })
		prior := j.prior[g]
		if prior == 0 {
			prior = 300
		}
		med := prior
		if len(pts) > 0 {
			ds := make([]float64, len(pts))
			for i, p := range pts {
				ds[i] = p.d
			}
			med = median(ds)
		}
		rel := 0.25
		var res []float64
		for k := 1; k < len(pts)-1; k++ {
			a, b := pts[k-1], pts[k+1]
			pred := a.d + (b.d-a.d)*float64(pts[k].pos-a.pos)/float64(b.pos-a.pos)
			res = append(res, (pred-pts[k].d)/math.Max(pred, 1))
		}
		if len(res) >= 3 {
			sq := 0.0
			for _, r := range res {
				sq += r * r
			}
			rel = math.Max(0.06, math.Sqrt(sq/float64(len(res))))
		}
		for _, u := range us {
			if u.State == stSucceeded {
				u.Est, u.EstErr = u.ranTime, 0
				continue
			}
			if u.Gen {
				u.Est, u.EstErr = prior, prior*0.3
				continue
			}
			if !axis || len(pts) == 0 {
				u.Est = med
				u.EstErr = med * math.Max(rel, 0.15)
				if len(pts) == 0 {
					u.EstErr = med * 0.5
				}
				continue
			}
			k := sort.Search(len(pts), func(i int) bool { return pts[i].pos >= u.Pos })
			switch {
			case k > 0 && k < len(pts):
				a, b := pts[k-1], pts[k]
				f := float64(u.Pos-a.pos) / float64(b.pos-a.pos)
				v := a.d + (b.d-a.d)*f
				u.Est = v
				u.EstErr = math.Abs(a.d-b.d)/2*math.Min(1, 2*math.Min(f, 1-f)+0.2) + rel*v
			case k == 0:
				u.Est, u.EstErr = pts[0].d, pts[0].d*math.Max(0.35, rel)
			default:
				l := pts[len(pts)-1]
				u.Est, u.EstErr = l.d, l.d*math.Max(0.35, rel)
			}
		}
	}
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	m := len(c) / 2
	if len(c)%2 == 1 {
		return c[m]
	}
	return (c[m-1] + c[m]) / 2
}

// remaining returns the remaining time of a unit, not weighted by slots.
func (s *Sim) remaining(u *Unit) float64 {
	switch u.State {
	case stSucceeded:
		return 0
	}
	if u.Node != "" {
		el := s.t - u.StartAt
		if el < u.Est {
			return u.Est - el
		}
		return 0.25 * el
	}
	return u.Est
}

// genWork is the expected child work of one unexpanded generator: the job's
// observed fan-out once three generators have expanded, otherwise the prior.
func (s *Sim) genWork(j *Job) (mean, sd, chain float64) {
	var sums, chains []float64
	for _, e := range j.Expansions {
		sum, longest := 0.0, 0.0
		for _, c := range e.Children {
			u := j.Units[c]
			sum += u.Est * float64(u.Slots)
			longest = math.Max(longest, u.Est)
		}
		sums = append(sums, sum)
		chains = append(chains, longest)
	}
	if len(sums) < 3 {
		return j.genPrior, j.genPriorSD, j.genPrior / 2
	}
	m := 0.0
	for _, v := range sums {
		m += v
	}
	m /= float64(len(sums))
	v := 0.0
	for _, x := range sums {
		v += (x - m) * (x - m)
	}
	sdv := math.Sqrt(v / float64(len(sums)-1))
	return m, math.Max(sdv, m*0.05), median(chains) * 1.6
}

// attainable is the slots j can use now: per pool, the ready slots not used
// by other jobs, within the project's quota and borrowing limit.
func (s *Sim) attainable(j *Job, future bool) float64 {
	total := 0.0
	p := s.projBy[j.Project]
	for _, pool := range j.Pools {
		ready, other, projOther := 0, 0, 0
		for _, n := range s.nodes {
			if n.Pool != pool || !n.ready(s.t) {
				continue
			}
			ready += n.Slots
			for _, r := range n.running {
				if r.j == j {
					continue
				}
				// Short units of jobs behind this one in dispatch order free
				// their slots to it soon; a running gang does not.
				if r.j.ML == nil && s.rankPos[r.j] > s.rankPos[j] && s.rankPos[j] > 0 {
					continue
				}
				other += r.u.Slots
				if r.j.Project == j.Project {
					projOther += r.u.Slots
				}
			}
		}
		q := p.Quotas[pool]
		avail := ready - other
		if future {
			avail = ready
			projOther = 0
		}
		lim := q[0] + q[1] - projOther
		total += math.Max(0, math.Min(float64(avail), float64(lim)))
	}
	return math.Min(total, float64(j.Cap))
}

// pendingBurst is the burst slots requested for j's pools that are not yet
// ready, and the seconds until they are.
func (s *Sim) pendingBurst(j *Job) (slots, wait float64) {
	for _, c := range s.capReqs {
		if c.State != "Provisioning" || !contains(c.Jobs, j.key()) {
			continue
		}
		pending := 0.0
		for _, n := range s.nodes {
			if n.Pool == c.Pool && n.burst && !n.ready(s.t) && !n.Disabled {
				pending += float64(n.Slots)
				w := math.Max(n.ReadyAt, n.WarmUntil) + 75 - s.t
				if n.Provisioned {
					w = n.WarmUntil - s.t
				}
				wait = math.Max(wait, w)
			}
		}
		slots += pending
	}
	return slots, wait
}

func (s *Sim) estimateAll() {
	for _, j := range s.jobs {
		if j.ML != nil {
			s.mlEstimate(j)
			continue
		}
		s.unitEstimates(j)
		s.workAndPath(j)
	}
	s.propagateDeadlines()
	for _, j := range s.jobs {
		s.slackAndRisk(j)
	}
	s.updateCapacity()
	for _, j := range s.jobs {
		j.rankScore = s.rankScore(j)
	}
	s.rankPos = map[*Job]int{}
	for i, j := range s.ranked() {
		s.rankPos[j] = i + 1
	}
}

func (s *Sim) workAndPath(j *Job) {
	e := &j.est
	W, varSum := 0.0, 0.0
	region := map[string]float64{}
	unexp := 0
	for _, u := range j.Units {
		// Failed units wait for a person; they are not remaining work.
		if u == nil || u.State == stSucceeded || u.State == stFailed {
			continue
		}
		r := s.remaining(u)
		W += r * float64(u.Slots)
		err := u.EstErr
		if u.Node != "" {
			err = 0.1 * r
		}
		region[fmt.Sprint(u.Group, "/", u.Pos/24)] += err * float64(u.Slots)
		if u.Gen && !u.Expanded {
			unexp++
		}
	}
	gm, gsd, gchain := s.genWork(j)
	W += float64(unexp) * gm
	varSum += float64(unexp) * gsd * gsd
	for _, v := range region {
		varSum += v * v
	}
	e.W = W
	e.Sigma = math.Sqrt(varSum)
	e.W90 = W + 1.28*e.Sigma
	e.Running = s.running(j)

	// Critical path through unfinished units.
	memo := map[int]float64{}
	var finish func(u *Unit, depth int) float64
	finish = func(u *Unit, depth int) float64 {
		if v, ok := memo[u.I]; ok {
			return v
		}
		r := s.remaining(u)
		if u.Gen && !u.Expanded {
			r += gchain
		}
		best := 0.0
		if depth < 64 {
			for _, d := range u.Deps {
				x := j.Units[d]
				if x == nil || x.State == stSucceeded {
					continue
				}
				best = math.Max(best, finish(x, depth+1))
			}
		}
		memo[u.I] = r + best
		return r + best
	}
	tp := 0.0
	for _, u := range j.Units {
		if u == nil || u.State == stSucceeded || u.State == stFailed {
			continue
		}
		tp = math.Max(tp, finish(u, 0))
	}
	e.TPath = tp
	e.Attainable = s.attainable(j, !j.ready || j.Hold)
}

func (j *Job) duration90() float64 {
	a := math.Max(j.est.Attainable, 1)
	return math.Max(j.est.W90/a, j.est.TPath)
}

// propagateDeadlines sets each job's effective deadline: its own, and no
// later than each dependent's effective deadline minus that dependent's
// estimated duration.
func (s *Sim) propagateDeadlines() {
	dependents := map[string][]*Job{}
	for _, j := range s.jobs {
		for _, d := range j.DependsOn {
			dependents[d] = append(dependents[d], j)
		}
	}
	for _, j := range s.jobs {
		j.eff, j.hasEff = j.Deadline, j.HasDeadline
	}
	for iter := 0; iter < 8; iter++ {
		for _, j := range s.jobs {
			for _, d := range dependents[j.key()] {
				if !d.hasEff {
					continue
				}
				c := d.eff - d.duration90()
				if !j.hasEff || c < j.eff {
					j.eff, j.hasEff = c, true
				}
			}
		}
	}
}

// startDelay is when a blocked job can start: the latest upstream ETA.
func (s *Sim) startDelay(j *Job) (d50, d90 float64) {
	for _, k := range j.DependsOn {
		u := s.jobBy[k]
		if u == nil || u.Done {
			continue
		}
		d50 = math.Max(d50, u.est.EtaP50-s.t)
		d90 = math.Max(d90, u.est.EtaP90-s.t)
	}
	return d50, d90
}

func (s *Sim) slackAndRisk(j *Job) {
	e := &j.est
	e.Message, e.Burst, e.HeavyFlagged = "", nil, nil
	if j.Done || j.FailedJob {
		e.EtaP50, e.EtaP90 = j.DoneAt, j.DoneAt
		e.Required, e.Slack = 0, 0
		if j.hasEff {
			e.Slack = j.eff - j.DoneAt
		}
		e.Feasible = !j.FailedJob
		if j.FailedJob {
			e.Message = fmt.Sprintf("%s failed: units are out of retries. Requeue them after fixing the cause.", j.Name)
		}
		s.flagHeavy(j)
		return
	}
	a := math.Max(e.Attainable, 0)
	pend, wait := s.pendingBurst(j)
	d50, d90 := 0.0, 0.0
	if !j.ready {
		d50, d90 = s.startDelay(j)
	}
	T := math.Inf(1)
	if j.hasEff {
		T = j.eff - s.t
	}
	effA := a
	if pend > 0 && T > wait && !math.IsInf(T, 1) {
		effA = a + pend*(T-wait)/T
	}
	aa := math.Max(effA, 1)
	e.EtaP50 = s.t + d50 + math.Max(e.W/aa, e.TPath)
	e.EtaP90 = s.t + d90 + math.Max(e.W90/aa, e.TPath)
	if math.IsInf(T, 1) {
		e.Required, e.Slack, e.Feasible = 0, 0, true
		s.flagHeavy(j)
		return
	}
	e.Required = e.W90 / math.Max(T-d90, 60)
	e.Slack = T - d90 - math.Max(e.W90/aa, e.TPath)
	e.Feasible = e.EtaP90 <= j.eff
	e.DeadlineMissBy = e.EtaP90 - j.eff
	s.flagHeavy(j)
	s.markRisk(j, aa)
	if j.Hold {
		e.Message = fmt.Sprintf("Held: no new units are released. Released now, it finishes by %s.", s.clock(e.EtaP90))
		if !e.Feasible {
			e.Message = fmt.Sprintf("Held: no new units are released. Released now, it would still miss %s by about %s.", s.clock(j.eff), approx(e.DeadlineMissBy))
		}
		return
	}
	if e.Feasible {
		return
	}
	if !j.ready {
		late := ""
		for _, d := range j.DependsOn {
			if u := s.jobBy[d]; u != nil && !u.Done && (late == "" || u.est.EtaP90 > s.jobBy[late].est.EtaP90) {
				late = d
			}
		}
		name := late
		if u := s.jobBy[late]; u != nil {
			name = u.Name
		}
		e.Message = fmt.Sprintf("Will miss %s by about %s: waiting on %s.", s.clock(j.eff), approx(e.DeadlineMissBy), name)
		return
	}
	e.Burst = s.burstOffer(j, a+pend, T)
	if e.Burst == nil {
		e.Message = fmt.Sprintf("%s will miss %s by about %s at current capacity. No burst pool it may use can close it in time.", j.Name, s.clock(j.eff), approx(e.DeadlineMissBy))
		return
	}
	e.Message = fmt.Sprintf("%s will miss %s by about %s at current capacity. +%d %s slots closes it (est. $%.0f).",
		j.Name, s.clock(j.eff), approx(e.DeadlineMissBy), e.Burst.Slots, e.Burst.Pool, e.Burst.Cost)
}

func (s *Sim) burstOffer(j *Job, a, T float64) *BurstOffer {
	for _, pn := range j.Pools {
		p := s.poolBy[pn]
		if p == nil || p.Burst == nil {
			continue
		}
		b := p.Burst
		prov := b.ProvSeconds + 75
		if T <= prov+60 {
			continue
		}
		n := math.Ceil((j.est.W90 - a*T) / (T - prov))
		if n <= 0 {
			continue
		}
		per := float64(p.SlotsPerNode)
		n = math.Ceil(n/per) * per
		if int(n) > b.MaxSlots-b.Requested {
			continue
		}
		hours := math.Max(j.est.W90/(a+n)-prov, 600) / 3600
		cost := n * b.Cost * hours
		pr := s.projBy[j.Project]
		return &BurstOffer{Pool: pn, Slots: int(n), Cost: math.Round(cost), NeedsApproval: cost > pr.BurstBudget-pr.BurstSpent}
	}
	return nil
}

// flagHeavy marks regions whose interpolated cost exceeds three times the
// job median, once one of their frames is measured.
func (s *Sim) flagHeavy(j *Job) {
	if j.AxisKind != "frame" || len(j.Passes) == 0 {
		return
	}
	g := j.Passes[len(j.Passes)-1]
	n := 0
	for _, u := range j.Units {
		if u != nil && u.Group == g && u.Pos+1 > n {
			n = u.Pos + 1
		}
	}
	est := make([]float64, n)
	meas := make([]bool, n)
	var ds []float64
	for _, u := range j.Units {
		if u == nil || u.Group != g {
			continue
		}
		est[u.Pos] = u.Est
		if u.State == stSucceeded {
			meas[u.Pos] = true
			ds = append(ds, u.ranTime)
		}
	}
	if len(ds) < 3 {
		return
	}
	med := median(ds)
	for p := 0; p < n; {
		if est[p] < 3*med {
			p++
			continue
		}
		q, any, sum := p, false, 0.0
		for q < n && est[q] >= 3*med {
			any = any || meas[q]
			sum += est[q]
			q++
		}
		if any {
			h := Heavy{From: p, To: q - 1, Factor: round1(sum / float64(q-p) / med)}
			for _, c := range j.Heavy {
				if c.From <= q-1 && c.To >= p {
					h.Label = c.Label
				}
			}
			j.est.HeavyFlagged = append(j.est.HeavyFlagged, h)
		}
		p = q
	}
}

type slotHeap []float64

func (h slotHeap) Len() int           { return len(h) }
func (h slotHeap) Less(a, b int) bool { return h[a] < h[b] }
func (h slotHeap) Swap(a, b int)      { h[a], h[b] = h[b], h[a] }
func (h *slotHeap) Push(x any)        { *h = append(*h, x.(float64)) }
func (h *slotHeap) Pop() any {
	old := *h
	v := old[len(old)-1]
	*h = old[:len(old)-1]
	return v
}

// markRisk projects when each waiting unit finishes by list scheduling over
// the attainable slots, and marks those projected after the deadline.
func (s *Sim) markRisk(j *Job, a float64) {
	slots := int(math.Max(1, math.Round(a)))
	h := make(slotHeap, 0, slots)
	for _, u := range j.Units {
		if u != nil && u.Node != "" && len(h) < slots {
			h = append(h, s.remaining(u))
		}
	}
	for len(h) < slots {
		h = append(h, 0)
	}
	heap.Init(&h)
	infl := 1.0
	if j.est.W > 0 {
		infl = j.est.W90 / j.est.W
	}
	d50, _ := s.startDelay(j)
	T := j.eff - s.t - d50
	for _, i := range j.order {
		u := j.Units[i]
		if u == nil {
			continue
		}
		u.Risk = false
		if u.State == stSucceeded || u.Node != "" || u.State == stFailed {
			continue
		}
		start := heap.Pop(&h).(float64)
		fin := start + u.Est*infl
		heap.Push(&h, fin)
		u.Risk = fin > T
	}
}

func (s *Sim) rankScore(j *Job) int {
	switch {
	case j.Done || j.FailedJob:
		return 5
	case j.Hold:
		return 4
	case !j.ready:
		return 3
	}
	if j.ML != nil {
		return 1
	}
	e := j.est
	if !e.Feasible && e.Burst == nil && j.ML == nil {
		return 2
	}
	if float64(e.Running) < e.Required {
		return 0
	}
	return 1
}

func (s *Sim) updateCapacity() {
	for _, j := range s.jobs {
		b := j.est.Burst
		var req *CapReq
		provisioning := false
		for _, c := range s.capReqs {
			if !contains(c.Jobs, j.key()) {
				continue
			}
			switch c.State {
			case "PendingApproval":
				req = c
			case "Provisioning":
				provisioning = true
			}
		}
		if b == nil || j.Hold || !j.ready {
			if req != nil {
				s.dropCapReq(req)
			}
			continue
		}
		if provisioning {
			continue
		}
		if req == nil {
			req = &CapReq{Pool: b.Pool, Jobs: []string{j.key()}, Project: j.Project}
			for _, in := range j.Inputs {
				req.Prewarm = append(req.Prewarm, in.View)
			}
			s.capReqs = append(s.capReqs, req)
		}
		req.Slots = b.Slots
		req.EstCost = b.Cost
		req.Until = j.eff
		req.Reason = fmt.Sprintf("%s misses %s by about %s at current capacity", j.Name, s.clock(j.eff), approx(j.est.DeadlineMissBy))
		req.State = "PendingApproval"
		if !b.NeedsApproval {
			s.approve(req)
		}
	}
}

func (s *Sim) dropCapReq(c *CapReq) {
	out := s.capReqs[:0]
	for _, x := range s.capReqs {
		if x != c {
			out = append(out, x)
		}
	}
	s.capReqs = out
}

// approve provisions the burst nodes for a capacity request.
func (s *Sim) approve(c *CapReq) {
	p := s.poolBy[c.Pool]
	if p == nil || p.Burst == nil || c.State != "PendingApproval" {
		return
	}
	c.State = "Provisioning"
	c.approved = s.t
	if pr := s.projBy[c.Project]; pr != nil {
		pr.BurstSpent += c.EstCost
	}
	p.Burst.Requested += c.Slots
	need := c.Slots / p.SlotsPerNode
	for _, n := range s.nodes {
		if need == 0 {
			break
		}
		if n.Pool == c.Pool && !n.Provisioned && n.ReadyAt == 0 {
			n.ReadyAt = s.t + p.Burst.ProvSeconds
			n.burst = true
			need--
		}
	}
	count := 0
	for _, n := range s.nodes {
		if n.Pool == c.Pool {
			count++
		}
	}
	for ; need > 0; need-- {
		count++
		s.nodes = append(s.nodes, &Node{
			Name: fmt.Sprintf("%s-z1-%02d", c.Pool, count), Pool: c.Pool, Locality: "us-west-2a",
			Slots: p.SlotsPerNode, ReadyAt: s.t + p.Burst.ProvSeconds, burst: true,
			CacheCap: 2.0e12, CacheHit: 0.2,
		})
	}
}

// approx formats a duration the way production says it: "about 3 h".
func approx(sec float64) string {
	switch {
	case sec < 90:
		return "1 min"
	case sec < 3600:
		return fmt.Sprintf("%d min", int(math.Round(sec/60/5)*5+0.5))
	case sec < 3*3600:
		h := math.Round(sec/1800) / 2
		return fmt.Sprintf("%g h", h)
	}
	return fmt.Sprintf("%d h", int(math.Round(sec/3600)))
}

func (s *Sim) clock(sec float64) string {
	return s.at(sec).Format("Mon 15:04")
}
