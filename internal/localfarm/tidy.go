package localfarm

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// toolTestLabel names, on a tool's test job, the tool it tests.
const toolTestLabel = "c4.avalanche.io/tool-test"

// supersededTests are the failed tool tests a newer test of the same tool
// in the same namespace has replaced: the farm's own leftovers from an
// earlier publish (an older image, an older tree), never a person's work,
// which up removes so they do not sit in Needs-you. Each is
// namespace/name, with the tool it tested.
func supersededTests(jobs []unstructured.Unstructured) (old []unstructured.Unstructured) {
	newest := map[string]int64{} // namespace/tool: the latest test's creation
	for _, j := range jobs {
		tool := j.GetLabels()[toolTestLabel]
		if tool == "" {
			continue
		}
		k := j.GetNamespace() + "/" + tool
		if t := j.GetCreationTimestamp().Unix(); t > newest[k] {
			newest[k] = t
		}
	}
	for _, j := range jobs {
		tool := j.GetLabels()[toolTestLabel]
		if tool == "" {
			continue
		}
		phase, _, _ := unstructured.NestedString(j.Object, "status", "phase")
		if phase == "Failed" && j.GetCreationTimestamp().Unix() < newest[j.GetNamespace()+"/"+tool] {
			old = append(old, j)
		}
	}
	sort.Slice(old, func(a, b int) bool {
		return old[a].GetNamespace()+old[a].GetName() < old[b].GetNamespace()+old[b].GetName()
	})
	return old
}

// tidyToolTests removes the farm's superseded failed tool tests and says
// what it removed, in one line.
func tidyToolTests(ctx context.Context, c client.Client, o UpOptions) error {
	var l unstructured.UnstructuredList
	l.SetGroupVersionKind(workJobKind.GroupVersion().WithKind("WorkJobList"))
	if err := c.List(ctx, &l, client.HasLabels{toolTestLabel}); err != nil {
		return nil // a farm without them, or not yet: nothing to tidy
	}
	old := supersededTests(l.Items)
	var tools []string
	for i := range old {
		if err := c.Delete(ctx, &old[i]); err != nil && !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("removing %s/%s, a failed test of an older %s: %w", old[i].GetNamespace(), old[i].GetName(), old[i].GetLabels()[toolTestLabel], err)
		}
		if t := old[i].GetLabels()[toolTestLabel]; !slices.Contains(tools, t) {
			tools = append(tools, t)
		}
	}
	if len(old) > 0 {
		o.logf("tools     removed %s of an older %s, since a newer one ran (c4 farm tools shows each tool's newest)\n",
			map[bool]string{true: "1 failed test", false: fmt.Sprintf("%d failed tests", len(old))}[len(old) == 1], andList(tools))
	}
	return nil
}
