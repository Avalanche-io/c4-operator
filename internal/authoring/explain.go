package authoring

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Explain is what a plan's jobs lower to, for a person who wants to learn
// the operator: each job's WorkJob, class and params, inputs, template,
// units and deadline, then what the farm does differently from the file.
func Explain(p *Plan) string {
	var b strings.Builder
	for _, j := range p.Jobs {
		req := j.Request
		var spec c4v1.WorkJobSpec
		_ = json.Unmarshal(req.Spec, &spec)
		fmt.Fprintf(&b, "%s  WorkJob %s/%s\n", req.Name, p.Project, req.Name)
		fmt.Fprintf(&b, "  class %s · params {%s}\n", spec.Class, params(spec.Params))
		var ins []string
		for _, in := range req.Inputs {
			if in.MountPath == ToolsMount {
				continue
			}
			what := "view " + short(in.View)
			if in.FromJob != "" {
				what = "fromJob " + in.FromJob
			}
			mode := "writable"
			if in.ReadOnly {
				mode = "read-only"
			}
			ins = append(ins, fmt.Sprintf("%s at %s (%s)", what, in.MountPath, mode))
		}
		if len(ins) > 0 {
			line := "  inputs   " + strings.Join(ins, ", ")
			if len(j.After) > 0 {
				line += "   dependsOn [" + strings.Join(j.After, ", ") + "]"
			}
			b.WriteString(line + "\n")
		}
		if cs := spec.Template.Spec.Containers; len(cs) > 0 {
			c := cs[0]
			fmt.Fprintf(&b, "  template image %s\n", c.Image)
			fmt.Fprintf(&b, "           command %s · tool %s@%s (%s) at %s\n", strings.Join(c.Command, " "), j.Tool.Name, j.Tool.Version, short(j.Tool.View), ToolsMount)
			if r := resources(c.Resources); r != "" {
				fmt.Fprintf(&b, "           resources %s\n", r)
			}
		}
		fmt.Fprintf(&b, "  units    %s\n", j.What())
		fmt.Fprintf(&b, "  deadline %s\n", j.Due)
		for _, k := range sortedKeys(j.Annotations) {
			fmt.Fprintf(&b, "  annotation %s=%s\n", k, j.Annotations[k])
		}
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "note: %s\n", w)
	}
	return b.String()
}

func params(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + m[k]
	}
	return strings.Join(parts, ", ")
}

func resources(r corev1.ResourceRequirements) string {
	var parts []string
	for _, n := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage, "nvidia.com/gpu"} {
		q, ok := r.Requests[n]
		if !ok {
			continue
		}
		s := fmt.Sprintf("%s %s", n, q.String())
		if _, limited := r.Limits[n]; limited {
			s += " (limit)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// short is a C4 ID cut for reading: c4…<last 4>.
func short(id string) string {
	if len(id) > 12 && strings.HasPrefix(id, "c4") {
		return "c4…" + id[len(id)-4:]
	}
	return id
}
