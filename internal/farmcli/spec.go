package farmcli

import (
	"fmt"

	"sigs.k8s.io/yaml"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// readSpec reads a template file: a whole WorkJob (its spec is used) or
// a bare WorkJob spec, in YAML or JSON.
func readSpec(data []byte, into *c4v1.WorkJobSpec) error {
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return err
	}
	if probe.Kind == "WorkJob" {
		var j c4v1.WorkJob
		if err := yaml.UnmarshalStrict(data, &j); err != nil {
			return err
		}
		*into = j.Spec
	} else if err := yaml.UnmarshalStrict(data, into); err != nil {
		return err
	}
	if into.Class == "" || len(into.Template.Spec.Containers) == 0 {
		return fmt.Errorf("a template needs spec.class and a container in spec.template")
	}
	return nil
}
