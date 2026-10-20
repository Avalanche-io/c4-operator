// Package queue is the work system's controller (DESIGN.md section 10.2):
// every tick it reads pools, projects, jobs and unit pods, asks the
// scheduling policy (work/sched) what to release, preempt and
// request, and carries that out. Units become pods only when they can
// run; kube-scheduler places them.
package queue

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/order"
	"github.com/Avalanche-io/c4-operator/work/process"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// SlotResource is the extended resource each pool node advertises, one per
// slot, and each unit pod requests. kube-scheduler then places units by
// slot, as the queue counts them.
const SlotResource corev1.ResourceName = "c4.avalanche.io/slots"

// Queue is the controller. Run it under leader election: it keeps state in
// memory between ticks (rebuilt from WorkJob status on start).
type Queue struct {
	Client client.Client
	// Every is the tick period (default 2 s).
	Every  time.Duration
	Policy sched.Policy
	Cfg    sched.Config
	Log    logr.Logger
	Now    func() time.Time
	// CaptureWait bounds how long a finished unit waits for its capture
	// annotation before it counts as done without one.
	CaptureWait time.Duration

	mu      sync.Mutex
	folds   int
	st      sched.State
	jobs    map[types.UID]*jobMem
	spentMu sync.Mutex
	spent   map[string]float64 // burst dollars per project today
	day     int
	last    time.Time
	// Loc is the farm's time zone, for the times in estimate messages.
	Loc *time.Location
	// Signer signs the queue's decisions and job records; nil signs none.
	Signer *provenance.Signer
	// PolicyName names the policy in decision records.
	PolicyName string
	// Placement (experimental) locates pools at sites and prices moving a
	// job's inputs between them (placement.go); nil keeps every pool a job
	// may use as holding its inputs.
	Placement Placement
	// Stores opens a project's store, to fold unit changesets into job
	// outputs. Without it, outputs are not folded.
	Stores StoreFunc
	// tlog is the transition log's head (book.go), read once per leader.
	tlog *bookLog
	// fc is the last forecast pass (forecast.go).
	fc forecastMem
	// PriorityClass, when it names a PriorityClass that exists, is set on
	// unit pods whose template names none (config/manager: c4-unit, with
	// preemptionPolicy Never, so only the queue preempts units).
	PriorityClass string
	// HolderTransfer is the farm's switch for holder-transfer credential
	// leases (lease.go).
	HolderTransfer bool
	pcReady        bool
	pcMissing      bool // logged once
	// Namespace is the operator's namespace, where the queue keeps the
	// pointers to each project's saved calibrations (persist.go). Empty,
	// or without Stores, calibrations are not saved.
	Namespace string
	// EgressProxy is the farm's egress proxy, [<namespace>/]<service>:<port>
	// (the operator's namespace by default): units whose tool
	// allows hosts reach them through it (tools.go). Empty: none.
	EgressProxy string
	// HUDService is the HUD's Service, [<namespace>/]<service>:<port>:
	// units whose tool grants c4 reach it (tools.go).
	HUDService string
	// cal holds live calibrations, shareHist the projects that share
	// theirs (calibrate.go), calp their persistence (persist.go).
	cal       calibrations
	fcal      finishCals
	shareHist map[string]bool
	calp      calPersist
	// durations are recent unit run times by project and class, the
	// prior for work with no history of its own (classprior.go).
	durations map[string]classDurations
	// rework is each project's review and rerun history (rework.go).
	rework map[string]map[string]*reworkStat
	// rounds is each project's loop history by class and generator key
	// (rounds.go).
	rounds map[string]map[string]*roundsStat
	// profiles are the latest finished job's durations per project, shot,
	// class and unit key (history.go).
	profiles map[profileKey]*shotProfile
	// books are each project's derivations, digests the image digest last
	// seen for each image reference, and lastPlatform each job's GPU
	// platform (derive.go).
	books        map[string]*process.Book
	digests      map[string]string
	lastPlatform map[string]string
	// milestones are the Milestones by namespace at the last tick
	// (milestone.go).
	milestones map[string][]*c4v1.Milestone
	// sites is each pool's site at the last tick (recompute.go). derivMu
	// guards it and the books map: the Placement reads both during a tick,
	// which holds mu.
	sites map[string]string
	// poolOwners are the owners each pool runs work for, at the last tick
	// (Pool spec.owners; none is anyone's). derivMu guards it too.
	poolOwners map[string][]string
	// subs are the input substitutes by project, site and view.
	subs    map[string]map[string]map[string]substitute
	derivMu sync.Mutex
	// Progress, when set, holds the progress running units report
	// (progress.go); without it units are estimated from elapsed time.
	Progress *ProgressCache
	// Safepoints, when set, holds the safe points running units announce
	// (safepoint.go), filled like Progress: a unit that reports them may be
	// stopped softly, at its next one.
	Safepoints *SafepointCache
	// policies are the projects' deadline policies by project and class.
	policies map[string]map[string]forecast.Policy
	// Last decisions, for the HUD, and each job's deadline policy then
	// (DeadlineOptions).
	lastSnap sched.Snapshot
	lastDec  sched.Decisions
	lastPol  map[string]forecast.Policy
	lastAdj  map[string]func(time.Time, forecast.Band) forecast.Band
	// approvals are the jobs a capacity request waits on a person for.
	approvals map[string]bool
}

// NeedLeaderElection makes the queue run on one replica only.
func (q *Queue) NeedLeaderElection() bool { return true }

// Start runs ticks until ctx ends.
func (q *Queue) Start(ctx context.Context) error {
	if q.Every <= 0 {
		q.Every = 2 * time.Second
	}
	if q.Policy == nil {
		q.Policy = sched.Decide
	}
	if q.Now == nil {
		loc := q.Loc
		if loc == nil {
			loc = time.UTC
		}
		// The policy writes times into its messages; they read in the
		// farm's own time zone.
		q.Now = func() time.Time { return time.Now().In(loc) }
	}
	if q.CaptureWait <= 0 {
		q.CaptureWait = 2 * time.Minute
	}
	t := time.NewTicker(q.Every)
	defer t.Stop()
	for {
		if err := q.Tick(ctx); err != nil && ctx.Err() == nil {
			q.Log.Error(err, "tick")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// jobMem is what the queue knows about a job beyond its pods. It is
// written to status every tick and rebuilt from status on start.
type jobMem struct {
	key     string // namespace/name
	plan    *class.Plan
	planGen int64
	planErr error
	// expansions are the generators evaluated, in order; docs caches
	// their documents; unitOut is each succeeded unit's captured output
	// view, for units that read it (graph.go).
	expansions []expansionRec
	docs       map[string][]byte
	unitOut    map[int]string
	// gateSince is when each unit waiting for a gate joined its line.
	gateSince map[int]time.Time
	// measure is the latest reading of the job's measure (measure.go).
	measure *c4v1.WorkMeasure
	// reused are units that succeeded by reusing an earlier run's output
	// (reuse.go), with that output.
	reused map[int]string
	// spend is each unit's spend since the queue started, spentPod the
	// pods counted, and spendBase and usageBase what status recorded
	// before (spend.go).
	spend               map[int]*unitSpend
	spentPod            map[types.UID]bool
	procBase, procHours float64
	// firstMillis is each unit's first pass run time, to price its reruns
	// (rework.go).
	firstMillis map[int]int64
	// substituted are the input substitutes its units read, by mount and
	// site (substitute.go).
	substituted map[string]c4v1.WorkInputSubstitute
	// verdicts are the verdicts carried out, and reviews what they last
	// decided per unit (verdict.go).
	verdicts []verdictRec
	reviews  map[int]*c4v1.WorkReview
	// interruptions are attempts the farm stopped (interrupt.go).
	interruptions  []interruptRec
	interruptCount int
	succeeded      map[int]bool
	failed         map[int]bool
	attempts       map[int]int // attempts made, including the running one
	millis         map[int]int64
	out            *output
	foldedPod      map[types.UID]bool
	// causes are the job's failures and flags by cause (causes.go).
	causes        map[string]*causeRec
	causesDropped int
	outLoad       string // the stored chain to load before the next fold
	outIndex      string // the stored provenance index to load before the next fold
	launched      map[int]time.Time
	nodeFails     map[string]int
	blacklist     map[string]bool
	// resume holds, per unit, the view a yielded attempt captured: the
	// next attempt mounts it and continues from its checkpoint.
	resume map[int]string
	// yieldAsked is when a unit was asked to yield.
	yieldAsked map[int]time.Time
	// moveTo is the pool a unit moved at a safe point runs in next
	// (safepoint_decide.go).
	moveTo      map[int]string
	preemptions map[int]int
	preemptBase int // preemptions counted before the last restart
	yields      []yieldRecord
	requeued    string
	// accepted are failed units a person accepted (also in failed);
	// acks the standing acknowledgements, acked the last request's token
	// (states.go).
	accepted map[int]bool
	// superseded units ended because a review chose others (verdict.go);
	// refold says the output still holds their changesets.
	superseded map[int]bool
	refold     bool
	// steers are messages to units while they run, and steered the last
	// request's token (steer.go).
	steers  []*steerRec
	steered string
	// unitFailed are the nodes each unit failed on, which its retries
	// avoid (avoidFor). In memory: after a restart a retry may go back
	// once.
	unitFailed map[int]map[string]bool
	// toolsWait says why a unit waits for its tool (tools.go).
	toolsWait string
	// needs are what units stopped for, and supplied the last supply's
	// token (needs.go).
	needs    map[int]*needsRec
	supplied string
	// asks are the questions units asked one another, and woken the ones
	// a finished answerer was woken for (asks.go).
	asks      []*askRec
	woken     map[string]bool
	acks      []*ackRecord
	acked     string
	decisions []decisionRecord
	// waits are the job's wait units and escalated units, open and
	// decided (wait.go).
	waits *waitMem
	// loopsNoted are the loops (by first generator) whose rounds the
	// project's history counts (rounds.go).
	loopsNoted map[int]bool
	jobRecord  string
	lastRun    time.Time
	readyAt    time.Time
	started    time.Time
	finished   time.Time
	wrote      time.Time
	// ended is when each succeeded unit finished, and calPts the
	// summaries kept for live calibration (calibrate.go).
	ended   map[int]time.Time
	calPts  []calPoint
	learned bool
	// sugg is the job's suggested deadline (suggest.go).
	sugg *suggestMem
	// finPts are the medians kept for live finish calibration.
	// profiled: the job's durations are in the shot history.
	profiled bool
	// proc is the process of the job's last recorded derivation, and
	// unsealedWhy why its pod was not sealed (derive.go).
	proc, unsealedWhy string
	finPts            []finishPoint
	finLast           int
	finLearned        bool
}

func newMem() *jobMem {
	return &jobMem{succeeded: map[int]bool{}, failed: map[int]bool{}, accepted: map[int]bool{}, superseded: map[int]bool{}, attempts: map[int]int{},
		millis: map[int]int64{}, launched: map[int]time.Time{}, nodeFails: map[string]int{}, blacklist: map[string]bool{},
		resume: map[int]string{}, yieldAsked: map[int]time.Time{}, preemptions: map[int]int{},
		out: newOutput(), foldedPod: map[types.UID]bool{}, ended: map[int]time.Time{}, unitOut: map[int]string{}}
}

// yieldRecord is one yield of a checkpointing unit.
type yieldRecord struct {
	Unit   int
	At     time.Time
	Reason string
	View   string
}

func (m *jobMem) load(spec *c4v1.WorkJobSpec, st *c4v1.WorkJobStatus) {
	for _, i := range decode(st.UnitStates["succeeded"]) {
		m.succeeded[i] = true
	}
	for _, i := range decode(st.UnitStates["failed"]) {
		m.failed[i] = true
	}
	for _, i := range decode(st.UnitStates["superseded"]) {
		m.superseded[i] = true
		m.refold = true // the same view again if it was refolded
	}
	for _, pair := range strings.Split(st.Attempts, ",") {
		i, n, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		ii, err1 := strconv.Atoi(i)
		nn, err2 := strconv.Atoi(n)
		if err1 == nil && err2 == nil {
			m.attempts[ii] = nn
		}
	}
	for i, ms := range st.UnitMillis {
		if ms > 0 {
			m.millis[i] = ms
		}
	}
	m.requeued = st.Requeued
	m.loadAcks(st)
	m.loadWaits(st)
	m.loadSteers(st)
	m.loadNeeds(st)
	m.loadCauses(st)
	m.loadAsks(st)
	m.loadLoops(st)
	// Yields and preemptions before a restart stay in the record.
	for _, y := range st.Yields {
		m.yields = append(m.yields, yieldRecord{Unit: int(y.Unit), At: y.At.Time, Reason: y.Reason, View: y.View})
	}
	m.preemptBase = int(st.Preemptions)
	m.sugg = loadSuggestion(st.Suggestion)
	for _, d := range st.Decisions {
		m.decisions = append(m.decisions, decisionRecord{Unit: int(d.Unit), At: d.At.Time, Kind: d.Kind, Reason: d.Reason, For: d.For, Record: d.Record})
	}
	if st.Output != nil {
		m.jobRecord = st.Output.Record
	}
	if o := st.Output; o != nil {
		m.outLoad = o.Chain
		m.outIndex = o.Provenance
		m.out.count = int(o.Changesets)
	}
	for k, view := range st.Resume {
		if i, err := strconv.Atoi(k); err == nil {
			m.resume[i] = view
		}
	}
	for _, x := range st.Expansions {
		m.expansions = append(m.expansions, expansionRec{Unit: int(x.Unit), First: int(x.First), Units: int(x.Units),
			Doc: x.Document, Record: x.Record, Error: x.Error, At: x.At.Time})
	}
	for k, view := range st.UnitOutputs {
		if i, err := strconv.Atoi(k); err == nil {
			m.unitOut[i] = view
		}
	}
	for _, x := range st.Interruptions {
		m.interruptions = append(m.interruptions, interruptRec{Unit: int(x.Unit), At: x.At.Time, Reason: x.Reason, Node: x.Node})
	}
	m.interruptCount = int(st.Interrupted)
	if st.Measure != nil && st.Measure.Unit >= 0 && st.Measure.Document != "" {
		x := *st.Measure
		m.measure = &x
	}
	m.loadSpend(st)
	m.loadSubstitutes(st)
	for _, i := range decode(st.Reused) {
		if m.reused == nil {
			m.reused = map[int]string{}
		}
		m.reused[i] = m.unitOut[i]
	}
	for _, g := range st.Gates {
		if g.Position > 0 && g.Since != nil {
			if m.gateSince == nil {
				m.gateSince = map[int]time.Time{}
			}
			m.gateSince[int(g.Unit)] = g.Since.Time
		}
	}
	for _, x := range st.Verdicts {
		m.verdicts = append(m.verdicts, verdictRec{WorkVerdict: x, pod: x.Pod})
	}
	for k, r := range st.Reviews {
		if i, err := strconv.Atoi(k); err == nil {
			if m.reviews == nil {
				m.reviews = map[int]*c4v1.WorkReview{}
			}
			r := r
			m.reviews[i] = &r
		}
	}
	// Units a review escalated wait for a person as failed units do.
	for _, i := range decode(st.UnitStates["flagged"]) {
		m.failed[i] = true
	}
	for _, n := range st.Blacklist {
		if !slices.Contains(spec.AvoidNodes, n) {
			m.blacklist[n] = true
		}
	}
	if st.Started != nil {
		m.started = st.Started.Time
	}
	if st.Finished != nil {
		m.finished = st.Finished.Time
	}
}

func decode(s string) []int {
	out, _ := order.DecodeSet(s)
	return out
}

// view is one job during a tick.
type view struct {
	job       *c4v1.WorkJob
	key       string // namespace/name
	mem       *jobMem
	driver    class.Driver
	project   string
	obj       class.Objective
	pods      map[int]*corev1.Pod // live unit pods by index
	done      []*corev1.Pod       // terminal pods to delete once recorded
	running   map[int]runInfo
	finishing map[int]bool // succeeded, waiting for capture
	preflight map[int]bool // live pods still preparing
	runnable  []int
	blocked   bool
	// after lists the jobs it waits on that have not succeeded (keys).
	after []string
	// stuckOn names a dependency that failed: the job cannot start.
	stuckOn string
	// unschedulable says why no unit of the job can ever be placed.
	unschedulable string
	// avoid is the job's blacklist: automatic plus person-set nodes.
	avoid map[string]bool
	pools []string
	sum   estimate.Summary
	est   *estimate.Estimator
	// source is where the effective deadline comes from: spec,
	// suggested or class.
	source string
	// milestone is the Milestone its deadline comes from (milestone.go).
	milestone string
	// sources are the ways to get its inputs to pools at other sites
	// (placement.go), by pool.
	sources map[string][]sched.Source
	// subs are the input substitutes it reads at each site, by input
	// index (substitute.go).
	subs map[string]map[int]substitute
	// safe is each running unit's last safe point (safepoint.go).
	safe map[int]Safepoint
	// waiting are units blocked on units of their own job, and stuck
	// those that wait on a failed one (graph.go); planWait says why the
	// unit table could not be rebuilt yet.
	waiting []int
	stuck   map[int]bool
	// openWaits are the wait units whose units are done, waiting for
	// their decision (wait.go).
	openWaits map[int]bool
	// runRem is each running unit's expected time left, in seconds, for
	// the policy (sched.RunningUnit.Remaining).
	runRem map[int]float64
	// waking are the finished units released this tick only to answer
	// questions, with the questions' ids (asks.go).
	waking map[int][]string
	// roundsExp is what the queue expects of each loop's remaining rounds,
	// by first generator (rounds.go).
	roundsExp map[int]roundsExp
	planWait  string
	// locked are units waiting for a holder of what they claim (holds.go)
	// or for a gate, and gates the job's units at gates (gates.go).
	locked map[int]lockInfo
	gates  []c4v1.WorkGateEntry
	// measureShort says how a finished job falls short of its required
	// measure (measure.go).
	measureShort string
}

type runInfo struct {
	pool    string
	slots   int
	started time.Time
	node    string
}

// Tick is one round: observe, decide, act, report.
func (q *Queue) Tick(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.jobs == nil {
		q.jobs = map[types.UID]*jobMem{}
		q.spentMu.Lock()
		q.spent = map[string]float64{}
		q.spentMu.Unlock()
	}
	q.folds = 0
	now := q.Now()
	if d := now.YearDay(); d != q.day {
		q.spentMu.Lock()
		q.day, q.spent = d, map[string]float64{}
		q.spentMu.Unlock()
	}

	var (
		pools    c4v1.PoolList
		nodes    corev1.NodeList
		jobs     c4v1.WorkJobList
		pods     corev1.PodList
		projects c4v1.ProjectList
		crs      c4v1.CapacityRequestList
	)
	for _, l := range []struct {
		list client.ObjectList
		opts []client.ListOption
	}{
		{&pools, nil}, {&nodes, nil}, {&jobs, nil}, {&projects, nil}, {&crs, nil},
		{&pods, []client.ListOption{client.HasLabels{c4v1.LabelJob}}},
	} {
		if err := q.Client.List(ctx, l.list, l.opts...); err != nil {
			return err
		}
	}

	q.milestones = q.listMilestones(ctx)
	poolInfo := q.pools(ctx, pools.Items, nodes.Items, pods.Items, crs.Items)
	sites, owners := map[string]string{}, map[string][]string{}
	for name, p := range poolInfo {
		sites[name] = p.site
		if o := p.spec.Spec.Owners; len(o) > 0 {
			owners[name] = o
		}
	}
	q.derivMu.Lock()
	q.sites, q.poolOwners = sites, owners
	q.derivMu.Unlock()
	nsProject := map[string]string{}
	q.shareHist = map[string]bool{}
	for _, p := range projects.Items {
		for _, ns := range p.Spec.Namespaces {
			nsProject[ns] = p.Name
		}
		if w := p.Spec.Work; w != nil && w.ShareHistory == "aggregate" {
			q.shareHist[p.Name] = true
		}
	}
	q.loadCalibrations(ctx)
	q.loadPolicies(projects.Items)
	q.approvals = approvals(crs.Items)
	q.noteSubstitutes(jobs.Items, nsProject)
	podsByJob := map[string][]*corev1.Pod{}
	for i := range pods.Items {
		p := &pods.Items[i]
		key := p.Namespace + "/" + p.Labels[c4v1.LabelJob]
		podsByJob[key] = append(podsByJob[key], p)
	}
	phases := map[string]string{}
	for _, j := range jobs.Items {
		phases[j.Namespace+"/"+j.Name] = j.Status.Phase
	}

	seen := map[types.UID]bool{}
	var views, fviews []*view
	for i := range jobs.Items {
		j := &jobs.Items[i]
		seen[j.UID] = true
		v := q.observe(ctx, j, podsByJob[j.Namespace+"/"+j.Name], phases, nsProject, poolInfo, now)
		if v == nil {
			continue
		}
		if isForecast(j) {
			// A forecast is estimated, never decided on or released
			// (forecast.go).
			fviews = append(fviews, v)
			continue
		}
		views = append(views, v)
		q.noteProfiles(v)
	}
	q.releaseOrphans(ctx, podsByJob, phases)
	for uid := range q.jobs {
		if !seen[uid] {
			delete(q.jobs, uid)
		}
	}

	q.applyGates(views, now)
	q.placeJobs(ctx, views, poolInfo)
	q.noteSafepoints(views, now)
	snap := q.snapshot(now, poolInfo, projects.Items, views)
	cfg := q.Cfg
	if cfg.Loc == nil {
		cfg.Loc = q.Loc
	}
	dec := q.Policy(snap, &q.st, cfg)
	pol := make(map[string]forecast.Policy, len(views))
	adj := make(map[string]func(time.Time, forecast.Band) forecast.Band, len(views))
	for _, v := range views {
		pol[v.key] = q.policyFor(v)
		if a := q.adjustFor(v); a != nil {
			adj[v.key] = a
		}
	}
	q.lastSnap, q.lastDec, q.lastPol, q.lastAdj = snap, dec, pol, adj
	q.suggest(ctx, now, snap, dec, views)
	q.checkPriorityClass(ctx)
	q.act(ctx, now, dec, views, poolInfo)
	q.decideSafepoints(ctx, now, dec, views)
	for _, v := range views {
		q.routeAsks(ctx, v, now)
	}
	q.deliverSteers(ctx, now, views)
	q.stage(ctx, dec, views, poolInfo)
	q.meter(now, views, poolInfo)
	q.capacity(ctx, now, dec, poolInfo, crs.Items)
	q.forecastPass(now, snap, cfg, dec, views, fviews)
	q.report(ctx, now, dec, views)
	q.report(ctx, now, q.fc.dec, fviews)
	q.milestoneStatus(ctx, q.milestones, jobs.Items)
	q.saveCalibrations(ctx, now)
	q.last = now
	return nil
}

// observe folds a job's pods into its memory and works out what is
// runnable. Terminal pods are recorded here and deleted after the status
// that records them is written.
func (q *Queue) observe(ctx context.Context, j *c4v1.WorkJob, pods []*corev1.Pod, phases, nsProject map[string]string, pools map[string]*pool, now time.Time) *view {
	m, ok := q.jobs[j.UID]
	if !ok {
		m = newMem()
		m.load(&j.Spec, &j.Status)
		q.jobs[j.UID] = m
	}
	v := &view{job: j, key: j.Namespace + "/" + j.Name, mem: m, pods: map[int]*corev1.Pod{},
		running: map[int]runInfo{}, finishing: map[int]bool{}, preflight: map[int]bool{}}
	v.project = nsProject[j.Namespace]
	if v.project == "" {
		v.project = "namespace/" + j.Namespace
	}
	d, err := class.Lookup(j.Spec.Class)
	if err != nil {
		m.planErr = err
		return v
	}
	v.driver = d
	m.key = v.key
	if m.plan == nil || m.planGen != j.Generation {
		p, err := q.planJob(ctx, v)
		var loading errLoading
		switch {
		case errors.As(err, &loading):
			// The store did not give back an expansion document yet: keep
			// the table as it was, or wait for one.
			v.planWait = loading.Error()
			q.Log.Error(err, "plan", "job", v.key)
			if m.plan == nil {
				return v
			}
		default:
			m.plan, m.planErr, m.planGen = p, err, j.Generation
		}
	}
	if m.planErr != nil {
		return v
	}
	plan := m.plan
	v.pools = j.Spec.Pools
	if len(v.pools) == 0 {
		v.pools = d.Pools()
	}
	v.pools, v.unschedulable = q.servingPools(j, v.pools)
	if isForecast(j) {
		q.observeForecast(v, pools, now)
		return v
	}
	// A person's requeue request: run these units again.
	if req := j.Annotations[c4v1.AnnotationRequeue]; req != "" {
		token, set, _ := strings.Cut(req, ":")
		if token != m.requeued {
			for _, i := range decode(set) {
				if i < 0 || i >= len(plan.Units) {
					continue
				}
				delete(m.succeeded, i)
				delete(m.failed, i)
				delete(m.millis, i)
				delete(m.ended, i)
				m.forgetAcks(i)
				m.attempts[i] = 0
			}
			// Work run again is reviewed again: the job's review reopens,
			// so an earlier approval does not pass the new work.
			if r, ok := plan.Review(); ok && len(decode(set)) > 0 {
				delete(m.succeeded, r)
				delete(m.failed, r)
				delete(m.millis, r)
				delete(m.ended, r)
				delete(m.waitRecs().recs, r)
			}
			m.requeued = token
			m.finished = time.Time{}
			// Moments planned before the requeue did not foresee it.
			m.calPts, m.learned = nil, true
			m.finPts, m.finLearned = nil, true
		}
	}
	// People taking or accepting failed units.
	applyAcks(j, m, len(plan.Units))
	// People deciding what waits for them (wait.go).
	applyDecides(j, m, now)
	// People steering units while they run (steer.go).
	applySteers(j, m)
	// People supplying what units stopped for (needs.go).
	applySupplies(j, m)
	// Nodes a person allowed again leave the automatic blacklist.
	for _, n := range strings.Split(j.Annotations[c4v1.AnnotationAllowNodes], ",") {
		if n = strings.TrimSpace(n); n != "" && m.blacklist[n] {
			delete(m.blacklist, n)
			delete(m.nodeFails, n)
		}
	}
	v.avoid = map[string]bool{}
	for n := range m.blacklist {
		v.avoid[n] = true
	}
	for _, n := range j.Spec.AvoidNodes {
		v.avoid[n] = true
	}
	// A unit larger than any node of the job's pools would wait forever.
	widest, known := 0, false
	for _, name := range v.pools {
		if p := pools[name]; p != nil {
			widest, known = max(widest, p.perNode), true
		}
	}
	switch {
	case v.unschedulable != "":
	case !known:
		v.unschedulable = fmt.Sprintf("none of the job's pools %v exists", v.pools)
	case plan.Units[0].Slots > widest:
		v.unschedulable = fmt.Sprintf("a unit needs %d slots and the largest node in pools %v has %d", plan.Units[0].Slots, v.pools, widest)
	}
	retries := 2
	if j.Spec.Retries != nil {
		retries = int(*j.Spec.Retries)
	}
	limit := int(j.Spec.NodeErrorLimit)
	if limit <= 0 {
		limit = 3
	}
	capture := needsCapture(j)

	for _, p := range pods {
		if !metaOwned(p, j) {
			continue
		}
		idx, err := strconv.Atoi(p.Labels[c4v1.LabelUnit])
		if err != nil || idx < 0 || idx >= len(plan.Units) {
			continue
		}
		delete(m.launched, idx)
		if p.Annotations[c4v1.AnnotationWake] != "" {
			// An answerer woken to answer: its end leaves the unit as it
			// was, and nothing it wrote is kept (asks.go).
			switch {
			case p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed:
				if !m.foldedPod[p.UID] {
					q.endWake(ctx, v, idx, p, now)
					m.foldedPod[p.UID] = true
				}
				v.done = append(v.done, p)
			case p.DeletionTimestamp != nil && letGo(p, now):
				if err := q.release(ctx, p); err != nil {
					q.Log.Error(err, "release unit pod", "pod", p.Name)
				}
			default:
				v.pods[idx] = p
				v.running[idx] = runInfo{pool: p.Labels[c4v1.LabelPool], slots: plan.Units[idx].Slots, started: startOf(p), node: p.Spec.NodeName}
			}
			continue
		}
		if m.superseded[idx] {
			// A review chose others: the pod is stopped and whatever it
			// did is dropped (verdict.go).
			switch {
			case p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed:
				v.done = append(v.done, p)
			case p.DeletionTimestamp == nil:
				if err := q.Client.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
					q.Log.Error(err, "stop a superseded unit", "pod", p.Name)
				}
			case letGo(p, now):
				if err := q.release(ctx, p); err != nil {
					q.Log.Error(err, "release unit pod", "pod", p.Name)
				}
			}
			continue
		}
		slots := plan.Units[idx].Slots
		switch {
		case p.DeletionTimestamp != nil && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed:
			if letGo(p, now) {
				// Deleted before it was placed, or its node is gone: it
				// will not end on its own, and nothing ran to record.
				if err := q.release(ctx, p); err != nil {
					q.Log.Error(err, "release unit pod", "pod", p.Name)
				}
				if attemptOf(p) == m.attempts[idx] && m.attempts[idx] > 0 && p.Spec.NodeName == "" {
					m.attempts[idx]--
				}
				continue
			}
			// Still running on its node, inside its grace: at its next
			// safe point it is told to stop (safepoint_decide.go).
			v.pods[idx] = p
			v.running[idx] = runInfo{pool: p.Labels[c4v1.LabelPool], slots: slots, started: startOf(p), node: p.Spec.NodeName}
		case p.Status.Phase == corev1.PodSucceeded:
			if capture && p.Annotations[c4v1.AnnotationOutputTree] == "" && now.Sub(finishOf(p, now)) < q.CaptureWait {
				v.finishing[idx] = true
				continue
			}
			if !q.foldPod(ctx, v, p, idx) {
				// The output could not take the unit's changeset yet (the
				// store is unreachable); keep the pod and try next tick.
				v.finishing[idx] = true
				continue
			}
			if !q.noteSpend(ctx, v, idx, p, pools, float64(runMillis(p, now))/3.6e6) {
				v.finishing[idx] = true
				continue
			}
			if reason := p.Annotations[c4v1.AnnotationYield]; reason != "" && !m.succeeded[idx] {
				// A unit that yielded at a checkpoint: its captured view is
				// where the next attempt resumes. Not a failure, not done.
				tree := p.Annotations[c4v1.AnnotationOutputTree]
				if _, ok := d.(class.Resumer); ok {
					tree = p.Annotations[c4v1.AnnotationOutputCheckpoint]
				}
				if tree != "" && m.resume[idx] != tree {
					m.resume[idx] = tree
					m.yields = append(m.yields, yieldRecord{Unit: idx, At: finishOf(p, now), Reason: reason, View: tree})
				}
				if attemptOf(p) == m.attempts[idx] && m.attempts[idx] > 0 {
					m.attempts[idx]--
				}
				delete(m.yieldAsked, idx)
				v.done = append(v.done, p)
				continue
			}
			vid, vdoc, ok := q.verdictOf(ctx, v, p)
			if !ok {
				v.finishing[idx] = true
				continue
			}
			mid, mdoc, ok := q.measureOf(ctx, v, p)
			if !ok {
				v.finishing[idx] = true
				continue
			}
			if !m.succeeded[idx] && !m.failed[idx] && plan.Units[idx].Generator && !m.expanded(idx) {
				ok, refused := q.expand(ctx, v, idx, p, now)
				if !ok {
					v.finishing[idx] = true
					continue
				}
				if refused {
					// A document the queue refused adds nothing: the
					// generator failed, and a person must act.
					m.failed[idx] = true
					v.done = append(v.done, p)
					continue
				}
			}
			if mid != "" && !m.succeeded[idx] {
				q.noteMeasure(v, idx, mid, mdoc, finishOf(p, now))
			}
			rerun := false
			if vid != "" && !m.succeeded[idx] && !m.failed[idx] {
				ok, refused, again := q.carryOut(ctx, v, idx, p, vid, vdoc, now)
				if !ok {
					v.finishing[idx] = true
					continue
				}
				if refused {
					// A verdict the queue refused does nothing: the review
					// failed, and a person must act.
					m.failed[idx] = true
					v.done = append(v.done, p)
					continue
				}
				rerun = again
			}
			if !m.succeeded[idx] {
				m.succeeded[idx] = true
				m.millis[idx] = runMillis(p, now)
				q.noteDuration(v.project, v.job.Spec.Class, float64(m.millis[idx])/1000)
				if idx < len(m.plan.Units) {
					// A rerun (a rework, or a review after one) against the
					// unit's first pass: what reruns of its key cost.
					if first := m.firstMillis[idx]; first > 0 {
						q.noteRerun(v.project, v.job.Spec.Class, m.plan.Units[idx].Key, float64(m.millis[idx])/float64(first))
					} else if m.millis[idx] > 0 {
						if m.firstMillis == nil {
							m.firstMillis = map[int]int64{}
						}
						m.firstMillis[idx] = m.millis[idx]
					}
				}
				m.ended[idx] = finishOf(p, now)
				if tree := p.Annotations[c4v1.AnnotationOutputTree]; tree != "" {
					m.unitOut[idx] = tree
				}
				q.recordDerivation(ctx, v, idx, p, m.millis[idx])
			}
			if rerun {
				// It reworked units: the review runs again after them.
				delete(m.succeeded, idx)
				delete(m.millis, idx)
				delete(m.ended, idx)
				m.attempts[idx] = 0
			}
			v.done = append(v.done, p)
		case p.Status.Phase == corev1.PodFailed && interruptedBy(p) != "":
			// The farm stopped it: no retry spent, no mark on the node.
			// What it ran still cost.
			q.noteSpend(ctx, v, idx, p, pools, float64(runMillis(p, now))/3.6e6)
			if !m.failed[idx] && !m.succeeded[idx] && !m.foldedPod[p.UID] {
				m.interrupted(idx, p, interruptedBy(p), now)
				m.foldedPod[p.UID] = true
			}
			v.done = append(v.done, p)
		case p.Status.Phase == corev1.PodFailed && isNeeds(p):
			// It stopped for information its brief lacked: no retry, no
			// mark on the node (needs.go).
			q.noteSpend(ctx, v, idx, p, pools, float64(runMillis(p, now))/3.6e6)
			if !m.failed[idx] && !m.succeeded[idx] && attemptOf(p) >= m.attempts[idx] {
				q.noteNeeds(ctx, v, idx, p, d, now)
			}
			v.done = append(v.done, p)
		case p.Status.Phase == corev1.PodFailed:
			q.noteSpend(ctx, v, idx, p, pools, float64(runMillis(p, now))/3.6e6)
			// Counted once, though the pod may be seen again before it is
			// cleaned up.
			if !m.failed[idx] && !m.succeeded[idx] && attemptOf(p) >= m.attempts[idx] && !m.foldedPod[p.UID] {
				m.foldedPod[p.UID] = true
				did := "retried"
				if m.attempts[idx] > retries {
					did = "failed"
				}
				key, title := exitCause(p)
				m.noteCause("exit", key, title, idx, p.Spec.NodeName, finishOf(p, now), did)
				if n := p.Spec.NodeName; n != "" {
					// Its retries go elsewhere (avoidFor).
					if m.unitFailed == nil {
						m.unitFailed = map[int]map[string]bool{}
					}
					if m.unitFailed[idx] == nil {
						m.unitFailed[idx] = map[string]bool{}
					}
					m.unitFailed[idx][n] = true
					m.nodeFails[n]++
					if m.nodeFails[n] >= limit {
						if !m.blacklist[n] {
							m.noteCause("node", "node:"+n, fmt.Sprintf("node %s: failed %d units, kept off this job", n, m.nodeFails[n]),
								idx, n, finishOf(p, now), "blacklisted")
							if c := m.causes[key]; c != nil {
								c.addDid("blacklisted")
							}
						}
						m.blacklist[n] = true
					}
				}
				if m.attempts[idx] > retries {
					m.failed[idx] = true
				}
			}
			v.done = append(v.done, p)
		default:
			v.pods[idx] = p
			v.running[idx] = runInfo{pool: p.Labels[c4v1.LabelPool], slots: slots, started: startOf(p), node: p.Spec.NodeName}
			if PreflightOf(p) != nil {
				v.preflight[idx] = true
			}
		}
	}
	// Units launched in an earlier tick whose pods the cache does not show
	// yet hold their slots.
	for idx, at := range m.launched {
		if now.Sub(at) > 30*time.Second {
			delete(m.launched, idx)
			continue
		}
		if _, ok := v.running[idx]; !ok {
			v.running[idx] = runInfo{pool: "", slots: plan.Units[idx].Slots, started: at}
		}
	}

	// Dependencies.
	for _, dep := range j.Spec.DependsOn {
		if ph := phases[j.Namespace+"/"+dep]; ph != "Succeeded" {
			v.blocked = true
			v.after = append(v.after, j.Namespace+"/"+dep)
			if ph == "Failed" && v.stuckOn == "" {
				v.stuckOn = j.Namespace + "/" + dep
			}
		}
	}
	if !v.blocked && m.readyAt.IsZero() {
		m.readyAt = now
		if len(j.Spec.DependsOn) == 0 {
			m.readyAt = j.CreationTimestamp.Time
		}
	}
	// A job with no deadline of its own takes its Milestone's due.
	oj := j
	v.milestone = ""
	if ms := milestoneFor(q.milestones[j.Namespace], j); ms != nil {
		cp := *j
		cp.Spec.Deadline = &metav1.Time{Time: ms.Spec.Due.Time}
		oj, v.milestone = &cp, ms.Name
	}
	v.obj = d.Objective(oj, m.readyAt)
	q.applySuggestion(v)
	if len(v.running) > 0 {
		m.lastRun = now
		if m.started.IsZero() {
			m.started = now
		}
	}
	if m.refold && q.refoldOutput(ctx, v) {
		m.refold = false
	}
	v.runnable, v.waiting, v.stuck = unitWaits(plan, m, v)
	if q.reuseUnits(ctx, v, now) {
		// Units that waited on a reused one may run now.
		v.runnable, v.waiting, v.stuck = unitWaits(plan, m, v)
	}
	q.serveRounds(ctx, v, now)
	q.noteLoops(v)
	v.serveWakes()
	q.serveWaits(v, now)
	applyHolds(v)
	q.estimate(v, now)
	return v
}

// estimate rebuilds the job's estimator from its measured units and
// summarizes the remaining work.
func (q *Queue) estimate(v *view, now time.Time) {
	plan := v.mem.plan
	cfg := plan.Estimate
	cfg.Calibration = q.cal.forJob(v.project, v.job.Spec.Class, q.shareHist[v.project])
	e := estimate.New(cfg)
	byKey := q.shotProfiles(v)
	q.useDerivations(context.Background(), v, byKey)
	for key, byPos := range byKey {
		e.SetProfile(key, byPos)
	}
	hasPrior := map[string]bool{}
	if plan.Prior != nil {
		for _, u := range plan.Units {
			e.SetPrior(u.Key, *plan.Prior)
			hasPrior[u.Key] = true
			break
		}
	}
	unitPriors(e, plan, hasPrior)
	if plan.Prior == nil && len(byKey) == 0 {
		// No guess from the job and no shot history: what the class's
		// units took in the project, rather than the class default.
		if p, ok := q.classPrior(v.project, v.job.Spec.Class); ok {
			for _, u := range plan.Units {
				if !hasPrior[u.Key] {
					e.SetPrior(u.Key, p)
					hasPrior[u.Key] = true
				}
			}
		}
	}
	q.forecastPriors(e, v)
	noteExpansions(e, plan, v.mem)
	// A unit's key learns from its first pass: a rerun (a rework, which
	// resumes from the unit's last output) is priced apart (rework.go),
	// and a unit sent back keeps the pass it ran.
	took := map[int]int64{}
	for i, ms := range v.mem.millis {
		took[i] = ms
	}
	for i, ms := range v.mem.firstMillis {
		if i < len(plan.Units) {
			took[i] = ms
		}
	}
	idxs := make([]int, 0, len(took))
	for i := range took {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		u := plan.Units[i]
		e.Observe(u.Key, u.Pos, float64(took[i])/1000)
	}
	var queued []estimate.Unit
	var run []estimate.RunningUnit
	// A unit running again after a pass (a rework, a review after one)
	// resumes from its last output: what is left is what reruns of its
	// key take (rework.go), counted as work retained.
	retained := func(idx int) float64 {
		u := plan.Units[idx]
		if v.mem.firstMillis[idx] <= 0 {
			return 0
		}
		return max(0, 1-q.reworkFrac(v.project, v.job.Spec.Class, u.Key)) * e.Predict(u.Key, u.Pos).Mean
	}
	unit := func(idx int) estimate.Unit {
		u := plan.Units[idx]
		eu := estimate.Unit{Key: u.Key, Pos: u.Pos, Slots: u.Slots, Exists: u.Exists, Done: retained(idx)}
		if u.Generator && !v.mem.expanded(idx) {
			eu.Gen = u.Key
		}
		return eu
	}
	for _, idx := range v.runnable {
		queued = append(queued, unit(idx))
	}
	for idx := range v.locked {
		queued = append(queued, unit(idx))
	}
	for _, idx := range v.waiting {
		// a wait unit holds no slot: its time is on the critical path only
		if !v.stuck[idx] && !isWait(plan, idx) {
			queued = append(queued, unit(idx))
		}
	}
	for idx := range v.openWaits {
		// a unit waiting on a wait it opened runs again once decided
		if !isWait(plan, idx) {
			queued = append(queued, unit(idx))
		}
	}
	for idx, r := range v.running {
		ru := estimate.RunningUnit{Unit: unit(idx), Elapsed: math.Max(0, now.Sub(r.started).Seconds())}
		if pod := v.pods[idx]; pod != nil {
			ru.Progress, _ = q.Progress.Get(pod.Namespace, pod.Name, now)
		}
		run = append(run, ru)
	}
	reworkUnits, reworkPath := q.expectedRework(v, e)
	queued = append(queued, reworkUnits...)
	roundUnits, roundPath, roundsExp := q.expectedRounds(v, e)
	queued = append(queued, roundUnits...)
	v.roundsExp = roundsExp
	s := e.Summarize(queued, run)
	// The critical path is the longest chain of remaining work through
	// the unit graph; without edges, the longest single remaining unit.
	rem := map[int]float64{}
	v.runRem = map[int]float64{}
	for _, idx := range v.runnable {
		u := plan.Units[idx]
		rem[idx] = e.Predict(u.Key, u.Pos).Mean - retained(idx)
	}
	for idx := range v.locked {
		u := plan.Units[idx]
		rem[idx] = e.Predict(u.Key, u.Pos).Mean - retained(idx)
	}
	for _, idx := range v.waiting {
		if !v.stuck[idx] {
			u := plan.Units[idx]
			rem[idx] = e.Predict(u.Key, u.Pos).Mean
			if isWait(plan, idx) {
				rem[idx] = waitRemaining(plan, v.mem, idx, now)
			}
		}
	}
	for idx := range v.openWaits {
		rem[idx] = waitRemaining(plan, v.mem, idx, now)
		if !isWait(plan, idx) {
			u := plan.Units[idx]
			rem[idx] += math.Max(0, e.Predict(u.Key, u.Pos).Mean-retained(idx))
		}
	}
	for idx, r := range v.running {
		u := plan.Units[idx]
		var progress float64
		if pod := v.pods[idx]; pod != nil {
			progress, _ = q.Progress.Get(pod.Namespace, pod.Name, now)
		}
		rem[idx], _ = e.RemainderAt(e.Predict(u.Key, u.Pos), retained(idx), math.Max(0, now.Sub(r.started).Seconds()), progress)
		v.runRem[idx] = rem[idx]
	}
	for idx, d := range reworkPath {
		if _, ok := rem[idx]; ok {
			rem[idx] += d
		}
	}
	for idx, d := range roundPath {
		if _, ok := rem[idx]; ok {
			rem[idx] += d
		}
	}
	s.TPath = criticalPath(plan, v.mem, rem, func(key string) float64 { return e.Generator(key).Longest })
	if len(rem) <= 2000 {
		// Units that must take turns for what they claim run one after
		// another.
		s.TPath = math.Max(s.TPath, holdChains(plan, rem))
	}
	v.sum, v.est = s, e
	v.mem.notePoint(now, len(v.mem.millis), len(plan.Units), s)
}

// snapshot builds the policy's input.
func (q *Queue) snapshot(now time.Time, pools map[string]*pool, projects []c4v1.Project, views []*view) sched.Snapshot {
	s := sched.Snapshot{Now: now}
	if q.Placement != nil {
		s.Links = q.Placement.Links()
	}
	free := 0
	names := make([]string, 0, len(pools))
	for n := range pools {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := pools[n]
		sp := sched.Pool{Name: n, Slots: p.slots, Used: p.used, Attrs: p.spec.Spec.Attributes, CostPerSlotHour: p.cost}
		if b := p.spec.Spec.Burst; b != nil {
			sp.Burst = &sched.Burst{MaxSlots: int(b.MaxNodes) * p.perNode,
				Provisioning:    time.Duration(b.ProvisioningSeconds) * time.Second,
				CostPerSlotHour: p.burstCost, Requested: p.requested, Provisioned: p.slots}
			sp.CostPerSlotHour = p.burstCost
		}
		free += max(0, p.slots-p.used)
		s.Pools = append(s.Pools, sp)
	}
	for _, p := range projects {
		sp := sched.Project{Name: p.Name, Weight: 1, Quotas: map[string]sched.Quota{}}
		if w := p.Spec.Work; w != nil {
			for _, qt := range w.Quotas {
				bl := -1
				if qt.BorrowingLimit != nil {
					bl = int(*qt.BorrowingLimit)
				}
				sp.Quotas[qt.Pool] = sched.Quota{Nominal: int(qt.Nominal), BorrowingLimit: bl}
			}
			sp.Weight = floatOr(w.FairShareWeight, 1)
			sp.BurstBudgetLeft = math.Max(0, floatOr(w.BurstBudgetPerDay, 0)-q.Spent(p.Name))
			sp.LateCostPerHour = floatOr(w.LateCostPerHour, 0)
		}
		s.Projects = append(s.Projects, sp)
	}
	for _, v := range views {
		if v.mem.plan == nil || v.mem.planErr != nil || v.unschedulable != "" || isFinished(v) {
			continue
		}
		s.Jobs = append(s.Jobs, schedJob(v, free))
	}
	return s
}

// schedJob is a job as the policy sees it; free is the slots free in the
// farm, which bounds how many runnable units are listed.
func schedJob(v *view, free int) sched.Job {
	plan := v.mem.plan
	j := sched.Job{
		ID: v.key, Project: v.project, Class: v.job.Spec.Class,
		Submitted: v.job.CreationTimestamp.Time, Ready: v.mem.readyAt,
		Deadline: v.obj.Deadline, RequiredRate: v.obj.Rate,
		Pools: v.pools, MaxParallel: int(v.job.Spec.MaxParallel),
		SlotsPerUnit: plan.Units[0].Slots, Running: map[string]int{},
		Estimate: v.sum, Hold: v.job.Spec.Nudge.Hold || v.job.Spec.Nudge.HoldStop, HoldStop: v.job.Spec.Nudge.HoldStop,
		Boost: floatOr(v.job.Spec.Nudge.Boost, 0), LastRunning: v.mem.lastRun,
		Preemptible: v.driver.Preemptible(), Checkpoints: v.driver.Checkpoints(),
	}
	j.Sources = v.sources
	// sched propagates deadlines up the dependency graph and starts a
	// waiting job no earlier than its upstream jobs finish (sched.Job.After).
	j.After = v.after
	j.Blocked = len(v.waiting) - len(v.stuck) + len(v.locked)
	if v.blocked {
		j.Blocked += len(v.runnable)
	} else {
		j.RunnableTotal = len(v.runnable)
		k := min(len(v.runnable), free/max(1, j.SlotsPerUnit)+1)
		for _, idx := range v.runnable[:k] {
			u := plan.Units[idx]
			j.Runnable = append(j.Runnable, sched.UnitRef{ID: unitID(v.key, idx), Info: u.Info, Slots: u.Slots})
		}
	}
	for idx, r := range v.running {
		j.Running[r.pool] += r.slots
		ru := sched.RunningUnit{ID: unitID(v.key, idx), Pool: r.pool, Slots: r.slots, Started: r.started, Preemptions: 0}
		if left := v.runRem[idx]; left > 0 {
			ru.Remaining = time.Duration(left * float64(time.Second))
		}
		// What stopping a unit now loses is the work since its last safe
		// point; a unit that reports them can be asked to stop at the next.
		if sp, ok := v.safe[idx]; ok {
			ru.LastCheckpoint = sp.At
			j.Checkpoints = true
		}
		j.RunningUnits = append(j.RunningUnits, ru)
	}
	sort.Slice(j.RunningUnits, func(a, b int) bool { return j.RunningUnits[a].ID < j.RunningUnits[b].ID })
	return j
}

func isFinished(v *view) bool {
	plan := v.mem.plan
	return plan != nil && len(v.mem.succeeded)+len(v.mem.failed)+len(v.stuck) >= len(plan.Units) && len(v.running) == 0
}

// act creates pods for dispatched units and stops preempted ones.
func (q *Queue) act(ctx context.Context, now time.Time, dec sched.Decisions, views []*view, pools map[string]*pool) {
	byKey := map[string]*view{}
	for _, v := range views {
		byKey[v.key] = v
	}
	room := map[string]int{}
	for n, p := range pools {
		room[n] = p.slots - p.used
	}
	for _, d := range dec.Dispatch {
		room[d.Pool] -= d.Slots
	}
	guard := claimGuard{}
	for _, d := range dec.Dispatch {
		key, idx, ok := parseUnitID(d.Unit)
		v := byKey[key]
		p := pools[d.Pool]
		if !ok || v == nil || p == nil {
			continue
		}
		if isForecast(v.job) {
			// Never: a forecast is left out of the live decision
			// (forecast.go), so this is a third guard.
			q.Log.Error(nil, "refusing to release a forecast job's unit", "job", key, "unit", idx)
			continue
		}
		if !guard.admitGate(v, idx) || !guard.admit(v, idx) {
			// Its claims conflict with a unit started this tick: it waits.
			continue
		}
		if to := v.mem.moveTo[idx]; to != "" {
			// Moved at its safe point: it runs in the cheaper pool if
			// that still has room.
			delete(v.mem.moveTo, idx)
			if p2 := pools[to]; p2 != nil && room[to] >= d.Slots {
				room[to] -= d.Slots
				room[d.Pool] += d.Slots
				p = p2
			}
		}
		if ok, err := q.ensureFromJobs(ctx, v, byKey); err != nil || !ok {
			if err != nil {
				q.Log.Error(err, "input from job", "job", key)
			}
			continue
		}
		if _, byCheckpoint := v.driver.(class.Resumer); !byCheckpoint && v.mem.resume[idx] != "" {
			view := v.mem.resume[idx]
			ready, err := q.ensureResume(ctx, v.job, idx, view)
			if err != nil {
				q.Log.Error(err, "resume materialization", "job", key, "unit", idx)
				continue
			}
			if !ready {
				continue
			}
		}
		w := v.mem.rework(idx)
		if w != nil && w.Pool != "" && d.Pool != w.Pool {
			// A rework names the only pool the unit may use.
			p2 := pools[w.Pool]
			if p2 == nil || room[w.Pool] < d.Slots {
				continue
			}
			room[w.Pool] -= d.Slots
			room[d.Pool] += d.Slots
			p = p2
		}
		if w != nil && len(w.Inputs) > 0 {
			ready, err := q.ensureReworkInputs(ctx, v, idx, w)
			if err != nil {
				q.Log.Error(err, "rework input", "job", key, "unit", idx)
				continue
			}
			if !ready {
				continue
			}
		}
		if w := v.mem.blockDecision(idx); w != nil && w.view != "" {
			// The reply to the wait it opened, mounted at $C4_REPLY.
			ready, err := q.ensureMaterialization(ctx, v.job, replyName(v.job.Name, idx), w.view, "")
			if err != nil {
				q.Log.Error(err, "reply", "job", key, "unit", idx)
				continue
			}
			if !ready {
				continue
			}
		}
		if len(v.mem.plan.Units[idx].Inputs) > 0 {
			ready, err := q.ensureUnitInputs(ctx, v, idx)
			if err != nil {
				q.Log.Error(err, "unit input", "job", key, "unit", idx)
				continue
			}
			if !ready {
				continue
			}
		}
		if ok, why := q.leaseReady(ctx, v.job, p.spec.Name, now); !ok {
			q.Log.V(1).Info("waiting for a credential lease", "job", key, "unit", idx, "pool", p.spec.Name, "why", why)
			continue
		}
		tools, ok, why := q.toolsReady(ctx, v, idx)
		if !ok {
			// The job's status says so (condition Tools).
			v.mem.toolsWait = why
			q.Log.V(1).Info("waiting for a tool", "job", key, "unit", idx, "why", why)
			continue
		}
		if tools != nil {
			v.mem.toolsWait = ""
		}
		v.mem.attempts[idx]++
		pod := q.podFor(v, idx, v.mem.attempts[idx], p)
		if ids := v.waking[idx]; len(ids) > 0 {
			// Woken only to answer (asks.go).
			pod.Annotations[c4v1.AnnotationWake] = strings.Join(ids, ",")
			for c := range pod.Spec.Containers {
				pod.Spec.Containers[c].Env = append(pod.Spec.Containers[c].Env, corev1.EnvVar{Name: "C4_ANSWER_ONLY", Value: strings.Join(ids, ",")})
			}
			if v.mem.woken == nil {
				v.mem.woken = map[string]bool{}
			}
			for _, id := range ids {
				v.mem.woken[id] = true
			}
			q.recordDecision(ctx, v, idx, "wake", "to answer "+strings.Join(ids, ", "), "", now)
		}
		if tools != nil {
			applyTools(pod, v.job.Name, tools, q.proxyURL(), q.hudURL())
		}
		q.leaseOutside(ctx, pod, p.spec.Name)
		if err := q.Client.Create(ctx, pod); err != nil {
			v.mem.attempts[idx]--
			q.Log.Error(err, "create unit pod", "job", key, "unit", idx)
			continue
		}
		v.mem.launched[idx] = now
		q.Log.Info("released", "job", key, "unit", idx, "pool", d.Pool, "tier", d.Tier)
	}
	for _, pr := range dec.Preempt {
		key, idx, ok := parseUnitID(pr.Unit)
		v := byKey[key]
		if !ok || v == nil {
			continue
		}
		pod := v.pods[idx]
		if pod == nil {
			continue
		}
		v.mem.preemptions[idx]++
		_, safe := q.safepointOf(v, idx)
		if pr.Yield && (v.driver.Checkpoints() || safe) {
			// A soft stop: ask the unit to stop at its next safe point (a
			// checkpoint, a frame or chunk boundary, the end of a turn).
			// It sees the request through its downward API volume, with
			// the reason and the deadline; c4-unit safepoint tells it to
			// stop there, capture keeps what it saved, and after the
			// deadline the hard stop below ends it anyway.
			if _, asked := v.mem.yieldAsked[idx]; asked {
				continue
			}
			patch := client.MergeFrom(pod.DeepCopy())
			if pod.Annotations == nil {
				pod.Annotations = map[string]string{}
			}
			pod.Annotations[c4v1.AnnotationYield] = string(pr.Reason)
			pod.Annotations[c4v1.AnnotationYieldBy] = now.Add(yieldGrace).UTC().Format(time.RFC3339)
			if err := q.Client.Patch(ctx, pod, patch); err != nil {
				q.Log.Error(err, "ask to yield", "job", key, "unit", idx)
				continue
			}
			v.mem.yieldAsked[idx] = now
			q.recordDecision(ctx, v, idx, "yield", string(pr.Reason), pr.For, now)
			q.Log.Info("yield requested", "job", key, "unit", idx, "reason", pr.Reason, "for", pr.For)
			continue
		}
		// A preempted attempt is not a failure: it does not count toward
		// retries.
		if v.mem.attempts[idx] > 0 {
			v.mem.attempts[idx]--
		}
		if err := q.Client.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			q.Log.Error(err, "preempt", "job", key, "unit", idx)
			continue
		}
		q.recordDecision(ctx, v, idx, "stop", string(pr.Reason), pr.For, now)
		q.Log.Info("preempted", "job", key, "unit", idx, "reason", pr.Reason, "for", pr.For)
	}
	// A unit asked to yield that has not by its deadline is stopped.
	for _, v := range views {
		for idx, at := range v.mem.yieldAsked {
			if now.Sub(at) < yieldGrace {
				continue
			}
			if pod := v.pods[idx]; pod != nil {
				q.Client.Delete(ctx, pod)
			}
			delete(v.mem.yieldAsked, idx)
		}
	}
	// Hold-stop: stop every running unit of the job. A unit asked above to
	// stop softly (its class checkpoints, or it announces safe points) is
	// left to stop at its next safe point (the deadline still stops one
	// that does not), so its work since its last safe point is kept
	// (DESIGN 10.13).
	for _, v := range views {
		if !v.job.Spec.Nudge.HoldStop {
			continue
		}
		for idx, pod := range v.pods {
			_, safe := v.safe[idx]
			if _, asked := v.mem.yieldAsked[idx]; asked && (v.driver.Checkpoints() || safe) {
				continue
			}
			if v.mem.attempts[idx] > 0 {
				v.mem.attempts[idx]--
			}
			q.Client.Delete(ctx, pod)
		}
	}
}

// avoidFor is the nodes unit idx's pod keeps off in pool p: those a person
// named (spec.avoidNodes), the job's automatic blacklist, and the nodes the
// unit itself failed on, so its retries run elsewhere. When that would
// leave it no node of the pool, the unit's own failures give way first,
// then the blacklist: a unit is never kept off every node by the farm's
// guesses, only by a person's choice.
func avoidFor(v *view, idx int, p *pool) []string {
	named := map[string]bool{}
	for _, n := range v.job.Spec.AvoidNodes {
		named[n] = true
	}
	layers := []map[string]bool{named, v.mem.blacklist, v.mem.unitFailed[idx]}
	open := func(k int) bool {
		if len(p.nodes) == 0 {
			return true // nothing to check against (a burst pool not up yet)
		}
		for _, n := range p.nodes {
			out := false
			for _, l := range layers[:k] {
				out = out || l[n]
			}
			if !out {
				return true
			}
		}
		return false
	}
	k := len(layers)
	for k > 1 && !open(k) {
		k--
	}
	set := map[string]bool{}
	for _, l := range layers[:k] {
		for n := range l {
			set[n] = true
		}
	}
	var bad []string
	for n := range set {
		bad = append(bad, n)
	}
	sort.Strings(bad)
	return bad
}

// podFor builds a unit's pod from the job's template.
func (q *Queue) podFor(v *view, idx, attempt int, p *pool) *corev1.Pod {
	j := v.job
	u := v.mem.plan.Units[idx]
	tpl := j.Spec.Template.DeepCopy()
	pod := &corev1.Pod{ObjectMeta: tpl.ObjectMeta, Spec: tpl.Spec}
	// A unit can be launched again after a preemption with the same
	// attempt number, so names are generated; the launch expectation in
	// jobMem keeps a unit from being released twice.
	pod.Name = ""
	pod.GenerateName = fmt.Sprintf("%s-u%d-", j.Name, idx)
	pod.Namespace = j.Namespace
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[c4v1.LabelJob] = j.Name
	pod.Labels[c4v1.LabelUnit] = strconv.Itoa(idx)
	pod.Labels[c4v1.LabelPool] = p.spec.Name
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[c4v1.AnnotationAttempt] = strconv.Itoa(attempt)
	if owner := j.Annotations[c4v1.AnnotationOwner]; owner != "" {
		// whose unit it is: a credential lease serves only its holder's
		pod.Annotations[c4v1.AnnotationOwner] = owner
	}
	steerPod(v.mem, pod, idx, q.Now())
	if ai := j.Annotations[c4v1.AnnotationAIUsage]; ai != "" {
		// Task records carry the job's declared use of models.
		pod.Annotations[c4v1.AnnotationAIUsage] = ai
	}
	t := true
	pod.Finalizers = append(pod.Finalizers, c4v1.FinalizerUnit)
	pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob",
		Name: j.Name, UID: j.UID, Controller: &t, BlockOwnerDeletion: &t}}
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	// A unit holds no Kubernetes API token: it reaches the farm only
	// through what the queue gives it (its control directory, its pod's
	// annotations, and the HUD with a token a tool grants).
	noToken := false
	pod.Spec.AutomountServiceAccountToken = &noToken
	if pod.Spec.PriorityClassName == "" && q.pcReady {
		pod.Spec.PriorityClassName = q.PriorityClass
	}
	if pod.Spec.TerminationGracePeriodSeconds == nil {
		if g := q.graceFor(v); g > 0 {
			s := int64(g / time.Second)
			pod.Spec.TerminationGracePeriodSeconds = &s
		}
	}
	if d := q.doNotDisrupt(v, idx); d > 0 && pod.Annotations[AnnotationDoNotDisrupt] == "" {
		pod.Annotations[AnnotationDoNotDisrupt] = d.String()
	}
	if pod.Spec.NodeSelector == nil {
		pod.Spec.NodeSelector = map[string]string{}
	}
	for k, val := range p.spec.Spec.NodeSelector {
		pod.Spec.NodeSelector[k] = val
	}
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, p.spec.Spec.Tolerations...)
	if bad := avoidFor(v, idx, p); len(bad) > 0 {
		pod.Spec.Affinity = notOnNodes(pod.Spec.Affinity, bad)
	}
	env := append([]corev1.EnvVar{
		{Name: "C4_JOB", Value: j.Name}, {Name: "C4_NAMESPACE", Value: j.Namespace}, {Name: "C4_UNIT", Value: strconv.Itoa(idx)},
		{Name: "C4_ATTEMPT", Value: strconv.Itoa(attempt)}, {Name: "C4_UNIT_LABEL", Value: u.Label},
	}, u.Env...)
	// Every unit sees its pod's annotations, so any unit that announces
	// safe points (c4-unit safepoint) sees a soft stop.
	{
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "c4-podinfo", VolumeSource: corev1.VolumeSource{
			DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: []corev1.DownwardAPIVolumeFile{
				{Path: "annotations", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations"}}}}}})
		for c := range pod.Spec.Containers {
			pod.Spec.Containers[c].VolumeMounts = append(pod.Spec.Containers[c].VolumeMounts,
				corev1.VolumeMount{Name: "c4-podinfo", MountPath: "/etc/c4/podinfo", ReadOnly: true})
		}
		env = append(env, corev1.EnvVar{Name: "C4_PODINFO", Value: "/etc/c4/podinfo/annotations"})
		// The control directory, where the node daemon writes the farm's
		// answer at each safe point (safepoint_decide.go).
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "c4-control",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		for c := range pod.Spec.Containers {
			pod.Spec.Containers[c].VolumeMounts = append(pod.Spec.Containers[c].VolumeMounts,
				corev1.VolumeMount{Name: "c4-control", MountPath: "/etc/c4/control", ReadOnly: true})
		}
		env = append(env, corev1.EnvVar{Name: "C4_CONTROL", Value: "/etc/c4/control"})
	}
	resumed, output := false, false
	for i, in := range j.Spec.Inputs {
		name := fmt.Sprintf("c4-input-%d", i)
		if !in.ReadOnly && !output {
			// C4_OUTPUT is the first writable input: where the unit writes
			// what is captured, .c4/progress and preview/<frame> among it.
			// The same input resumes from a yielded attempt's view.
			env = append(env, corev1.EnvVar{Name: "C4_OUTPUT", Value: in.MountPath})
			output = true
		}
		if in.FromJob != "" {
			in.Materialization = fromJobName(j.Name, in.FromJob)
		}
		if !in.ReadOnly && !resumed && v.mem.resume[idx] != "" {
			// The first writable input resumes from the view the last
			// attempt captured.
			in.Materialization = resumeName(j.Name, idx)
			resumed = true
			env = append(env, corev1.EnvVar{Name: "C4_RESUME_VIEW", Value: v.mem.resume[idx]})
		}
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			CSI: &corev1.CSIVolumeSource{Driver: c4v1.DriverName, ReadOnly: &in.ReadOnly,
				VolumeAttributes: map[string]string{c4v1.VolumeAttrMaterialization: in.Materialization, c4v1.VolumeAttrClass: j.Spec.Class}}}})
		for c := range pod.Spec.Containers {
			pod.Spec.Containers[c].VolumeMounts = append(pod.Spec.Containers[c].VolumeMounts,
				corev1.VolumeMount{Name: name, MountPath: in.MountPath, ReadOnly: in.ReadOnly})
		}
	}
	for k, in := range u.Inputs {
		// Another unit's output, read-only (graph.go).
		name, ro := fmt.Sprintf("c4-unit-input-%d", k), true
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			CSI: &corev1.CSIVolumeSource{Driver: c4v1.DriverName, ReadOnly: &ro,
				VolumeAttributes: map[string]string{c4v1.VolumeAttrMaterialization: unitInputName(j.Name, in.From), c4v1.VolumeAttrClass: j.Spec.Class}}}})
		for c := range pod.Spec.Containers {
			pod.Spec.Containers[c].VolumeMounts = append(pod.Spec.Containers[c].VolumeMounts,
				corev1.VolumeMount{Name: name, MountPath: in.MountPath, ReadOnly: true})
		}
	}
	substitutePod(v, pod, p.site)
	if w := v.mem.rework(idx); w != nil {
		applyRework(pod, j.Name, idx, w)
	}
	if w := v.mem.blockDecision(idx); w != nil {
		applyDecision(pod, j.Name, idx, w)
	}
	if e := suppliedEnv(v.mem, idx); e != nil {
		for c := range pod.Spec.Containers {
			pod.Spec.Containers[c].Env = append(pod.Spec.Containers[c].Env, e...)
		}
	}
	if r, ok := v.driver.(class.Resumer); ok && v.mem.resume[idx] != "" {
		env = append(env, r.ResumeEnv(v.mem.resume[idx])...)
	}
	for c := range pod.Spec.Containers {
		pod.Spec.Containers[c].Env = append(pod.Spec.Containers[c].Env, env...)
	}
	// The first container holds the unit's slots.
	if len(pod.Spec.Containers) > 0 {
		c := &pod.Spec.Containers[0]
		qty := resource.MustParse(strconv.Itoa(u.Slots))
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Requests[SlotResource] = qty
		c.Resources.Limits[SlotResource] = qty
	}
	return pod
}

func notOnNodes(a *corev1.Affinity, nodes []string) *corev1.Affinity {
	if a == nil {
		a = &corev1.Affinity{}
	}
	if a.NodeAffinity == nil {
		a.NodeAffinity = &corev1.NodeAffinity{}
	}
	req := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil {
		req = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}}
	}
	for i := range req.NodeSelectorTerms {
		req.NodeSelectorTerms[i].MatchExpressions = append(req.NodeSelectorTerms[i].MatchExpressions,
			corev1.NodeSelectorRequirement{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpNotIn, Values: nodes})
	}
	a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = req
	return a
}

// meter adds burst spending per project: slots its units hold in burst
// pools, times the pool's burst price, over the time since the last tick.
func (q *Queue) meter(now time.Time, views []*view, pools map[string]*pool) {
	if q.last.IsZero() {
		return
	}
	h := now.Sub(q.last).Hours()
	for _, v := range views {
		for _, r := range v.running {
			p := pools[r.pool]
			if p == nil || p.spec.Spec.Burst == nil {
				continue
			}
			q.spentMu.Lock()
			q.spent[v.project] += float64(r.slots) * p.burstCost * h
			q.spentMu.Unlock()
		}
	}
}

func unitID(key string, idx int) string { return key + "#" + strconv.Itoa(idx) }

// fromJobName is the Materialization through which a job mounts another
// job's output.
func fromJobName(job, from string) string { return fmt.Sprintf("%s-from-%s", job, from) }

// InputMaterialization names the Materialization a job's input mounts.
func InputMaterialization(job string, in c4v1.WorkInput) string {
	if in.FromJob != "" {
		return fromJobName(job, in.FromJob)
	}
	return in.Materialization
}

// resumeName is the Materialization a resumed unit mounts.
func resumeName(job string, idx int) string { return fmt.Sprintf("%s-resume-u%d", job, idx) }

func parseUnitID(id string) (string, int, bool) {
	key, n, ok := strings.Cut(id, "#")
	if !ok {
		return "", 0, false
	}
	i, err := strconv.Atoi(n)
	return key, i, err == nil
}

func needsCapture(j *c4v1.WorkJob) bool {
	for _, in := range j.Spec.Inputs {
		if !in.ReadOnly {
			return true
		}
	}
	return false
}

func metaOwned(p *corev1.Pod, j *c4v1.WorkJob) bool {
	for _, o := range p.OwnerReferences {
		if o.UID == j.UID {
			return true
		}
	}
	return false
}

func attemptOf(p *corev1.Pod) int {
	n, _ := strconv.Atoi(p.Annotations[c4v1.AnnotationAttempt])
	return n
}

func startOf(p *corev1.Pod) time.Time {
	if p.Status.StartTime != nil {
		return p.Status.StartTime.Time
	}
	return p.CreationTimestamp.Time
}

// finishOf is when the unit's containers ended.
func finishOf(p *corev1.Pod, now time.Time) time.Time {
	var end time.Time
	for _, c := range p.Status.ContainerStatuses {
		if t := c.State.Terminated; t != nil && t.FinishedAt.After(end) {
			end = t.FinishedAt.Time
		}
	}
	if end.IsZero() {
		return now
	}
	return end
}

// runMillis is the run time of the unit's containers.
func runMillis(p *corev1.Pod, now time.Time) int64 {
	var start, end time.Time
	for _, c := range p.Status.ContainerStatuses {
		t := c.State.Terminated
		if t == nil {
			continue
		}
		if start.IsZero() || t.StartedAt.Time.Before(start) {
			start = t.StartedAt.Time
		}
		if t.FinishedAt.After(end) {
			end = t.FinishedAt.Time
		}
	}
	if start.IsZero() {
		start, end = startOf(p), finishOf(p, now)
	}
	return max(1, end.Sub(start).Milliseconds())
}

func floatOr(s string, def float64) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	return f
}

// ensureResume makes the Materialization a resumed unit mounts point at the
// view its last attempt captured, and reports whether it is Ready.
func (q *Queue) ensureResume(ctx context.Context, j *c4v1.WorkJob, idx int, view string) (bool, error) {
	return q.ensureMaterialization(ctx, j, resumeName(j.Name, idx), view, "")
}

// ensureMaterialization makes a Materialization the job owns resolve view,
// and reports whether it is Ready.
// ensureMaterialization makes or updates a Materialization of view. record,
// if set, is the signed record of where the view came from (the upstream
// job's record), which the controller verifies and links.
func (q *Queue) ensureMaterialization(ctx context.Context, j *c4v1.WorkJob, name, view, record string) (bool, error) {
	var m c4v1.Materialization
	err := q.Client.Get(ctx, types.NamespacedName{Namespace: j.Namespace, Name: name}, &m)
	if apierrors.IsNotFound(err) {
		t := true
		m = c4v1.Materialization{
			ObjectMeta: metav1.ObjectMeta{Namespace: j.Namespace, Name: name,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob",
					Name: j.Name, UID: j.UID, Controller: &t, BlockOwnerDeletion: &t}}},
			Spec: c4v1.MaterializationSpec{Source: c4v1.ManifestSource{ID: view}},
		}
		if record != "" {
			m.Annotations = map[string]string{c4v1.AnnotationSourceRecord: record}
		}
		return false, q.Client.Create(ctx, &m)
	}
	if err != nil {
		return false, err
	}
	if m.Spec.Source.ID != view {
		patch := client.MergeFrom(m.DeepCopy())
		m.Spec.Source = c4v1.ManifestSource{ID: view}
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		delete(m.Annotations, c4v1.AnnotationSourceRecord)
		if record != "" {
			m.Annotations[c4v1.AnnotationSourceRecord] = record
		}
		return false, q.Client.Patch(ctx, &m, patch)
	}
	return m.Status.Phase == c4v1.PhaseReady && m.Status.ObservedGeneration == m.Generation, nil
}

// foldPod folds a finished unit pod's captured changeset into its job's
// output, once per pod. It reports false when the fold must be retried.
func (q *Queue) foldPod(ctx context.Context, v *view, p *corev1.Pod, idx int) bool {
	m := v.mem
	cs := p.Annotations[c4v1.AnnotationOutputChangeset]
	if cs == "" || m.foldedPod[p.UID] || q.Stores == nil {
		return true
	}
	if !q.foldChangeset(ctx, v, idx, cs, p.Annotations[c4v1.AnnotationOutputProvenance]) {
		return false
	}
	m.foldedPod[p.UID] = true
	return true
}

// foldChangeset folds one changeset of unit idx, with its task record,
// into the job's output. It reports false when it must be tried again.
func (q *Queue) foldChangeset(ctx context.Context, v *view, idx int, cs, record string) bool {
	m := v.mem
	if cs == "" || q.Stores == nil {
		return true
	}
	if q.folds >= maxFoldsPerTick {
		return false
	}
	q.folds++
	st, err := q.Stores(ctx, v.project)
	if err != nil {
		q.Log.Error(err, "open store for fold", "job", v.key)
		return false
	}
	if st == nil {
		// A namespace without a Project has no store to fold into.
		return true
	}
	if err := q.loadOutput(st, v); err != nil {
		q.Log.Error(err, "load output", "job", v.key)
		return false
	}
	if err := q.fold(ctx, st, m.out, idx, cs, record); err != nil {
		q.Log.Error(err, "fold", "job", v.key, "unit", idx)
		return false
	}
	return true
}

// maxFoldsPerTick bounds the store work one tick does.
const maxFoldsPerTick = 32

// Spent is a project's burst spending today, in US dollars.
func (q *Queue) Spent(project string) float64 {
	q.spentMu.Lock()
	defer q.spentMu.Unlock()
	return q.spent[project]
}

// ensureFromJobs makes the Materializations of a job's fromJob inputs
// point at the upstream jobs' output views, and reports whether all are
// Ready. Data flows between jobs as views (DESIGN.md section 10.5).
func (q *Queue) ensureFromJobs(ctx context.Context, v *view, byKey map[string]*view) (bool, error) {
	j := v.job
	for _, in := range j.Spec.Inputs {
		if in.FromJob == "" {
			continue
		}
		var up c4v1.WorkJob
		if err := q.Client.Get(ctx, types.NamespacedName{Namespace: j.Namespace, Name: in.FromJob}, &up); err != nil {
			return false, err
		}
		if up.Status.Phase != "Succeeded" || up.Status.Output == nil || up.Status.Output.View == "" {
			return false, nil
		}
		if q.Signer != nil && up.Status.Output.Record == "" {
			// The upstream job's record names what this input mounts; it
			// is signed when the job ends, within a tick or two.
			return false, nil
		}
		ready, err := q.ensureMaterialization(ctx, j, fromJobName(j.Name, in.FromJob), up.Status.Output.View, up.Status.Output.Record)
		if err != nil || !ready {
			return false, err
		}
	}
	return true, nil
}

// checkPriorityClass notes whether the unit PriorityClass exists: a pod
// naming one that does not is refused at admission.
func (q *Queue) checkPriorityClass(ctx context.Context) {
	q.pcReady = false
	if q.PriorityClass == "" {
		return
	}
	var pc schedulingv1.PriorityClass
	if err := q.Client.Get(ctx, types.NamespacedName{Name: q.PriorityClass}, &pc); err != nil {
		if !q.pcMissing {
			q.Log.Info("unit PriorityClass not usable; unit pods get none", "priorityClass", q.PriorityClass, "err", err.Error())
			q.pcMissing = true
		}
		return
	}
	q.pcReady, q.pcMissing = true, false
}
