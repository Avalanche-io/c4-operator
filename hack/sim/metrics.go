package main

import (
	"math"
	"sort"

	"github.com/Avalanche-io/c4-operator/work/sched"
)

// jobOutcome is one job's result in one run.
type jobOutcome struct {
	ID, Kind, Project      string
	GPU, Expand            bool
	Units                  int
	WorkSlotHours          float64
	HeavyMult              float64
	Submit, Deadline, Done float64 // hours from the start of the week
}

type kindStat struct {
	Jobs, Missed int
	LateHours    float64
}

// runResult is what one (policy, seed) run measured.
type runResult struct {
	Policy string
	Seed   uint64

	ByKind map[string]*kindStat // dailies, preview, delivery, ml
	CITTG  []float64            // minutes, every pipeline

	Util map[string]float64 // busy fraction of owned pools over the week

	UsefulGPUh, LostGPUh float64
	UsefulCPUh, LostCPUh float64
	MLUsefulh, MLLosth   float64
	BurstCost            float64
	BurstSlotHours       float64

	Jobs      []jobOutcome
	PreemptBy map[string]int

	preemptions   int
	burstRequests int
	decides       int
	approvalJobs  map[string]bool
	Unfinished    int
	end           float64
	eta           *etaRunResult // the ETA experiment's records (etaeval.go)
}

func newResult(policy string, seed uint64) *runResult {
	return &runResult{Policy: policy, Seed: seed, ByKind: map[string]*kindStat{}, PreemptBy: map[string]int{}, Util: map[string]float64{}, approvalJobs: map[string]bool{}}
}

func (r *runResult) work(kind string, gpu bool, useful, lost float64) {
	if gpu {
		r.UsefulGPUh += useful / hour
		r.LostGPUh += lost / hour
	} else if kind != "ci" {
		r.UsefulCPUh += useful / hour
		r.LostCPUh += lost / hour
	}
	if kind == "ml" {
		r.MLUsefulh += useful / hour
		r.MLLosth += lost / hour
	}
}

func (r *runResult) stat(kind string) *kindStat {
	s := r.ByKind[kind]
	if s == nil {
		s = &kindStat{}
		r.ByKind[kind] = s
	}
	return s
}

func (r *runResult) outcome(j *job, done float64) {
	js := j.spec
	w := 0.0
	for _, u := range js.units {
		w += u.dur * float64(u.slots)
	}
	r.Jobs = append(r.Jobs, jobOutcome{ID: js.id, Kind: js.kind, Project: js.project, GPU: js.gpu, Expand: js.expand,
		Units: len(js.units), WorkSlotHours: w / hour, HeavyMult: js.heavyMult,
		Submit: js.submit / hour, Deadline: j.deadline / hour, Done: done / hour})
}

func (r *runResult) jobDone(j *job) {
	r.outcome(j, j.finish)
	js := j.spec
	if js.kind == "ci" {
		r.CITTG = append(r.CITTG, (j.finish-js.submit)/60)
		return
	}
	s := r.stat(js.kind)
	s.Jobs++
	if late := j.finish - j.deadline; late > 0 {
		s.Missed++
		s.LateHours += late / hour
	}
}

func (r *runResult) unfinished(j *job, end float64) {
	r.Unfinished++
	r.outcome(j, end)
	js := j.spec
	if js.kind == "ci" {
		r.CITTG = append(r.CITTG, (end-js.submit)/60)
		return
	}
	s := r.stat(js.kind)
	s.Jobs++
	if late := end - j.deadline; late > 0 {
		s.Missed++
		s.LateHours += late / hour
	}
}

func (r *runResult) pool(p *pool) {
	if p.def.burst != nil {
		r.BurstCost += p.bill
		r.BurstSlotHours += p.bill / p.def.burst.price
		return
	}
	r.Util[p.def.name] = p.busy / (float64(p.def.slots) * 7 * day)
}

func (r *runResult) burstSuggestion(b sched.BurstSuggestion) {
	if !b.Auto && b.Closes {
		r.approvalJobs[b.Job] = true
	}
}

// renderTotals sums the render kinds.
func (r *runResult) renderTotals() kindStat {
	var t kindStat
	for _, k := range []string{"dailies", "preview", "delivery"} {
		if s := r.ByKind[k]; s != nil {
			t.Jobs += s.Jobs
			t.Missed += s.Missed
			t.LateHours += s.LateHours
		}
	}
	return t
}

func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func meanSD(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return math.NaN(), math.NaN()
	}
	m := 0.0
	for _, x := range xs {
		m += x
	}
	m /= float64(len(xs))
	if len(xs) < 2 {
		return m, 0
	}
	v := 0.0
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	return m, math.Sqrt(v / float64(len(xs)-1))
}
