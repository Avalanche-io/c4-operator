package diagnose

import (
	"os"
	"strings"
	"testing"
)

// The committed bundle reads back as the scheduler's own types, each
// decision the farm's, and every synthetic name with its pseudonym.
func TestRead(t *testing.T) {
	f, err := os.Open("../../hack/sim/testdata/gpu-tight.tgz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Snapshots) == 0 || !strings.HasPrefix(b.ID, "c4") || !strings.Contains(b.README, b.ID) {
		t.Fatalf("bundle %s, %d snapshots", b.ID, len(b.Snapshots))
	}
	for i, s := range b.Snapshots {
		if !s.Faithful || s.Policy != "objective" {
			t.Errorf("snapshot %d: %s, faithful %v", i, s.Policy, s.Faithful)
		}
		for _, j := range s.Input.Jobs {
			if !strings.HasPrefix(s.Names[j.ID], "j.") {
				t.Errorf("job %s has no pseudonym: %q", j.ID, s.Names[j.ID])
			}
		}
		if len(s.Input.Jobs) > 0 && len(s.Decided.Jobs) == 0 {
			t.Errorf("snapshot %d decided nothing", i)
		}
	}
}
