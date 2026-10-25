package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/egress"
)

// sealed checks a job's networks at submit (TOOL 5): every network its
// tool names is one the farm defines, refused here rather than left
// waiting at admission; and a job a unit submits reaches no network its
// own job does not, so a unit cannot widen itself by submitting. own is
// the unit's own job, nil when the viewer is not a unit.
func (s *Server) sealed(ctx context.Context, v *api.Viewer, spec *c4v1.WorkJobSpec, own *c4v1.WorkJob) (int, error) {
	var nets []string
	if spec.Tool != nil {
		nets = spec.Tool.Network
	}
	if own != nil {
		var may []string
		if own.Spec.Tool != nil {
			may = own.Spec.Tool.Network
		}
		var wider []string
		for _, n := range nets {
			if !slices.Contains(may, n) && !slices.Contains(wider, n) {
				wider = append(wider, n)
			}
		}
		if len(wider) > 0 {
			them := "them"
			if len(wider) == 1 {
				them = "it"
			}
			reach := "no network"
			if len(may) > 0 {
				reach = "only " + strings.Join(may, ", ")
			}
			return http.StatusForbidden, identityError{fmt.Errorf("%s: a unit's job reaches no network its own job does not: %s/%s reaches %s, and this job would reach %s; submit it as a person, or as a unit whose tool declares %s",
				v.User, own.Namespace, own.Name, reach, strings.Join(wider, ", "), them)}
		}
	}
	if len(nets) == 0 {
		return 0, nil
	}
	defined, err := egress.LoadNetworks(ctx, s.reader(), s.Namespace)
	if err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("tool %s: the farm's networks could not be read: %v", spec.Tool.Name, err)
	}
	if _, missing := defined.Expand(nets); len(missing) > 0 {
		return http.StatusUnprocessableEntity, fmt.Errorf("tool: %s", egress.Missing(spec.Tool.Name, missing, s.Namespace))
	}
	return 0, nil
}
