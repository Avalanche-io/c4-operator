package node

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/fill"
	"github.com/Avalanche-io/c4-operator/internal/membership"
	"github.com/Avalanche-io/c4-operator/internal/metrics"
	"github.com/Avalanche-io/c4-operator/internal/rendezvous"
	"github.com/Avalanche-io/c4-operator/internal/tier"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// regionalSources returns the regional replicas for an object, starting
// at the one this node's name selects among the object's top r homes, so
// a hot object is served by r replicas, not one.
func (d *Daemon) regionalSources(ctx context.Context, project string, key []byte) []tier.Source {
	members, err := d.Members.Regional(ctx)
	if err != nil || len(members) == 0 {
		return nil
	}
	byName := map[string]membership.Member{}
	for _, m := range members {
		byName[m.Name] = m
	}
	order := rendezvous.Spread(membership.Names(members), key, d.Cfg.RegionalR, d.Cfg.NodeName)
	out := make([]tier.Source, 0, len(order))
	for _, name := range order {
		m := byName[name]
		out = append(out, &tier.HTTPSource{SourceName: m.Name, SourceKind: "regional", Base: "http://" + m.Addr, Auth: true, Token: d.Token, Client: d.httpc})
	}
	return out
}

// homes returns the object's rendezvous homes in this locality, and
// whether this node is one of them.
func (d *Daemon) homes(ctx context.Context, key []byte) ([]membership.Member, bool) {
	peers, err := d.Members.Peers(ctx)
	if err != nil || len(peers) == 0 {
		return nil, true
	}
	byName := map[string]membership.Member{}
	for _, m := range peers {
		byName[m.Name] = m
	}
	top := rendezvous.Top(membership.Names(peers), key, d.Cfg.HomesK)
	out := make([]membership.Member, 0, len(top))
	self := false
	for _, name := range top {
		if name == d.Cfg.NodeName {
			self = true
			continue
		}
		out = append(out, byName[name])
	}
	return out, self
}

// localRoute is where this node fetches an object it needs: its homes
// first unless it is one, then the regional tier. A Direct project reads
// its store shard instead.
func (d *Daemon) localRoute(pc *projectCtx, id c4.ID) tier.Route {
	if pc.direct != nil {
		src := tier.ShardSource{Shard: pc.direct.For(id)}
		return func() []tier.Source { return []tier.Source{src} }
	}
	key := objectKey(pc.part, id)
	project := pc.p.Name
	return func() []tier.Source {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var out []tier.Source
		homes, self := d.homes(ctx, key)
		if !self {
			for _, h := range homes {
				out = append(out, &tier.HTTPSource{SourceName: h.Name, SourceKind: "peer", Base: "http://" + h.Addr, Hop: tier.HopPeer, Token: d.Token, Client: d.httpc})
			}
		}
		return append(out, d.regionalSources(ctx, project, key)...)
	}
}

// peerRoute answers a peer's request: a home fetches what it lacks from
// the regional tier (or its store, for a Direct project), coalesced with
// any other request for the object; a non-home answers 404 and never
// fetches, so requests never cascade from peer to peer.
func (d *Daemon) peerRoute(r *http.Request, project string, id c4.ID) tier.Route {
	p, err := d.Projects.Get(r.Context(), project)
	if err != nil {
		return nil
	}
	pc, err := d.projectCtx(r.Context(), p)
	if err != nil {
		return nil
	}
	key := objectKey(pc.part, id)
	if _, self := d.homes(r.Context(), key); !self {
		return nil
	}
	if pc.direct != nil {
		src := tier.ShardSource{Shard: pc.direct.For(id)}
		return func() []tier.Source { return []tier.Source{src} }
	}
	return func() []tier.Source {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return d.regionalSources(ctx, project, key)
	}
}

// regionalForUpload lists the regional replicas a captured object goes
// to: its regional home first, so the next stage finds it cached there.
func (d *Daemon) regionalForUpload(project string, id c4.ID) []*tier.HTTPSource {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := d.Projects.Get(ctx, project)
	if err != nil {
		return nil
	}
	members, err := d.Members.Regional(ctx)
	if err != nil {
		return nil
	}
	byName := map[string]membership.Member{}
	for _, m := range members {
		byName[m.Name] = m
	}
	var out []*tier.HTTPSource
	for _, name := range rendezvous.Rank(membership.Names(members), objectKey(p.Partition(), id)) {
		m := byName[name]
		out = append(out, &tier.HTTPSource{SourceName: m.Name, SourceKind: "regional", Base: "http://" + m.Addr, Auth: true, Token: d.Token, Client: d.httpc})
	}
	return out
}

// fetchResult is how an object became present.
type fetchResult int

const (
	fetchHit fetchResult = iota
	fetchJoin
	fetchFill
)

// fetchObject makes id present in the partition's cache: a hit, a join
// onto a fill already in flight (a peer's request or another publish), or
// a new fill through the tiers.
func (d *Daemon) fetchObject(ctx context.Context, pc *projectCtx, id c4.ID) (fetchResult, error) {
	if pc.cache.Has(id) {
		pc.cache.Touch(id)
		return fetchHit, nil
	}
	key := pc.part + "/" + id.String()
	for {
		f, started, err := d.fills.Join(key, pc.cache.TempFile, func(f *fill.Flight) { d.runFill(f, pc, id) })
		if errors.Is(err, fill.ErrBusy) {
			// Admission control applies to peers; a local publish waits
			// its turn rather than failing.
			select {
			case <-time.After(50 * time.Millisecond):
				continue
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		if err != nil {
			return 0, err
		}
		err = f.Wait(ctx)
		f.Release()
		if err != nil {
			return 0, err
		}
		if started {
			return fetchFill, nil
		}
		return fetchJoin, nil
	}
}

func (d *Daemon) runFill(f *fill.Flight, pc *projectCtx, id c4.ID) {
	ctx, cancel := context.WithTimeout(context.Background(), d.Cfg.PublishTimeout)
	defer cancel()
	fe := d.fetcher
	if pc.p.Spec.Store.VerifyFill {
		v := *d.fetcher
		v.Verify = true
		fe = &v
	}
	if err := fe.Fill(ctx, f, pc.p.Name, id, d.localRoute(pc, id)); err != nil {
		f.Fail(err)
		return
	}
	f.Commit(func(tmp string) (string, error) {
		if err := pc.cache.Commit(tmp, id); err != nil {
			return "", err
		}
		return pc.cache.Path(id), nil
	})
}

// records returns a record source that reads through this node's cache.
func (d *Daemon) records(pc *projectCtx) view.RecordSource {
	return view.RecordFunc(func(ctx context.Context, id c4.ID) ([]byte, error) {
		if _, err := d.fetchObject(ctx, pc, id); err != nil {
			return nil, err
		}
		return pc.cache.ReadAll(id)
	})
}

// ensureObjects makes every object present, in parallel, largest first.
func (d *Daemon) ensureObjects(ctx context.Context, pc *projectCtx, objs []view.Object) error {
	workers := d.Cfg.FetchWorkers
	var (
		wg    sync.WaitGroup
		next  atomic.Int64
		mu    sync.Mutex
		first error
		done  atomic.Int64
		of    int64
	)
	for _, o := range objs {
		of += max(o.Size, 0)
	}
	report := progressOf(ctx)
	report(0, of)
	for range min(workers, len(objs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(objs) {
					return
				}
				mu.Lock()
				stop := first != nil
				mu.Unlock()
				if stop {
					return
				}
				res, err := d.fetchObject(ctx, pc, objs[i].ID)
				if err != nil {
					mu.Lock()
					if first == nil {
						first = fmt.Errorf("object %s: %w", objs[i].ID, err)
					}
					mu.Unlock()
					return
				}
				metrics.NodeObjects.WithLabelValues(pc.p.Name, [...]string{"hit", "join", "fetch"}[res]).Inc()
				report(done.Add(max(objs[i].Size, 0)), of)
			}
		}()
	}
	wg.Wait()
	return first
}

// sink writes captured content: through the regional tier for a Tiered
// project, which recomputes every ID, or to the store for a Direct one.
func (d *Daemon) sink(pc *projectCtx) captureSink {
	if pc.direct != nil {
		return directSink{pc.direct}
	}
	return d.uploader
}
