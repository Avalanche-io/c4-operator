package placement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/rendezvous"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/tier"
	"github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/work/topology"
)

// Regional reaches each site's regional replicas (topology.Site.Regional)
// over the tier API: Holdings by HEAD with X-C4-Cached: only at each
// object's home, Warmer by POST /warm to each home with the objects homed
// there. The token is the operator's own (audience c4-tier); the replicas
// admit /warm for it alone.
type Regional struct {
	Topo   *topology.Topology
	Client *http.Client
	Token  tier.TokenFunc
	// Workers bounds concurrent HEADs per call (default 32).
	Workers int
}

func (r *Regional) members(site string) ([]string, error) {
	s := r.Topo.Site(site)
	if s == nil || len(s.Regional) == 0 {
		return nil, fmt.Errorf("site %s has no regional replicas", site)
	}
	return s.Regional, nil
}

func (r *Regional) do(req *http.Request, project string) (*http.Response, error) {
	req.Header.Set(tier.HeaderProject, project)
	if r.Token != nil {
		if t := r.Token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	c := r.Client
	if c == nil {
		c = http.DefaultClient
	}
	return c.Do(req)
}

// Held asks each object's home replica at site whether it holds it.
func (r *Regional) Held(ctx context.Context, site, project string, ids []c4.ID) (map[c4.ID]bool, error) {
	members, err := r.members(site)
	if err != nil {
		return nil, err
	}
	workers := r.Workers
	if workers <= 0 {
		workers = 32
	}
	out := make(map[c4.ID]bool, len(ids))
	var mu sync.Mutex
	var first error
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id c4.ID) {
			defer wg.Done()
			defer func() { <-sem }()
			home := rendezvous.Rank(members, id[:])[0]
			req, _ := http.NewRequestWithContext(ctx, http.MethodHead, "http://"+home+"/"+id.String(), nil)
			req.Header.Set(tier.HeaderCached, "only")
			resp, err := r.do(req, project)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
				}
				return
			}
			resp.Body.Close()
			out[id] = resp.StatusCode == http.StatusOK
		}(id)
	}
	wg.Wait()
	return out, first
}

// Warm asks each object's home replica at site to fetch it.
func (r *Regional) Warm(ctx context.Context, site, project string, objs []Object) error {
	members, err := r.members(site)
	if err != nil {
		return err
	}
	byHome := map[string][]tier.WarmObject{}
	for _, o := range objs {
		home := rendezvous.Rank(members, o.ID[:])[0]
		byHome[home] = append(byHome[home], tier.WarmObject{ID: o.ID.String(), Size: o.Size})
	}
	for home, list := range byHome {
		body, _ := json.Marshal(tier.WarmRequest{Objects: list})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+home+"/warm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := r.do(req, project)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("warm at %s (%s): %s", site, home, resp.Status)
		}
	}
	return nil
}

// StoreViews lists views from each project's store.
type StoreViews struct {
	Stores func(ctx context.Context, project string) (*storeurl.Sharded, error)
}

// Objects loads the view and returns its unique objects.
func (s StoreViews) Objects(ctx context.Context, project, id string) ([]Object, error) {
	vid, err := c4.Parse(id)
	if err != nil {
		return nil, err
	}
	st, err := s.Stores(ctx, project)
	if err != nil {
		return nil, err
	}
	v, err := view.Load(ctx, vid, view.StoreRecords(st), 16)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, o := range v.Objects() {
		out = append(out, Object{ID: o.ID, Size: o.Size})
	}
	return out, nil
}
