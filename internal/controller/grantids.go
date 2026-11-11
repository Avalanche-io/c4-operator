package controller

import (
	"context"
	"fmt"
	"sync"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// grantIDs are the IDs a grant may match a source by, each computed from
// content that was read and hashed, never taken from a line that states
// it: the source's own byte ID; for a changeset or a chain, the view it
// starts from (its root record read and checked) and the view its sections
// make, applied over the base's records as view.Apply wrote them; for any
// other description, the view it resolves to with no filter, whatever the
// Materialization's include. A RootID, the description's root lines as
// stated, is not one: a hand-made c4m can state any.
func (r *MaterializationReconciler) grantIDs(ctx context.Context, store view.Store, data []byte, sourceID c4.ID) ([]c4.ID, error) {
	checked := view.Checked(store)
	ids := []c4.ID{sourceID}
	if data == nil {
		var err error
		if data, err = view.ReadObject(checked, sourceID); err != nil {
			return nil, err
		}
	}
	if ch, err := view.ReadChain(data); err == nil {
		if _, err := view.ReadObject(checked, ch.Base); err != nil && ch.Base != view.EmptyID() {
			return nil, fmt.Errorf("its base %s: %w", ch.Base, err)
		}
		ids = append(ids, ch.Base)
		at := ch.Base
		for _, sec := range ch.Sections {
			a, err := view.ApplyPatch(ctx, at, sec.Patch, view.StoreRecords(checked))
			if err != nil {
				return nil, fmt.Errorf("applying it over %s: %w", at, err)
			}
			at = a.ID
		}
		return append(ids, at), nil
	}
	if id, ok := r.views.get(sourceID); ok {
		return append(ids, id), nil
	}
	all, err := view.NewFilter(nil)
	if err != nil {
		return nil, err
	}
	res, err := view.FromBytes(data, checked, all)
	if err != nil {
		return nil, err
	}
	id := res.View().ID
	r.views.put(sourceID, id)
	return append(ids, id), nil
}

// viewMemo remembers the unfiltered view a description resolves to, by
// the description's byte ID: a source never changes, so its view is
// resolved once, not on every reconcile.
type viewMemo struct {
	mu sync.Mutex
	m  map[c4.ID]c4.ID
}

// viewMemoMax bounds the memo; past it, it starts again.
const viewMemoMax = 4096

func (v *viewMemo) get(source c4.ID) (c4.ID, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	id, ok := v.m[source]
	return id, ok
}

func (v *viewMemo) put(source, id c4.ID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m == nil || len(v.m) >= viewMemoMax {
		v.m = map[c4.ID]c4.ID{}
	}
	v.m[source] = id
}
