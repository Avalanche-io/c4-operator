package queue

import (
	"math"
	"testing"
	"time"
)

func TestClassPrior(t *testing.T) {
	q := &Queue{}
	if _, ok := q.classPrior("show-a", "render"); ok {
		t.Fatal("a prior from nothing")
	}
	for _, s := range []float64{6, 5, 7, 6, 60} {
		q.noteDuration("show-a", "render", s)
	}
	p, ok := q.classPrior("show-a", "render")
	// median 6; deviations 0,1,1,0,54 give a MAD of 1, so 1.48, under the
	// floor of a quarter of the median: the outlier does not move it.
	if !ok || p.Mean != 6 || math.Abs(p.SD-1.5) > 1e-9 {
		t.Fatalf("prior %+v", p)
	}
	// Another project borrows it only if this one shares its history.
	if _, ok := q.classPrior("show-b", "render"); ok {
		t.Fatal("borrowed without sharing")
	}
	q.shareHist = map[string]bool{"show-a": true}
	if p, ok := q.classPrior("show-b", "render"); !ok || p.Mean != 6 {
		t.Fatalf("shared prior %+v %v", p, ok)
	}
	// It is saved with the project's calibrations and restored after
	// what runs since a start.
	rec := q.calRecordOf("show-a", time.Now())
	q2 := &Queue{}
	q2.noteDuration("show-a", "render", 8)
	q2.restoreProject(&rec)
	if w := q2.durations["show-a"]["render"]; len(w) != 6 || w[5] != 8 {
		t.Fatalf("restored %v", w)
	}
}
