package authoring

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
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
// farm's words; the network to its licence servers alone is not one.
func TestToolRefusals(t *testing.T) {
	tl := &Tool{Name: "sim", Kind: "frames", Gate: "merge"}
	tl.Runs.Image = "c4/runtime-base"
	tl.Needs.Network = []string{"licences", "pypi.org"}
	tl.Needs.Farm = "read"
	errs := ToolRefusals(tl)
	want := []string{
		"sim reaches the network (pypi.org), which this farm cannot fence yet",
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
