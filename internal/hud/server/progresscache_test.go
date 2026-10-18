package server

import "testing"

func TestProgressReadings(t *testing.T) {
	fm := farmMetrics{nodes: map[string]series{
		"w1": {{name: "c4_task_progress", labels: map[string]string{"namespace": "show-a", "pod": "u-1"}, value: 0.4},
			{name: "c4_node_publish_seconds_bucket", labels: map[string]string{"le": "1"}, value: 3}},
		"w2": {{name: "c4_task_progress", labels: map[string]string{"namespace": "show-b", "pod": "u-2"}, value: 0.9}},
	}}
	got := progressReadings(fm)
	if len(got) != 2 || got[[2]string{"show-a", "u-1"}] != 0.4 || got[[2]string{"show-b", "u-2"}] != 0.9 {
		t.Fatalf("readings %v", got)
	}
}
