package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
// folds. Every section of the chain is sealed by the view after that fold,
// so each intermediate output is a checkpoint that can be verified and
// mounted (DESIGN.md sections 2.5 and 10.5).
type output struct {
	view   c4.ID
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
}

// Fold is one fold of a job's output: the unit, its changeset and its task
// record.
type Fold struct {
	Unit      int    `json:"unit"`
	Changeset string `json:"changeset"`
	Record    string `json:"record,omitempty"`
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

// load restores an output from its stored chain after a restart.
func (o *output) load(st *storeurl.Sharded, chainID string) error {
	id, err := c4.Parse(chainID)
	if err != nil {
		return err
	}
	data, err := c4view.ReadObject(st, id)
	if err != nil {
		return err
	}
	ch, err := c4view.ReadChain(data)
	if err != nil {
		return err
	}
	o.chain, o.id, o.view = data, id, ch.Head()
	return nil
}

// fold applies the changeset csID, which unit idx's capture produced, to
// the job's output.
func (q *Queue) fold(ctx context.Context, st *storeurl.Sharded, o *output, idx int, csID, record string) error {
	id, err := c4.Parse(csID)
	if err != nil {
		return err
	}
	data, err := c4view.ReadObject(st, id)
	if err != nil {
		return err
	}
	cs, err := c4view.ReadChain(data)
	if err != nil {
		return fmt.Errorf("changeset %s: %w", csID, err)
	}
	if len(cs.Sections) == 0 {
		return nil
	}
	if o.view.IsNil() {
		o.view = cs.Base
		o.chain = []byte(cs.Base.String() + "\n")
	}
	src := c4view.StoreRecords(st)
	cur := o.view
	texts := sectionTexts(data)
	if len(texts) != len(cs.Sections) {
		return fmt.Errorf("changeset %s: %d sections, %d section texts", csID, len(cs.Sections), len(texts))
	}
	for i, sec := range cs.Sections {
		for _, p := range patchPaths(sec.Patch) {
			if w, ok := o.writer[p]; ok && w != idx {
				o.conflicts = append(o.conflicts, fmt.Sprintf("%s written by units %d and %d", p, w, idx))
			}
			o.writer[p] = idx
			if record != "" {
				o.records[p] = record
				o.dirty = true
			}
		}
		res, err := c4view.ApplyPatch(ctx, cur, sec.Patch, src)
		if err != nil {
			return fmt.Errorf("fold unit %d: %w", idx, err)
		}
		for _, r := range res.Records {
			if _, err := st.PutBytes(ctx, r.Data); err != nil {
				return err
			}
		}
		// The section's patch text, sealed by the output view it makes.
		o.chain = append(o.chain, texts[i]...)
		o.chain = append(o.chain, []byte(res.ID.String()+"\n")...)
		cur = res.ID
	}
	if record != "" {
		o.units[idx] = record
		o.dirty = true
	}
	chainID, err := st.PutBytes(ctx, o.chain)
	if err != nil {
		return err
	}
	o.view, o.id = cur, chainID
	o.folded[idx]++
	o.count++
	o.folds = append(o.folds, Fold{Unit: idx, Changeset: csID, Record: record})
	o.dirty = true
	return nil
}

// refoldOutput rebuilds the job's output from its fold log without the
// changesets of its superseded units: a new chain, each section again
// sealed by the view it makes. It reports false when it must be tried
// again (the store is unreachable).
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
		if err := q.fold(ctx, st, o, f.Unit, f.Changeset, f.Record); err != nil {
			q.Log.Error(err, "refold", "job", v.key, "unit", f.Unit)
			return false
		}
	}
	if o.view.IsNil() && len(m.out.chain) > 0 {
		// Every fold was superseded: the output is the job's input view.
		ch, err := c4view.ReadChain(m.out.chain)
		if err != nil {
			q.Log.Error(err, "refold", "job", v.key)
			return false
		}
		o.view, o.chain = ch.Base, []byte(ch.Base.String()+"\n")
		if o.id, err = st.PutBytes(ctx, o.chain); err != nil {
			q.Log.Error(err, "refold", "job", v.key)
			return false
		}
	}
	q.Log.Info("refolded", "job", v.key, "from", m.out.view.String(), "to", o.view.String(), "folds", len(o.folds))
	o.dirty = true
	m.out = o
	return true
}

// loadOutput restores the output's chain and index after a restart, once.
func (q *Queue) loadOutput(st *storeurl.Sharded, v *view) error {
	m := v.mem
	if m.outLoad != "" {
		if err := m.out.load(st, m.outLoad); err != nil {
			return err
		}
		m.outLoad = ""
	}
	if m.outIndex != "" {
		if err := m.out.loadIndex(st, m.outIndex); err != nil {
			return err
		}
		m.outIndex = ""
	}
	return nil
}

// sectionTexts returns the patch text of each section of chain bytes: the
// lines between the base line and each checkpoint line, as ReadChain
// splits them.
func sectionTexts(data []byte) [][]byte {
	var out [][]byte
	var cur []byte
	first := true
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		t := strings.TrimSpace(string(line))
		if t == "" {
			continue
		}
		if len(t) == 90 && strings.HasPrefix(t, "c4") && !strings.ContainsAny(t, " \t") {
			if first {
				first = false
				continue
			}
			if len(bytes.TrimSpace(cur)) > 0 {
				out = append(out, cur)
			}
			cur = nil
			continue
		}
		if !bytes.HasSuffix(line, []byte("\n")) {
			line = append(append([]byte(nil), line...), '\n')
		}
		cur = append(cur, line...)
	}
	return out
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
