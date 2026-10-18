package estimate

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

// A calibration written as JSON and restored gives the same counts and
// quantiles, past a full ring (whose order is kept) as well as before.
func TestCalibrationSnapshotRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	c := NewCalibration(50)
	c.Min = 1
	strata := []Stratum{{0, 0, 0}, {1, 2, 0}, {1, 2, 1}, {3, 5, 2}, {4, 1, 0}}
	for i := range 700 {
		c.Add(strata[i%len(strata)], r.NormFloat64()*float64(1+i%3))
	}
	b, err := json.Marshal(c.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var s CalibrationSnapshot
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	d := RestoreCalibration(s, 50)
	d.Min = 1
	if !reflect.DeepEqual(c.Snapshot(), d.Snapshot()) {
		t.Fatal("restored scores differ")
	}
	for _, st := range s.Strata {
		k := st.Stratum()
		if c.Count(k) != d.Count(k) || c.Count(k) == 0 {
			t.Fatalf("%v: count %d, restored %d", k, c.Count(k), d.Count(k))
		}
		for _, q := range []float64{0.1, 0.5, 0.8, 0.9, 0.95} {
			x, _ := c.Quantile(k, q)
			y, _ := d.Quantile(k, q)
			if x != y {
				t.Fatalf("%v q%.2f: %v, restored %v", k, q, x, y)
			}
		}
	}
	// The ring keeps evicting oldest first after a restore.
	k := strata[0]
	for range 50 {
		c.Add(k, 1e6)
		d.Add(k, 1e6)
	}
	if x, _ := d.Quantile(k, 0.01); x != 1e6 {
		t.Fatalf("restored ring did not evict its oldest scores: %v", x)
	}
	if !reflect.DeepEqual(c.Snapshot(), d.Snapshot()) {
		t.Fatal("rings diverged after a restore")
	}
}

// Restoring keeps the newest scores when capacity is smaller, merges a
// stratum listed twice in order, and drops scores that are not finite.
func TestRestoreCalibrationBounds(t *testing.T) {
	k := Stratum{Sampled: 2, Measured: anyBand, Open: anyBand}
	s := CalibrationSnapshot{Strata: []StratumScores{
		{2, -1, -1, []float64{1, 2, 3}},
		{2, -1, -1, []float64{4, math.NaN(), 5, math.Inf(1)}},
	}}
	c := RestoreCalibration(s, 3)
	if got := c.Snapshot().Strata[0].Scores; !reflect.DeepEqual(got, []float64{3, 4, 5}) {
		t.Fatalf("kept %v", got)
	}
	if c.Count(k) != 3 {
		t.Fatalf("count %d", c.Count(k))
	}
	if len(RestoreCalibration(CalibrationSnapshot{}, 3).Snapshot().Strata) != 0 || len((*Calibration)(nil).Snapshot().Strata) != 0 {
		t.Fatal("empty snapshot")
	}
}
