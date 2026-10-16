package main

import (
	"fmt"
	"math"
	"sort"
)

// CIState models presubmit pipelines over a source monorepo and its merge
// queue.
type CIState struct {
	next      float64
	seq       int
	changes   []*Change
	flaky     map[string]*Flaky
	mq        []*MQEntry
	ttg       []float64
	head      string
	merged    []float64
	waits     []float64
	entered   int
	ejections int
}

type Change struct {
	ID, Title, Author string
	Created           float64
	Pre, Land         *Job
	TTG               float64
	Failed            bool
	seq               int
	gone              float64
}

type CIJob struct {
	Change *Change
	Kind   string // presubmit or land
}

type Flaky struct {
	Target   string
	Flips    int
	LastSeen float64
	Views    []string
}

type MQEntry struct {
	Change  *Change
	Entered float64
	State   string // waiting, testing, merged, ejected
	Base    string
	Done    float64
}

var ciTargets = []string{
	"//pipeline/usd:resolve_test",
	"//pipeline/houdini:hda_loader_test",
	"//pipeline/usd:stage_test",
	"//pipeline/nuke:ocio_test",
	"//pipeline/c4:publish_test",
	"//pipeline/core:config_test",
	"//pipeline/review:proxy_test",
	"//pipeline/farm:labels_test",
	"//pipeline/shotgrid:sync_test",
	"//pipeline/render:chunk_test",
	"//pipeline/core:paths_test",
	"//pipeline/usd:layer_test",
	"//pipeline/c4:manifest_test",
	"//pipeline/maya:export_test",
}

var ciTitles = []struct{ title, author string }{
	{"usd: cache resolver lookups per stage", "ana.k"},
	{"nuke: fix OCIO v2 config path on Linux render nodes", "ben.r"},
	{"houdini: load HDAs from the asset view, not the share", "chen.l"},
	{"c4: stream manifests in the publish step", "dev.p"},
	{"render: chunk size per pass from frame history", "eli.m"},
	{"review: H.264 proxies with burned-in timecode", "ana.k"},
	{"shotgrid: retry version sync on 429", "fay.o"},
	{"farm: label GPU nodes by VRAM", "ben.r"},
	{"core: drop the py3.9 path shims", "chen.l"},
	{"usd: lazy-load payloads in the lighting template", "dev.p"},
}

func newCIState() *CIState {
	c := &CIState{flaky: map[string]*Flaky{}, head: fakeID("view", "pipeline", "main", "0")}
	return c
}

func (s *Sim) ciSeedHistory() {
	c := s.ci
	for i := 0; i < 48; i++ {
		v := 300 * math.Exp(0.35*s.rng.NormFloat64())
		if i%11 == 3 {
			v *= 2.6
		}
		c.ttg = append(c.ttg, v)
	}
	for i := 0; i < 9; i++ {
		c.merged = append(c.merged, -3500+float64(i)*380)
		c.waits = append(c.waits, 240+s.rng.Float64()*420)
	}
	c.entered = 11
	c.ejections = 1
	c.flaky["//pipeline/usd:resolve_test"] = &Flaky{Target: "//pipeline/usd:resolve_test", Flips: 4, LastSeen: -1900,
		Views: []string{fakeID("view", "resolve_test", "a"), fakeID("view", "resolve_test", "b")}}
	c.flaky["//pipeline/render:chunk_test"] = &Flaky{Target: "//pipeline/render:chunk_test", Flips: 2, LastSeen: -5200,
		Views: []string{fakeID("view", "chunk_test", "a")}}
}

func (s *Sim) ciTick() {
	c := s.ci
	if s.t >= c.next {
		s.newChange()
		c.next = s.t + 170 + s.rng.Float64()*140
	}
	active := 0
	for _, e := range c.mq {
		if e.State == "merged" || e.State == "ejected" {
			continue
		}
		active++
		if e.Change.Land == nil && active <= 2 {
			s.newLand(e)
		}
		if e.Change.Land != nil && s.running(e.Change.Land) > 0 {
			e.State = "testing"
		}
	}
	// Changes whose work is all finished leave the HUD 12 minutes later.
	keep := c.changes[:0]
	for _, ch := range c.changes {
		done := ch.Pre.Done || ch.Pre.FailedJob
		if ch.Land != nil {
			done = ch.Land.Done || ch.Land.FailedJob
		} else if ch.Pre.Done {
			done = false
		}
		if done && ch.gone == 0 {
			ch.gone = s.t + 720
		}
		if ch.gone > 0 && s.t >= ch.gone {
			s.removeJob(ch.Pre)
			if ch.Land != nil {
				s.removeJob(ch.Land)
			}
			continue
		}
		keep = append(keep, ch)
	}
	c.changes = keep
	mq := c.mq[:0]
	for _, e := range c.mq {
		if (e.State == "merged" || e.State == "ejected") && s.t-e.Done > 1200 {
			continue
		}
		mq = append(mq, e)
	}
	c.mq = mq
}

func (s *Sim) newChange() {
	c := s.ci
	c.seq++
	t := ciTitles[(c.seq-1)%len(ciTitles)]
	ch := &Change{ID: fmt.Sprintf("c%d", 48210+c.seq), Title: t.title, Author: t.author, Created: s.t, seq: c.seq}
	j := &Job{
		NS: "kestrel-ci", Name: ch.ID + "-presubmit", Project: "kestrel", Class: "ci-presubmit", Driver: "ci",
		Owner: ch.Author, Order: "failure-rate", AxisKind: "shard",
		Groups: []GroupDef{
			{Key: "build", Label: "build"},
			{Key: "discover", Label: "test discovery", Generator: true, Spans: []string{"test"}},
			{Key: "test", Label: "test shards"},
			{Key: "integration", Label: "integration"},
		},
		Pools: []string{"ci"}, Cap: 16, Created: s.t, LatencyTarget: 900, Retries: 1,
		BytesPerUnit: 3.1e9, StoreFrac: 0.002,
		Units:    make([]*Unit, 19),
		genPrior: 11 * 150, genPriorSD: 400,
		prior:  map[string]float64{"build": 210, "discover": 32, "test": 150, "integration": 220},
		Inputs: []Input{{View: fakeID("view", "pipeline", ch.ID), Materialization: "pipeline@" + ch.ID}},
		CI:     &CIJob{Change: ch, Kind: "presubmit"},
	}
	j.Units[0] = &Unit{I: 0, Group: "build", Label: "build //pipeline/...", State: stQueued, Cost: 150 + s.rng.Float64()*110, Slots: 1, Parent: -1}
	j.Units[1] = &Unit{I: 1, Group: "discover", Label: "discover test shards", State: stUnevaluated, Cost: 25 + s.rng.Float64()*15, Slots: 1, Gen: true, Parent: -1}
	j.Units[18] = &Unit{I: 18, Group: "integration", Label: "integration", State: stBlocked, Cost: 180 + s.rng.Float64()*80, Slots: 2, Deps: []int{0, 1}, Parent: -1}
	j.order = []int{1, 0, 18}
	j.expand = s.expandShards
	s.addJob(j)
	ch.Pre = j
	c.changes = append(c.changes, ch)
}

func (s *Sim) expandShards(_ *Sim, j *Job, g *Unit) []*Unit {
	ch := j.CI.Change
	n := 8 + s.rng.Intn(7)
	var kids []*Unit
	for k := 0; k < n; k++ {
		u := &Unit{
			I: 2 + k, Pos: k, Group: "test", Label: fmt.Sprintf("%s shard %d/%d", ciTargets[k%len(ciTargets)], 1, 1),
			State: stBlocked, Cost: 60 + s.rng.Float64()*180, Slots: 1, Deps: []int{0}, Parent: g.I,
		}
		u.Label = ciTargets[k%len(ciTargets)]
		if k == 0 && s.rng.Float64() < 0.65 {
			u.FailPlan = 1
		}
		if k == 1 && ch.seq%6 == 3 {
			u.FailPlan = 9
		}
		kids = append(kids, u)
	}
	return kids
}

func (s *Sim) newLand(e *MQEntry) {
	ch := e.Change
	j := &Job{
		NS: "kestrel-ci", Name: ch.ID + "-land", Project: "kestrel", Class: "ci-merge", Driver: "ci",
		Owner: ch.Author, Order: "sequential", AxisKind: "step",
		Groups: []GroupDef{{Key: "speculative", Label: "speculative base"}},
		Pools:  []string{"ci"}, Cap: 4, Created: s.t, LatencyTarget: 600, Retries: 1,
		DependsOn: []string{ch.Pre.key()}, BytesPerUnit: 2.2e9, StoreFrac: 0.001,
		prior:  map[string]float64{"speculative": 160},
		Inputs: []Input{{View: e.Base, Materialization: "speculative base"}},
		CI:     &CIJob{Change: ch, Kind: "land"},
	}
	j.Units = []*Unit{
		{I: 0, Pos: 0, Group: "speculative", Label: "build on speculative base", State: stBlocked, Cost: 110 + s.rng.Float64()*60, Slots: 2, Parent: -1},
		{I: 1, Pos: 1, Group: "speculative", Label: "test on speculative base", State: stBlocked, Cost: 120 + s.rng.Float64()*90, Slots: 2, Deps: []int{0}, Parent: -1},
	}
	if ch.seq%5 == 4 {
		j.Units[1].FailPlan = 9
	}
	j.order = []int{0, 1}
	s.addJob(j)
	ch.Land = j
	e.State = "waiting"
}

func (s *Sim) ciUnitFailed(j *Job, u *Unit) {}

func (s *Sim) ciUnitDone(j *Job, u *Unit) {
	if u.Fails == 0 || j.CI.Kind != "presubmit" {
		return
	}
	f := s.ci.flaky[u.Label]
	if f == nil {
		f = &Flaky{Target: u.Label}
		s.ci.flaky[u.Label] = f
	}
	f.Flips++
	f.LastSeen = s.t
	f.Views = append(f.Views, fakeID("view", u.Label, j.CI.Change.ID))
	if len(f.Views) > 3 {
		f.Views = f.Views[len(f.Views)-3:]
	}
}

func (s *Sim) ciJobDone(j *Job, ok bool) {
	c := s.ci
	ch := j.CI.Change
	if j.CI.Kind == "presubmit" {
		if !ok {
			ch.Failed = true
			return
		}
		ch.TTG = s.t - ch.Created
		c.ttg = append(c.ttg, ch.TTG)
		if len(c.ttg) > 80 {
			c.ttg = c.ttg[len(c.ttg)-80:]
		}
		c.entered++
		c.mq = append(c.mq, &MQEntry{Change: ch, Entered: s.t, State: "waiting", Base: s.specBase(ch)})
		return
	}
	for i, e := range c.mq {
		if e.Change != ch {
			continue
		}
		e.Done = s.t
		if ok {
			e.State = "merged"
			c.merged = append(c.merged, s.t)
			c.waits = append(c.waits, s.t-e.Entered)
			c.head = fakeID("view", "pipeline", "main", ch.ID)
		} else {
			e.State = "ejected"
			c.ejections++
		}
		// Changes behind are retested on a corrected base.
		for _, b := range c.mq[i+1:] {
			if b.State == "merged" || b.State == "ejected" {
				continue
			}
			b.Base = s.specBase(b.Change)
			if !ok && b.Change.Land != nil {
				s.retest(b.Change.Land)
			}
		}
		break
	}
}

func (s *Sim) specBase(ch *Change) string {
	parts := []string{s.ci.head}
	for _, e := range s.ci.mq {
		if e.Change == ch {
			break
		}
		if e.State == "waiting" || e.State == "testing" {
			parts = append(parts, e.Change.ID)
		}
	}
	parts = append(parts, ch.ID)
	return fakeID(append([]string{"spec"}, parts...)...)
}

func (s *Sim) retest(j *Job) {
	for _, u := range j.Units {
		if u.Node != "" {
			a := u.Attempts[len(u.Attempts)-1]
			a.End, a.Ended, a.Reason = s.t, true, "Stopped: base changed after an ejection; retested on the corrected base"
			s.release(j, u)
			u.Node = ""
		}
		u.State = stBlocked
		u.FailAt = 0
	}
	j.cursor = 0
}

// ciSummary is the CI class summary, scoped to the jobs the viewer sees.
func (s *Sim) ciSummary(v *viewer) map[string]any {
	c := s.ci
	if !v.sees("kestrel") {
		return map[string]any{"changes": []any{}, "ttg": map[string]any{"p50": 0, "p95": 0, "targetP50": 360, "targetP95": 900, "samples": []any{}},
			"flaky": []any{}, "mergeQueue": map[string]any{"depth": 0, "p50WaitSeconds": 0, "p95WaitSeconds": 0, "ejectRate": 0, "mergesPerHour": 0, "entries": []any{}},
			"runnerPools": []any{}}
	}
	var changes []map[string]any
	for i := len(c.changes) - 1; i >= 0; i-- {
		ch := c.changes[i]
		var pipes []map[string]any
		for _, st := range []string{"build", "test", "integration"} {
			pipes = append(pipes, s.stage(ch.Pre, st, ch))
		}
		if ch.Land != nil {
			pipes = append(pipes, s.stage(ch.Land, "speculative", ch))
		}
		changes = append(changes, map[string]any{"id": ch.ID, "title": ch.Title, "author": ch.Author, "pipelines": pipes})
	}
	samples := append([]float64(nil), c.ttg...)
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	pct := func(p float64) float64 {
		if len(sorted) == 0 {
			return 0
		}
		return math.Round(sorted[int(p*float64(len(sorted)-1))])
	}
	var flaky []map[string]any
	for _, f := range c.flaky {
		flaky = append(flaky, map[string]any{"target": f.Target, "flips": f.Flips, "lastSeen": s.at(f.LastSeen), "views": f.Views})
	}
	sort.Slice(flaky, func(a, b int) bool {
		if flaky[a]["flips"].(int) != flaky[b]["flips"].(int) {
			return flaky[a]["flips"].(int) > flaky[b]["flips"].(int)
		}
		return flaky[a]["target"].(string) < flaky[b]["target"].(string)
	})
	var entries []map[string]any
	depth, pos := 0, 0
	for _, e := range c.mq {
		p := 0
		if e.State == "waiting" || e.State == "testing" {
			depth++
			pos++
			p = pos
		}
		entries = append(entries, map[string]any{"change": e.Change.ID, "position": p, "state": e.State, "speculativeBase": e.Base})
	}
	merges := 0
	for _, m := range c.merged {
		if s.t-m <= 3600 {
			merges++
		}
	}
	w := append([]float64(nil), c.waits...)
	sort.Float64s(w)
	wp := func(p float64) float64 {
		if len(w) == 0 {
			return 0
		}
		return math.Round(w[int(p*float64(len(w)-1))])
	}
	used, slots := 0, 0
	for _, n := range s.nodes {
		if n.Pool == "ci" && n.Provisioned {
			used += n.used()
			slots += n.Slots
		}
	}
	return map[string]any{
		"changes": changes,
		"ttg": map[string]any{"p50": pct(0.5), "p95": pct(0.95), "targetP50": 360, "targetP95": 900,
			"samples": roundAll(samples)},
		"flaky": flaky,
		"mergeQueue": map[string]any{"depth": depth, "p50WaitSeconds": wp(0.5), "p95WaitSeconds": wp(0.95),
			"ejectRate": round2(float64(c.ejections) / math.Max(1, float64(c.entered))), "mergesPerHour": merges, "entries": entries},
		"runnerPools": []map[string]any{{"pool": "ci", "used": used, "slots": slots}},
	}
}

func roundAll(xs []float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = math.Round(x)
	}
	return out
}

// stage summarizes one stage of a change's pipeline.
func (s *Sim) stage(j *Job, group string, ch *Change) map[string]any {
	var first, last, ready float64 = math.Inf(1), 0, math.Inf(1)
	state := "waiting"
	n, done, failed, running, blocked, co := 0, 0, 0, 0, 0, 0.0
	outcome := map[string]int{}
	for _, u := range j.Units {
		if u == nil || (u.Group != group && !(group == "test" && u.Group == "discover")) {
			continue
		}
		n++
		if len(u.Attempts) > 0 {
			first = math.Min(first, u.Attempts[0].Start)
			co += u.Checkout
			outcome[u.CacheOut]++
		}
		if u.ReadyAt > 0 {
			ready = math.Min(ready, u.ReadyAt)
		}
		switch u.State {
		case stSucceeded:
			done++
			if a := u.Attempts[len(u.Attempts)-1]; a.End > last {
				last = a.End
			}
		case stFailed:
			failed++
			if a := u.Attempts[len(u.Attempts)-1]; a.End > last {
				last = a.End
			}
		case stBlocked:
			blocked++
		case stRunning, stRetrying:
			if u.Node != "" {
				running++
			}
		}
	}
	if math.IsInf(ready, 1) {
		ready = j.Created
		if j.ReadyAt > 0 {
			ready = j.ReadyAt
		}
	}
	switch {
	case failed > 0:
		state = "failed"
	case n > 0 && done == n:
		state = "passed"
	case running > 0 || done > 0:
		state = "running"
	case !j.ready || n == 0 || blocked == n:
		state = "blocked"
	}
	qw, run := 0.0, 0.0
	if !math.IsInf(first, 1) {
		qw = math.Max(0, first-ready)
		end := s.t
		if state == "passed" || state == "failed" {
			end = last
		}
		run = math.Max(0, end-first)
	} else if j.ready && state != "blocked" {
		qw = s.t - ready
	}
	best, bc := "warm", -1
	for k, v := range outcome {
		if v > bc || (v == bc && k < best) {
			best, bc = k, v
		}
	}
	nAtt := 0
	for _, v := range outcome {
		nAtt += v
	}
	ckt := 0.0
	if nAtt > 0 {
		ckt = co / float64(nAtt)
	}
	out := map[string]any{"job": j.key(), "stage": group, "state": state,
		"queueWaitSeconds": math.Round(qw), "runSeconds": math.Round(run),
		"checkoutSeconds": round1(ckt), "cacheOutcome": best}
	if group == "integration" && state == "passed" && ch.TTG > 0 {
		out["timeToGreenSeconds"] = math.Round(ch.TTG)
	}
	return out
}
