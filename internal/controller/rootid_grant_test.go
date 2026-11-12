package controller

import (
	"context"
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// A project that grants the view a changeset makes may materialize that
// changeset, with or without an include that does not reach the changed
// path: grant lookup finds the grant by the resolution's RootID, which is
// that view's ID whatever the filter.
func TestGrantByTheViewAChangesetMakes(t *testing.T) {
	st := newStore(t)
	v := st.ingest(t, show("rootid"))
	ctx := context.Background()
	frame := []byte("sh010 1005 rootid")
	id, err := st.store.PutBytes(ctx, frame)
	if err != nil {
		t.Fatal(err)
	}
	edit := view.Edit{Path: "shots/sh010/plates/sh010.1005.exr", Entry: view.FileEntry("", int64(len(frame)), id, false)}
	applied, err := view.Apply(ctx, v.ID, []view.Edit{edit}, view.StoreRecords(st.store))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range applied.Records {
		if _, err := st.store.PutBytes(ctx, r.Data); err != nil {
			t.Fatal(err)
		}
	}
	csID, err := st.store.PutBytes(ctx, applied.Changeset)
	if err != nil {
		t.Fatal(err)
	}
	ns := newNamespace(t, "rootid")
	putProject(t, "rootid", []string{ns}, st, c4v1.ScopeGrants, []c4v1.Grant{{Tree: applied.ID.String()}})
	putMat(t, ns, "whole", byID(csID))
	m := waitMat(t, ns, "whole", c4v1.PhaseReady, "Ready")
	if m.Status.RootID != applied.ID.String() {
		t.Errorf("no include: status.rootID %s, the view the changeset makes %s", m.Status.RootID, applied.ID)
	}
	putMat(t, ns, "editorial", byID(csID), "editorial/")
	m = waitMat(t, ns, "editorial", c4v1.PhaseReady, "Ready")
	if m.Status.RootID != applied.ID.String() {
		t.Errorf("include editorial/: status.rootID %s, the view the changeset makes %s", m.Status.RootID, applied.ID)
	}
}
