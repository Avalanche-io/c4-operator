package server

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A development viewer (--hud-dev-viewers) has no RBAC behind it, so its
// role's rights stand in for what a studio binds with a real sign-in:
// changing jobs and mounts in the projects it sees, for every role but
// facilities; approving a burst and granting submissions for producers;
// disabling a node for facilities; and everything for a wrangler or a
// farm admin. Reads are not checked here; every response is already
// filtered to the viewer's projects.

// devWriters names the roles that may write each resource.
var devWriters = map[string][]string{
	"workjobs":         {"artist", "supervisor", "producer", "wrangler", "farm-admin", "pipeline-td"},
	"materializations": {"artist", "supervisor", "producer", "wrangler", "farm-admin", "pipeline-td"},
	// a person's own agents, as config/rbac/people.yaml lets every people role
	"agentbundles":     {"artist", "supervisor", "producer", "wrangler", "farm-admin", "pipeline-td"},
	"capacityrequests": {"producer", "wrangler", "farm-admin"},
	"submitgrants":     {"producer", "wrangler", "farm-admin"},
	"nodes":            {"facilities", "wrangler", "farm-admin"},
	// the installer binds the farm to its owner
	"deploymentbindings": {"wrangler", "farm-admin"},
}

// devRole is a development viewer's role, or the HUD's guess for one
// that names none: wrangler for a viewer of every project, else artist.
func devRole(v *api.Viewer) string {
	switch {
	case v == nil:
		return ""
	case v.Role != "":
		return v.Role
	case v.All:
		return "wrangler"
	}
	return "artist"
}

// devMay says whether development viewer v may verb resource in ns.
func (s *Server) devMay(v *api.Viewer, ns, verb, resource string) error {
	role := devRole(v)
	if !slices.Contains(devWriters[resource], role) && !s.everyRole(context.Background(), v) {
		return fmt.Errorf("%s (%s) may not %s %s: on this farm that takes %s", v.User, role, verb, resource, strings.Join(devWriters[resource], ", "))
	}
	if ns == "" || v.All {
		return nil
	}
	if m := s.current(); m != nil {
		for _, p := range m.projects {
			if slices.Contains(p.Spec.Namespaces, ns) && v.Sees(p.Name) {
				return nil
			}
		}
	}
	return fmt.Errorf("%s may not %s %s in %s, which is in no project they see", v.User, verb, resource, ns)
}
