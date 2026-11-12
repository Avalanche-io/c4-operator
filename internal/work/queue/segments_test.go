package queue

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"testing"
)

// Read from its head after every save, the index is exactly the one in
// memory: every path's record (a later fold's wins, a fold with no record
// leaves the earlier one), every unit's, every fold, the output and the
// version, across checkpoints, saves that did not happen, and restarts,
// which take up the count of segments where it was (the independent
// reader's test; dropping records, units, the rewrite or the restored
// count from the segments each fails it or the next).
func TestSegmentsReadAsTheWholeIndex(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	check := func(when string) {
		t.Helper()
		ix, segs, err := readIndex(st, o.index)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if segs > indexCheckpointEvery {
			t.Fatalf("%s: %d segs", when, segs)
		}
		if !maps.Equal(ix.Records, o.records) {
			t.Fatalf("%s: records differ: folded %d, whole %d", when, len(ix.Records), len(o.records))
		}
		if !maps.Equal(ix.Units, o.units) {
			t.Fatalf("%s: units differ: folded %v, whole %v", when, ix.Units, o.units)
		}
		if !reflect.DeepEqual(ix.Folds, o.folds) {
			t.Fatalf("%s: folds differ: folded %d, whole %d", when, len(ix.Folds), len(o.folds))
		}
		if ix.Output != o.view.String() || ix.Version != indexVersion {
			t.Fatalf("%s: output %s want %s, version %d", when, ix.Output, o.view, ix.Version)
		}
	}
	saves := 0
	for i := 0; i < 160; i++ {
		rec := fmt.Sprintf("rec-%d", i)
		if i%11 == 0 {
			rec = "" // a fold with no record: keeps the earlier record of its paths
		}
		cs := unitChangeset(t, st, in, map[string]string{fmt.Sprintf("f%02d.txt", i%23): fmt.Sprintf("u%d", i), fmt.Sprintf("g%03d.txt", i): "x"})
		if err := q.fold(ctx, st, o, Fold{Unit: i % 37, Changeset: cs, Record: rec, Run: fmt.Sprintf("pod-%d", i)}); err != nil {
			t.Fatal(err)
		}
		if i%3 == 1 {
			continue // a tick whose save failed, or two folds in one tick
		}
		if err := o.saveIndex(ctx, st, "ns/job"); err != nil {
			t.Fatal(err)
		}
		saves++
		check(fmt.Sprintf("fold %d", i))
		if i%41 == 40 {
			re := restored(t, st, o)
			if _, err := re.loadIndex(ctx, st, o.index.String(), "ns/job", nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if re.segs != o.segs {
				t.Fatalf("restart: segs %d, was %d", re.segs, o.segs)
			}
			o = re
			check(fmt.Sprintf("restart at %d", i))
		}
	}
	if saves < 100 {
		t.Fatalf("%d saves", saves)
	}
}

// An earlier fold changed in place (its run named after an upgrade, so no
// longer BeforeRuns) reaches the read: that save is a whole index.
func TestAFoldChangedInPlaceIsSavedWhole(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	csA := unitChangeset(t, st, in, map[string]string{"a.txt": "a"})
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: csA, Record: "r0", BeforeRuns: true}); err != nil {
		t.Fatal(err)
	}
	if err := o.saveIndex(ctx, st, "ns/job"); err != nil {
		t.Fatal(err)
	}
	csB := unitChangeset(t, st, in, map[string]string{"b.txt": "b"})
	if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: csB, Record: "r1", Run: "p1"}); err != nil {
		t.Fatal(err)
	}
	if err := o.saveIndex(ctx, st, "ns/job"); err != nil {
		t.Fatal(err)
	}
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: csA, Record: "r0", Run: "p0"}); err != nil {
		t.Fatal(err)
	}
	if err := o.saveIndex(ctx, st, "ns/job"); err != nil {
		t.Fatal(err)
	}
	ix, segs, err := readIndex(st, o.index)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Folds[0].BeforeRuns || segs != 0 || !reflect.DeepEqual(ix.Folds, o.folds) {
		t.Fatalf("segs %d, folds %+v", segs, ix.Folds)
	}
}
