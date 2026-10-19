// Package placement (experimental) is the queue's Placement: which site a
// pool is at, what each site lacks of a job's inputs, the links between
// sites, and moving inputs ahead of staged work.
//
// What a site lacks is a C4 ID set difference: the objects of the views a
// job mounts, less those the site's regional replicas hold (HEAD with
// X-C4-Cached: only) and those already on their way there. It is learned
// in the background (Run) and cached, so the queue never waits on the
// network: a job whose inputs are not known yet gets no Sources and runs
// where it may, as without a Placement. An identical output needs no
// special case: it has the same IDs, so a site that holds it lacks nothing.
//
// Moving goes over the topology's links from the project's store site,
// first come first served on each link (a ledger of committed bytes), and
// is carried out by asking the target site's regional replicas to fetch the
// objects (POST /warm). Recomputing an input at the target site is offered
// only with a Recomputer, which runs the producing work there: for exact
// processes, which write the input the job names, and for validated ones
// to a job that accepts them, which the queue then points at the
// recompute's output at that site (input substitution); never for
// volatile ones.
package placement

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/process"
	"github.com/Avalanche-io/c4-operator/work/sched"
	"github.com/Avalanche-io/c4-operator/work/topology"
)

// Object is one object of a view.
type Object struct {
	ID   c4.ID
	Size int64
}

// Views lists the objects of a view.
type Views interface {
	Objects(ctx context.Context, project, view string) ([]Object, error)
}

// Holdings says which of ids a site's tiers hold themselves.
type Holdings interface {
	Held(ctx context.Context, site, project string, ids []c4.ID) (map[c4.ID]bool, error)
}

// Warmer asks a site's tiers to fetch objects.
type Warmer interface {
	Warm(ctx context.Context, site, project string, objs []Object) error
}

// RecomputeCost is the queue's Queue.RecomputeCost.
type RecomputeCost func(project, output string, u float64) (process.Key, []process.Input, process.Cost, bool)

// Recomputer runs the work that produces an output at a site.
type Recomputer interface {
	// view is the input it rebuilds; by is when the stage needs it.
	Recompute(ctx context.Context, project string, key process.Key, inputs []process.Input, site, view string, by time.Time) error
}

// Provider implements the queue's Placement.
type Provider struct {
	Topo     *topology.Topology
	Views    Views
	Holdings Holdings
	Warmer   Warmer
	// Cost and Recomputer enable recompute offers; either nil disables them.
	Cost       RecomputeCost
	Recomputer Recomputer
	// OutShare is outputs' size as a share of inputs', moved back over the
	// same link and priced with the move (default 0.2).
	OutShare float64
	// TTL is how long what a site lacks stands before it is checked again
	// (default 5 minutes).
	TTL time.Duration
	Now func() time.Time
	Log logr.Logger

	mu      sync.Mutex
	lack    map[lackKey]*lackEntry
	want    map[lackKey]bool
	objects map[string][]Object            // by project/view; views are immutable
	coming  map[string]map[c4.ID]time.Time // by site: objects on their way and when they land
	busy    map[string]time.Time           // by link: when it has moved what is committed to it
	// rebuilding is, by site, the views a recompute is rebuilding there
	// and when it is due: its output's objects are not known until it
	// has run (a validated one writes other bytes), so the view itself
	// counts as on its way.
	rebuilding map[string]map[string]time.Time
}

type lackKey struct{ site, project, view string }

type lackEntry struct {
	missing []Object
	at      time.Time
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) ttl() time.Duration {
	if p.TTL > 0 {
		return p.TTL
	}
	return 5 * time.Minute
}

func (p *Provider) outShare() float64 {
	if p.OutShare > 0 {
		return p.OutShare
	}
	return 0.2
}

func (p *Provider) init() {
	if p.lack == nil {
		p.lack, p.want = map[lackKey]*lackEntry{}, map[lackKey]bool{}
		p.objects, p.coming, p.busy = map[string][]Object{}, map[string]map[c4.ID]time.Time{}, map[string]time.Time{}
		p.rebuilding = map[string]map[string]time.Time{}
	}
}

// Site is the pool's "site" attribute, else the site of its nodes' region
// when they agree.
func (p *Provider) Site(pool *c4v1.Pool, regions []string) string {
	if s := pool.Spec.Attributes["site"]; s != "" && p.Topo.Site(s) != nil {
		return s
	}
	site := ""
	for _, r := range regions {
		s := p.Topo.SiteOfRegion(r)
		if s == "" || site != "" && s != site {
			return ""
		}
		site = s
	}
	return site
}

// missing is what site lacks of view, from the cache; ok false while it
// is not known. A stale or unknown entry is queued for Refresh.
func (p *Provider) missing(project, view, site string) ([]Object, bool) {
	k := lackKey{site, project, view}
	e := p.lack[k]
	if e == nil || p.now().Sub(e.at) > p.ttl() {
		p.want[k] = true
	}
	if e == nil {
		return nil, false
	}
	return e.missing, true
}

// lacking is what site lacks of views, less what is on its way, and when
// the last of what is on its way lands (zero: nothing is). A view a
// recompute is rebuilding at site is on its way until the recompute is
// due.
func (p *Provider) lacking(project string, views []string, site string) (objs []Object, landing time.Time, ok bool) {
	seen := map[c4.ID]bool{}
	coming := p.coming[site]
	var rest []string
	for _, v := range views {
		if due, on := p.rebuilding[site][v]; on {
			if due.After(landing) {
				landing = due
			}
			continue
		}
		rest = append(rest, v)
	}
	// Every view is asked about, so one Refresh learns them all.
	ok = true
	for _, v := range rest {
		if _, known := p.missing(project, v, site); !known {
			ok = false
		}
	}
	if !ok {
		return nil, time.Time{}, false
	}
	for _, v := range rest {
		m, _ := p.missing(project, v, site)
		for _, o := range m {
			if seen[o.ID] {
				continue
			}
			seen[o.ID] = true
			if at, on := coming[o.ID]; on {
				if at.After(landing) {
					landing = at
				}
				continue
			}
			objs = append(objs, o)
		}
	}
	return objs, landing, true
}

func size(objs []Object) float64 {
	n := 0.0
	for _, o := range objs {
		n += float64(o.Size)
	}
	return n
}

func viewsOf(inputs []process.Input) []string {
	out := make([]string, 0, len(inputs))
	for _, in := range inputs {
		out = append(out, in.View)
	}
	return out
}

// Sources lists the ways to get the inputs to site: nil when it has them,
// when its store is there, or while what it lacks is not known; one
// "staging" source when all it lacks is on its way; otherwise a move over
// each link from the project's store site, and with a Recomputer a
// recompute of each input whose producing process allows it.
func (p *Provider) Sources(project string, inputs []process.Input, site string, accept bool) []sched.Source {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init()
	origin := p.Topo.StoreSite(project)
	if origin == "" || origin == site || p.Topo.Site(site) == nil {
		return nil
	}
	objs, landing, ok := p.lacking(project, viewsOf(inputs), site)
	if !ok {
		return nil
	}
	now := p.now()
	if len(objs) == 0 {
		if landing.After(now) {
			return []sched.Source{{Via: "staging", Latency: landing.Sub(now)}}
		}
		return nil
	}
	bytes := size(objs)
	var out []sched.Source
	for _, l := range p.Topo.Between(origin, site) {
		out = append(out, sched.Source{Via: "move", Channel: l.Channel, Link: l.Name, Bytes: bytes,
			Latency: l.Latency(), Cost: l.Cost(bytes, bytes*p.outShare())})
	}
	if p.Cost != nil && p.Recomputer != nil {
		out = append(out, p.recomputes(project, inputs, site, origin, accept)...)
	}
	return out
}

// recomputes offers recomputing each input at site in place of moving it:
// moving that computation's own inputs and the job's other inputs instead.
func (p *Provider) recomputes(project string, inputs []process.Input, site, origin string, accept bool) []sched.Source {
	var out []sched.Source
	for i, in := range inputs {
		_, cin, cost, ok := p.Cost(project, in.View, 1)
		if !ok || !(cost.Class == process.Exact || cost.Class == process.Validated && accept) {
			continue
		}
		views := viewsOf(cin)
		for j, other := range inputs {
			if j != i {
				views = append(views, other.View)
			}
		}
		objs, _, known := p.lacking(project, views, site)
		if !known {
			continue
		}
		bytes := size(objs)
		for _, l := range p.Topo.Between(origin, site) {
			out = append(out, sched.Source{Via: "recompute", Channel: l.Channel, Link: l.Name, Bytes: bytes,
				Latency: l.Latency(), Cost: l.Cost(bytes, bytes*p.outShare()), Work: cost.W90,
				Longest: time.Duration(cost.Longest * float64(time.Second)), Class: string(cost.Class), View: in.View})
		}
	}
	return out
}

// Links are the topology's links with the bytes still committed to each.
func (p *Provider) Links() []sched.Link {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init()
	now := p.now()
	out := make([]sched.Link, 0, len(p.Topo.Links))
	for _, l := range p.Topo.Links {
		left := math.Max(0, p.busy[l.Name].Sub(now).Seconds())
		out = append(out, sched.Link{Name: l.Name, BytesPerSecond: l.BytesPerSecond(), Committed: left * l.BytesPerSecond()})
	}
	return out
}

// Stage carries out an approved stage. A move asks the site's tiers to
// fetch what the site lacks of the inputs over the stage's link, commits
// their bytes to the link behind what is already committed, and counts
// them as on their way until they land. A recompute is stageRecompute.
func (p *Provider) Stage(ctx context.Context, project string, inputs []process.Input, site string, st sched.Stage) error {
	if st.Source.Via == "recompute" {
		return p.stageRecompute(ctx, project, inputs, site, st)
	}
	p.mu.Lock()
	p.init()
	objs, _, ok := p.lacking(project, viewsOf(inputs), site)
	link := p.link(st.Source.Link)
	p.mu.Unlock()
	if !ok || len(objs) == 0 || link == nil {
		return nil
	}
	if err := p.Warmer.Warm(ctx, site, project, objs); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.commit(link, site, objs, time.Time{})
	return nil
}

// stageRecompute rebuilds the input the stage names (Source.View) at
// site, as recomputes priced it: it moves what the site lacks of that
// computation's own inputs and of the job's other inputs, then has the
// Recomputer run the computation there, due when the stage's delay ends.
// The rebuilt input counts as on its way until then, so the job reads as
// staging and is not staged again.
func (p *Provider) stageRecompute(ctx context.Context, project string, inputs []process.Input, site string, st sched.Stage) error {
	if p.Cost == nil || p.Recomputer == nil {
		return nil
	}
	key, cin, _, ok := p.Cost(project, st.Source.View, 1)
	if !ok {
		return nil
	}
	views := viewsOf(cin)
	for _, in := range inputs {
		if in.View != st.Source.View {
			views = append(views, in.View)
		}
	}
	p.mu.Lock()
	p.init()
	objs, _, known := p.lacking(project, views, site)
	link := p.link(st.Source.Link)
	p.mu.Unlock()
	if !known || link == nil {
		return nil
	}
	if len(objs) > 0 {
		if err := p.Warmer.Warm(ctx, site, project, objs); err != nil {
			return err
		}
		p.mu.Lock()
		p.commit(link, site, objs, time.Time{})
		p.mu.Unlock()
	}
	due := p.now().Add(st.Delay)
	if err := p.Recomputer.Recompute(ctx, project, key, cin, site, st.Source.View, due); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rebuilding[site] == nil {
		p.rebuilding[site] = map[string]time.Time{}
	}
	p.rebuilding[site][st.Source.View] = due
	return nil
}

// link is the topology's link of that name.
func (p *Provider) link(name string) *topology.Link {
	for i := range p.Topo.Links {
		if p.Topo.Links[i].Name == name {
			return &p.Topo.Links[i]
		}
	}
	return nil
}

// commit counts objs as on their way to site: over link, behind what is
// committed to it, landing after its latency; or, with no link, landing
// at land.
func (p *Provider) commit(link *topology.Link, site string, objs []Object, land time.Time) {
	if link != nil {
		now := p.now()
		start := p.busy[link.Name]
		if start.Before(now) {
			start = now
		}
		done := start.Add(time.Duration(size(objs) / link.BytesPerSecond() * float64(time.Second)))
		p.busy[link.Name] = done
		land = done.Add(link.Latency())
	}
	if p.coming[site] == nil {
		p.coming[site] = map[c4.ID]time.Time{}
	}
	for _, o := range objs {
		p.coming[site][o.ID] = land
	}
}

// Refresh learns what sites lack for the views the queue asked about, and
// forgets objects that should have landed (the next check finds them).
func (p *Provider) Refresh(ctx context.Context) {
	p.mu.Lock()
	p.init()
	keys := make([]lackKey, 0, len(p.want))
	for k := range p.want {
		keys = append(keys, k)
	}
	p.want = map[lackKey]bool{}
	now := p.now()
	for site, m := range p.coming {
		for id, at := range m {
			if now.After(at.Add(p.ttl())) {
				delete(m, id)
			}
		}
		if len(m) == 0 {
			delete(p.coming, site)
		}
	}
	for site, m := range p.rebuilding {
		for view, due := range m {
			if now.After(due.Add(p.ttl())) {
				delete(m, view)
			}
		}
		if len(m) == 0 {
			delete(p.rebuilding, site)
		}
	}
	p.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].site != keys[j].site {
			return keys[i].site < keys[j].site
		}
		return keys[i].view < keys[j].view
	})
	for _, k := range keys {
		objs, err := p.objectsOf(ctx, k.project, k.view)
		if err != nil {
			p.Log.V(1).Info("placement: view", "view", k.view, "err", err.Error())
			continue
		}
		ids := make([]c4.ID, len(objs))
		for i, o := range objs {
			ids[i] = o.ID
		}
		held, err := p.Holdings.Held(ctx, k.site, k.project, ids)
		if err != nil {
			p.Log.V(1).Info("placement: holdings", "site", k.site, "err", err.Error())
			continue
		}
		var miss []Object
		for _, o := range objs {
			if !held[o.ID] {
				miss = append(miss, o)
			}
		}
		p.mu.Lock()
		p.lack[k] = &lackEntry{missing: miss, at: p.now()}
		p.mu.Unlock()
	}
}

func (p *Provider) objectsOf(ctx context.Context, project, view string) ([]Object, error) {
	key := project + "/" + view
	p.mu.Lock()
	objs, ok := p.objects[key]
	p.mu.Unlock()
	if ok {
		return objs, nil
	}
	objs, err := p.Views.Objects(ctx, project, view)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.objects[key] = objs
	p.mu.Unlock()
	return objs, nil
}

// Run refreshes every interval until ctx ends.
func (p *Provider) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		p.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
