package queue

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
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
	// id is the stored chain's head: the newest block, one fold's patch
	// whose first line names the block before (chainBlock), or every
	// chainRestEvery blocks the view before it, whose listing is stored, so
	// a reader resolves at most that many blocks. Before any fold it is the
	// job's input view itself. blocks counts the blocks since the last that
	// rests on a view. Until 2026-11-12 the chain was the whole text of the
	// folds so far, stored again on every fold: quadratic.
	id     c4.ID
	blocks int
	// input is the job's input view, the base of its first fold: a refold
	// of nothing starts from it. Kept in the index (Index.Input).
	input c4.ID
	// base is the stored chain's first line as load read it: the input of a
	// job whose chain is from before blocks and whose index has no input.
	loadedBase c4.ID
	folded     map[int]int // unit -> folds of it (a resumed unit folds each yield)
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
	// What changed since index was stored, for the next segment: records
	// and units set, folds appended (the last newFolds), and whether an
	// earlier fold was changed in place (rewrite: the next is a checkpoint).
	// segs counts the segments since the last checkpoint.
	newRecords map[string]string
	newUnits   map[int]string
	newFolds   int
	rewrite    bool
	segs       int
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
	// of its own and is folded as its unit is marked succeeded: its record
	// is the earlier run's, and that run is not this fold's.
	Run string `json:"run,omitempty"`
	// BeforeRuns marks a fold read from an index written before folds
	// named their runs, whose task record is not there to say which pod
	// it was of (the node had no signer, or the record cannot be read).
	// Nothing says whether a finished pod of that unit with that changeset
	// is this fold's run or a later one, so such a pod is folded, and
	// reported, once (fold).
	BeforeRuns bool `json:"beforeRuns,omitempty"`
}

func newOutput() *output {
	return &output{folded: map[int]int{}, writer: map[string]int{}, records: map[string]string{}, units: map[int]string{}}
}

// indexVersion is the version of an index whose folds name their runs.
const indexVersion = 2

// Index is a job's provenance index as stored: one object, or (since
// 2026-11-12) a segment that names the one before it (Prev) and holds only
// what changed since: the records and units set, the folds appended, and
// the output after them. A segment with no Prev is the whole index, a
// checkpoint, written every indexCheckpointEvery segments and whenever an
// earlier fold changes. ReadIndex folds a head's segments into one Index.
// Before, every change stored the whole index again, so a job folded n
// times stored n indexes of up to n folds each: quadratic.
type Index struct {
	// Version is indexVersion, or absent in an index written before folds
	// named their runs (loadIndex gives those their runs).
	Version int               `json:"version,omitempty"`
	Job     string            `json:"job"`
	Output  string            `json:"output"`
	Input   string            `json:"input,omitempty"` // the job's input view, the first fold's base
	Prev    string            `json:"prev,omitempty"`  // the segment before; empty in a whole index
	Records map[string]string `json:"records"`         // path -> task record ID
	Units   map[int]string    `json:"units"`           // unit -> its latest task record ID
	Folds   []Fold            `json:"folds,omitempty"`
}

// segment is how a segment is stored: its folds under appended, and folds
// a string, so that a manager from before segments, which reads folds as
// a list, fails on a segment (its fold waits, the HUD answers an error)
// instead of taking it for the whole index, folding again what it has
// already folded and writing over the history with what it read.
type segment struct {
	Version  int               `json:"version,omitempty"`
	Job      string            `json:"job"`
	Output   string            `json:"output"`
	Input    string            `json:"input,omitempty"`
	Prev     string            `json:"prev"`
	Records  map[string]string `json:"records"`
	Units    map[int]string    `json:"units"`
	Appended []Fold            `json:"appended,omitempty"`
	Folds    string            `json:"folds"`
}

const segmentMark = "a segment of a provenance index: read it from its head"

// stored is how readIndex reads either form: a whole index's folds are a
// list, a segment's are under appended.
type stored struct {
	Index
	Folds    json.RawMessage `json:"folds,omitempty"`
	Appended []Fold          `json:"appended,omitempty"`
}

// indexCheckpointEvery is how many segments follow a whole index before
// the next: a reader reads at most this many and one more, and a job
// folded n times stores n/64 whole indexes, not n.
const indexCheckpointEvery = 64

// maxIndexSegments bounds how many segments ReadIndex follows.
const maxIndexSegments = 4 * indexCheckpointEvery

// ReadIndex reads the provenance index whose head is id: a whole index,
// or a segment, folded with the ones it names back to a whole index.
func ReadIndex(st c4view.Store, id c4.ID) (*Index, error) {
	ix, _, err := readIndex(st, id)
	return ix, err
}

// readIndex is ReadIndex, saying also how many segments came after the
// whole index.
func readIndex(st c4view.Store, id c4.ID) (*Index, int, error) {
	var segs []Index
	for cur := id; ; {
		if len(segs) >= maxIndexSegments {
			return nil, 0, fmt.Errorf("provenance index %s: more than %d segments", id, maxIndexSegments)
		}
		data, err := c4view.ReadObject(st, cur)
		if err != nil {
			return nil, 0, err
		}
		var raw stored
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, 0, fmt.Errorf("provenance index segment %s: %w", cur, err)
		}
		ix := raw.Index
		if ix.Prev != "" {
			ix.Folds = raw.Appended
		} else if len(raw.Folds) > 0 {
			if err := json.Unmarshal(raw.Folds, &ix.Folds); err != nil {
				return nil, 0, fmt.Errorf("provenance index %s: folds: %w", cur, err)
			}
		}
		segs = append(segs, ix)
		if ix.Prev == "" {
			break
		}
		next, err := c4.Parse(ix.Prev)
		if err != nil {
			return nil, 0, fmt.Errorf("provenance index segment %s: prev: %w", cur, err)
		}
		cur = next
	}
	out := segs[len(segs)-1]
	if out.Records == nil {
		out.Records = map[string]string{}
	}
	if out.Units == nil {
		out.Units = map[int]string{}
	}
	for i := len(segs) - 2; i >= 0; i-- {
		d := segs[i]
		for k, v := range d.Records {
			out.Records[k] = v
		}
		for k, v := range d.Units {
			out.Units[k] = v
		}
		out.Folds = append(out.Folds, d.Folds...)
		out.Output, out.Version = d.Output, d.Version
		if d.Input != "" {
			out.Input = d.Input
		}
	}
	out.Prev = ""
	return &out, len(segs) - 1, nil
}

// saveIndex stores the provenance index when it changed.
func (o *output) saveIndex(ctx context.Context, st *storeurl.Sharded, job string) error {
	if !o.dirty {
		return nil
	}
	full := o.index.IsNil() || o.rewrite || o.segs >= indexCheckpointEvery || o.newFolds > len(o.folds)
	var b []byte
	var err error
	if full {
		b, err = json.Marshal(Index{Version: indexVersion, Job: job, Output: o.view.String(), Input: idString(o.input), Records: o.records, Units: o.units, Folds: o.folds})
	} else {
		b, err = json.Marshal(segment{Version: indexVersion, Job: job, Output: o.view.String(), Input: idString(o.input), Prev: o.index.String(),
			Records: o.newRecords, Units: o.newUnits, Appended: o.folds[len(o.folds)-o.newFolds:], Folds: segmentMark})
	}
	if err != nil {
		return err
	}
	id, err := st.PutBytes(ctx, b)
	if err != nil {
		return err
	}
	o.index, o.dirty = id, false
	o.newRecords, o.newUnits, o.newFolds, o.rewrite = nil, nil, 0, false
	if full {
		o.segs = 0
	} else {
		o.segs++
	}
	return nil
}

// errIndexNotConverted marks an index from before folds named their runs
// that could not be given them this tick, because the store could not be
// asked for a fold's task record. Nothing is known about that fold until it
// can, so nothing of the job is folded meanwhile.
var errIndexNotConverted = errors.New("the index from before the upgrade is not converted yet")

// loadIndex restores the provenance index after a restart.
//
// An index written before folds named their runs is given them here, from
// its own entries: each carries the fold's task record, and the record
// names the pod it was signed for. So after an upgrade a finished pod that
// is still listed is known as folded, and is not folded again over what
// other units have written since. That holds for a unit that succeeded and
// for one that yielded alike, where the job's status could speak only for
// the first, and wrongly when a rework had begun and the status had not
// been written.
//
// It is done whole or not at all, and can be done again. Every entry is
// settled first: it gets its run, or it is marked as having none
// (Fold.BeforeRuns) because the record is not in the store, is not the
// object its ID names, does not verify, or is not this fold's. Then the
// index is saved with its version, and only then does loadIndex return, so
// no fold of the job is made before that. A store that cannot be asked
// settles nothing: the error is errIndexNotConverted, the index stays as
// it was, and the next tick starts over, as a manager that stopped part
// way does.
//
// What was found wrong with a record is returned in says, for the job's
// conflicts; a record that is simply not there says nothing.
func (o *output) loadIndex(ctx context.Context, st *storeurl.Sharded, id, job string, roots *x509.CertPool, read map[string]recordRun, asked *int) (says []string, err error) {
	pid, err := c4.Parse(id)
	if err != nil {
		return nil, err
	}
	head, segs, err := readIndex(st, pid)
	if err != nil {
		return nil, err
	}
	ix := *head
	// Every fold with no run is settled, whatever the others have: an old
	// index a run-naming manager later added to has both kinds.
	if ix.Version < indexVersion {
		for i := range ix.Folds {
			f := &ix.Folds[i]
			if f.Run != "" {
				continue
			}
			// Kept by the fold, not by the record alone: two folds can
			// name one record (a unit's run, and a reuse of it), and what
			// the record says differs for each.
			key := fmt.Sprintf("%d %s %s", f.Unit, f.Changeset, f.Record)
			r, ok := read[key]
			if !ok {
				if asked != nil && *asked >= maxRecordReadsPerTick {
					return nil, fmt.Errorf("%w: %d of its %d folds settled so far", errIndexNotConverted, i, len(ix.Folds))
				}
				if asked != nil {
					*asked++
				}
				if r, err = runOfRecord(st, roots, job, f); err != nil {
					return nil, fmt.Errorf("%w: the task record of unit %d's fold: %v", errIndexNotConverted, f.Unit, err)
				}
				if read != nil {
					read[key] = r
				}
			}
			if r.why != "" {
				says = append(says, fmt.Sprintf("unit %d's fold from before the upgrade: %s; which run it was of is not known", f.Unit, r.why))
			}
			f.Run, f.BeforeRuns = r.run, r.run == "" && !r.reused
		}
	}
	was := *o
	if ix.Records != nil {
		o.records = ix.Records
	}
	if ix.Units != nil {
		o.units = ix.Units
	}
	o.folds = ix.Folds
	o.index, o.segs = pid, segs
	if in, err := c4.Parse(ix.Input); err == nil && ix.Input != "" {
		o.input = in
	} else if o.input.IsNil() {
		o.input = o.loadedBase // an index from before it kept the input
	}
	if ix.Version < indexVersion && len(ix.Folds) > 0 {
		o.dirty, o.rewrite = true, true // its folds were given their runs: stored whole
		if err := o.saveIndex(ctx, st, job); err != nil {
			*o = was
			return nil, fmt.Errorf("%w: saving it: %v", errIndexNotConverted, err)
		}
	}
	return says, nil
}

// maxRecordReadsPerTick bounds the store reads one tick spends converting
// old indexes, over all its jobs and however many folds ask (the count is
// the queue's, reset each tick). What was read is kept (loadOutput), so a
// long log is converted over several ticks, and still saved only when
// every fold is settled.
const maxRecordReadsPerTick = 64

// recordRun is what a fold's task record says of its run.
type recordRun struct {
	run string // the pod's UID, when the record can be believed
	why string // what is wrong with the record, when something is
	// reused: the record is a good one of another unit or job, so the fold
	// is a reused computation and has no run of its own.
	reused bool
}

// runOfRecord reads what the fold f's task record says of its run. An
// error is the store failing to answer, which says nothing about the
// record. A record the store simply does not hold says nothing either,
// and leaves the fold with no run.
//
// The record is checked as far as that costs nothing: its bytes must hash
// to the ID the index names, it must be a task record of this fold's
// changeset, and where the queue has the farm's roots its signature and
// signer are verified. Without roots it is believed as far as the index
// that names it, which is not signed either. A good record of another
// unit, or of another job, is a reused computation's: the fold was made
// from that earlier run's record and has no run of its own. A record that
// names no job or unit (the node could no longer see the pod when it
// signed) is taken as the fold's own: the other reading would fold every
// such pod again.
func runOfRecord(st *storeurl.Sharded, roots *x509.CertPool, job string, f *Fold) (recordRun, error) {
	wrong := func(format string, args ...any) (recordRun, error) {
		return recordRun{why: fmt.Sprintf(format, args...)}, nil
	}
	if f.Record == "" {
		return recordRun{}, nil
	}
	id, err := c4.Parse(f.Record)
	if err != nil {
		return wrong("its task record ID %q is not a C4 ID", f.Record)
	}
	data, err := c4view.ReadObject(st, id)
	if errors.Is(err, storeurl.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return recordRun{}, nil
	}
	if err != nil {
		return recordRun{}, err
	}
	short := shortRecord(f.Record)
	var pred json.RawMessage
	if roots != nil {
		vr, err := provenance.VerifyBundle(data, roots)
		if err != nil {
			return wrong("its task record %s does not verify (%v)", short, err)
		}
		if vr.PredicateType != provenance.PredicateTask {
			return wrong("record %s is a %s record, not a task record", short, vr.PredicateType)
		}
		if err := provenance.CheckSigner(vr); err != nil {
			return wrong("its task record %s: %v", short, err)
		}
		pred = vr.Predicate
	} else {
		var b provenance.Bundle
		var stmt provenance.AnyStatement
		if json.Unmarshal(data, &b) != nil {
			return wrong("record %s is not a task record", short)
		}
		payload, err := base64.StdEncoding.DecodeString(b.DSSEEnvelope.Payload)
		if err != nil || json.Unmarshal(payload, &stmt) != nil || stmt.PredicateType != provenance.PredicateTask {
			return wrong("record %s is not a task record", short)
		}
		pred = stmt.Predicate
	}
	var task provenance.TaskPredicate
	if err := json.Unmarshal(pred, &task); err != nil {
		return wrong("its task record %s does not read (%v)", short, err)
	}
	if task.Output.Changeset != f.Changeset {
		return wrong("its task record %s is of another changeset", short)
	}
	inv := task.Invocation
	if inv.Unit != "" && inv.Unit != strconv.Itoa(f.Unit) || inv.Job != "" && inv.Namespace+"/"+inv.Job != job {
		return recordRun{reused: true}, nil
	}
	if inv.PodUID == "" {
		return wrong("its task record %s names no pod", short)
	}
	return recordRun{run: inv.PodUID}, nil
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
	// The next block rests on the view, whatever the chain is: a block, a
	// whole chain from before blocks (still read as it was), or the view.
	o.id, o.view, o.blocks = id, view, chainRestEvery
	if id == view {
		return nil
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
	o.loadedBase = ch.Base
	return nil
}

// chainRestEvery is how many blocks follow one that rests on a view
// before the next does: resolving a chain's head reads at most this many.
const chainRestEvery = 64

// chainBlock is one fold's section as a block of the chain: the
// changeset's patch, decoded and encoded again by the c4m library with
// on's ID as its first line (a block before, or a view whose listing is
// stored: the spec's external base reference). A block holds one section,
// never more.
func chainBlock(changeset []byte, on c4.ID) ([]byte, error) {
	m, err := c4m.NewDecoder(bytes.NewReader(changeset)).Decode()
	if err != nil {
		return nil, err
	}
	m.Base = c4.ID{}
	var buf bytes.Buffer
	if err := c4m.NewEncoder(&buf).EncodePatch(&c4m.PatchResult{OldID: on, Patch: m}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func idString(id c4.ID) string {
	if id.IsNil() {
		return ""
	}
	return id.String()
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
// tick and leaves the unit finishing, and the job's status says why
// (foldChangeset).
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
	input := o.input
	if input.IsNil() {
		input = cur
	}
	// The head moves only when the whole fold is stored, so a fold that
	// fails and is tried again never stores a section twice in the chain.
	head, blocks := o.id, o.blocks
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
		// output: one block of the chain, resting on the block before or,
		// every chainRestEvery blocks and before the first, on the view it
		// was made over. A section that changed nothing adds nothing.
		if len(res.Changeset) > 0 {
			on := head
			if head.IsNil() || blocks >= chainRestEvery {
				on, blocks = cur, 0
			}
			blk, err := chainBlock(res.Changeset, on)
			if err != nil {
				return fmt.Errorf("fold unit %d: chain block: %w", idx, err)
			}
			if head, err = st.PutBytes(ctx, blk); err != nil {
				return err
			}
			blocks++
		}
		cur = res.ID
	}
	if head.IsNil() {
		// Nothing has changed the job's input yet: the chain is that view
		// alone, whose listing is stored.
		head = cur
	}
	o.view, o.id, o.blocks, o.input = cur, head, blocks, input
	for _, p := range paths {
		o.writer[p] = idx
		o.fresh = append(o.fresh, freshPath{unit: idx, path: p})
		if f.Record != "" {
			o.records[p] = f.Record
			if o.newRecords == nil {
				o.newRecords = map[string]string{}
			}
			o.newRecords[p] = f.Record
		}
	}
	if f.Record != "" {
		o.units[idx] = f.Record
		if o.newUnits == nil {
			o.newUnits = map[int]string{}
		}
		o.newUnits[idx] = f.Record
	}
	if f.Run != "" {
		// A pod whose unit and changeset match a fold from before runs
		// were named, with no record to say which pod that was: it may be
		// that fold's own run seen again after the upgrade, or a later
		// one. It has been folded either way, and is reported, once, as
		// what it is: a fold that may have been made twice, not one that
		// was.
		for i := range o.folds {
			if d := &o.folds[i]; d.BeforeRuns && d.Unit == idx && d.Changeset == f.Changeset {
				d.BeforeRuns = false
				o.rewrite = true // an earlier fold changed: the next is whole
				conflicts = append(conflicts, fmt.Sprintf("a run of unit %d may have been folded twice after an upgrade: a fold of it from before names no run and has no task record that can be believed, so this pod may be that same run or a later one; if it is the same, and another unit wrote the same paths in between, this unit's older write now stands", idx))
			}
		}
	}
	o.conflicts = append(o.conflicts, conflicts...)
	o.folded[idx]++
	o.count++
	o.folds = append(o.folds, f)
	o.newFolds++
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
	if err := q.loadOutput(ctx, st, v); err != nil {
		return q.foldFailed(v, -1, "load output", err)
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
			if v.foldWait == "" {
				v.foldWait = fmt.Sprintf("unit %d, rebuilding the output without superseded units: %v", f.Unit, err)
			}
			return false
		}
	}
	if o.view.IsNil() && !m.out.view.IsNil() {
		// Every fold was superseded: the output is the job's input view,
		// and its chain is that view alone. The input is the base of the
		// chain there was, or the old output itself where no fold had
		// changed it.
		base := m.out.input
		if base.IsNil() {
			base = m.out.view // no fold had changed it
		}
		o.id, o.view, o.input = base, base, base
	}
	q.Log.Info("refolded", "job", v.key, "from", m.out.view.String(), "to", o.view.String(), "folds", len(o.folds))
	o.dirty = true
	o.fresh = nil // written before, not news
	m.out = o
	return true
}

// loadOutput restores the output's chain and index after a restart, once.
func (q *Queue) loadOutput(ctx context.Context, st *storeurl.Sharded, v *view) error {
	m := v.mem
	if m.outLoad != "" {
		if err := m.out.load(st, m.outLoad, m.outView); err != nil {
			return err
		}
		m.outLoad, m.outView = "", ""
	}
	if m.outIndex != "" {
		if m.outRuns == nil {
			m.outRuns = map[string]recordRun{}
		}
		says, err := m.out.loadIndex(ctx, st, m.outIndex, v.key, q.Roots, m.outRuns, &q.recordReads)
		if err != nil {
			return err
		}
		m.out.conflicts = append(m.out.conflicts, says...)
		m.outIndex, m.outRuns = "", nil
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
