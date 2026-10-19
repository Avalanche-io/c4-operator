package placement

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/tier"
	"github.com/Avalanche-io/c4-operator/work/process"
	"github.com/Avalanche-io/c4-operator/work/sched"
	"github.com/Avalanche-io/c4-operator/work/topology"
)

func id(s string) c4.ID { return c4.Identify(strings.NewReader(s)) }

var (
	plate, asset, cache = Object{ID: id("plate"), Size: 4e9}, Object{ID: id("asset"), Size: 1e9}, Object{ID: id("cache"), Size: 2e9}
)

type fakeViews map[string][]Object

func (f fakeViews) Objects(_ context.Context, _, v string) ([]Object, error) { return f[v], nil }

type fakeHoldings struct {
	mu   sync.Mutex
	held map[string]map[c4.ID]bool // by site
}

func (f *fakeHoldings) Held(_ context.Context, site, _ string, ids []c4.ID) (map[c4.ID]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[c4.ID]bool{}
	for _, i := range ids {
		out[i] = f.held[site][i]
	}
	return out, nil
}

type fakeWarmer struct {
	calls [][]Object
}

func (f *fakeWarmer) Warm(_ context.Context, _, _ string, objs []Object) error {
	f.calls = append(f.calls, objs)
	return nil
}

func topo(t *testing.T) *topology.Topology {
	t.Helper()
	tp, err := topology.Parse([]byte(`{"version": 1,
	  "sites": [{"name": "lon", "region": "eu-london", "kind": "onprem", "stores": ["show"]},
	            {"name": "van", "region": "ca-vancouver", "kind": "onprem"}],
	  "links": [{"name": "lon-van", "a": "lon", "b": "van", "channel": "interconnect", "gbps": 8, "latencySeconds": 60, "egressPerGB": 0.02, "ingressPerGB": 0.02}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func provider(t *testing.T) (*Provider, *fakeHoldings, *fakeWarmer, *clock) {
	t.Helper()
	h := &fakeHoldings{held: map[string]map[c4.ID]bool{"van": {asset.ID: true}}}
	w := &fakeWarmer{}
	c := &clock{t: time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)}
	p := &Provider{Topo: topo(t), Views: fakeViews{"plates": {plate, asset}, "fx": {asset, cache}}, Holdings: h, Warmer: w, Now: c.now}
	return p, h, w, c
}

var inputs = []process.Input{{Mount: "/plates", View: "plates"}, {Mount: "/fx", View: "fx"}}

// What van lacks is the inputs' objects less what it holds, each object
// counted once across views; it is priced over the link with outputs back.
func TestSourcesMoveWhatTheSiteLacks(t *testing.T) {
	p, _, _, _ := provider(t)
	if s := p.Sources("show", inputs, "van", false); s != nil {
		t.Fatalf("sources before anything is known: %+v", s)
	}
	p.Refresh(context.Background())
	s := p.Sources("show", inputs, "van", false)
	if len(s) != 1 || s[0].Via != "move" || s[0].Link != "lon-van" || s[0].Bytes != 6e9 || s[0].Latency != time.Minute {
		t.Fatalf("sources %+v", s)
	}
	// 6 GB in and 1.2 GB back at $0.04/GB.
	if math.Abs(s[0].Cost-0.288) > 1e-9 {
		t.Fatalf("cost %v", s[0].Cost)
	}
	if s := p.Sources("show", inputs, "lon", false); s != nil {
		t.Fatalf("the store's site lacks nothing: %+v", s)
	}
}

// A stage warms van with what it lacks, commits its bytes to the link,
// and the inputs read as on their way until the site holds them.
func TestStageWarmsAndCommits(t *testing.T) {
	p, h, w, c := provider(t)
	p.Sources("show", inputs, "van", false) // the queue asks; Refresh learns
	p.Refresh(context.Background())
	src := p.Sources("show", inputs, "van", false)[0]
	if err := p.Stage(context.Background(), "show", inputs, "van", sched.Stage{Source: src}); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 1 || len(w.calls[0]) != 2 {
		t.Fatalf("warm calls %+v", w.calls)
	}
	// 6 GB at 1 GB/s: 6 s committed; it lands after the minute of latency.
	if l := p.Links()[0]; math.Abs(l.Committed-6e9) > 1 {
		t.Fatalf("link %+v", l)
	}
	s := p.Sources("show", inputs, "van", false)
	if len(s) != 1 || s[0].Via != "staging" || s[0].Latency != 66*time.Second {
		t.Fatalf("while on its way: %+v", s)
	}
	// Landed: the next check finds it held.
	h.mu.Lock()
	h.held["van"][plate.ID], h.held["van"][cache.ID] = true, true
	h.mu.Unlock()
	c.t = c.t.Add(10 * time.Minute)
	p.Sources("show", inputs, "van", false) // stale: queued for a check
	p.Refresh(context.Background())
	if s := p.Sources("show", inputs, "van", false); s != nil {
		t.Fatalf("after landing: %+v", s)
	}
}

type recomputeCall struct {
	project string
	key     process.Key
	inputs  []process.Input
	site    string
	view    string
	by      time.Time
}

type fakeRecomputer struct{ calls *[]recomputeCall }

func (f fakeRecomputer) Recompute(_ context.Context, project string, key process.Key, inputs []process.Input, site, view string, by time.Time) error {
	if f.calls != nil {
		*f.calls = append(*f.calls, recomputeCall{project, key, inputs, site, view, by})
	}
	return nil
}

// Recompute is offered for exact processes, for validated ones only to a
// job that accepts them (the queue points its input at the recompute's
// output), and never for volatile ones.
func TestRecomputeOffers(t *testing.T) {
	for _, c := range []struct {
		class  process.Class
		accept bool
		want   bool
	}{{process.Exact, false, true}, {process.Validated, false, false}, {process.Validated, true, true}, {process.Volatile, true, false}} {
		p, _, _, _ := provider(t)
		p.Recomputer = fakeRecomputer{}
		p.Cost = func(_, output string, _ float64) (process.Key, []process.Input, process.Cost, bool) {
			if output != "fx" {
				return process.Key{}, nil, process.Cost{}, false
			}
			return process.Key{}, []process.Input{{View: "plates"}}, process.Cost{W90: 3600, Longest: 600, Class: c.class}, true
		}
		p.Sources("show", inputs, "van", c.accept)
		p.Refresh(context.Background())
		got := false
		for _, s := range p.Sources("show", inputs, "van", c.accept) {
			if s.Via == "recompute" {
				got = true
				// It moves the plate (its own input, which the job's other
				// input also needs) and not the fx cache.
				if s.Bytes != 4e9 || s.Work != 3600 || s.Longest != 10*time.Minute || s.Class != string(c.class) {
					t.Fatalf("%s: recompute %+v", c.class, s)
				}
			}
		}
		if got != c.want {
			t.Fatalf("%s accept=%v: recompute offered %v", c.class, c.accept, got)
		}
	}
}

// A recompute stage moves what van lacks of the computation's inputs and
// the job's other inputs (the plate), has the Recomputer rebuild fx at van
// due when the stage's delay ends, and counts fx as on its way
// until then: the job reads as staging, not as lacking fx.
func TestStageRecompute(t *testing.T) {
	p, _, w, c := provider(t)
	var calls []recomputeCall
	p.Recomputer = fakeRecomputer{&calls}
	key := process.Key{Process: "render-fx", Inputs: "plates"}
	p.Cost = func(_, output string, _ float64) (process.Key, []process.Input, process.Cost, bool) {
		if output != "fx" {
			return process.Key{}, nil, process.Cost{}, false
		}
		return key, []process.Input{{Mount: "/in", View: "plates"}}, process.Cost{W90: 3600, Longest: 600, Class: process.Exact}, true
	}
	p.Sources("show", inputs, "van", false)
	p.Refresh(context.Background())
	var src sched.Source
	for _, s := range p.Sources("show", inputs, "van", false) {
		if s.Via == "recompute" {
			src = s
		}
	}
	if src.View != "fx" {
		t.Fatalf("recompute source %+v", src)
	}
	if err := p.Stage(context.Background(), "show", inputs, "van", sched.Stage{Source: src, Delay: 20 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 1 || len(w.calls[0]) != 1 || w.calls[0][0].ID != plate.ID {
		t.Fatalf("warm calls %+v, want the plate alone", w.calls)
	}
	due := c.t.Add(20 * time.Minute)
	if len(calls) != 1 || calls[0].key != key || calls[0].site != "van" || calls[0].view != "fx" || !calls[0].by.Equal(due) || len(calls[0].inputs) != 1 || calls[0].inputs[0].View != "plates" {
		t.Fatalf("recompute calls %+v", calls)
	}
	s := p.Sources("show", inputs, "van", false)
	if len(s) != 1 || s[0].Via != "staging" || s[0].Latency != 20*time.Minute {
		t.Fatalf("while recomputing: %+v", s)
	}
}

func TestSite(t *testing.T) {
	p, _, _, _ := provider(t)
	attr := &c4v1.Pool{Spec: c4v1.PoolSpec{Attributes: map[string]string{"site": "van"}}}
	plain := &c4v1.Pool{}
	switch {
	case p.Site(attr, nil) != "van":
		t.Fatal("site attribute")
	case p.Site(plain, []string{"eu-london", "eu-london"}) != "lon":
		t.Fatal("site from region")
	case p.Site(plain, []string{"eu-london", "ca-vancouver"}) != "":
		t.Fatal("a pool across sites has no site")
	}
}

// Regional asks each object's home with X-C4-Cached: only and the
// operator's token, and sends /warm the objects homed there.
func TestRegional(t *testing.T) {
	var mu sync.Mutex
	var warmed []tier.WarmObject
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer op" || r.Header.Get(tier.HeaderProject) != "show" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodHead:
			if r.Header.Get(tier.HeaderCached) != "only" {
				http.Error(w, "want cached-only", http.StatusBadRequest)
				return
			}
			if strings.TrimPrefix(r.URL.Path, "/") == asset.ID.String() {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPost:
			var req tier.WarmRequest
			json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			warmed = append(warmed, req.Objects...)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	tp := topo(t)
	tp.Site("van").Regional = []string{strings.TrimPrefix(srv.URL, "http://")}
	r := &Regional{Topo: tp, Token: func() string { return "op" }}
	held, err := r.Held(context.Background(), "van", "show", []c4.ID{plate.ID, asset.ID})
	if err != nil || held[plate.ID] || !held[asset.ID] {
		t.Fatalf("held %v %v", held, err)
	}
	if err := r.Warm(context.Background(), "van", "show", []Object{plate}); err != nil {
		t.Fatal(err)
	}
	if len(warmed) != 1 || warmed[0].ID != plate.ID.String() || warmed[0].Size != 4e9 {
		t.Fatalf("warmed %+v", warmed)
	}
}
