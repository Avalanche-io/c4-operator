package main

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Priced links between sites (Point.Links, with Sites above 1). A CPU
// job's inputs are at its project's home site; it may also use every
// other site's owned CPUs once its inputs are staged there over a link.
// sched sees those pools with Job.Sources (one Source per channel to the
// site) and the links' committed bytes, and decides Stage; the engine
// moves the bytes first come first served on the link and makes the
// pool usable for the job when they arrive.
//
// Channels and prices are bands from public cloud list prices, as
// published (on-demand, US regions, before discounts), used as
// illustrative bands and not quotes: inter-region transfer about
// $0.02/GB; internet egress about $0.09/GB; a private interconnect about
// $0.02/GB out plus about $0.02/GB of transit processing at the other
// end; a shipped storage device about $300 a job plus about $0.03/GB,
// days in transit. Ingress is free on the major clouds. Outputs (a fifth
// of the inputs) come back over the same channel at the same rate.
type channelDef struct {
	name    string
	bps     float64 // bytes per second the link carries
	latency float64 // seconds before the first byte lands (setup, transit)
	perGB   float64 // dollars per GB moved, both ends
	flat    float64 // dollars per transfer
}

var channels = map[string]channelDef{
	"interconnect": {"interconnect", 1.25e9, 60, 0.02 + 0.02, 0},
	"inter-region": {"inter-region", 6.25e8, 30, 0.02, 0},
	"internet":     {"internet", 1.25e8, 30, 0.09, 0},
	"shipment":     {"shipment", 1e12, 3 * day, 0.03, 300},
}

// outShare is the outputs' size as a share of the inputs'.
const outShare = 0.2

type linkDef struct {
	name string
	a, b int // sites, a < b
	ch   channelDef
}

// buildLinks gives every pair of sites the internet and a shipment lane;
// neighbouring sites on the ring also have a private interconnect, the
// others an inter-region route.
func buildLinks(sites int) []linkDef {
	var out []linkDef
	for a := range sites {
		for b := a + 1; b < sites; b++ {
			near := b == a+1 || a == 0 && b == sites-1
			names := []string{"inter-region", "internet", "shipment"}
			if near {
				names[0] = "interconnect"
			}
			for _, n := range names {
				out = append(out, linkDef{name: fmt.Sprintf("s%d-s%d-%s", a, b, n), a: a, b: b, ch: channels[n]})
			}
		}
	}
	return out
}

// linkRun is a link's queue: busy is when it has moved every byte
// committed to it.
type linkRun struct {
	def  linkDef
	busy float64
}

func (e *engine) initLinks() {
	e.linkBy = map[string]*linkRun{}
	for _, ld := range e.w.links {
		lr := &linkRun{def: ld}
		e.linkRuns = append(e.linkRuns, lr)
		e.linkBy[ld.name] = lr
	}
	e.poolSite = map[string]int{}
	for _, p := range e.w.pools {
		e.poolSite[p.name] = p.site
	}
}

func (e *engine) linkSnaps() []sched.Link {
	var out []sched.Link
	for _, lr := range e.linkRuns {
		out = append(out, sched.Link{Name: lr.def.name, BytesPerSecond: lr.def.ch.bps,
			Committed: math.Max(0, lr.busy-e.now) * lr.def.ch.bps})
	}
	return out
}

// remoteSite reports the site of a pool the job may use only once its
// inputs are there, and whether it is such a pool.
func (e *engine) remoteSite(j *job, pool string) (int, bool) {
	if j.spec.inBytes <= 0 {
		return 0, false
	}
	s := e.poolSite[pool]
	return s, s != j.spec.site && !j.staged[s]
}

func (e *engine) sources(j *job) map[string][]sched.Source {
	var out map[string][]sched.Source
	for _, pool := range j.spec.pools {
		s, far := e.remoteSite(j, pool)
		if !far {
			continue
		}
		if out == nil {
			out = map[string][]sched.Source{}
		}
		if until, ok := j.staging[s]; ok {
			out[pool] = []sched.Source{{Via: "staging", Latency: secsDur(until - e.now)}}
			continue
		}
		a, b := min(s, j.spec.site), max(s, j.spec.site)
		gb := j.spec.inBytes / 1e9
		for _, lr := range e.linkRuns {
			if lr.def.a != a || lr.def.b != b {
				continue
			}
			ch := lr.def.ch
			out[pool] = append(out[pool], sched.Source{Via: "move", Channel: ch.name, Link: lr.def.name, Bytes: j.spec.inBytes,
				Latency: secsDur(ch.latency), Cost: ch.flat + gb*(1+outShare)*ch.perGB})
		}
	}
	return out
}

func secsDur(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// stage applies the decision's stages: an approved one queues its bytes
// on its link and makes its pool usable for the job when they arrive.
func (e *engine) stage(d sched.Decisions) {
	for _, st := range d.Stage {
		if !st.Auto {
			if st.Closes {
				e.res.StageApprovals++
			}
			continue
		}
		var j *job
		for _, x := range e.active {
			if x.spec.id == st.Job {
				j = x
				break
			}
		}
		lr := e.linkBy[st.Source.Link]
		if j == nil || lr == nil {
			e.violate("stage-invalid", st.Job, fmt.Sprintf("stage to %s over unknown link %q", st.Pool, st.Source.Link))
			continue
		}
		s, far := e.remoteSite(j, st.Pool)
		if _, busy := j.staging[s]; !far || busy {
			e.violate("stage-invalid", st.Job, fmt.Sprintf("staged to %s, whose site already has or is getting its inputs", st.Pool))
			continue
		}
		start := math.Max(e.now, lr.busy)
		lr.busy = start + st.Source.Bytes/lr.def.ch.bps
		arrive := lr.busy + st.Source.Latency.Seconds()
		if math.Abs(arrive-e.now-st.Delay.Seconds()) > 1 {
			e.violate("stage-delay", st.Job, fmt.Sprintf("staged to %s expecting %s, the link delivers in %s", st.Pool, st.Delay.Round(time.Second), secsDur(arrive-e.now).Round(time.Second)))
		}
		j.staging[s] = arrive
		e.push(&event{t: arrive, kind: evStaged, j: j, n: s})
		e.spent[st.Project] += st.Cost
		e.res.StageCost += st.Cost
		e.res.Stages++
	}
}

func (e *engine) staged(j *job, site int) {
	j.staged[site] = true
	delete(j.staging, site)
	e.requestDecide()
}

// far reports whether the job's inputs have not reached the pool's site.
func (e *engine) far(j *job, pool string) bool {
	_, far := e.remoteSite(j, pool)
	return far
}

// checkStageDelay flags a stage whose transfer alone outlasts the time to
// the job's deadline (its planned deadline, JobState.Deadline) when the
// job could run where its inputs are, or another source to any pool
// arrives in time.
func (c *checker) checkStageDelay(v *view, st sched.Stage) {
	js := v.d.Jobs[st.Job]
	left := js.Deadline.Sub(v.s.Now)
	if js.Deadline.IsZero() || st.Delay < left {
		return
	}
	j := c.snapJob(v, st.Job)
	if j == nil {
		return
	}
	other := len(j.Pools) > 0
	for pool, srcs := range j.Sources {
		for _, src := range srcs {
			if pool == st.Pool && src == st.Source {
				continue
			}
			if d := srcDelay(v.s, src); d < left {
				other = true
			}
		}
	}
	if other {
		c.violate("stage-misses", st.Job, fmt.Sprintf("staged to %s via %s arriving in %s, %s before its deadline", st.Pool, st.Source.Channel, st.Delay.Round(time.Minute), left.Round(time.Minute)))
	}
}

// srcDelay is a source's delay from the snapshot's links, as sched
// computes it, without this decision's own stages.
func srcDelay(s sched.Snapshot, src sched.Source) time.Duration {
	d := src.Latency.Seconds()
	if src.Bytes > 0 {
		for _, l := range s.Links {
			if l.Name == src.Link && l.BytesPerSecond > 0 {
				d += (l.Committed + src.Bytes) / l.BytesPerSecond
			}
		}
	}
	return secsDur(d)
}

// localPools is s with each job's Pools cut to those its inputs have
// reached, as sched does (Job.Sources); the caller's slices are not
// written.
func localPools(s sched.Snapshot) sched.Snapshot {
	cloned := false
	for i := range s.Jobs {
		if len(s.Jobs[i].Sources) == 0 {
			continue
		}
		if !cloned {
			s.Jobs = slices.Clone(s.Jobs)
			cloned = true
		}
		j := &s.Jobs[i]
		var here []string
		for _, p := range j.Pools {
			if _, far := j.Sources[p]; !far {
				here = append(here, p)
			}
		}
		j.Pools = here
	}
	return s
}

// noStage (-no-stage) runs sched with staging off, for comparison.
var noStage bool
