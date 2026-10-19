package queue

import (
	"context"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/process"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Placement (experimental) tells the queue which site each pool's nodes are
// at and what it takes to get a job's inputs to another site, so the policy
// can move work there (sched.Job.Sources, sched.Decisions.Stage). With a nil
// Placement every pool a job may use is treated as holding its inputs, as
// before: units read them through the tiers wherever they run.
//
// The queue calls it once per tick, never waiting on the network: an
// implementation answers from what it already knows and learns the rest in
// the background (internal/work/placement).
type Placement interface {
	// Site is the site of a pool, from its "site" attribute or the
	// topology.kubernetes.io/region of its nodes; "" when unknown, which
	// treats the pool as holding every input.
	Site(pool *c4v1.Pool, regions []string) string
	// Sources lists the ways to get a job's inputs (the views it mounts)
	// to site. Nil means the site has them, or what it lacks is not known
	// yet. acceptValidated says the job accepts a validated recompute of
	// an input (AnnotationAcceptValidated).
	Sources(project string, inputs []process.Input, site string, acceptValidated bool) []sched.Source
	// Links are the links between sites, with the bytes committed to them.
	Links() []sched.Link
	// Stage starts an approved stage: moving the job's inputs to site.
	Stage(ctx context.Context, project string, inputs []process.Input, site string, st sched.Stage) error
}

// placeJobs fills each job's Sources from the Placement: for every pool the
// job may use at a site, the ways to get its inputs there, when the site
// lacks them.
func (q *Queue) placeJobs(ctx context.Context, views []*view, pools map[string]*pool) {
	if q.Placement == nil {
		return
	}
	for _, v := range views {
		v.sources = nil
		if v.mem.plan == nil || isFinished(v) {
			continue
		}
		inputs, ok := q.inputViews(ctx, v.job)
		if !ok || len(inputs) == 0 {
			continue
		}
		accept := v.job.Annotations[c4v1.AnnotationAcceptValidated] == "true"
		for _, name := range v.pools {
			p := pools[name]
			if p == nil || p.site == "" {
				continue
			}
			// At a site holding a substitute for an input, the job reads
			// that (substitute.go).
			if srcs := q.Placement.Sources(v.project, q.substitute(ctx, v, inputs, p.site), p.site, accept); srcs != nil {
				if v.sources == nil {
					v.sources = map[string][]sched.Source{}
				}
				v.sources[name] = srcs
			}
		}
	}
}

// stage carries out the policy's approved stages.
func (q *Queue) stage(ctx context.Context, dec sched.Decisions, views []*view, pools map[string]*pool) {
	if q.Placement == nil || len(dec.Stage) == 0 {
		return
	}
	byKey := map[string]*view{}
	for _, v := range views {
		byKey[v.key] = v
	}
	for _, st := range dec.Stage {
		v, p := byKey[st.Job], pools[st.Pool]
		if !st.Auto || v == nil || p == nil || p.site == "" {
			continue
		}
		inputs, ok := q.inputViews(ctx, v.job)
		if !ok {
			continue
		}
		if err := q.Placement.Stage(ctx, v.project, inputs, p.site, st); err != nil {
			q.Log.Error(err, "stage", "job", st.Job, "pool", st.Pool, "via", st.Source.Via)
			continue
		}
		q.Log.Info("staging", "job", st.Job, "pool", st.Pool, "site", p.site, "via", st.Source.Via, "channel", st.Source.Channel,
			"bytes", st.Source.Bytes, "delay", st.Delay.String(), "moveCost", st.MoveCost, "runCost", st.RunCost)
	}
}
