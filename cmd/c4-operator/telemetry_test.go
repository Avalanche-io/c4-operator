package main

import (
	"reflect"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
)

// Every --work-policy is recorded in a diagnostic bundle as a policy a
// replay runs, and the one it runs is the one the manager ran.
func TestTelemetryNamesEveryPolicy(t *testing.T) {
	for _, name := range []string{"", "objective", "edf", "fifo"} {
		pol, err := policyFor(name)
		if err != nil {
			t.Fatal(err)
		}
		replay := telemetry.PolicyOf(name).Func()
		if replay == nil || reflect.ValueOf(replay).Pointer() != reflect.ValueOf(pol).Pointer() {
			t.Errorf("--work-policy %q replays through another policy", name)
		}
	}
}
