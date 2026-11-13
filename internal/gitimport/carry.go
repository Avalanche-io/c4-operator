package gitimport

import (
	"context"
	"fmt"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Carried is what Carry made for one mapping: the new changeset of each
// line it carried, as old changeset to new (what the renamed line records
// beside the views), and the objects to store before that line is
// written: each new changeset and the records along its paths.
type Carried struct {
	Changesets view.Renames
	Objects    [][]byte
	Lines      int // lines carried
}

// Carry makes, for a library change whose renames are r, a changeset for
// every line of m whose parent or view r renames: its stored changeset
// carried to the parent's new ID. A stored changeset says what it means
// only against the base it was written over (a removal is the base's
// entry restated exactly), and a renamed base records some entries
// differently (c4 #107: a directory's size), so its text over the new ID
// would mean something else to any reader, the c4m library's included: a
// removal of such a directory reads as a write of it, and nothing is
// removed. So each entry is read against the stored base, whose records
// stay, and done to the new parent (view.ApplyPatchFrom), which gives the
// changeset over the new parent that says it. It must come to the line's
// renamed view. Chains are then made of the new changesets (renameLines):
// every chain stored or served is text any reader resolves by the spec.
//
// Nothing is written. An error names the ref, the line and the commit, and
// no line is carried when any one cannot be: a changeset that does not
// come to the renamed view, one ApplyPatchFrom refuses (a directory
// written by its C4 ID alone over one the parent holds), or one the store
// does not hold.
func Carry(ctx context.Context, m []Commit, r view.Renames, src view.RecordSource) (*Carried, error) {
	out := &Carried{Changesets: view.Renames{}}
	for i := range m {
		c := &m[i]
		if c.Commit == "" || c.RefOnly || c.Changeset.IsNil() {
			continue
		}
		parent, produced := r.Current(c.ParentView), r.Current(c.View)
		if parent == c.ParentView && produced == c.View {
			continue
		}
		fail := func(format string, args ...any) error {
			return fmt.Errorf("ref %s, mapping line %d (commit %s): its changeset %s, carried to the renamed parent %s: %s", c.Ref, i+1, c.Commit, c.Changeset, parent, fmt.Sprintf(format, args...))
		}
		data, err := src.Record(ctx, c.Changeset)
		if err != nil {
			return nil, fail("%v", err)
		}
		one, err := view.ReadChain(data)
		if err != nil {
			return nil, fail("%v", err)
		}
		if len(one.Sections) != 1 {
			return nil, fail("it reads as %d sections", len(one.Sections))
		}
		res, err := view.ApplyPatchFrom(ctx, parent, one.Base, one.Sections[0].Patch, src)
		if err != nil {
			return nil, fail("%v", err)
		}
		if res.ID != produced {
			return nil, fail("it comes to %s, and the line's view is now %s", res.ID, produced)
		}
		if len(res.Changeset) == 0 {
			return nil, fail("it changes nothing over the new parent")
		}
		out.Lines++
		if res.ChangesetID == c.Changeset {
			continue
		}
		out.Changesets[c.Changeset] = res.ChangesetID
		out.Objects = append(out.Objects, res.Changeset)
		for _, rec := range res.Records {
			out.Objects = append(out.Objects, rec.Data)
		}
	}
	return out, nil
}
