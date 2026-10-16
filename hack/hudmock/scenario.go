package main

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// The scenario: Friday morning on a shared farm. Project harbor (a feature)
// has an FX shot that will miss its delivery without a burst, a lighting job
// whose frames expand into passes, a held job, a delivery chain and a LoRA
// fine-tune. Project kestrel (episodic) has a large crowd render, a flood
// wedge, and CI on its pipeline monorepo with a merge queue.

var pdt = time.FixedZone("PDT", -7*3600)

func newSim(seed int64) *Sim {
	s := &Sim{
		seed:   seed,
		rng:    rand.New(rand.NewSource(seed)),
		epoch:  time.Date(2026, 10, 23, 8, 40, 0, 0, pdt),
		poolBy: map[string]*Pool{},
		projBy: map[string]*Project{},
		jobBy:  map[string]*Job{},
		ci:     newCIState(),
	}
	s.classes = []Class{
		{Name: "render", Driver: "render", Lens: "render", SLOType: "deadline", Turn: 86400},
		{Name: "sim", Driver: "sim", Lens: "frames", SLOType: "deadline", Turn: 172800},
		{Name: "media", Driver: "media", Lens: "frames", SLOType: "deadline", Turn: 14400},
		{Name: "ci-presubmit", Driver: "ci", Lens: "ci", SLOType: "latency", P50: 360, P95: 900},
		{Name: "ci-merge", Driver: "ci", Lens: "ci", SLOType: "latency", P50: 300, P95: 600},
		{Name: "ml-finetune", Driver: "ml", Lens: "ml", SLOType: "deadline", Turn: 172800},
	}
	s.pools = []*Pool{
		{Name: "cpu", Class: "cpu", CapType: "on-demand", SlotsPerNode: 8, Cost: 0.68},
		{Name: "gpu", Class: "gpu", GPUModel: "L40S", VRAM: "48Gi", CapType: "on-demand", SlotsPerNode: 4, Cost: 1.10},
		{Name: "ci", Class: "cpu", CapType: "on-demand", SlotsPerNode: 12, Cost: 0.34},
		{Name: "spot", Class: "gpu", GPUModel: "L4", VRAM: "24Gi", CapType: "spot", SlotsPerNode: 4, Cost: 2.40,
			Burst: &BurstCfg{MaxSlots: 32, ProvSeconds: 240, Cost: 2.40}},
	}
	for _, p := range s.pools {
		s.poolBy[p.Name] = p
	}
	node := func(name, pool, loc string, slots int, cache, hit float64) *Node {
		n := &Node{Name: name, Pool: pool, Locality: loc, Slots: slots, Provisioned: true, CacheCap: 3.8e12, CacheBytes: cache, CacheHit: hit}
		s.nodes = append(s.nodes, n)
		return n
	}
	node("cpu-r1-01", "cpu", "rack r1", 8, 2.9e12, 0.962)
	node("cpu-r1-02", "cpu", "rack r1", 8, 3.1e12, 0.968)
	node("cpu-r2-03", "cpu", "rack r2", 8, 2.4e12, 0.951)
	node("cpu-r2-04", "cpu", "rack r2", 8, 2.6e12, 0.944).FailRate = 0.22
	node("gpu-r3-01", "gpu", "rack r3", 4, 3.3e12, 0.973)
	node("gpu-r3-02", "gpu", "rack r3", 4, 3.0e12, 0.969)
	node("gpu-r3-03", "gpu", "rack r3", 4, 2.7e12, 0.958)
	node("ci-r4-01", "ci", "rack r4", 8, 1.6e12, 0.991)
	node("ci-r4-02", "ci", "rack r4", 8, 1.5e12, 0.989)
	sp := node("spot-z1-01", "spot", "us-west-2a", 4, 1.1e12, 0.902)
	sp.CacheCap = 2.0e12
	ph := node("spot-z1-02", "spot", "us-west-2a", 4, 0, 0)
	ph.Provisioned, ph.CacheCap = false, 2.0e12

	s.projects = []*Project{
		{Name: "harbor", Quotas: map[string][2]int{"cpu": {16, 16}, "gpu": {8, 4}, "spot": {0, 32}, "ci": {0, 0}},
			BurstBudget: 300, BurstSpent: 262, BytesMat: 8.61e13, BytesStore: 9.2e11, WorkerHours: map[[2]string]float64{}, GPUHours: 61.4},
		{Name: "kestrel", Quotas: map[string][2]int{"cpu": {16, 16}, "gpu": {4, 0}, "spot": {0, 16}, "ci": {16, 0}},
			BurstBudget: 300, BurstSpent: 0, BytesMat: 5.37e13, BytesStore: 6.1e11, WorkerHours: map[[2]string]float64{}, GPUHours: 0},
	}
	for _, p := range s.projects {
		s.projBy[p.Name] = p
	}
	s.projBy["harbor"].WorkerHours[[2]string{"gpu", "render"}] = 41.2
	s.projBy["harbor"].WorkerHours[[2]string{"cpu", "render"}] = 66.8
	s.projBy["harbor"].WorkerHours[[2]string{"gpu", "ml-finetune"}] = 20.2
	s.projBy["kestrel"].WorkerHours[[2]string{"cpu", "render"}] = 118.5
	s.projBy["kestrel"].WorkerHours[[2]string{"ci", "ci-presubmit"}] = 22.7

	day := func(d, h, m int) float64 {
		return time.Date(2026, 10, d, h, m, 0, 0, pdt).Sub(s.epoch).Seconds()
	}

	plates := Input{Materialization: "sh040-plates", View: fakeID("view", "sh040-plates")}
	simIn := Input{FromJob: "harbor-render/sh040-sim-v003", View: fakeID("view", "sh040-sim-v003", "out")}

	// Done: the sim cache that feeds the FX render.
	simJob := s.frameJob(frameSpec{ns: "harbor-render", name: "sh040-sim-v003", project: "harbor", class: "sim", driver: "sim",
		owner: "ivo", frames: 240, start: 1001, passes: []string{"sim"}, pools: []string{"cpu"}, cap: 1, order: "sequential",
		median: map[string]float64{"sim": 140}, bytes: 4.2e9})
	for i, u := range simJob.Units {
		u.State = stSucceeded
		u.ranTime = u.Cost
		u.Attempts = []*Attempt{{N: 1, Node: "cpu-r1-01", Start: -14400 + float64(i)*55, End: -14400 + float64(i)*55 + u.Cost, Ended: true}}
		u.Node = ""
		u.Change = fakeID("changeset", simJob.key(), fmt.Sprint(i))
	}
	simJob.Done, simJob.DoneAt = true, -1200
	simJob.Changesets = 240
	simJob.OutputView = simIn.View
	simJob.Created = -15000

	fx := s.frameJob(frameSpec{ns: "harbor-render", name: "sh040-fx-v003", project: "harbor", class: "render", driver: "render",
		owner: "maya", frames: 240, start: 1001, passes: []string{"beauty"}, pools: []string{"gpu", "spot"}, cap: 24, order: "binary",
		median: map[string]float64{"beauty": 690}, bytes: 2.2e10, measured: true, fps: 24,
		heavy:  []Heavy{{From: 99, To: 149, Factor: 3.4, Label: "hero FX: tanker hull breach"}},
		inputs: []Input{plates, simIn}, deps: []string{"harbor-render/sh040-sim-v003"}})
	fx.Units[fx.order[38]].FailPlan = 1
	fx.Units[fx.order[5]].FailPlan = 3

	comp := s.frameJob(frameSpec{ns: "harbor-render", name: "sh040-comp-v011", project: "harbor", class: "render", driver: "render",
		owner: "noor", frames: 240, start: 1001, passes: []string{"comp"}, pools: []string{"cpu"}, cap: 24, order: "sequential",
		median: map[string]float64{"comp": 95}, bytes: 6.4e9, fps: 24,
		inputs: []Input{plates, {FromJob: "harbor-render/sh040-fx-v003"}}, deps: []string{"harbor-render/sh040-fx-v003"}})
	_ = comp

	s.mediaJob("harbor-media", "sh040-transcode-v011", "harbor", "noor", 8, "segment", 260, []string{"harbor-render/sh040-comp-v011"}, 0)

	light := s.genJob()
	_ = light

	s.frameJob(frameSpec{ns: "harbor-render", name: "sh050-comp-v004", project: "harbor", class: "render", driver: "render",
		owner: "noor", frames: 96, start: 1001, passes: []string{"comp"}, pools: []string{"cpu"}, cap: 16, order: "sequential",
		median: map[string]float64{"comp": 80}, bytes: 5.1e9, fps: 24,
		inputs: []Input{{FromJob: "harbor-render/sh050-light-v002"}}, deps: []string{"harbor-render/sh050-light-v002"}})
	s.mediaJob("harbor-media", "sh050-transcode-v004", "harbor", "noor", 4, "segment", 240, []string{"harbor-render/sh050-comp-v004"}, 0)
	del := s.mediaJob("harbor-media", "harbor-delivery-wk43", "harbor", "rosa", 1, "package", 900,
		[]string{"harbor-media/sh040-transcode-v011", "harbor-media/sh050-transcode-v004"}, day(23, 12, 30))
	del.Units[0].Label = "package and send to client"

	held := s.frameJob(frameSpec{ns: "harbor-render", name: "sh010-light-v014", project: "harbor", class: "render", driver: "render",
		owner: "maya", frames: 120, start: 1001, passes: []string{"beauty"}, pools: []string{"cpu"}, cap: 12, order: "sequential",
		median: map[string]float64{"beauty": 330}, bytes: 1.4e10, fps: 24, deadline: day(23, 19, 0)})
	held.Hold = true
	for i := 0; i < 36; i++ {
		u := held.Units[i]
		u.State, u.ranTime = stSucceeded, u.Cost
		u.Attempts = []*Attempt{{N: 1, Node: "cpu-r1-02", Start: -5400 + float64(i)*60, End: -5400 + float64(i)*60 + u.Cost, Ended: true}}
		u.Change = fakeID("changeset", held.key(), fmt.Sprint(i))
	}
	held.Changesets = 36

	s.mlJob(day(24, 18, 0))

	s.frameJob(frameSpec{ns: "kestrel-render", name: "ep104-crowd-v006", project: "kestrel", class: "render", driver: "render",
		owner: "jo", frames: 2000, start: 1001, passes: []string{"crowd", "cloth", "beauty"}, pools: []string{"cpu"}, cap: 16, order: "sequential",
		median: map[string]float64{"crowd": 190, "cloth": 140, "beauty": 260}, bytes: 9.0e9, fps: 24, deadline: day(26, 9, 0)})

	wedge := s.frameJob(frameSpec{ns: "kestrel-render", name: "ep104-flood-wedge-v002", project: "kestrel", class: "sim", driver: "sim",
		owner: "lee", frames: 1000, start: 1, passes: wedgeNames(24), pools: []string{"cpu"}, cap: 8, order: "binary",
		median: map[string]float64{}, bytes: 1.1e9, deadline: day(25, 20, 0)})
	for i, g := range wedge.Groups {
		g.Label = fmt.Sprintf("wedge %02d  visc %.2f", i, 0.02+0.01*float64(i))
		wedge.Groups[i] = g
		wedge.prior[g.Key] = 45
	}

	s.ciSeedHistory()
	return s
}

func wedgeNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("w%02d", i)
	}
	return out
}

type frameSpec struct {
	ns, name, project, class, driver, owner string
	frames, start                           int
	passes, pools                           []string
	cap                                     int
	order                                   string
	median                                  map[string]float64
	bytes                                   float64
	measured                                bool
	fps                                     int
	heavy                                   []Heavy
	inputs                                  []Input
	deps                                    []string
	deadline                                float64
}

// frameJob builds a render or sim job: one unit per frame per pass, with
// indexes pass-major in frame order.
func (s *Sim) frameJob(f frameSpec) *Job {
	j := &Job{
		NS: f.ns, Name: f.name, Project: f.project, Class: f.class, Driver: f.driver, Owner: f.owner,
		Order: f.order, AxisKind: "frame", AxisStart: f.start, AxisStep: 1, Passes: f.passes, Fps: f.fps,
		Pools: f.pools, Cap: f.cap, DependsOn: f.deps, Heavy: f.heavy, Inputs: f.inputs,
		BytesPerUnit: f.bytes, StoreFrac: 0.011, Measured: f.measured, prior: map[string]float64{},
	}
	if f.deadline != 0 {
		j.Deadline, j.HasDeadline = f.deadline, true
	}
	for k, v := range f.median {
		j.prior[k] = v * 1.15
	}
	state := stQueued
	if len(f.deps) > 0 {
		state = stBlocked
	}
	for pi, pass := range f.passes {
		j.Groups = append(j.Groups, GroupDef{Key: pass, Label: pass})
		med := f.median[pass]
		if med == 0 {
			med = 40
		}
		for p := 0; p < f.frames; p++ {
			c := med * math.Exp(0.13*s.rng.NormFloat64())
			for _, h := range f.heavy {
				c *= heavyFactor(p, h)
			}
			// Gentle drift along the shot, as camera moves change cost.
			c *= 1 + 0.18*math.Sin(float64(p)/float64(f.frames)*math.Pi*2+float64(pi))
			u := &Unit{I: pi*f.frames + p, Pos: p, Group: pass, Label: fmt.Sprintf("%d %s", f.start+p, pass),
				State: state, Cost: c, Slots: 1, Parent: -1}
			if len(f.passes) == 1 {
				u.Label = fmt.Sprint(f.start + p)
			}
			j.Units = append(j.Units, u)
		}
	}
	pos := make([]int, f.frames)
	for i := range pos {
		pos[i] = i
	}
	if f.order == "binary" {
		pos = binaryOrder(f.frames)
	}
	for _, p := range pos {
		for pi := range f.passes {
			j.order = append(j.order, pi*f.frames+p)
		}
	}
	s.addJob(j)
	return j
}

func heavyFactor(p int, h Heavy) float64 {
	ramp := 4.0
	switch {
	case p >= h.From && p <= h.To:
		return h.Factor
	case p < h.From && float64(h.From-p) <= ramp:
		return 1 + (h.Factor-1)*(1-float64(h.From-p)/(ramp+1))
	case p > h.To && float64(p-h.To) <= ramp:
		return 1 + (h.Factor-1)*(1-float64(p-h.To)/(ramp+1))
	}
	return 1
}

func (s *Sim) mediaJob(ns, name, project, owner string, n int, kind string, med float64, deps []string, deadline float64) *Job {
	j := &Job{NS: ns, Name: name, Project: project, Class: "media", Driver: "media", Owner: owner, Order: "sequential",
		AxisKind: kind, Pools: []string{"cpu"}, Cap: n, DependsOn: deps, BytesPerUnit: 3e9, StoreFrac: 0.01,
		prior: map[string]float64{kind: med}}
	if deadline != 0 {
		j.Deadline, j.HasDeadline = deadline, true
	}
	j.Groups = []GroupDef{{Key: kind, Label: kind}}
	for i := 0; i < n; i++ {
		j.Units = append(j.Units, &Unit{I: i, Pos: i, Group: kind, Label: fmt.Sprintf("%s %d", kind, i+1), State: stBlocked,
			Cost: med * (0.9 + 0.2*s.rng.Float64()), Slots: 1, Parent: -1})
		j.order = append(j.order, i)
	}
	s.addJob(j)
	return j
}

// genJob is sh050-light-v002: each frame starts as a generator (a RIB
// evaluation) that expands into shadow and reflection passes and a beauty
// pass that depends on both. Child indexes are reserved pass-major, so index
// order is layout order.
func (s *Sim) genJob() *Job {
	const n = 96
	j := &Job{
		NS: "harbor-render", Name: "sh050-light-v002", Project: "harbor", Class: "render", Driver: "render", Owner: "maya",
		Order: "binary", AxisKind: "frame", AxisStart: 1001, AxisStep: 1, Fps: 24,
		Passes: []string{"shadow", "reflection", "beauty"},
		Groups: []GroupDef{
			{Key: "rib", Label: "RIB expand", Generator: true, Spans: []string{"shadow", "reflection", "beauty"}},
			{Key: "shadow", Label: "shadow"}, {Key: "reflection", Label: "reflection"}, {Key: "beauty", Label: "beauty"},
		},
		Pools: []string{"cpu"}, Cap: 16, Created: 2280, BytesPerUnit: 1.2e10, StoreFrac: 0.009,
		Units:    make([]*Unit, n*4),
		genPrior: 1000, genPriorSD: 350,
		prior:  map[string]float64{"rib": 150, "shadow": 140, "reflection": 190, "beauty": 520},
		Inputs: []Input{{Materialization: "sh050-plates", View: fakeID("view", "sh050-plates")}, {Materialization: "harbor-assets@v212", View: fakeID("view", "harbor-assets", "212")}},
	}
	for p := 0; p < n; p++ {
		j.Units[p] = &Unit{I: p, Pos: p, Group: "rib", Label: fmt.Sprintf("%d rib", 1001+p), State: stUnevaluated,
			Cost: 110 + 60*s.rng.Float64(), Slots: 1, Gen: true, Parent: -1}
	}
	for _, p := range binaryOrder(n) {
		j.order = append(j.order, p)
	}
	j.expand = func(s *Sim, j *Job, g *Unit) []*Unit {
		var kids []*Unit
		base := map[string]float64{"shadow": 120, "reflection": 170, "beauty": 470}
		for pi, pass := range []string{"shadow", "reflection", "beauty"} {
			i := n + pi*n + g.Pos
			c := base[pass] * math.Exp(0.15*s.rng.NormFloat64()) * (1 + 0.5*math.Sin(float64(g.Pos)/14))
			u := &Unit{I: i, Pos: g.Pos, Group: pass, Label: fmt.Sprintf("%d %s", 1001+g.Pos, pass), State: stQueued,
				Cost: c, Slots: 1, Parent: g.I}
			if pass == "beauty" {
				u.Deps = []int{n + g.Pos, 2*n + g.Pos}
				u.State = stBlocked
			}
			kids = append(kids, u)
		}
		return kids
	}
	j.Deadline, j.HasDeadline = 0, false
	s.addJob(j)
	return j
}

func (s *Sim) mlJob(deadline float64) *Job {
	m := &MLState{Ranks: 4, TotalSteps: 9000, StepSec: 1.2, CkptEvery: 500, RestoreSec: 90, Wrapped: "JobSet",
		DatasetView: fakeID("view", "harbor-lookdev-dataset", "v7"), CodeView: fakeID("view", "lora-trainer", "3f2a"),
		BaseView: fakeID("view", "base-model", "sdxl-1.0")}
	j := &Job{NS: "harbor-ml", Name: "harbor-lora-lookdev-v2", Project: "harbor", Class: "ml-finetune", Driver: "ml", Owner: "kai",
		Order: "gang", AxisKind: "rank", Pools: []string{"gpu"}, Cap: 4, Deadline: deadline, HasDeadline: true,
		Measured: true, Created: -5600, ML: m, BytesPerUnit: 0, prior: map[string]float64{},
		Inputs: []Input{{Materialization: "harbor-lookdev-dataset@v7", View: m.DatasetView}, {Materialization: "base checkpoint", View: m.BaseView}},
	}
	for r := 0; r < 4; r++ {
		key := fmt.Sprintf("rank%d", r)
		j.Groups = append(j.Groups, GroupDef{Key: key, Label: fmt.Sprintf("rank %d", r)})
		j.Units = append(j.Units, &Unit{I: r, Pos: 0, Group: key, Label: fmt.Sprintf("rank %d", r), State: stRunning, Slots: 1, Parent: -1,
			Attempts: []*Attempt{
				{N: 1, Node: "gpu-r3-02", Start: -5400, End: -3900, Ended: true, Exit: 1, Reason: "NCCL watchdog timeout on rank 2; JobSet restarted the set"},
			}})
		j.order = append(j.order, r)
	}
	// Pre-history: one failure at step 1175, restarted from the step-1000 checkpoint.
	for st := 500; st <= 1000; st += 500 {
		at := -5400 + m.RestoreSec + float64(st)*m.StepSec
		m.Checkpoints = append(m.Checkpoints, Ckpt{View: fakeID("ckpt", j.key(), fmt.Sprint(st)), Step: st, At: at, BytesNew: 6.4e8, BytesDedup: 1.39e10})
	}
	m.Lost = (175*m.StepSec + m.RestoreSec) * 4 / 3600
	m.Useful = 1000 * m.StepSec * 4 / 3600
	m.LastCkpt = 1000
	m.SegStart, m.SegStep, m.Running = -3810, 1000, true
	m.lastResume = -3810
	n := s.nodeBy("gpu-r3-01")
	for _, u := range j.Units {
		u.Node = n.Name
		u.StartAt = -3810
		u.Attempts = append(u.Attempts, &Attempt{N: 2, Node: n.Name, Start: -3810})
		n.running = append(n.running, &runRef{j: j, u: u})
	}
	for st := 1500; float64(st) <= m.step(0); st += 500 {
		at := m.SegStart + m.RestoreSec + float64(st-1000)*m.StepSec
		m.Checkpoints = append(m.Checkpoints, Ckpt{View: fakeID("ckpt", j.key(), fmt.Sprint(st)), Step: st, At: at, BytesNew: 6.1e8 + float64(st%7)*1.3e7, BytesDedup: 1.39e10})
		m.LastCkpt = st
		m.Useful += 500 * m.StepSec * 4 / 3600
	}
	j.ready = true
	s.addJob(j)
	return j
}
