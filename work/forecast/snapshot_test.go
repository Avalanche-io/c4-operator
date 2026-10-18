package forecast

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"
)

// A finish calibration written as JSON and restored calibrates bands the
// same way, past a full ring as well as before.
func TestFinishCalibrationSnapshotRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	c := NewFinishCalibration(40)
	c.Min = 1
	keys := []FinishKey{{"render", 0}, {"render", 2}, {"ci", 1}, {"ml", AnyStage}}
	for i := range 300 {
		c.Add(keys[i%len(keys)], r.NormFloat64()*0.3)
	}
	b, err := json.Marshal(c.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var s FinishCalibrationSnapshot
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	d := RestoreFinishCalibration(s, 40)
	d.Min = 1
	if !reflect.DeepEqual(c.Snapshot(), d.Snapshot()) {
		t.Fatal("restored scores differ")
	}
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	band := Band{P50: now.Add(time.Hour), P80: now.Add(2 * time.Hour), P90: now.Add(3 * time.Hour), P95: now.Add(4 * time.Hour)}
	for _, ks := range s.Keys {
		k := ks.Key()
		if c.Count(k) != d.Count(k) || c.Count(k) == 0 {
			t.Fatalf("%v: count %d, restored %d", k, c.Count(k), d.Count(k))
		}
		if c.Apply(k, now, band) != d.Apply(k, now, band) {
			t.Fatalf("%v: restored calibration moves the band differently", k)
		}
	}
	for range 40 {
		c.Add(keys[0], 5)
		d.Add(keys[0], 5)
	}
	if !reflect.DeepEqual(c.Snapshot(), d.Snapshot()) {
		t.Fatal("rings diverged after a restore")
	}
}

func TestRestoreFinishCalibrationBounds(t *testing.T) {
	s := FinishCalibrationSnapshot{Keys: []FinishScores{
		{"render", AnyStage, []float64{1, 2, 3}},
		{"render", AnyStage, []float64{math.Inf(-1), 4, 5}},
	}}
	c := RestoreFinishCalibration(s, 3)
	if got := c.Snapshot().Keys[0].Scores; !reflect.DeepEqual(got, []float64{3, 4, 5}) {
		t.Fatalf("kept %v", got)
	}
	if len((*FinishCalibration)(nil).Snapshot().Keys) != 0 {
		t.Fatal("nil snapshot")
	}
}
