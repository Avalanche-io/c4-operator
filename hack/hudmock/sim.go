package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"
)

// dt is the simulation step in simulated seconds. The scenario is a pure
// function of the seed and the number of steps taken, so a frozen run is
// reproducible.
const dt = 2.0

// Unit states, as the API reports them.
const (
	stUnevaluated = "unevaluated"
	stQueued      = "queued"
	stBlocked     = "blocked"
	stRunning     = "running"
	stRetrying    = "retrying"
	stSucceeded   = "succeeded"
	stFailed      = "failed"
)

type Attempt struct {
	N      int
	Node   string
	Start  float64
	End    float64
	Ended  bool
	Exit   int
	Reason string
}

type Unit struct {
	I        int
	Pos      int
	Group    string
	Label    string
	State    string
	Cost     float64 // duration of a successful attempt, simulated seconds
	Est      float64 // current estimate, seconds
	EstErr   float64
	Slots    int
	Deps     []int
	Gen      bool
	Expanded bool
	Children []int
	Parent   int
	FailPlan int // attempts that fail before one succeeds
	Fails    int
	Attempts []*Attempt
	Node     string
	StartAt  float64
	FailAt   float64
	Risk     bool
	Change   string // output changeset
	Checkout float64
	CacheOut string
	ReadyAt  float64
	lastBad  string
	failMsg  string
	failExit int
	ranTime  float64
}

type GroupDef struct {
	Key       string
	Label     string
	Generator bool
	Spans     []string
}

type Heavy struct {
	From, To int // positions
	Factor   float64
	Label    string
}

type Expansion struct {
	Gen      int
	Children []int
	At       float64
}

type Input struct {
	Materialization string
	View            string
	FromJob         string
}

type Estimate struct {
	W, W90, Sigma   float64
	TPath           float64
	Required        float64
	Attainable      float64
	Running         int
	Slack           float64
	Feasible        bool
	EtaP50, EtaP90  float64
	Message         string
	Burst           *BurstOffer
	HeavyFlagged    []Heavy
	DeadlineMissBy  float64
	ProjectedFinish map[int]float64
}

type BurstOffer struct {
	Pool          string  `json:"pool"`
	Slots         int     `json:"slots"`
	Cost          float64 `json:"cost"`
	NeedsApproval bool    `json:"needsApproval"`
}

type Job struct {
	NS, Name, Project, Class, Driver, Owner string

	Order      string
	AxisKind   string
	AxisStart  int
	AxisStep   int
	AxisLabels []string
	Groups     []GroupDef
	Passes     []string
	Fps        int

	Units []*Unit // indexed by unit index; nil for reserved, not yet created
	order []int   // dispatch order of unit indexes

	Pools          []string
	Cap            int
	DependsOn      []string
	Deadline       float64
	HasDeadline    bool
	LatencyTarget  float64
	Hold, HoldStop bool
	Boost          float64
	Blacklist      []string
	Created        float64
	Heavy          []Heavy
	Expansions     []Expansion
	Inputs         []Input
	OutputView     string
	Changesets     int
	Retries        int
	NodeErrorLimit int
	nodeErrs       map[string]int
	BytesPerUnit   float64
	StoreFrac      float64
	Measured       bool
	Done           bool
	FailedJob      bool
	DoneAt         float64
	ReadyAt        float64
	ready          bool

	genPrior   float64 // expected child work per generator, slot-seconds
	genPriorSD float64
	expand     func(s *Sim, j *Job, g *Unit) []*Unit
	prior      map[string]float64

	est         Estimate
	eff         float64
	hasEff      bool
	rankScore   int
	cursor      int
	bytesMat    float64
	bytesStore  float64
	ML          *MLState
	CI          *CIJob
	removeAfter float64
}

func (j *Job) key() string { return j.NS + "/" + j.Name }

type Pool struct {
	Name         string
	Class        string
	GPUModel     string
	VRAM         string
	CapType      string
	SlotsPerNode int
	Cost         float64
	Burst        *BurstCfg
}

type BurstCfg struct {
	MaxSlots    int
	ProvSeconds float64
	Cost        float64
	Requested   int
	Provisioned int
}

type Node struct {
	Name        string
	Pool        string
	Locality    string
	Slots       int
	Disabled    bool
	Provisioned bool
	ReadyAt     float64 // provisioned at
	WarmUntil   float64
	FailRate    float64
	CacheBytes  float64
	CacheCap    float64
	CacheHit    float64
	running     []*runRef
	burst       bool
}

type runRef struct {
	j *Job
	u *Unit
}

func (n *Node) used() int {
	u := 0
	for _, r := range n.running {
		u += r.u.Slots
	}
	return u
}

type Project struct {
	Name        string
	Quotas      map[string][2]int // pool -> nominal, borrowingLimit
	BurstBudget float64
	BurstSpent  float64
	BytesMat    float64
	BytesStore  float64
	WorkerHours map[[2]string]float64
	GPUHours    float64
	// ContainerHours is pod run time by pool, not weighted by slots.
	ContainerHours map[string]float64
	Windows        []ProductionWindow
}

// ProductionWindow is a declared fixed-fee period for a project.
type ProductionWindow struct {
	Name     string
	From, To time.Time
}

type CapReq struct {
	Pool     string
	Slots    int
	Until    float64
	Reason   string
	Jobs     []string
	EstCost  float64
	State    string // PendingApproval, Provisioning, Provisioned
	Prewarm  []string
	Project  string
	approved float64
}

type Class struct {
	Name, Driver, Lens string
	SLOType            string
	P50, P95, Turn     float64
}

type failRec struct {
	node, job, project string
	at                 float64
	nodeFault          bool
}

type Sim struct {
	seed     int64
	rng      *rand.Rand
	epoch    time.Time
	t        float64
	stepN    int
	pools    []*Pool
	poolBy   map[string]*Pool
	nodes    []*Node
	projects []*Project
	projBy   map[string]*Project
	classes  []Class
	jobs     []*Job
	jobBy    map[string]*Job
	capReqs  []*CapReq
	failures []failRec
	done     []float64 // completion times, for units per hour
	ci       *CIState
	version  uint64
	rankPos  map[*Job]int
}

func (s *Sim) at(sec float64) time.Time {
	return s.epoch.Add(time.Duration(sec * float64(time.Second)))
}

func (s *Sim) addJob(j *Job) {
	if j.nodeErrs == nil {
		j.nodeErrs = map[string]int{}
	}
	if j.Boost == 0 {
		j.Boost = 1
	}
	if j.Retries == 0 {
		j.Retries = 2
	}
	if j.NodeErrorLimit == 0 {
		j.NodeErrorLimit = 3
	}
	if j.OutputView == "" {
		j.OutputView = fakeID("view", j.key(), "empty")
	}
	s.jobs = append(s.jobs, j)
	s.jobBy[j.key()] = j
}

func (s *Sim) removeJob(j *Job) {
	delete(s.jobBy, j.key())
	out := s.jobs[:0]
	for _, x := range s.jobs {
		if x != j {
			out = append(out, x)
		}
	}
	s.jobs = out
}

// step advances the simulation by dt.
func (s *Sim) step() {
	s.t += dt
	s.stepN++
	s.advanceNodes()
	s.finishUnits()
	s.mlTick()
	s.ciTick()
	s.unblock()
	if s.stepN%5 == 1 {
		s.estimateAll()
	}
	s.dispatch()
	s.version++
}

func (s *Sim) advanceNodes() {
	for _, n := range s.nodes {
		if !n.Provisioned && n.ReadyAt > 0 && s.t >= n.ReadyAt {
			n.Provisioned = true
			n.WarmUntil = s.t + 75
		}
		if n.Provisioned && s.t < n.WarmUntil {
			n.CacheBytes = math.Min(n.CacheCap, n.CacheBytes+n.CacheCap*0.012*dt)
			n.CacheHit = math.Min(0.94, n.CacheHit+0.02)
		}
	}
	for _, p := range s.pools {
		if p.Burst == nil {
			continue
		}
		prov := 0
		for _, n := range s.nodes {
			if n.Pool == p.Name && n.burst && n.Provisioned && s.t >= n.WarmUntil {
				prov += n.Slots
			}
		}
		p.Burst.Provisioned = prov
	}
	for _, c := range s.capReqs {
		if c.State == "Provisioning" && s.poolBy[c.Pool].Burst.Provisioned >= c.Slots {
			c.State = "Provisioned"
		}
	}
}

func (n *Node) ready(t float64) bool {
	return n.Provisioned && !n.Disabled && t >= n.WarmUntil
}

func (s *Sim) nodeBy(name string) *Node {
	for _, n := range s.nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func (s *Sim) release(j *Job, u *Unit) {
	n := s.nodeBy(u.Node)
	if n == nil {
		return
	}
	out := n.running[:0]
	for _, r := range n.running {
		if r.u != u {
			out = append(out, r)
		}
	}
	n.running = out
}

func (s *Sim) finishUnits() {
	for _, n := range s.nodes {
		refs := append([]*runRef(nil), n.running...)
		for _, r := range refs {
			j, u := r.j, r.u
			if j.ML != nil {
				continue
			}
			if u.FailAt > 0 && s.t >= u.FailAt {
				s.failUnit(j, u)
				continue
			}
			if s.t >= u.StartAt+u.Cost {
				s.succeedUnit(j, u)
			}
		}
	}
}

func (s *Sim) poolClass(pool string) string {
	if p := s.poolBy[pool]; p != nil {
		return p.Class
	}
	return "cpu"
}

func (s *Sim) account(j *Job, u *Unit, seconds float64) {
	n := s.nodeBy(u.Node)
	if n == nil {
		return
	}
	p := s.projBy[j.Project]
	hours := seconds * float64(u.Slots) / 3600
	p.WorkerHours[[2]string{n.Pool, j.Class}] += hours
	if p.ContainerHours == nil {
		p.ContainerHours = map[string]float64{}
	}
	p.ContainerHours[n.Pool] += seconds / 3600
	if s.poolClass(n.Pool) == "gpu" {
		p.GPUHours += hours
	}
}

func (s *Sim) succeedUnit(j *Job, u *Unit) {
	a := u.Attempts[len(u.Attempts)-1]
	a.End, a.Ended, a.Exit = s.t, true, 0
	s.account(j, u, s.t-u.StartAt)
	u.ranTime = s.t - u.StartAt
	s.release(j, u)
	u.State = stSucceeded
	u.FailAt = 0
	defer func() { u.Node = "" }()
	u.Change = fakeID("changeset", j.key(), fmt.Sprint(u.I, len(u.Attempts)))
	j.Changesets++
	j.OutputView = fakeID("view", j.key(), fmt.Sprint(j.Changesets))
	s.done = append(s.done, s.t)
	mat := j.BytesPerUnit
	store := mat * j.StoreFrac
	j.bytesMat += mat
	j.bytesStore += store
	p := s.projBy[j.Project]
	p.BytesMat += mat
	p.BytesStore += store
	if n := s.nodeBy(u.Node); n != nil {
		n.CacheBytes = math.Min(n.CacheCap, n.CacheBytes+mat*0.02)
	}
	if u.Gen && j.expand != nil {
		kids := j.expand(s, j, u)
		u.Expanded = true
		exp := Expansion{Gen: u.I, At: s.t}
		for _, k := range kids {
			j.Units[k.I] = k
			u.Children = append(u.Children, k.I)
			exp.Children = append(exp.Children, k.I)
			j.order = append(j.order, k.I)
		}
		// Dependents of the generator now wait on its children.
		for _, x := range j.Units {
			if x == nil || x == u {
				continue
			}
			for di, d := range x.Deps {
				if d != u.I {
					continue
				}
				x.Deps = append(x.Deps[:di:di], x.Deps[di+1:]...)
				x.Deps = append(x.Deps, u.Children...)
				break
			}
		}
		j.Expansions = append(j.Expansions, exp)
	}
	if j.CI != nil {
		s.ciUnitDone(j, u)
	}
	s.checkJobDone(j)
}

func (s *Sim) checkJobDone(j *Job) {
	if j.Done {
		return
	}
	all, stuck := true, false
	for _, u := range j.Units {
		if u == nil {
			continue
		}
		if u.State == stFailed {
			stuck = true
		}
		if u.State != stSucceeded {
			all = false
		}
	}
	if all {
		j.Done = true
		j.DoneAt = s.t
		if j.CI != nil {
			s.ciJobDone(j, true)
		}
		return
	}
	if !stuck {
		return
	}
	// A job with a failed unit and nothing left that can run has failed.
	for _, u := range j.Units {
		if u == nil {
			continue
		}
		if u.State == stRunning || u.State == stRetrying || u.State == stUnevaluated {
			return
		}
		if u.State == stQueued && s.depsMet(j, u) {
			return
		}
	}
	if !j.FailedJob {
		j.FailedJob = true
		j.DoneAt = s.t
		if j.CI != nil {
			s.ciJobDone(j, false)
		}
	}
}

func (s *Sim) failUnit(j *Job, u *Unit) {
	a := u.Attempts[len(u.Attempts)-1]
	a.End, a.Ended = s.t, true
	a.Exit = u.failExit
	a.Reason = u.failMsg
	s.account(j, u, s.t-u.StartAt)
	s.release(j, u)
	s.failures = append(s.failures, failRec{node: u.Node, job: j.key(), project: j.Project, at: s.t, nodeFault: u.failExit == 139})
	j.nodeErrs[u.Node]++
	if j.nodeErrs[u.Node] >= j.NodeErrorLimit && !contains(j.Blacklist, u.Node) {
		j.Blacklist = append(j.Blacklist, u.Node)
	}
	u.lastBad = u.Node
	u.Fails++
	u.FailAt = 0
	u.Node = ""
	if u.Fails > j.Retries {
		u.State = stFailed
		s.checkJobDone(j)
		return
	}
	u.State = stRetrying
	if j.CI != nil {
		s.ciUnitFailed(j, u)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (s *Sim) depsMet(j *Job, u *Unit) bool {
	if !j.ready {
		return false
	}
	for _, d := range u.Deps {
		x := j.Units[d]
		if x == nil || x.State != stSucceeded {
			return false
		}
	}
	return true
}

func (s *Sim) unblock() {
	for _, j := range s.jobs {
		if !j.ready {
			ok := s.t >= j.Created
			for _, d := range j.DependsOn {
				dj := s.jobBy[d]
				if dj != nil && !dj.Done {
					ok = false
				}
			}
			if ok {
				j.ready = true
				j.ReadyAt = s.t
				if j.LatencyTarget > 0 {
					j.Deadline = s.t + j.LatencyTarget
					j.HasDeadline = true
				}
			}
		}
		for _, u := range j.Units {
			if u == nil {
				continue
			}
			switch u.State {
			case stBlocked:
				if s.depsMet(j, u) {
					u.State = stQueued
					u.ReadyAt = s.t
				}
			case stQueued:
				if !s.depsMet(j, u) {
					u.State = stBlocked
				}
			}
		}
	}
}

// usedIn is the slots a project uses in a pool.
func (s *Sim) usedIn(project, pool string) int {
	used := 0
	for _, n := range s.nodes {
		if n.Pool != pool {
			continue
		}
		for _, r := range n.running {
			if r.j.Project == project {
				used += r.u.Slots
			}
		}
	}
	return used
}

func (s *Sim) running(j *Job) int {
	c := 0
	for _, u := range j.Units {
		if u != nil && u.Node != "" {
			c += u.Slots
		}
	}
	return c
}

func (s *Sim) runnable(j *Job) bool {
	return j.ready && !j.Hold && !j.Done && j.ML == nil
}

// nextUnits returns up to k units of j that could start now, in dispatch order.
func (s *Sim) nextUnits(j *Job, k int) []*Unit {
	var out []*Unit
	// Generators go first (they reveal work), but take at most half the
	// job's slots, so expanded children keep moving.
	gens := 0
	for _, u := range j.Units {
		if u != nil && u.Gen && u.Node != "" {
			gens++
		}
	}
	genCap := gens >= (j.Cap+1)/2
	for c := j.cursor; c < len(j.order); c++ {
		u := j.Units[j.order[c]]
		if u == nil {
			continue
		}
		if u.Node != "" || (u.State != stQueued && u.State != stRetrying && u.State != stUnevaluated) {
			if c == j.cursor && (u.State == stSucceeded) {
				j.cursor++
			}
			continue
		}
		if u.Gen && genCap {
			continue
		}
		if !s.depsMet(j, u) {
			continue
		}
		out = append(out, u)
		if len(out) >= k {
			break
		}
	}
	return out
}

func (s *Sim) pickNode(j *Job, u *Unit, reserved map[string]bool) *Node {
	var best *Node
	bestFree := 0
	for _, pool := range j.Pools {
		if reserved[pool] {
			continue
		}
		p := s.projBy[j.Project]
		q := p.Quotas[pool]
		if s.usedIn(j.Project, pool)+u.Slots > q[0]+q[1] {
			continue
		}
		for _, n := range s.nodes {
			if n.Pool != pool || !n.ready(s.t) || contains(j.Blacklist, n.Name) || n.Name == u.lastBad {
				continue
			}
			free := n.Slots - n.used()
			if free < u.Slots || free <= bestFree {
				continue
			}
			best, bestFree = n, free
		}
		if best != nil {
			return best
		}
	}
	return best
}

func (s *Sim) start(j *Job, u *Unit, n *Node) {
	u.Node = n.Name
	u.StartAt = s.t
	if u.State == stQueued || u.State == stUnevaluated {
		u.State = stRunning
	}
	a := &Attempt{N: len(u.Attempts) + 1, Node: n.Name, Start: s.t}
	u.Attempts = append(u.Attempts, a)
	n.running = append(n.running, &runRef{j: j, u: u})
	switch {
	case u.Fails < u.FailPlan:
		u.FailAt = s.t + u.Cost*(0.35+0.3*s.rng.Float64())
		u.failExit, u.failMsg = failReason(j, u, false)
	case n.FailRate > 0 && s.rng.Float64() < n.FailRate:
		u.FailAt = s.t + u.Cost*(0.2+0.5*s.rng.Float64())
		u.failExit, u.failMsg = failReason(j, u, true)
	default:
		u.FailAt = 0
	}
	r := s.rng.Float64()
	switch {
	case r < 0.72:
		u.CacheOut, u.Checkout = "warm", 1.2+s.rng.Float64()
	case r < 0.95:
		u.CacheOut, u.Checkout = "delta", 4+4*s.rng.Float64()
	default:
		u.CacheOut, u.Checkout = "cold", 30+20*s.rng.Float64()
	}
}

func failReason(j *Job, u *Unit, node bool) (int, string) {
	if node {
		return 139, "SIGSEGV in texture cache reader (node fault suspected)"
	}
	switch j.Driver {
	case "ci":
		if u.FailPlan > j.Retries {
			return 1, "2 tests failed: TestHDALoaderVersion, TestHDALoaderFallback"
		}
		return 1, "TestStageCacheConcurrent timed out after 30s"
	case "render":
		if u.FailPlan > j.Retries {
			return 1, "missing texture: /show/sh040/tex/hull_dmg_v004.<UDIM>.tx"
		}
		return 137, "OOMKilled: exceeded 48 GiB GPU memory in volume pass"
	}
	return 1, "exit status 1"
}

// dispatch fills free capacity: deadline need first, then least slack.
func (s *Sim) dispatch() {
	reserved := map[string]bool{}
	for _, j := range s.jobs {
		if j.ML != nil && j.ML.resumePending {
			if s.mlTryStart(j) {
				continue
			}
			for _, p := range j.Pools {
				reserved[p] = true
			}
		}
	}
	ranked := s.ranked()
	for pass := 0; pass < 2; pass++ {
		for _, j := range ranked {
			if !s.runnable(j) {
				continue
			}
			for guard := 0; guard < 64; guard++ {
				run := s.running(j)
				if run >= j.Cap {
					break
				}
				if pass == 0 && float64(run) >= math.Ceil(j.est.Required*j.Boost) {
					break
				}
				cands := s.nextUnits(j, 3)
				started := false
				for _, u := range cands {
					n := s.pickNode(j, u, reserved)
					if n == nil {
						continue
					}
					s.start(j, u, n)
					started = true
					break
				}
				if !started {
					break
				}
			}
		}
	}
}

// ranked returns jobs in dispatch order.
func (s *Sim) ranked() []*Job {
	js := append([]*Job(nil), s.jobs...)
	sort.SliceStable(js, func(a, b int) bool {
		x, y := js[a], js[b]
		if x.rankScore != y.rankScore {
			return x.rankScore < y.rankScore
		}
		switch x.rankScore {
		case 0:
			rx := float64(x.est.Running) / math.Max(x.est.Required, 0.01)
			ry := float64(y.est.Running) / math.Max(y.est.Required, 0.01)
			if math.Abs(rx-ry) > 0.1 {
				return rx < ry
			}
			return x.est.Slack < y.est.Slack
		case 1:
			return x.est.Slack < y.est.Slack
		case 2:
			return x.est.W < y.est.W
		case 3:
			return x.eff < y.eff
		}
		return x.Created < y.Created
	})
	return js
}

// mustHaveDeadline returns the job's effective deadline in simulated seconds.
func (s *Sim) effDeadline(j *Job) (float64, bool) {
	return j.eff, j.hasEff
}

func (s *Sim) projectOf(key string) string {
	if j := s.jobBy[key]; j != nil {
		return j.Project
	}
	return ""
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
func round2(x float64) float64 { return math.Round(x*100) / 100 }
