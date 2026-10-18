package server

import "testing"

// A unit's own progress line, read by its node and scraped, is shown as
// measured; a unit without one is filled by time over its estimate.
func TestMeasuredProgress(t *testing.T) {
	s := farm(t)
	s.m.metrics.nodes = map[string]series{"w1": {{name: "c4_task_progress",
		labels: map[string]string{"namespace": "show-a", "pod": "sh040-light-v007-u4"}, value: 0.4}}}
	var d struct {
		Progress map[string]struct {
			Fraction float64 `json:"fraction"`
			Measured bool    `json:"measured"`
		} `json:"progress"`
	}
	if code := get(t, s, "/api/v1/jobs/show-a/sh040-light-v007?as=wrangler", &d); code != 200 {
		t.Fatal(code)
	}
	if p := d.Progress["4"]; !p.Measured || p.Fraction != 0.4 {
		t.Fatalf("unit 4 (reports 0.4): %+v", p)
	}
	if p := d.Progress["5"]; p.Measured {
		t.Fatalf("unit 5 reports nothing but is measured: %+v", p)
	}
}
