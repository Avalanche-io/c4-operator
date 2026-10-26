package localfarm

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A failed tool test goes only when its tool has a newer published
// version than the one it tested, the project's tool or else the farm's: a
// test of the newest version stays whatever its end, a passed one stays, a
// person's job is never touched, and another project's tool is its own.
func TestStaleTests(t *testing.T) {
	job := func(ns, name, tool, version, phase string) unstructured.Unstructured {
		var u unstructured.Unstructured
		u.SetNamespace(ns)
		u.SetName(name)
		if tool != "" {
			u.SetLabels(map[string]string{toolTestLabel: tool})
			u.SetAnnotations(map[string]string{toolTestVersion: version})
		}
		unstructured.SetNestedField(u.Object, phase, "status", "phase")
		return u
	}
	tool := func(ns, name, version string, versions ...string) unstructured.Unstructured {
		var u unstructured.Unstructured
		u.SetNamespace(ns)
		u.SetName(name)
		unstructured.SetNestedField(u.Object, version, "status", "version")
		var vs []any
		for _, v := range versions {
			vs = append(vs, map[string]any{"version": v})
		}
		unstructured.SetNestedSlice(u.Object, vs, "status", "versions")
		return u
	}
	newest := newestVersions([]unstructured.Unstructured{
		tool("c4-system", "comp", "v2", "v1", "v2"),
		tool("c4-system", "render", "v1", "v1"),
		tool("glassworks", "comp", "v1", "v1"),       // glassworks' own comp
		tool("c4-system", "notes", "v1", "v1", "v2"), // v2 published, its test failed: v1 still in use
	})
	jobs := []unstructured.Unstructured{
		job("saltmarsh", "comp-farmtest-207760c4", "comp", "v1", "Failed"),     // v2 replaced it: goes
		job("saltmarsh", "comp-farmtest-9a8b7c6d", "comp", "v2", "Succeeded"),  // the newest: stays
		job("saltmarsh", "render-farmtest-11111111", "render", "v1", "Failed"), // the newest render: stays
		job("glassworks", "comp-test-22222222", "comp", "v1", "Failed"),        // glassworks' comp is at v1: stays
		job("saltmarsh", "sm0140-comp-v001", "", "", "Failed"),                 // a person's: never
		job("saltmarsh", "notes-farmtest-33333333", "notes", "v1", "Failed"),   // v2 was published: goes
		job("saltmarsh", "notes-farmtest-44444444", "notes", "v2", "Failed"),   // the newest, failed: stays
	}
	var got []string
	for _, s := range staleTests(jobs, newest) {
		got = append(got, s.job.GetNamespace()+"/"+s.job.GetName()+" "+s.tool+" "+s.tested+"->"+s.now)
	}
	if strings.Join(got, ", ") != "saltmarsh/comp-farmtest-207760c4 comp v1->v2, saltmarsh/notes-farmtest-33333333 notes v1->v2" {
		t.Errorf("stale %v", got)
	}
}
