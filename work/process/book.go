package process

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// Class is a derivation class (c4 design/derivation-records.md): what a
// verifier could recompute. It is a verification mode, never a grade
// ("class is verification, never worth").
type Class string

const (
	// Exact: a fresh run reproduces the output's bytes. Only exact,
	// verified by re-runs, licenses reuse in place of moving the output.
	Exact Class = "exact"
	// Validated: a fresh run produces an output that passes a stated test,
	// not necessarily the same bytes. Recompute when the consumer accepts
	// a validated result; never reuse.
	Validated Class = "validated"
	// Volatile: a fresh run would not reproduce the output. The recorded
	// output is the only one; recomputing makes a new output, not it.
	Volatile Class = "volatile"
)

// ParseClass reads a declared class; anything else is Volatile, the
// default, since class is testimony a job makes and is never inferred.
func ParseClass(s string) Class {
	switch Class(s) {
	case Exact, Validated:
		return Class(s)
	}
	return Volatile
}

// Run is one succeeded unit.
type Run struct {
	Key Key
	// Program is the process's program (ProgramID): where evidence and
	// class accrue.
	Program string
	Inputs  []Input
	// Declared is the class the job declared for its process, and Sealed
	// says the unit's pod could read nothing outside its closure: no
	// network egress, no host paths, every image pinned by digest. An
	// exact declaration from an unsealed pod is capped at validated, since
	// whatever else it read is not in the record.
	Declared Class
	Sealed   bool
	// Output is the C4 ID of the view the unit wrote (its input view
	// with its changes applied), Changeset the C4 ID of those changes
	// (empty when it changed nothing), and Record its signed task record.
	Output    string
	Changeset string
	Record    string
	// Producer names the unit that ran: its job (namespace/name) and the
	// unit's index in the job's plan. A recompute reruns that unit.
	Producer Producer
	// Work is slot-seconds (run time times slots); Duration seconds.
	Work, Duration float64
	At             time.Time
}

// Producer is the unit that ran a computation.
type Producer struct {
	Job  string `json:"job"`
	Unit int    `json:"unit"`
}

// Cost is what getting an output by recomputing it costs, from the
// record. It is location-free: where inputs and outputs sit, and what
// moving them costs, is the caller's to join.
type Cost struct {
	// Work is slot-seconds to recompute (mean and spread), W90 its 90th
	// percentile, and Longest the longest unit in seconds, the floor on
	// wall time whatever the slots.
	Work    estimate.Prior
	W90     float64
	Longest float64
	// Basis is "exact" (this computation ran before) or "program" (its
	// program ran on other units or inputs).
	Basis string
	Runs  int
	// Class is the process's effective class (Book.Class), and Verified
	// says an exact class has been confirmed by re-runs. Reuse, an
	// existing output of this computation, is set only when the class is
	// Exact and Verified.
	Class    Class
	Verified bool
	Reuse    string
}

// MinVerified is how many matching re-runs verify a declared-exact
// process.
const MinVerified = 2

// maxEntries bounds the computations a Book keeps; the least recently
// run go first.
const maxEntries = 100000

type stats struct {
	N, Mean, M2 float64
}

func (s *stats) add(x float64) {
	s.N++
	d := x - s.Mean
	s.Mean += d / s.N
	s.M2 += d * (x - s.Mean)
}

func (s stats) sd() float64 {
	if s.N < 2 {
		return 0
	}
	return math.Sqrt(s.M2 / (s.N - 1))
}

type entry struct {
	Program  string         `json:"program"`
	Inputs   []Input        `json:"inputs"`
	Work     stats          `json:"work"`
	Duration stats          `json:"duration"`
	Outputs  map[string]int `json:"outputs"`
	Last     time.Time      `json:"last"`
	// Changesets and Records are, per output, the changes that made it
	// and the task record of the run that first wrote it, for reuse.
	Changesets map[string]string `json:"changesets,omitempty"`
	Records    map[string]string `json:"records,omitempty"`
	// Producer is the unit that last ran the computation, the one a
	// recompute reruns (the latest, being the likeliest to still exist).
	Producer *Producer `json:"producer,omitempty"`
}

type procRecord struct {
	Declared   Class `json:"declared"`
	Unsealed   bool  `json:"unsealed"`
	Verified   int   `json:"verified"`
	Mismatched int   `json:"mismatched"`
	Work       stats `json:"work"`
	Duration   stats `json:"duration"`
}

// class is the effective class: the declared one, with an exact
// declaration demoted to validated once a re-run falsified it (c4's rule:
// any sensitivity demotes exact to validated). The demotion is permanent
// for the process; a new image or parameters is a new process.
func (p *procRecord) class() (Class, bool) {
	c, v, _ := p.classWhy()
	return c, v
}

// classWhy also says why an exact declaration is not honored as exact.
func (p *procRecord) classWhy() (Class, bool, string) {
	switch {
	case p == nil:
		return Volatile, false, ""
	case p.Declared == Exact && p.Mismatched > 0:
		return Validated, false, "declared exact, but a re-run wrote a different output"
	case p.Declared == Exact && p.Unsealed:
		return Validated, false, "declared exact, but a unit ran in a pod that was not sealed (network egress, host paths or an image without a digest)"
	case p.Declared == Exact:
		return Exact, p.Verified >= MinVerified, ""
	}
	return p.Declared, false, ""
}

// Book is a record of computations. Keep one per project: input view IDs
// identify content, so a book shared across projects would tell one
// project what another holds. It is safe for concurrent use; the zero
// value is ready.
type Book struct {
	// VerifyRate is the share of reuse opportunities for a deterministic
	// process that are recomputed instead, to keep checking it
	// (NeedsVerification); 0.05 when zero.
	VerifyRate float64

	mu       sync.RWMutex
	entries  map[Key]*entry
	procs    map[string]*procRecord
	byOutput map[string]Key
	dirty    bool
}

func (b *Book) init() {
	if b.entries == nil {
		b.entries = map[Key]*entry{}
		b.procs = map[string]*procRecord{}
		b.byOutput = map[string]Key{}
	}
}

// Record adds a succeeded unit. A run of a computation that ran before is
// a re-run: if it wrote the one output every earlier run wrote, it counts
// toward verifying the process; otherwise it falsifies an exact
// declaration. Re-runs are counted whatever the declared class, so a
// person can see that an undeclared process has repeated so far
// (Evidence) and decide to declare it exact; the book never promotes on
// its own.
func (b *Book) Record(r Run) {
	if r.Key.Process == "" || r.Key.Inputs == "" || r.Output == "" {
		return
	}
	if r.Program == "" {
		r.Program = r.Key.Process
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.init()
	b.dirty = true
	p := b.procs[r.Program]
	if p == nil {
		p = &procRecord{}
		b.procs[r.Program] = p
	}
	p.Declared = ParseClass(string(r.Declared))
	if !r.Sealed {
		p.Unsealed = true
	}
	e := b.entries[r.Key]
	if e == nil {
		e = &entry{Program: r.Program, Inputs: append([]Input(nil), r.Inputs...), Outputs: map[string]int{}}
		b.entries[r.Key] = e
	} else {
		switch _, same := e.Outputs[r.Output]; {
		case same && len(e.Outputs) == 1:
			p.Verified++
		default:
			p.Mismatched++
		}
	}
	e.Outputs[r.Output]++
	if _, ok := e.Changesets[r.Output]; !ok {
		if e.Changesets == nil {
			e.Changesets, e.Records = map[string]string{}, map[string]string{}
		}
		e.Changesets[r.Output], e.Records[r.Output] = r.Changeset, r.Record
	}
	e.Work.add(r.Work)
	e.Duration.add(r.Duration)
	if r.At.After(e.Last) {
		e.Last = r.At
	}
	if r.Producer.Job != "" {
		pr := r.Producer
		e.Producer = &pr
	}
	p.Work.add(r.Work)
	p.Duration.add(r.Duration)
	b.byOutput[r.Output] = r.Key
	b.evict()
}

func (b *Book) evict() {
	if len(b.entries) <= maxEntries {
		return
	}
	type aged struct {
		k  Key
		at time.Time
	}
	all := make([]aged, 0, len(b.entries))
	for k, e := range b.entries {
		all = append(all, aged{k, e.Last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, a := range all[:len(all)-maxEntries] {
		for out := range b.entries[a.k].Outputs {
			if b.byOutput[out] == a.k {
				delete(b.byOutput, out)
			}
		}
		delete(b.entries, a.k)
	}
}

// Class returns a program's effective class, and whether an exact class
// is verified.
func (b *Book) Class(process string) (Class, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.procs[process].class()
}

// Why says why a process's declared class is not its effective class
// (an exact declaration demoted or capped), or "" when it is.
func (b *Book) Why(process string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, _, why := b.procs[process].classWhy()
	return why
}

// Has reports whether a process has run.
func (b *Book) Has(process string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.procs[process]
	return ok
}

// Evidence returns how many re-runs of a process's computations wrote the
// output earlier runs wrote, and how many wrote another.
func (b *Book) Evidence(process string) (matched, mismatched int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if p := b.procs[process]; p != nil {
		return p.Verified, p.Mismatched
	}
	return 0, 0
}

// ProducedBy returns the computation that wrote an output, its program,
// and its inputs.
func (b *Book) ProducedBy(output string) (Key, string, []Input, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	k, ok := b.byOutput[output]
	if !ok {
		return Key{}, "", nil, false
	}
	e := b.entries[k]
	return k, e.Program, append([]Input(nil), e.Inputs...), true
}

// ProducerOf returns the unit that last ran k.
func (b *Book) ProducerOf(k Key) (Producer, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	e := b.entries[k]
	if e == nil || e.Producer == nil {
		return Producer{}, false
	}
	return *e.Producer, true
}

// Recompute returns what recomputing k costs: from k's own runs when it
// has any, else from its program's runs (other units, other inputs). It
// reports false when the program has never run. program may be empty
// when k ran before. What a recompute yields depends on the
// class: the same output (Exact), an acceptable different one
// (Validated), or a new output that is not a substitute for the recorded
// one (Volatile); the caller weighs that.
func (b *Book) Recompute(k Key, program string) (Cost, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	e := b.entries[k]
	if e != nil {
		program = e.Program
	}
	if program == "" {
		program = k.Process
	}
	p := b.procs[program]
	if p == nil {
		return Cost{}, false
	}
	var c Cost
	c.Class, c.Verified = p.class()
	work, dur := p.Work, p.Duration
	c.Basis = "program"
	if e != nil {
		work, dur, c.Basis = e.Work, e.Duration, "exact"
		if c.Class == Exact && c.Verified && len(e.Outputs) == 1 {
			for out := range e.Outputs {
				c.Reuse = out
			}
		}
	}
	c.Runs = int(work.N)
	sd := work.sd()
	if work.N < 2 {
		// One run says little about spread: assume a quarter of it, as
		// the estimator does before it has residuals.
		sd = 0.25 * work.Mean
	}
	c.Work = estimate.Prior{Mean: work.Mean, SD: sd}
	c.W90 = work.Mean + 1.2816*sd
	c.Longest = dur.Mean
	return c, true
}

// Repeats returns how many times computation k ran and how many distinct
// outputs those runs wrote.
func (b *Book) Repeats(k Key) (runs, outputs int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	e := b.entries[k]
	if e == nil {
		return 0, 0
	}
	for _, n := range e.Outputs {
		runs += n
	}
	return runs, len(e.Outputs)
}

// Reuse is an output that can stand in for running its computation
// again: the view, the changes that made it, and the record of the run
// that wrote it.
type Reuse struct {
	Output, Changeset, Record string
}

// ReuseOf returns an existing output of computation k, when its program
// is exact and verified by re-runs and every run of k wrote that one
// output (Recompute's Reuse), with what folding it needs. It reports
// false otherwise, and for an output recorded before changesets were.
func (b *Book) ReuseOf(k Key, program string) (Reuse, bool) {
	c, ok := b.Recompute(k, program)
	if !ok || c.Reuse == "" {
		return Reuse{}, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	e := b.entries[k]
	if e == nil {
		return Reuse{}, false
	}
	cs, known := e.Changesets[c.Reuse]
	if !known {
		return Reuse{}, false
	}
	return Reuse{Output: c.Reuse, Changeset: cs, Record: e.Records[c.Reuse]}, true
}

// NeedsVerification reports whether a reuse opportunity for an exact,
// verified process should be recomputed instead, to keep the declaration
// falsifiable. u is a uniform draw in [0, 1) the caller supplies (seeded
// however it likes, so decisions are reproducible).
func (b *Book) NeedsVerification(process string, u float64) bool {
	rate := b.VerifyRate
	if rate <= 0 {
		rate = 0.05
	}
	c, ok := b.Class(process)
	return c == Exact && ok && u < rate
}

// Len is the number of computations recorded.
func (b *Book) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.entries)
}
