package topology

import (
	"math"
	"strings"
	"testing"
)

const farm = `{
  "version": 1,
  "sites": [
    {"name": "lon", "region": "eu-london", "lat": 51.5, "lon": -0.1, "kind": "onprem", "tz": "Europe/London",
     "regional": ["lon-regional:8443"], "stores": ["tidecaller"]},
    {"name": "van", "region": "ca-vancouver", "lat": 49.3, "lon": -123.1, "kind": "onprem"}
  ],
  "links": [
    {"name": "lon-van-ic", "a": "lon", "b": "van", "channel": "interconnect", "gbps": 10, "latencySeconds": 60,
     "egressPerGB": 0.02, "ingressPerGB": 0.02},
    {"name": "lon-van-net", "a": "van", "b": "lon", "channel": "internet", "gbps": 1, "egressPerGB": 0.09}
  ]
}`

func TestParse(t *testing.T) {
	tp, err := Parse([]byte(farm))
	if err != nil {
		t.Fatal(err)
	}
	if tp.SiteOfRegion("ca-vancouver") != "van" || tp.StoreSite("tidecaller") != "lon" || tp.StoreSite("x") != "" {
		t.Fatalf("lookups: %+v", tp)
	}
	ls := tp.Between("van", "lon")
	if len(ls) != 2 || ls[0].Name != "lon-van-ic" {
		t.Fatalf("between: %+v", ls)
	}
	ic := ls[0]
	if ic.BytesPerSecond() != 1.25e9 || ic.Latency().Seconds() != 60 {
		t.Fatalf("throughput %v latency %v", ic.BytesPerSecond(), ic.Latency())
	}
	// 10 GB in, 2 GB back at $0.04/GB both ends.
	if c := ic.Cost(10e9, 2e9); math.Abs(c-0.48) > 1e-9 {
		t.Fatalf("cost %v", c)
	}
}

func TestParseBareSites(t *testing.T) {
	tp, err := Parse([]byte(`[{"name": "lon", "region": "eu-london", "kind": "onprem"}]`))
	if err != nil || tp.Version != Version || len(tp.Sites) != 1 {
		t.Fatalf("%+v %v", tp, err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, c := range []struct{ doc, want string }{
		{`{"version": 2, "sites": []}`, "version 2"},
		{`{"version": 1, "sites": [{"name": "a", "kind": "attic"}]}`, "kind"},
		{`{"version": 1, "sites": [{"name": "a", "kind": "cloud"}], "links": [{"name": "l", "a": "a", "b": "b", "channel": "internet", "gbps": 1}]}`, "two different sites"},
		{`{"version": 1, "sites": [{"name": "a", "kind": "cloud"}, {"name": "b", "kind": "cloud"}], "links": [{"name": "l", "a": "a", "b": "b", "channel": "pigeon", "gbps": 1}]}`, "channel"},
		{`{"version": 1, "sites": [{"name": "a", "kind": "cloud"}, {"name": "b", "kind": "cloud"}], "links": [{"name": "l", "a": "a", "b": "b", "channel": "internet"}]}`, "gbps"},
	} {
		if _, err := Parse([]byte(c.doc)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.doc, err, c.want)
		}
	}
}
