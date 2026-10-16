package queue

import (
	"bytes"
	"context"
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
}

func newOutput() *output { return &output{folded: map[int]int{}, writer: map[string]int{}} }

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
func (q *Queue) fold(ctx context.Context, st *storeurl.Sharded, o *output, idx int, csID string) error {
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
	chainID, err := st.PutBytes(ctx, o.chain)
	if err != nil {
		return err
	}
	o.view, o.id = cur, chainID
	o.folded[idx]++
	o.count++
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
