// Package pinmove re-identifies the views a project store holds after a c4
// library change gives some of them new IDs (c4 #107: a directory's null
// size kept null). Nothing stored is rewritten. Each view a holder names
// (a Project's grants, a Materialization, a finished WorkJob, a git-import
// mapping) is read as its records are stored and identified with the
// library now (view.Reidentify); a view whose ID changed has its records
// written again under the new ID, beside the old, and joins the project's
// renames table (view.Renames), a content-addressed object the Project's
// status names. Grants gain an entry for the new ID, Materializations are
// resolved again, and mappings gain a renamed line. Readers given an old ID
// resolve it through the table (view.ResolveOld).
//
// Every step is content-keyed, so a run that stops part way is resumed by
// running it again, and a run with nothing left to do writes nothing.
package pinmove

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Store is what the migration needs of a project store.
type Store interface {
	view.Store
	// PutBytes stores data under its C4 ID, unless the store holds it.
	PutBytes(ctx context.Context, data []byte) (c4.ID, error)
}

// MaxRecord bounds what is read of an object a holder names, as a view's
// record: a holder may name content, and content is not read whole to
// find it is not a view. A root record of a million entries is under it.
const MaxRecord = 256 << 20

// Held is one C4 ID a holder names, and where: the holder ("Project p",
// "Materialization ns/name") and the field's JSON path in it.
type Held struct {
	ID     c4.ID
	Holder string
	Path   string
}

// IDsIn is every C4 ID obj names, with its JSON path: each string in obj's
// JSON that is one C4 ID. A field that holds an ID among other text (a
// `blocks:` mark, a "seen" line) is not a view's ID and is not taken.
func IDsIn(holder string, obj any) ([]Held, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	var out []Held
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(path+"."+k, x[k])
			}
		case []any:
			for i, e := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		case string:
			if len(x) != 90 || !strings.HasPrefix(x, "c4") {
				return
			}
			if id, err := c4.Parse(x); err == nil {
				out = append(out, Held{ID: id, Holder: holder, Path: path})
			}
		}
	}
	walk("", v)
	return out, nil
}

// Finding is what the survey found of one C4 ID the holders name.
type Finding struct {
	ID c4.ID
	// New is the ID the view has now; ID itself when it did not change.
	New c4.ID
	// NotAView is why the ID was not read as a view: a description, a
	// changeset, a record, content. Missing is set when the store does not
	// hold it. Neither is an error: the migration renames views, and the
	// rest is left as it is.
	NotAView string
	Missing  bool
	Held     []Held
	records  []view.Record
}

// Renamed reports whether the view's ID changed.
func (f *Finding) Renamed() bool { return f.NotAView == "" && !f.Missing && f.New != f.ID }

// Plan is what a run would do for one project: the findings for every ID
// its holders name, sorted by ID, and the table as it would be.
type Plan struct {
	Findings []*Finding
	// Prior is the table the Project's status names, and Table the table
	// with this run's renames added.
	Prior, Table view.Renames
}

// Renames are this run's renames: the ones the prior table lacks.
func (p *Plan) Renames() view.Renames {
	r := view.Renames{}
	for old, cur := range p.Table {
		if _, ok := p.Prior[old]; !ok {
			r[old] = cur
		}
	}
	return r
}

// Survey reads every view the holders name and identifies it with the
// library now. It writes nothing.
func Survey(ctx context.Context, st Store, prior view.Renames, held []Held, workers int) (*Plan, error) {
	by := map[c4.ID]*Finding{}
	for _, h := range held {
		f := by[h.ID]
		if f == nil {
			f = &Finding{ID: h.ID}
			by[h.ID] = f
		}
		f.Held = append(f.Held, h)
	}
	p := &Plan{Prior: prior, Table: view.Renames{}}
	for old, cur := range prior {
		p.Table[old] = cur
	}
	src := &source{st: st}
	for _, f := range by {
		p.Findings = append(p.Findings, f)
		if cur := prior.Current(f.ID); cur != f.ID {
			f.New = cur // renamed by an earlier run
			continue
		}
		v, err := view.Reidentify(ctx, f.ID, src, workers)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case src.failed() != nil:
			return nil, fmt.Errorf("reading %s: %w", f.ID, src.failed())
		case errors.Is(err, errMissing):
			f.Missing = true
			continue
		default:
			f.NotAView = err.Error()
			continue
		}
		f.New = v.ID
		if v.ID == f.ID {
			continue
		}
		if f.records, err = v.Records(); err != nil {
			return nil, fmt.Errorf("view %s re-identified as %s: %w", f.ID, v.ID, err)
		}
		p.Table[f.ID] = v.ID
	}
	sort.Slice(p.Findings, func(i, j int) bool { return p.Findings[i].ID.String() < p.Findings[j].ID.String() })
	return p, nil
}

// WriteTable stores the records of every view this run renames, and then
// the table, and returns the table's ID: the commit point, named in the
// Project's status by the caller only once this returns. With nothing
// renamed it writes nothing and returns the prior table's ID.
func (p *Plan) WriteTable(ctx context.Context, st Store, prior c4.ID) (c4.ID, error) {
	if len(p.Renames()) == 0 {
		return prior, nil
	}
	for _, f := range p.Findings {
		for _, r := range f.records {
			if _, err := st.PutBytes(ctx, r.Data); err != nil {
				return prior, fmt.Errorf("records of %s: %w", f.New, err)
			}
		}
	}
	return st.PutBytes(ctx, p.Table.Bytes())
}

// ExtendGrants is grants with an entry beside each one whose tree the
// table renames: the new ID, the same paths. An entry already there is not
// added again. It returns the entries added.
func ExtendGrants(grants []c4v1.Grant, r view.Renames) ([]c4v1.Grant, []c4v1.Grant) {
	key := func(g c4v1.Grant) string { return g.Tree + " " + strings.Join(g.Paths, "\x00") }
	have := map[string]bool{}
	for _, g := range grants {
		have[key(g)] = true
	}
	out := append([]c4v1.Grant(nil), grants...)
	var added []c4v1.Grant
	for _, g := range grants {
		old, err := c4.Parse(g.Tree)
		if err != nil {
			continue
		}
		cur := r.Current(old)
		if cur == old {
			continue
		}
		n := c4v1.Grant{Tree: cur.String(), Paths: append([]string(nil), g.Paths...)}
		if have[key(n)] {
			continue
		}
		have[key(n)] = true
		out = append(out, n)
		added = append(added, n)
	}
	return out, added
}

// Finished reports whether a WorkJob has finished: Succeeded or Failed, or
// a forecast, which never runs. Only finished jobs are migrated; a live
// one is drained first (its output's chain rests on its views as stored).
func Finished(j *c4v1.WorkJob) bool {
	switch j.Status.Phase {
	case "Succeeded", "Failed", "Forecast":
		return true
	}
	return false
}

// errMissing is a record the store does not hold, and errTooLarge an
// object over MaxRecord: neither stops a run.
var (
	errMissing  = errors.New("not in the store")
	errTooLarge = fmt.Errorf("over %d bytes, so not read as a record", MaxRecord)
)

// source reads records for the survey, checked and bounded. A store that
// does not hold a record, or holds an object too large to be one, is an
// answer about that ID. Anything else (a store that cannot be read, a
// wrong object under an ID) is kept, and stops the run: an ID left
// unexamined is never taken for one that is not a view.
type source struct {
	st   Store
	mu   sync.Mutex
	hard error
}

func (s *source) Record(_ context.Context, id c4.ID) ([]byte, error) {
	rc, err := s.st.Open(id)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, storeurl.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", id, errMissing)
	}
	if err != nil {
		return nil, s.fail(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, MaxRecord+1))
	if err != nil {
		return nil, s.fail(err)
	}
	if len(data) > MaxRecord {
		return nil, fmt.Errorf("%s: %w", id, errTooLarge)
	}
	if got := c4.Identify(bytes.NewReader(data)); got != id {
		return nil, s.fail(fmt.Errorf("the store holds %s under %s", got, id))
	}
	return data, nil
}

func (s *source) fail(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hard == nil {
		s.hard = err
	}
	return err
}

func (s *source) failed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hard
}
