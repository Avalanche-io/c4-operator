package main

import "time"

// The simulated farm: a mid-size VFX studio (three shows in production, an
// R&D group fine-tuning models, and an engineering team running CI on a
// monorepo).
//
// A CPU slot is one render node (32 vCPU, 128 GiB); a GPU slot is one GPU
// (48 GB class); a CI slot is one runner (8 vCPU).
//
// Burst prices are approximate public list prices for comparable cloud
// instance types (c7i.8xlarge for a CPU node, one L40S-class GPU as in
// g6e.xlarge), with spot at about 40 to 45 percent of on-demand. They are
// not quoted from a live price list. Provisioning delays include node boot,
// image pull and C4 cache pre-warm.

type poolDef struct {
	name  string
	slots int // owned slots; 0 for burst pools
	burst *burstDef
}

type burstDef struct {
	max   int
	delay time.Duration
	price float64 // dollars per slot-hour
}

var farmPools = []poolDef{
	{name: "cpu", slots: 240},
	{name: "gpu", slots: 32},
	{name: "ci", slots: 32},
	{name: "cpu-spot", burst: &burstDef{max: 160, delay: 7 * time.Minute, price: 0.60}},
	{name: "cpu-od", burst: &burstDef{max: 80, delay: 4 * time.Minute, price: 1.43}},
	{name: "gpu-spot", burst: &burstDef{max: 24, delay: 8 * time.Minute, price: 0.80}},
	{name: "gpu-od", burst: &burstDef{max: 16, delay: 5 * time.Minute, price: 1.86}},
}

type projectDef struct {
	name string
	// nominal quota per owned pool; borrowing is unlimited everywhere.
	nominal map[string]int
	// daily burst budget and lateness cost, dollars.
	budget, lateCost float64
	// manual baseline: priority bonus for the show.
	bonus int
}

var farmProjects = []projectDef{
	{name: "showA", nominal: map[string]int{"cpu": 110, "gpu": 8}, budget: 2000, lateCost: 600, bonus: 4},
	{name: "showB", nominal: map[string]int{"cpu": 80, "gpu": 6}, budget: 1200, lateCost: 450, bonus: 2},
	{name: "showC", nominal: map[string]int{"cpu": 50, "gpu": 4}, budget: 800, lateCost: 300, bonus: 0},
	{name: "rnd", nominal: map[string]int{"gpu": 14}},
	{name: "eng", nominal: map[string]int{"ci": 32}},
}

// farm is a set of pools and projects.
type farm struct {
	name     string
	pools    []poolDef
	projects []projectDef
}

var mainFarm = farm{name: "main", pools: farmPools, projects: farmProjects}

// gpuTightFarm is the sensitivity scenario: the same farm with half the
// owned GPUs (16) and every GPU quota halved, so ML fine-tuning and GPU
// renders contend.
func gpuTightFarm() farm {
	f := farm{name: "gpu16"}
	for _, p := range farmPools {
		if p.name == "gpu" {
			p.slots = 16
		}
		f.pools = append(f.pools, p)
	}
	for _, pr := range farmProjects {
		q := map[string]int{}
		for k, v := range pr.nominal {
			if k == "gpu" {
				v /= 2
			}
			q[k] = v
		}
		pr.nominal = q
		f.projects = append(f.projects, pr)
	}
	return f
}

// Static priorities a wrangler would set at submission (Manual baseline):
// type base plus the show bonus above.
var manualPriority = map[string]int{
	"delivery": 70,
	"dailies":  60,
	"preview":  50,
	"ci":       90,
	"ml":       30,
}

func poolsFor(kind string, gpu bool) []string {
	switch {
	case kind == "ci":
		return []string{"ci"}
	case kind == "ml":
		return []string{"gpu"}
	case gpu:
		return []string{"gpu", "gpu-spot", "gpu-od"}
	}
	return []string{"cpu", "cpu-spot", "cpu-od"}
}
