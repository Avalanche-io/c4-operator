package server

import (
	"context"
	"time"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// hudFarm is the farm as checking and lowering a submission or a tool see
// it (authoring.Farm), from the HUD's model and, for what the model does
// not keep (Milestones, tools), the API server.
type hudFarm struct {
	s   *Server
	ctx context.Context
	m   *model
}

// farmFor is the authoring.Farm of model m for one request.
func (s *Server) farmFor(ctx context.Context, m *model) authoring.Farm {
	return &hudFarm{s: s, ctx: ctx, m: m}
}

func (f *hudFarm) Tools(project string) []authoring.ToolRef {
	return f.s.toolRefs(f.ctx, f.m, project)
}

func (f *hudFarm) Tool(project, name, version string) (*authoring.Tool, authoring.ToolRef, error) {
	return f.s.toolAt(f.ctx, f.m, project, name, version)
}

func (f *hudFarm) Jobs(ns string) []string {
	var out []string
	for _, j := range f.m.jobs {
		if j.job.Namespace == ns {
			out = append(out, j.job.Name)
		}
	}
	return out
}

func (f *hudFarm) Milestones(ns string) map[string]time.Time {
	out := map[string]time.Time{}
	var l c4v1.MilestoneList
	if f.s.Client == nil || f.s.Client.List(f.ctx, &l) != nil {
		return out
	}
	for _, ms := range l.Items {
		if ms.Namespace == ns {
			out[ms.Name] = ms.Spec.Due.Time
		}
	}
	return out
}

// People, Roles and Parties: the HUD keeps no directory of them, so nil,
// which the check takes as not known rather than empty. Approvals and
// waits are refused at lowering for now anyway.
func (f *hudFarm) People() []string  { return nil }
func (f *hudFarm) Roles() []string   { return nil }
func (f *hudFarm) Parties() []string { return nil }

func (f *hudFarm) Root(project string) string {
	for _, p := range f.m.projects {
		if p.Name == project {
			return p.Spec.TreeRoot()
		}
	}
	return c4v1.DefaultProjectRoot
}

func (f *hudFarm) Namespace(project string) string {
	for _, p := range f.m.projects {
		if p.Name == project && len(p.Spec.Namespaces) > 0 {
			return p.Spec.Namespaces[0]
		}
	}
	return ""
}

// Cut: the farm keeps no shot's cut yet (a tracker sync would bring it),
// so a frames job must give its frames or follow a job that has them.
func (f *hudFarm) Cut(project, shot string) string { return "" }

// Location is the zone of the project's due-time convention.
func (f *hudFarm) Location(project string) *time.Location {
	for _, p := range f.m.projects {
		if p.Name != project || p.Spec.Work == nil || p.Spec.Work.DueTime == nil || p.Spec.Work.DueTime.TimeZone == "" {
			continue
		}
		if loc, err := time.LoadLocation(p.Spec.Work.DueTime.TimeZone); err == nil {
			return loc
		}
	}
	return time.UTC
}

// Policy: the farm keeps no policy model a submission can narrow yet, so
// nothing to check against; lowering refuses policy: for now.
func (f *hudFarm) Policy(string) *authoring.FarmPolicy { return nil }

// Trackers are the Trackers in the project's namespace.
func (f *hudFarm) Trackers(project string) []string {
	ns := f.Namespace(project)
	var l c4v1.TrackerList
	if ns == "" || f.s.Client == nil || f.s.Client.List(f.ctx, &l, client.InNamespace(ns)) != nil {
		return nil
	}
	out := []string{}
	for _, t := range l.Items {
		out = append(out, t.Name)
	}
	return out
}
