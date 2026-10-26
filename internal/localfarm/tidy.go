package localfarm

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A farm made before tool tests went quiet kept a failed test of an
// older version of a tool as a Needs-you item a person could neither
// acknowledge nor requeue. up removes those, and only those.
const (
	// toolTestLabel names, on a tool's test job, the tool it tests, and
	// toolTestVersion the version it tests.
	toolTestLabel   = "c4.avalanche.io/tool-test"
	toolTestVersion = "c4.avalanche.io/tool-test-version"
	// farmNamespace holds the farm's own tools (a farm-wide publish).
	farmNamespace = "c4-system"
)

var toolKind = schema.GroupVersionKind{Group: "c4.avalanche.io", Version: "v1alpha1", Kind: "Tool"}

// staleTest is a failed tool test a newer published version replaced.
type staleTest struct {
	job         unstructured.Unstructured
	tool        string
	tested, now string
}

// versionN is v<N>'s N; 0 for anything else.
func versionN(v string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(v, "v"))
	if err != nil || !strings.HasPrefix(v, "v") {
		return 0
	}
	return n
}

// newestVersions are each tool's newest published version, by
// namespace/name, from its status (version, and versions newest last).
func newestVersions(tools []unstructured.Unstructured) map[string]string {
	out := map[string]string{}
	for _, t := range tools {
		best, _, _ := unstructured.NestedString(t.Object, "status", "version")
		vs, _, _ := unstructured.NestedSlice(t.Object, "status", "versions")
		for _, v := range vs {
			if m, ok := v.(map[string]any); ok {
				if s, _ := m["version"].(string); versionN(s) > versionN(best) {
					best = s
				}
			}
		}
		if best != "" {
			out[t.GetNamespace()+"/"+t.GetName()] = best
		}
	}
	return out
}

// staleTests are the failed tool tests whose tool has a newer published
// version than the one they tested: the tool in the job's own namespace (a
// project's tool), else the farm's. A passed test, a test of the newest
// version, a test whose versions cannot be read, and every job that is not
// a tool test stay.
func staleTests(jobs []unstructured.Unstructured, newest map[string]string) []staleTest {
	var out []staleTest
	for _, j := range jobs {
		tool := j.GetLabels()[toolTestLabel]
		tested := j.GetAnnotations()[toolTestVersion]
		phase, _, _ := unstructured.NestedString(j.Object, "status", "phase")
		if tool == "" || phase != "Failed" || versionN(tested) == 0 {
			continue
		}
		now, ok := newest[j.GetNamespace()+"/"+tool]
		if !ok {
			now = newest[farmNamespace+"/"+tool]
		}
		if versionN(now) > versionN(tested) {
			out = append(out, staleTest{job: j, tool: tool, tested: tested, now: now})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		return out[a].job.GetNamespace()+out[a].job.GetName() < out[b].job.GetNamespace()+out[b].job.GetName()
	})
	return out
}

// tidyToolTests removes the farm's stale failed tool tests, saying each.
// Their signed records stay in the store; only the job goes.
func tidyToolTests(ctx context.Context, c client.Client, o UpOptions) error {
	var jobs, tools unstructured.UnstructuredList
	jobs.SetGroupVersionKind(workJobKind.GroupVersion().WithKind("WorkJobList"))
	tools.SetGroupVersionKind(toolKind.GroupVersion().WithKind("ToolList"))
	if err := c.List(ctx, &jobs, client.HasLabels{toolTestLabel}); err != nil {
		return nil // a farm without them yet: nothing to tidy
	}
	if err := c.List(ctx, &tools); err != nil {
		return nil
	}
	for _, s := range staleTests(jobs.Items, newestVersions(tools.Items)) {
		if err := c.Delete(ctx, &s.job); err != nil && !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("removing %s/%s, the failed test of %s %s: %w", s.job.GetNamespace(), s.job.GetName(), s.tool, s.tested, err)
		}
		o.logf("tools     removed the failed test of %s %s (%s/%s); %s replaced it\n", s.tool, s.tested, s.job.GetNamespace(), s.job.GetName(), s.now)
	}
	return nil
}
