package main

import (
	"bytes"
	"strings"
	"testing"
)

// Rotating the farm's telemetry key lets go of what it keeps, so with no
// one at a terminal it takes --yes, and says so before asking the farm
// anything; a subcommand it does not know is its usage.
func TestTelemetryRotateAsks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	err := run([]string{"telemetry", "rotate"}, &out, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "pass --yes where no one is at a terminal") {
		t.Errorf("rotate without --yes: %v", err)
	}
	for _, args := range [][]string{{"telemetry", "up"}, {"telemetry", "resolve"}, {"telemetry", "on", "b"}} {
		if err := run(args, &out, strings.NewReader("")); err == nil || !strings.HasPrefix(err.Error(), "usage: c4 farm telemetry") {
			t.Errorf("%v: %v", args, err)
		}
	}
}
