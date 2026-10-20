package metrics

import (
	"strings"
	"testing"
)

// No series names a pod, a job, a node, a namespace or an object: metrics
// leave the farm, and those names identify the work.
func TestNoIdentifyingLabels(t *testing.T) {
	if len(labelsOf) < 20 {
		t.Fatalf("only %d series declared", len(labelsOf))
	}
	banned := map[string]bool{"pod": true, "job": true, "unit": true, "node": true, "namespace": true, "object": true, "id": true}
	for name, labels := range labelsOf {
		if strings.HasPrefix(name, "c4_task_") {
			t.Errorf("%s: per-task series are served on /units, not in metrics", name)
		}
		for _, l := range labels {
			if banned[l] {
				t.Errorf("%s is labelled by %s", name, l)
			}
		}
	}
}
