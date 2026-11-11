package controller

import (
	"context"
	"sync"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// grantIDs are the IDs a grant may match a source by, each computed from
// content that was read and hashed, never taken from a line that states
// it: the source's own byte ID, and the views the resolver itself makes of
// it with no filter, whatever the Materialization's include: the first
// view of its chain (a changeset's base) and the last (the view a
// changeset makes, or a plain description's own). The same resolver mounts
// it, so what a grant is matched against is what is mounted; a second
// reading of the patch could differ from it (view.ApplyPatch takes an
// entry restated with another mode or date as a removal, where the
// resolver keeps it). A RootID, the description's root lines as stated, is
// not one: a hand-made c4m can state any. Every read is hashed
// (view.Checked), and the result is remembered by project and source byte
// ID, since a source never changes: a hit reads nothing, so it is never
// carried from one project's store to another's.
func (r *MaterializationReconciler) grantIDs(ctx context.Context, project string, store view.Store, data []byte, sourceID c4.ID) ([]c4.ID, error) {
	key := memoKey{project, sourceID}
	if ids, ok := r.views.get(key); ok {
		return ids, nil
	}
	all, err := view.NewFilter(nil)
	if err != nil {
		return nil, err
	}
	checked := view.Checked(store)
	var res *view.Resolved
	if data != nil {
		res, err = view.FromBytes(data, checked, all)
	} else {
		res, err = view.FromID(sourceID, checked, all)
	}
	if err != nil {
		return nil, err
	}
	ids := []c4.ID{sourceID, res.Chain[0].ID, res.View().ID}
	r.views.put(key, ids)
	return ids, nil
}

// viewMemo remembers, by project and a description's byte ID, the IDs
// grantIDs computed for it: a source never changes, so it is resolved for
// grant lookup once, not on every reconcile.
type viewMemo struct {
	mu sync.Mutex
	m  map[memoKey][]c4.ID
}

type memoKey struct {
	project string
	source  c4.ID
}

// viewMemoMax bounds the memo; past it, it starts again.
const viewMemoMax = 4096

func (v *viewMemo) get(k memoKey) ([]c4.ID, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ids, ok := v.m[k]
	return ids, ok
}

func (v *viewMemo) put(k memoKey, ids []c4.ID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m == nil || len(v.m) >= viewMemoMax {
		v.m = map[memoKey][]c4.ID{}
	}
	v.m[k] = ids
}
