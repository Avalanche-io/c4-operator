package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// StoreFunc opens a project's store.
type StoreFunc func(ctx context.Context, project string) (*storeurl.Sharded, error)

// output is a job's output: one view, built by folding each unit's
// captured changeset into it as the unit finishes, and the chain of those
// folds. Each fold is written into the chain as a changeset over the output
// before it, so every bare ID in the chain is an intermediate output that
// can be verified and mounted (DESIGN.md sections 2.5 and 10.5). The output
// after the last fold is view, kept beside the chain: a c4m file never ends
// with a bare C4 ID.
type output struct {
	view c4.ID
	// chain is the text of the folds so far, one changeset after another;
	// empty while there are none (the stored chain is then the job's input
	// view alone).
	chain  []byte
	id     c4.ID       // the stored chain
	folded map[int]int // unit -> folds of it (a resumed unit folds each yield)
	// paths written by earlier folds, for conflict reports. In memory
	// only: after a restart conflicts are found among new folds only.
	writer    map[string]int
	conflicts []string
	count     int
	// records maps each file the job wrote to the task record of the unit
	// that wrote it; index is its stored form.
	records map[string]string
	units   map[int]string // unit -> its latest task record
	index   c4.ID
	dirty   bool
	// folds is every fold in order, kept in the index, so the output can
	// be rebuilt without some units' changesets (refoldOutput).
	folds []Fold
	// fresh are the paths folded this tick, for the questions about them
	// (escalate.go).
	fresh []freshPath
}

// Fold is one fold of a job's output: the unit, its changeset and its task
// record, and the run that produced them.
type Fold struct {
	Unit      int    `json:"unit"`
	Changeset string `json:"changeset"`
	Record    string `json:"record,omitempty"`
	// Run names the run of the unit: its pod's UID. A run in the log is
	// never folded again. Empty for a reused computation, which has no run
	// of its own and is folded as its unit is marked succeeded.
	Run string `json:"run,omitempty"`
}

func newOutput() *output {
	return &output{folded: map[int]int{}, writer: map[string]int{}, records: map[string]string{}, units: map[int]string{}}
}

// Index is a job's provenance index as stored.
type Index struct {
	Job     string            `json:"job"`
	Output  string            `json:"output"`
	Records map[string]string `json:"records"` // path -> task record ID
	Units   map[int]string    `json:"units"`   // unit -> its latest task record ID
	Folds   []Fold            `json:"folds,omitempty"`
}

// saveIndex stores the provenance index when it changed.
func (o *output) saveIndex(ctx context.Context, st *storeurl.Sharded, job string) error {
	if !o.dirty {
		return nil
	}
	b, err := json.Marshal(Index{Job: job, Output: o.view.String(), Records: o.records, Units: o.units, Folds: o.folds})
	if err != nil {
		return err
	}
	id, err := st.PutBytes(ctx, b)
	if err != nil {
		return err
	}
	o.index, o.dirty = id, false
	return nil
}

// loadIndex restores the provenance index after a restart.
func (o *output) loadIndex(st *storeurl.Sharded, id string) error {
	pid, err := c4.Parse(id)
	if err != nil {
		return err
	}
	data, err := c4view.ReadObject(st, pid)
	if err != nil {
		return err
	}
	var ix Index
	if err := json.Unmarshal(data, &ix); err != nil {
		return err
	}
	if ix.Records != nil {
		o.records = ix.Records
	}
	if ix.Units != nil {
		o.units = ix.Units
	}
	o.folds = ix.Folds
	o.index = pid
	return nil
}

// load restores an output after a restart: its stored chain, and the view
// the job's status keeps beside it. A chain stored in the old form ends
// with that view's ID; the line is checked and not carried on, so the next
// fold stores the chain in the form the format allows.
func (o *output) load(st *storeurl.Sharded, chainID, viewID string) error {
	id, err := c4.Parse(chainID)
	if err != nil {
		return err
	}
	view, err := c4.Parse(viewID)
	if err != nil {
		return fmt.Errorf("output view %q: %w", viewID, err)
	}
	data, err := c4view.ReadObject(st, id)
	if err != nil {
		return err
	}
	ch, err := c4view.ReadChain(data)
	if err != nil {
		return fmt.Errorf("chain %s: %w", chainID, err)
	}
	if !ch.Seal.IsNil() && ch.Seal != view {
		return fmt.Errorf("chain %s ends with %s, and the job's output is %s", chainID, ch.Seal, view)
	}
	o.chain = nil
	if len(ch.Sections) > 0 {
		if o.chain, err = ch.Encode(); err != nil {
			return err
		}
	}
	o.id, o.view = id, view
	return nil
}

// fold brings one run of a unit, the changeset its capture produced, into
// the job's output.
//
// The changeset is a patch over the unit's own input, and the output is
// another view: other units have been folded into it, and so may this one
// have been. A patch entry means a write or a removal by what its own base
// holds, so each section is read against the view it was made over and what
// it means is done to the output (view.ApplyPatchFrom). Applied to the
// output as it stands, a file two units wrote alike, or one unit wrote
// twice, would be removed.
//
// Folds are kept in order, and the later one wins: where two units wrote
// one path differently the later fold's write is kept, and where a fold
// removes or replaces a directory that holds another unit's writes they go
// with it. Both are reported as conflicts, never refused.
//
// One thing is refused: a changeset that writes a directory by its C4 ID
// alone where a different directory already is, whose meaning is not ruled
// (view.ApplyPatchFrom). Capture never writes one. Such a fold fails every
// tick, is logged, and leaves the unit finishing.
//
// A run is folded once. Its pod can be seen again (after a restart the
// queue has no memory, and a finished pod may still be there), and folding
// it a second time would not always be nothing: if another unit wrote the
// same path in between, the second fold would put this unit's write back
// over it. So the fold log carries the run, and a run in the log is not
// folded again. A unit that runs again is another run, and is folded: its
// write is the later one.
//
// Nothing of the output changes unless the whole fold is stored: not the
// chain, and not the writers, records or conflicts kept beside it.
func (q *Queue) fold(ctx context.Context, st *storeurl.Sharded, o *output, f Fold) error {
	idx := f.Unit
	if f.Run != "" {
		for _, done := range o.folds {
			if done.Run == f.Run {
				return nil
			}
		}
	}
	id, err := c4.Parse(f.Changeset)
	if err != nil {
		return err
	}
	data, err := c4view.ReadObject(st, id)
	if err != nil {
		return err
	}
	cs, err := c4view.ReadChain(data)
	if err != nil {
		return fmt.Errorf("changeset %s: %w", f.Changeset, err)
	}
	if len(cs.Sections) == 0 {
		return nil
	}
	src := c4view.StoreRecords(st)
	cur := o.view
	if cur.IsNil() {
		cur = cs.Base
	}
	// The chain grows in a copy and replaces the output's only when the
	// whole fold is stored, so a fold that fails and is tried again never
	// leaves a section in the chain twice.
	chain := o.chain[:len(o.chain):len(o.chain)]
	var paths, conflicts []string
	named := map[string]bool{}
	for _, sec := range cs.Sections {
		for _, p := range patchPaths(sec.Patch) {
			if named[p] {
				continue
			}
			named[p] = true
			paths = append(paths, p)
			if w, ok := o.writer[p]; ok && w != idx {
				conflicts = append(conflicts, fmt.Sprintf("%s written by units %d and %d", p, w, idx))
			}
		}
		// Each section against its own base: the first's is the unit's
		// input, and a later one's is the view the sections before it made.
		res, err := c4view.ApplyPatchFrom(ctx, cur, sec.Base, sec.Patch, src)
		if err != nil {
			return fmt.Errorf("fold unit %d: %w", idx, err)
		}
		for _, r := range res.Records {
			if _, err := st.PutBytes(ctx, r.Data); err != nil {
				return err
			}
		}
		conflicts = append(conflicts, o.taken(idx, res.Dropped)...)
		// What the section did to the output, as a changeset over the
		// output, which is an ordinary changeset: the chain is read and
		// verified like any other. A section that changed nothing adds
		// nothing.
		chain = append(chain, res.Changeset...)
		cur = res.ID
	}
	stored := chain
	if len(stored) == 0 {
		// Nothing has changed the job's input yet: the chain is that view alone.
		if stored, err = (&c4view.Chain{Base: cur}).Encode(); err != nil {
			return err
		}
	}
	chainID, err := st.PutBytes(ctx, stored)
	if err != nil {
		return err
	}
	o.view, o.chain, o.id = cur, chain, chainID
	for _, p := range paths {
		o.writer[p] = idx
		o.fresh = append(o.fresh, freshPath{unit: idx, path: p})
		if f.Record != "" {
			o.records[p] = f.Record
		}
	}
	if f.Record != "" {
		o.units[idx] = f.Record
	}
	o.conflicts = append(o.conflicts, conflicts...)
	o.folded[idx]++
	o.count++
	o.folds = append(o.folds, f)
	o.dirty = true
	return nil
}

// taken reports, for the paths a fold by unit idx took out of the output
// without naming them (view.Applied.Dropped), the ones other units wrote:
// a file a directory replaced, and for a directory that was removed or
// replaced, how many paths under it each other unit had written or removed
// (the writers kept are of files, and a removal is kept like a write). It
// knows only the folds made since this manager started, and nothing of a
// directory another unit made and left empty.
func (o *output) taken(idx int, dropped []string) []string {
	var out []string
	for _, d := range dropped {
		if !strings.HasSuffix(d, "/") {
			if w, ok := o.writer[d]; ok && w != idx {
				out = append(out, fmt.Sprintf("%s written by unit %d, replaced by a directory by unit %d", d, w, idx))
			}
			continue
		}
		lost := map[int]int{}
		for p, w := range o.writer {
			if w != idx && strings.HasPrefix(p, d) {
				lost[w]++
			}
		}
		units := make([]int, 0, len(lost))
		for w := range lost {
			units = append(units, w)
		}
		sort.Ints(units)
		for _, w := range units {
			out = append(out, fmt.Sprintf("%s removed by unit %d: unit %d wrote or removed %d of the paths under it", d, idx, w, lost[w]))
		}
	}
	return out
}

// refoldOutput rebuilds the job's output from its fold log without the
// changesets of its superseded units: a new chain, each fold again a
// changeset over the output before it. It reports false when it must be
// tried again (the store is unreachable).
func (q *Queue) refoldOutput(ctx context.Context, v *view) bool {
	m := v.mem
	if q.Stores == nil {
		return true
	}
	st, err := q.Stores(ctx, v.project)
	if err != nil {
		q.Log.Error(err, "open store for refold", "job", v.key)
		return false
	}
	if st == nil {
		return true
	}
	if err := q.loadOutput(st, v); err != nil {
		q.Log.Error(err, "load output", "job", v.key)
		return false
	}
	drop := false
	for _, f := range m.out.folds {
		drop = drop || m.superseded[f.Unit]
	}
	if !drop {
		return true
	}
	o := newOutput()
	for _, f := range m.out.folds {
		if m.superseded[f.Unit] {
			continue
		}
		if err := q.fold(ctx, st, o, f); err != nil {
			q.Log.Error(err, "refold", "job", v.key, "unit", f.Unit)
			return false
		}
	}
	if o.view.IsNil() && !m.out.view.IsNil() {
		// Every fold was superseded: the output is the job's input view,
		// and its chain is that view alone. The input is the base of the
		// chain there was, or the old output itself where no fold had
		// changed it.
		base := m.out.view
		if len(m.out.chain) > 0 {
			ch, err := c4view.ReadChain(m.out.chain)
			if err != nil {
				q.Log.Error(err, "refold", "job", v.key)
				return false
			}
			base = ch.Base
		}
		alone, err := (&c4view.Chain{Base: base}).Encode()
		if err == nil {
			o.id, err = st.PutBytes(ctx, alone)
		}
		if err != nil {
			q.Log.Error(err, "refold", "job", v.key)
			return false
		}
		o.view = base
	}
	q.Log.Info("refolded", "job", v.key, "from", m.out.view.String(), "to", o.view.String(), "folds", len(o.folds))
	o.dirty = true
	o.fresh = nil // written before, not news
	m.out = o
	return true
}

// loadOutput restores the output's chain and index after a restart, once.
func (q *Queue) loadOutput(st *storeurl.Sharded, v *view) error {
	m := v.mem
	if m.outLoad != "" {
		if err := m.out.load(st, m.outLoad, m.outView); err != nil {
			return err
		}
		m.outLoad, m.outView = "", ""
	}
	if m.outIndex != "" {
		if err := m.out.loadIndex(st, m.outIndex); err != nil {
			return err
		}
		m.outIndex = ""
	}
	return nil
}

// patchPaths lists the file paths a patch writes or removes.
func patchPaths(m *c4m.Manifest) []string {
	var out, stack []string
	for _, e := range m.Entries {
		if e.Depth < len(stack) {
			stack = stack[:e.Depth]
		}
		if e.IsDir() {
			stack = append(stack[:e.Depth], strings.TrimSuffix(e.Name, "/"))
			continue
		}
		out = append(out, strings.Join(append(append([]string(nil), stack...), e.Name), "/"))
	}
	return out
}
