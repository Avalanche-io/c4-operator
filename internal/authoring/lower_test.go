package authoring

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/schema"
)

// Memory and GPUs are limited to what is requested and CPU only
// requested; sizes are binary, a named size and the job's needs override
// the tool's.
func TestRequestsAndLimits(t *testing.T) {
	gpu, two := 1, 2
	tl := &Tool{Name: "depth"}
	tl.Needs.Resources = Resources{CPU: 4, Memory: "16G", GPU: &gpu, Scratch: "50G"}
	tl.Needs.Sizes = map[string]Resources{"heavy": {Memory: "64G", GPU: &two}}
	rr, errs := requests(tl, "heavy", &Resources{CPU: 8})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	want := map[corev1.ResourceName]string{corev1.ResourceCPU: "8", corev1.ResourceMemory: "64Gi", corev1.ResourceEphemeralStorage: "50Gi", "nvidia.com/gpu": "2"}
	for name, q := range want {
		if got := rr.Requests[name]; got.String() != q {
			t.Errorf("request %s %s, want %s", name, got.String(), q)
		}
	}
	if len(rr.Limits) != 2 || rr.Limits.Memory().String() != "64Gi" || rr.Limits.Name("nvidia.com/gpu", "").String() != "2" {
		t.Errorf("limits %v", rr.Limits)
	}
	if _, errs := requests(tl, "huge", nil); len(errs) != 1 || errs[0].Fix != "its sizes are heavy" {
		t.Errorf("unknown size: %v", errs)
	}
	for in, out := range map[string]string{"64G": "64Gi", "64Gi": "64Gi", "512M": "512Mi", "1T": "1Ti", "100": "100", "8GB": "8Gi"} {
		if got := kubeSize(in); got != out {
			t.Errorf("kubeSize(%q) = %q, want %q", in, got, out)
		}
	}
}

// A tool is refused for each thing this farm cannot run yet, in the
// farm's words; a declared network is not one.
func TestToolRefusals(t *testing.T) {
	tl := &Tool{Name: "sim", Kind: "frames", Gate: "merge"}
	tl.Runs.Image = "c4/runtime-base"
	tl.Needs.Network = []string{"licences", "pypi"}
	tl.Needs.Farm = "read"
	errs := ToolRefusals(tl)
	want := []string{
		"sim passes the merge gate, which this farm cannot hold yet",
		"sim acts on the farm (read), which this farm cannot grant yet",
	}
	if len(errs) != len(want) {
		t.Fatalf("%v", errs)
	}
	for i, e := range errs {
		if e.Path != "tool" || e.Message != want[i] {
			t.Errorf("%d: %+v, want %q", i, e, want[i])
		}
	}
	ok := &Tool{Name: "karma", Kind: "frames"}
	ok.Runs.Image = "c4/houdini"
	ok.Needs.Network = []string{"licences"}
	if errs := ToolRefusals(ok); len(errs) != 0 {
		t.Fatalf("network to licence servers alone: %v", errs)
	}
}

// A tool job runs under its tool: the networks it declares (not the
// licence servers, which go with licences) or none, sealed. A tool's test
// job is lowered the same way, so a declared network applies to it too.
func TestToolJobsRunUnderTheirTool(t *testing.T) {
	tl := &Tool{Name: "fetch", Kind: "once"}
	tl.Runs.Image = "c4/runtime-base"
	tl.Runs.Command = []string{"true"}
	tl.Test = &schema.Test{Tree: "test/"}
	ref := ToolRef{Name: "fetch", Version: "v1", View: "c4view", Image: "registry.example/base@sha256:" + strings.Repeat("a", 64)}
	spec := func(req *api.SubmitRequest) c4v1.WorkJobSpec {
		t.Helper()
		var s c4v1.WorkJobSpec
		if err := json.Unmarshal(req.Spec, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	lower := func() (*api.SubmitRequest, *api.SubmitRequest) {
		t.Helper()
		job, errs := lowerTool(toolJob{tool: tl, ref: ref, project: "p", ns: "p", root: "/show", name: "fetch-v001"})
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		test, errs := LowerTest(tl, ref, "c4testtree", "p", "p", "/show")
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		return job, test
	}
	job, test := lower()
	for _, req := range []*api.SubmitRequest{job, test} {
		if s := spec(req); s.Tool == nil || s.Tool.Name != "fetch" || len(s.Tool.Network) != 0 {
			t.Fatalf("undeclared: %+v", s.Tool)
		}
	}
	tl.Needs.Network = []string{"licences", "studio-api"}
	job, test = lower()
	for _, req := range []*api.SubmitRequest{job, test} {
		if s := spec(req); s.Tool == nil || len(s.Tool.Network) != 1 || s.Tool.Network[0] != "studio-api" {
			t.Fatalf("declared: %+v", s.Tool)
		}
	}
}
