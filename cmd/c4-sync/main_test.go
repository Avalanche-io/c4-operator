package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

func TestParse(t *testing.T) {
	var out bytes.Buffer
	if _, err := parse(nil, &out); err == nil || !strings.Contains(err.Error(), "--hud") {
		t.Fatalf("want --hud required, got %v", err)
	}
	o, err := parse([]string{"--hud", "http://hud", "--interval", "30s"}, &out)
	if err != nil || o.hud != "http://hud" || o.interval != 30*time.Second || o.listen != ":8090" || o.agent != "c4-sync" {
		t.Fatalf("parse: %+v %v", o, err)
	}
	if _, err := parse([]string{"--hud", "x", "--interval", "1s"}, &out); err == nil {
		t.Fatal("a 1s interval should be refused")
	}
	out.Reset()
	if _, err := parse([]string{"--version"}, &out); err != errVersion || !strings.Contains(out.String(), "flow, github, kitsu, linear, mlflow") {
		t.Fatalf("version: %v %q", err, out.String())
	}
}

// The sample Tracker decodes strictly into the API type, so its fields
// are the ones c4-sync reads.
func TestSampleTracker(t *testing.T) {
	b, err := os.ReadFile("../../config/samples/tracker-github/tracker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var tr c4v1.Tracker
	if err := yaml.UnmarshalStrict(b, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Spec.Kind != "github" || tr.Spec.Scope["repo"] != "acme/widgets" || len(tr.Spec.Intake) != 1 ||
		tr.Spec.Intake[0].Job == nil || tr.Spec.Intake[0].Review.Wait != "review" {
		t.Fatalf("sample: %+v", tr.Spec)
	}
}

func TestSampleFlowTracker(t *testing.T) {
	b, err := os.ReadFile("../../config/samples/tracker-flow/tracker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var tr c4v1.Tracker
	if err := yaml.UnmarshalStrict(b, &tr); err != nil {
		t.Fatal(err)
	}
	in := tr.Spec.Intake[0]
	if tr.Spec.Kind != "flow" || in.Rework != "nextVersion" || in.Match.Fields["sg_status_list"] != "rdy" ||
		tr.Spec.StatusOut.States["in-review"] != "rev" || tr.Spec.VerdictsIn["status:fix"] != "rework" {
		t.Fatalf("sample: %+v", tr.Spec)
	}
}

func TestSampleMLflowTracker(t *testing.T) {
	b, err := os.ReadFile("../../config/samples/tracker-mlflow/tracker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var tr c4v1.Tracker
	if err := yaml.UnmarshalStrict(b, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Spec.Kind != "mlflow" || tr.Spec.People["mlflow-registry"] != "party:ml-leads" || tr.Spec.Intake[0].Match.Fields["status"] != "SCHEDULED" {
		t.Fatalf("sample: %+v", tr.Spec)
	}
}

func TestSampleKitsuTracker(t *testing.T) {
	b, err := os.ReadFile("../../config/samples/tracker-kitsu/tracker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var tr c4v1.Tracker
	if err := yaml.UnmarshalStrict(b, &tr); err != nil {
		t.Fatal(err)
	}
	in, fc := tr.Spec.Intake[0], tr.Spec.Intake[1]
	if tr.Spec.Kind != "kitsu" || in.Rework != "nextVersion" || in.Match.Fields["task_status"] != "ready" ||
		fc.As != "forecast" || fc.Forecast.Field != "estimation" || len(tr.Spec.StatusOut.States) != 0 || len(tr.Spec.VerdictsIn) != 0 {
		t.Fatalf("sample: %+v", tr.Spec)
	}
	if _, err := tracker.New(tr.Spec.Kind, tracker.Config{Endpoint: tr.Spec.Endpoint, Scope: tr.Spec.Scope}); err != nil {
		t.Fatal(err)
	}
}

func TestSampleLinearTracker(t *testing.T) {
	b, err := os.ReadFile("../../config/samples/tracker-linear/tracker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var tr c4v1.Tracker
	if err := yaml.UnmarshalStrict(b, &tr); err != nil {
		t.Fatal(err)
	}
	in := tr.Spec.Intake[0]
	if tr.Spec.Kind != "linear" || tr.Spec.Scope["team"] != "ENG" || len(in.Match.Labels) != 2 || in.Rework != "nextVersion" ||
		tr.Spec.VerdictsIn["state:Changes Requested"] != "rework" || tr.Spec.People["dana@example.com"] != "dana" {
		t.Fatalf("sample: %+v", tr.Spec)
	}
	if _, err := tracker.New(tr.Spec.Kind, tracker.Config{Scope: tr.Spec.Scope}); err != nil {
		t.Fatalf("c4-sync builds the sample's adapter: %v", err)
	}
}
